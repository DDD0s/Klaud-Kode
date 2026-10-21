// Drives a real kloud-kode-body process over HTTP, the same way the brain does.
import { test, before, after } from "node:test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";

const here = path.dirname(fileURLToPath(import.meta.url));
const TOKEN = "t".repeat(48);
let tmp;
let root;
let base;
let proc;
let id = 0;

/** One JSON-RPC call over the streamable HTTP transport. */
async function rpc(method, params, { token = TOKEN, inPath = true } = {}) {
  const url = inPath ? `${base}/mcp/${token}` : `${base}/mcp`;
  const res = await fetch(url, {
    method: "POST",
    headers: {
      "content-type": "application/json",
      accept: "application/json, text/event-stream",
      ...(inPath ? {} : { authorization: `Bearer ${token}` }),
    },
    body: JSON.stringify({ jsonrpc: "2.0", id: ++id, method, params }),
  });
  const text = await res.text();
  if (!res.ok) return { status: res.status, text };
  // enableJsonResponse still labels single replies as SSE in some versions.
  const line = text.startsWith("event:") || text.startsWith("data:") ? /data: (.*)/.exec(text)?.[1] : text;
  return { status: res.status, body: JSON.parse(line) };
}

/** Call a tool and return its parsed JSON payload. */
async function call(name, args = {}) {
  const r = await rpc("tools/call", { name, arguments: args });
  assert.equal(r.status, 200, r.text);
  const res = r.body.result;
  assert.ok(res, JSON.stringify(r.body));
  const text = res.content?.[0]?.text ?? "";
  if (res.isError) return { isError: true, message: text };
  try {
    return JSON.parse(text);
  } catch {
    return { raw: text };
  }
}

before(async () => {
  tmp = fs.mkdtempSync(path.join(os.tmpdir(), "kloud-body-test-"));
  root = path.join(tmp, "root");
  fs.mkdirSync(path.join(root, "sub"), { recursive: true });
  fs.writeFileSync(path.join(root, "hello.txt"), "line one\nline two\nline three\n");
  fs.writeFileSync(path.join(root, "sub", "note.md"), "# Title\nneedle here\nmore\n");
  fs.writeFileSync(path.join(root, "sub", "other.md"), "nothing\n");
  const port = 21000 + Math.floor(Math.random() * 15000);
  base = `http://127.0.0.1:${port}`;
  fs.writeFileSync(
    path.join(tmp, "config.json"),
    JSON.stringify({ port, host: "127.0.0.1", roots: { main: root }, defaultRoot: "main", token: "c".repeat(48) })
  );
  proc = spawn(process.execPath, [path.join(here, "..", "server.mjs"), "http"], {
    env: { ...process.env, KLOUD_KODE_BODY_CONFIG: path.join(tmp, "config.json"), KLOUD_KODE_BODY_TOKEN: TOKEN },
    stdio: ["ignore", "pipe", "pipe"],
  });
  let log = "";
  proc.stdout.on("data", (d) => (log += d));
  await new Promise((resolve, reject) => {
    const t = setTimeout(() => reject(new Error("body did not start: " + log)), 10_000);
    proc.stderr.on("data", (d) => {
      log += d;
      if (log.includes("/mcp/")) {
        clearTimeout(t);
        resolve();
      }
    });
  });
  const init = await rpc("initialize", {
    protocolVersion: "2025-06-18",
    capabilities: {},
    clientInfo: { name: "test", version: "1" },
  });
  assert.equal(init.status, 200, init.text);
});

after(() => proc?.kill());

test("a wrong or missing token is refused", async () => {
  assert.equal((await rpc("tools/list", {}, { token: "nope" })).status, 401);
  const res = await fetch(`${base}/mcp`, { method: "POST", body: "{}" });
  assert.equal(res.status, 401);
});

test("the token also works as a bearer header", async () => {
  const r = await rpc("tools/list", {}, { inPath: false });
  assert.equal(r.status, 200, r.text);
  assert.ok(r.body.result.tools.length > 10);
});

test("an MCP SDK client connects with a bearer header", async () => {
  const client = new Client({ name: "body-test", version: "1" }, { capabilities: {} });
  const transport = new StreamableHTTPClientTransport(new URL(base + "/mcp"), {
    requestInit: { headers: { authorization: "Bearer " + TOKEN } },
  });
  try {
    await client.connect(transport);
    const result = await client.listTools();
    assert.ok(result.tools.some((tool) => tool.name === "system_info"));
  } finally {
    await client.close();
  }
});

test("healthz needs no token and says nothing sensitive", async () => {
  const r = await fetch(`${base}/healthz`);
  assert.equal(r.status, 200);
  const j = await r.json();
  assert.deepEqual(Object.keys(j).sort(), ["ok", "service", "version"]);
  assert.equal(j.service, "kloud-kode-body");
});

test("the tool list covers shell, files, processes and nested MCP", async () => {
  const { body } = await rpc("tools/list");
  const names = body.result.tools.map((t) => t.name);
  assert.ok(!names.includes("body_info"), "the old body_info name must be gone");
  for (const n of [
    "system_info",
    "run_command",
    "start_process",
    "read_process",
    "stop_process",
    "list_processes",
    "read_file",
    "write_file",
    "edit_file",
    "list_directory",
    "find_files",
    "search_files",
    "stat_path",
    "create_directory",
    "delete_path",
    "move_path",
    "list_mcp_servers",
    "call_mcp_tool",
  ]) {
    assert.ok(names.includes(n), `missing tool ${n}`);
  }
  for (const t of body.result.tools) {
    assert.ok(t.description && t.description.length > 15, `${t.name} needs a real description`);
  }
});

test("tool names and descriptions never narrate a second machine", async () => {
  const { body } = await rpc("tools/list");
  for (const t of body.result.tools) {
    assert.doesNotMatch(
      `${t.name} ${t.description}`,
      /body_info|body server|through this one|nested|launcher|tunnel/i,
      `${t.name} breaks island voice`
    );
  }
});

test("run_command separates stdout from stderr and reports the exit code", async () => {
  const r = await call("run_command", { command: "echo out-here && echo err-here 1>&2 && exit 3" });
  assert.equal(r.exitCode, 3);
  assert.match(r.stdout, /out-here/);
  assert.match(r.stderr, /err-here/);
  assert.doesNotMatch(r.stdout, /err-here/, "stderr must not leak into stdout");
  assert.ok(typeof r.durationMs === "number");
});

test("renamed body configuration and token are removed from command environments", async () => {
  const script = path.join(tmp, "check-env.mjs");
  fs.writeFileSync(script, "process.stdout.write(JSON.stringify(Object.keys(process.env).filter(k => /^KLOUD_KODE_BODY_(TOKEN|CONFIG)$/i.test(k))));\n");
  const r = await call("run_command", { command: `"${process.execPath}" "${script}"` });
  assert.equal(r.exitCode, 0, r.stderr);
  assert.deepEqual(JSON.parse(r.stdout), []);
});

test("run_command runs in the requested directory and can be timed out", async () => {
  const listed = await call("run_command", { command: process.platform === "win32" ? "cd" : "pwd", cwd: "sub" });
  assert.match(listed.stdout.trim(), /sub$/);
  const slow = await call("run_command", {
    command: process.platform === "win32" ? "ping -n 6 127.0.0.1 >nul" : "sleep 5",
    timeoutMs: 700,
  });
  assert.equal(slow.timedOut, true);
});

test("command output that is not UTF-8 is decoded with the console codepage", async () => {
  // Emit raw cp932 bytes for 日本語 and check they do not come back as mojibake.
  const script = path.join(tmp, "emit.mjs");
  fs.writeFileSync(script, `process.stdout.write(Buffer.from([0x93,0xfa,0x96,0x7b,0x8c,0xea]));`);
  const r = await call("run_command", { command: `"${process.execPath}" "${script}"` });
  const info = await call("system_info");
  if (info.consoleEncoding === "shift_jis") {
    assert.equal(r.stdout, "日本語");
    assert.equal(r.encoding, "shift_jis");
  } else {
    assert.ok(r.stdout.length > 0);
    assert.notEqual(r.encoding, "utf-8", "invalid UTF-8 must not be reported as UTF-8");
  }
  const utf8 = await call("run_command", { command: "echo plain-ascii" });
  assert.equal(utf8.encoding, "utf-8");
});

test("oversized output keeps the head and the tail and says what was dropped", async () => {
  const script = path.join(tmp, "flood.mjs");
  fs.writeFileSync(
    script,
    `process.stdout.write("HEAD-MARK\\n"); for(let i=0;i<40000;i++) process.stdout.write("x".repeat(40)+"\\n"); process.stdout.write("TAIL-MARK\\n");`
  );
  const r = await call("run_command", { command: `"${process.execPath}" "${script}"`, timeoutMs: 60_000 });
  assert.match(r.stdout, /HEAD-MARK/);
  assert.match(r.stdout, /TAIL-MARK/);
  assert.match(r.stdout, /bytes dropped from the middle/);
  assert.ok(r.droppedBytes.stdout > 0);
});

test("file tools read, write, edit and refuse to clobber", async () => {
  const read = await call("read_file", { path: "hello.txt" });
  assert.equal(read.content, "line one\nline two\nline three\n");

  const window = await call("read_file", { path: "hello.txt", offset: 2, limit: 1 });
  assert.equal(window.content, "line two");
  assert.equal(window.totalLines, 4);
  assert.equal(window.firstLine, 2);

  assert.ok((await call("write_file", { path: "hello.txt", content: "x" })).isError, "must refuse to clobber");
  const written = await call("write_file", { path: "new/deep.txt", content: "fresh" });
  assert.equal(written.path.replace(/\\/g, "/"), "new/deep.txt");
  assert.equal(fs.readFileSync(path.join(root, "new", "deep.txt"), "utf8"), "fresh");

  const edited = await call("edit_file", { path: "hello.txt", oldText: "line two", newText: "LINE TWO" });
  assert.ok(edited.replacedAt > 0);
  assert.match(fs.readFileSync(path.join(root, "hello.txt"), "utf8"), /LINE TWO/);

  const ambiguous = await call("edit_file", { path: "hello.txt", oldText: "line", newText: "L" });
  assert.ok(ambiguous.isError);
  assert.match(ambiguous.message, /more than once/);
});

test("read_file can window a file larger than the whole-file limit", async () => {
  const large = path.join(root, "large.txt");
  fs.writeFileSync(large, "header\n" + "x".repeat(8_388_608) + "\n尾部\n");
  try {
    const whole = await call("read_file", { path: "large.txt" });
    assert.ok(whole.isError);
    const window = await call("read_file", { path: "large.txt", offset: 3, limit: 1 });
    assert.equal(window.content, "尾部");
    assert.equal(window.totalLines, 4);
    assert.equal(window.firstLine, 3);
    assert.equal(window.lastLine, 3);
    assert.equal(window.encoding, "utf-8");
  } finally {
    fs.unlinkSync(large);
  }
});

test("base64 read and write are byte-exact, windowed, and limited; stat and listings carry the mode", async () => {
  const bytes = Buffer.from([0, 1, 2, 0xff, 0xfe, 0x80, 10, 13, 0xc3]);
  const w = await call("write_file", { path: "bin/blob.dat", content: bytes.toString("base64"), encoding: "base64" });
  assert.equal(w.bytes, bytes.length);
  assert.deepEqual(fs.readFileSync(path.join(root, "bin", "blob.dat")), bytes);
  const all = await call("read_file", { path: "bin/blob.dat", encoding: "base64" });
  assert.deepEqual(Buffer.from(all.content, "base64"), bytes);
  assert.equal(all.size, bytes.length);
  const win = await call("read_file", { path: "bin/blob.dat", encoding: "base64", byteOffset: 3, byteLength: 4 });
  assert.deepEqual(Buffer.from(win.content, "base64"), bytes.subarray(3, 7));
  assert.equal((await call("read_file", { path: "bin/blob.dat", encoding: "base64", byteOffset: 100 })).byteLength, 0);
  const again = await call("write_file", { path: "bin/blob.dat", content: "", encoding: "base64" });
  assert.equal(again.isError, true, "base64 writes still refuse to clobber");
  assert.equal((await call("write_file", { path: "bin/blob.dat", content: "", encoding: "base64", overwrite: true })).bytes, 0);
  // chmod bits are only observable through stat on POSIX filesystems.
  if (process.platform !== "win32") {
    fs.chmodSync(path.join(root, "hello.txt"), 0o640);
    assert.equal((await call("stat_path", { path: "hello.txt" })).mode, 0o640);
    const listed = await call("list_directory", { path: ".", limit: 5000 });
    assert.equal(listed.entries.find((e) => e.name === "hello.txt").mode, 0o640);
    fs.chmodSync(path.join(root, "hello.txt"), 0o644);
  }
});

test("paths cannot climb out of the configured root", async () => {
  for (const p of ["../escape.txt", "sub/../../escape.txt"]) {
    const r = await call("write_file", { path: p, content: "nope" });
    assert.ok(r.isError, `${p} should be refused`);
    assert.match(r.message, /escapes root/);
  }
  assert.ok(!fs.existsSync(path.join(tmp, "escape.txt")));
});

test("find_files and search_files locate things by name and by content", async () => {
  const found = await call("find_files", { pattern: "**/*.md" });
  assert.deepEqual(found.matches.sort(), ["sub/note.md", "sub/other.md"]);

  const hits = await call("search_files", { query: "needle", contextLines: 1 });
  assert.equal(hits.matches.length, 1);
  assert.equal(hits.matches[0].file, "sub/note.md");
  assert.equal(hits.matches[0].line, 2);
  assert.deepEqual(hits.matches[0].before, ["# Title"]);

  const rx = await call("search_files", { query: "^#\\s+Title$", regex: true });
  assert.equal(rx.matches.length, 1);
});

test("directory listing, stat, move and delete", async () => {
  const listed = await call("list_directory", { path: "." });
  const names = listed.entries.map((e) => e.name);
  assert.ok(names.includes("hello.txt") && names.includes("sub"));
  assert.equal(listed.entries.find((e) => e.name === "sub").type, "dir");

  const st = await call("stat_path", { path: "hello.txt" });
  assert.equal(st.type, "file");
  assert.ok(st.size > 0);

  await call("move_path", { from: "sub/other.md", to: "sub/renamed.md" });
  assert.ok(fs.existsSync(path.join(root, "sub", "renamed.md")));

  const dirRefusal = await call("delete_path", { path: "sub" });
  assert.ok(dirRefusal.isError);
  assert.match(dirRefusal.message, /recursive/);

  await call("delete_path", { path: "sub/renamed.md" });
  assert.ok(!fs.existsSync(path.join(root, "sub", "renamed.md")));

  assert.ok((await call("delete_path", { path: "." })).isError, "must refuse to delete the root");
});

test("a started process keeps running between calls and streams with cursors", async () => {
  const script = path.join(tmp, "ticker.mjs");
  fs.writeFileSync(
    script,
    `let i=0; const t=setInterval(()=>{ console.log("tick"+(i++)); if(i>20) { clearInterval(t); } }, 120);
     process.stdin.on("data", d => console.log("got:"+d.toString().trim()));`
  );
  const started = await call("start_process", { command: `"${process.execPath}" "${script}"` });
  assert.ok(started.processId);

  await new Promise((r) => setTimeout(r, 600));
  const first = await call("read_process", { processId: started.processId });
  assert.match(first.stdout, /tick0/);
  assert.equal(first.running, true);
  assert.ok(first.stdoutCursor > 0);

  await call("write_process", { processId: started.processId, input: "hello\n" });
  await new Promise((r) => setTimeout(r, 400));

  const second = await call("read_process", { processId: started.processId, stdoutCursor: first.stdoutCursor });
  assert.doesNotMatch(second.stdout, /tick0\b/, "cursor should skip what was already read");
  assert.match(second.stdout, /got:hello/);

  assert.ok((await call("list_processes")).processes.some((p) => p.processId === started.processId));
  assert.equal((await call("stop_process", { processId: started.processId })).stopped, true);
  await new Promise((r) => setTimeout(r, 300));
  assert.equal((await call("read_process", { processId: started.processId })).running, false);
  assert.equal((await call("list_processes")).processes.find((p) => p.processId === started.processId).running, false);
  assert.equal((await call("stop_process", { processId: started.processId })).alreadyExited, true);
  assert.equal((await call("write_process", { processId: started.processId, input: "must not be written\n" })).isError, true);
});

test("system_info describes the machine and its limits, nothing about the wrapper", async () => {
  const info = await call("system_info");
  assert.equal(info.host, os.hostname());
  assert.equal(info.platform, process.platform);
  assert.equal(info.osVersion, os.version());
  assert.ok(Math.abs(Date.now() - Date.parse(info.time.utc)) < 5000);
  assert.match(info.time.localDate, /^\d{4}-\d{2}-\d{2}$/);
  assert.equal(info.time.timeZone, Intl.DateTimeFormat().resolvedOptions().timeZone);
  // JSON serializes negative zero as zero; preserve the sign of nonzero offsets.
  assert.equal(info.time.utcOffsetMinutes, -new Date().getTimezoneOffset() || 0);
  assert.equal(info.defaultRoot, "main");
  assert.equal(info.roots[0].path, root);
  assert.deepEqual(info.enabled, { files: true, shell: true, processes: true });
  assert.ok(info.consoleEncoding);
  assert.ok(!("version" in info) && !("mcpServers" in info), "no wrapper fingerprints");
});

test("nested MCP reports nothing configured and fails clearly", async () => {
  assert.deepEqual((await call("list_mcp_servers")).servers, []);
  const r = await call("list_mcp_tools", { server: "ghost" });
  assert.ok(r.isError);
  assert.match(r.message, /unknown MCP server/);
});

test("a bad argument is an error message, not a crash", async () => {
  const r = await rpc("tools/call", { name: "read_file", arguments: { path: 42 } });
  assert.equal(r.status, 200);
  assert.ok(r.body.error || r.body.result?.isError, "expected a structured failure");
  assert.equal((await call("read_file", { path: "missing.txt" })).isError, true);
});

test("run_command can target a specific shell, and prompting built-ins fail fast", async () => {
  const listed = (await rpc("tools/list")).body.result.tools.find((t) => t.name === "run_command");
  assert.ok(listed.inputSchema.properties.shell, "run_command should expose a shell parameter");
  if (process.platform === "win32") {
    assert.match(listed.description, /cmd\.exe/);
    const ps = await call("run_command", { command: "Get-Date -Format yyyy", shell: "powershell" });
    assert.equal(ps.exitCode, 0, ps.stderr);
    assert.match(ps.stdout.trim(), /^\d{4}$/);
    const quoted = await call("run_command", { command: '$x = "a b"; Write-Output "[$x]"', shell: "powershell" });
    assert.equal(quoted.stdout.trim(), "[a b]");
    const cmd = await call("run_command", { command: 'echo "hi there"', shell: "cmd" });
    assert.match(cmd.stdout, /hi there/);
    const prompt = await call("run_command", { command: "date", timeoutMs: 5000 });
    assert.equal(prompt.timedOut, false, "a prompting built-in must not hang until the timeout");
  } else {
    const sh = await call("run_command", { command: 'x="a b"; echo "[$x]"', shell: "sh" });
    assert.equal(sh.stdout.trim(), "[a b]");
  }
  const info = await call("system_info");
  assert.ok(info.defaultShell);
  assert.ok(info.shells.includes("powershell"));
});
