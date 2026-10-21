// Opt-in smoke tests against the real CLIs. Skipped unless pointed at a binary:
//   KOMPUTER_TEST_REAL_MUSE=/path/to/muse [KOMPUTER_TEST_REAL_MUSE_HOME=/isolated/home]
//     uses muse's offline echo provider, so no account or network is needed.
//   KOMPUTER_TEST_REAL_AGY=/path/to/agy [KOMPUTER_TEST_REAL_AGY_HOME=/isolated/home]
//     uses a fresh signed-out HOME unless one is given; checks MCP and the auth failure path,
//     then drives harness and Body turns, and agy's own tools on a real Body, through a fake Gemini API.
import test from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import fs from "node:fs";
import http from "node:http";
import path from "node:path";
import { apiFixture, haveBody, realBody } from "./helpers.mjs";
import { fakeGemini, functionResponses } from "./fake-gemini.mjs";
import { fuseMissing } from "../backends/antigravity.mjs";

const MUSE = process.env.KOMPUTER_TEST_REAL_MUSE;
const AGY = process.env.KOMPUTER_TEST_REAL_AGY;
const homeEnv = (home) => (home ? { HOME: home, XDG_CONFIG_HOME: path.join(home, ".config") } : {});

/** Minimal MCP server standing in for the body. */
async function bodyServer(t) {
  const seen = [];
  const server = http.createServer(async (req, res) => {
    let raw = "";
    for await (const c of req) raw += c;
    const msg = JSON.parse(raw || "{}");
    seen.push({ method: msg.method, auth: req.headers.authorization });
    if (msg.id === undefined) { res.writeHead(202); res.end(); return; }
    const result = msg.method === "initialize"
      ? { protocolVersion: msg.params?.protocolVersion || "2025-06-18", capabilities: { tools: {} }, serverInfo: { name: "body", version: "1" } }
      : msg.method === "tools/list" ? { tools: [{ name: "system_info", description: "info", inputSchema: { type: "object", properties: {} } }] } : {};
    res.writeHead(200, { "content-type": "application/json" });
    res.end(JSON.stringify({ jsonrpc: "2.0", id: msg.id, result }));
  });
  await new Promise((r) => server.listen(0, "127.0.0.1", r));
  t.after(() => new Promise((r) => server.close(r)));
  return { url: `http://127.0.0.1:${server.address().port}/mcp`, seen };
}

test("real muse (echo provider): session create and resume, body MCP startup, harness MCP startup", { skip: !MUSE && "set KOMPUTER_TEST_REAL_MUSE", timeout: 180_000 }, async (t) => {
  const body = await bodyServer(t);
  const mcp = (url) => JSON.stringify({ mcpServers: { komputer_use: { type: "http", url, headers: { Authorization: "Bearer body-token" } } } });
  const f = await apiFixture(t, {
    env: { KOMPUTER_SKIP_TUNNEL: "1", KOMPUTER_BACKENDS: "muse", MUSE_BIN: MUSE, KOMPUTER_MUSE_EXTRA_ARGS: "--provider echo",
      KOMPUTER_SYSTEM_PROMPT: "", ...homeEnv(process.env.KOMPUTER_TEST_REAL_MUSE_HOME) },
    prepare: (dir) => fs.writeFileSync(path.join(dir, ".mcp.json"), mcp(body.url)),
  });
  await f.ready();
  const opts = (h) => ({ headers: h, signal: AbortSignal.timeout(60_000) });
  const h = { "x-conversation-id": "real-muse" };
  const r1 = await f.request("/v1/chat/completions", { model: "komputer-muse", messages: [{ role: "user", content: "hello there" }] }, opts(h));
  assert.equal(r1.status, 200, r1.text + f.logs());
  assert.equal(r1.json().choices[0].message.content, "echo: hello there");
  assert.ok(body.seen.some((s) => s.method === "tools/list" && s.auth === "Bearer body-token"), "muse reached the body MCP with the interpolated header");
  const sid = r1.headers.get("x-komputer-backend-session");

  const r2 = await f.request("/v1/chat/completions", { model: "komputer-muse", messages: [{ role: "user", content: "hello there" },
    { role: "assistant", content: "echo: hello there" }, { role: "user", content: "second" }] }, opts(h));
  assert.equal(r2.status, 200, r2.text + f.logs());
  assert.equal(r2.json().choices[0].message.content, "echo: second");
  assert.equal(r2.headers.get("x-komputer-backend-session"), sid);
  assert.equal(r2.headers.get("x-komputer-session-recovered"), null, "a real resume is not mistaken for a lost session");

  const tools = [{ type: "function", function: { name: "lookup", parameters: { type: "object", properties: {} } } }];
  const r3 = await f.request("/v1/chat/completions", { model: "komputer-muse", tools, messages: [{ role: "user", content: "use a tool" }] }, opts({}));
  assert.equal(r3.status, 200, r3.text + f.logs());
  assert.equal(r3.json().choices[0].finish_reason, "stop", "the echo provider never calls tools, but the private MCP server had to start");

  const resp = await f.request("/v1/responses", { model: "komputer-muse", input: "via responses" }, opts({}));
  assert.equal(resp.status, 200, resp.text);
  assert.equal(resp.json().output[0].content[0].text, "echo: via responses");

  fs.writeFileSync(path.join(f.dir, ".mcp.json"), mcp("http://127.0.0.1:9/mcp"));
  const down = await f.request("/v1/chat/completions", { model: "komputer-muse", messages: [{ role: "user", content: "x" }] }, opts({}));
  assert.equal(down.status, 503, down.text);
  assert.equal(down.json().error.type, "body_unavailable");
});

// agy starts its global MCP servers before it checks the sign-in, so even a
// signed-out run proves the bridge path: `agy mcp add` registration, env
// inheritance into the stdio bridge, and the Body MCP handshake with its token.
test("real agy (signed out): bridge registration and Body MCP handshake, then 502 upstream_auth_error", { skip: !AGY && "set KOMPUTER_TEST_REAL_AGY", timeout: 120_000 }, async (t) => {
  const body = await bodyServer(t);
  const f = await apiFixture(t, {
    env: { KOMPUTER_SKIP_TUNNEL: "1", KOMPUTER_BACKENDS: "antigravity", AGY_BIN: AGY, ...homeEnv(process.env.KOMPUTER_TEST_REAL_AGY_HOME) },
    prepare: (dir) => fs.writeFileSync(path.join(dir, ".mcp.json"),
      JSON.stringify({ mcpServers: { komputer_use: { type: "http", url: body.url, headers: { Authorization: "Bearer body-token" } } } })),
  });
  await f.ready();
  const home = process.env.KOMPUTER_TEST_REAL_AGY_HOME || path.join(f.dir, "home");
  const entry = JSON.parse(fs.readFileSync(path.join(home, ".gemini", "config", "mcp_config.json"), "utf8")).mcpServers.komputer_use;
  assert.equal(entry.command, process.execPath, "startup registered the bridge with the real `agy mcp add`");
  assert.match(entry.args[0], /agy-mcp-bridge\.mjs$/);
  const r = await f.request("/v1/chat/completions", { model: "komputer-gemini-4-argon", reasoning_effort: "high", messages: [{ role: "user", content: "hi" }] },
    { signal: AbortSignal.timeout(90_000) });
  assert.equal(r.status, 502, r.text);
  assert.equal(r.json().error.type, "upstream_auth_error");
  assert.ok(body.seen.some((s) => s.method === "tools/list" && s.auth === "Bearer body-token"), `agy listed body tools through the bridge: ${JSON.stringify(body.seen)}`);
  assert.match(f.logs(), /permissions\.allow/, "startup warns that MCP tools need an allow rule");
});

// The real agy, signed out but pointed at a fake Gemini API, with a real Body standing in for
// the user's computer: agy's own run_command and file tools must act on the Body, not here.
test("real agy (Gemini API key, fake API): run_command and file tools act on the user's computer", { skip: (!AGY && "set KOMPUTER_TEST_REAL_AGY") || (!haveBody && "kloud-kode-body dependencies are not installed"), timeout: 180_000 }, async (t) => {
  const body = await realBody(t);
  const modes = ["commands", ...((await fuseMissing("python3")).length ? [] : ["all"])];
  const call = (name, args) => [{ functionCall: { name, args } }];
  for (const mode of modes) {
    const gemini = await fakeGemini(t, [
      call("run_command", { CommandLine: "ls -la; pwd; exit 3", toolSummary: "list", WaitMsBeforeAsync: 5000 }),
      call("read_file", { Path: `${body.root}/sub/note.md` }),
      call("read_file", { Path: "/etc/hostname" }),
      [{ text: "done" }],
    ]);
    const f = await apiFixture(t, {
      env: { KOMPUTER_SKIP_TUNNEL: "1", KOMPUTER_BACKENDS: "antigravity", AGY_BIN: AGY, KOMPUTER_AGY_PERMISSIONS: "skip",
        KOMPUTER_AGY_REMOTE_TOOLS: mode, GEMINI_API_KEY: "fake-key", GOOGLE_GEMINI_BASE_URL: gemini.url,
        KOMPUTER_CHILD_ENV_EXTRA: "GOOGLE_GEMINI_BASE_URL" },
      prepare: (dir) => {
        fs.writeFileSync(path.join(dir, ".mcp.json"), body.mcpJson);
        fs.mkdirSync(path.join(dir, "home", ".gemini", "antigravity-cli"), { recursive: true });
        fs.writeFileSync(path.join(dir, "home", ".gemini", "antigravity-cli", "settings.json"), JSON.stringify({ modelProvider: "gemini" }));
      },
    });
    await f.ready();
    const r = await f.request("/v1/chat/completions", { model: "komputer-agy", messages: [{ role: "user", content: "list my files" }] },
      { signal: AbortSignal.timeout(90_000) });
    assert.equal(r.status, 200, `${mode}: ${r.text}`);
    assert.equal(r.json().choices[0].message.content, "done");
    const [command, read, host] = functionResponses(gemini.requests).map((p) => JSON.stringify(p.response));
    assert.match(command, /Command exited with code 3/, mode);
    assert.match(command, /only-on-the-users-computer\.txt/, `${mode}: ls -la listed the user's computer`);
    assert.ok(!command.includes(body.root), `${mode}: paths read as agy's workspace`);
    if (mode === "all") assert.match(read, /# note/, "read_file reached the user's file through the mount");
    else assert.match(read, /cannot reach here\. Use run_command/);
    assert.match(host, mode === "all" ? /denied by pre-tool hook: \/etc\/hostname is on the service host/ : /denied by pre-tool hook: .*read_file cannot reach here/, mode);
  }
});

const agyEnv = (gemini, extra = {}) => ({ KOMPUTER_SKIP_TUNNEL: "1", KOMPUTER_BACKENDS: "antigravity", AGY_BIN: AGY, KOMPUTER_AGY_PERMISSIONS: "skip",
  GEMINI_API_KEY: "fake-key", GOOGLE_GEMINI_BASE_URL: gemini.url, KOMPUTER_CHILD_ENV_EXTRA: "GOOGLE_GEMINI_BASE_URL", ...extra });
const geminiSettings = (dir) => {
  fs.mkdirSync(path.join(dir, "home", ".gemini", "antigravity-cli"), { recursive: true });
  fs.writeFileSync(path.join(dir, "home", ".gemini", "antigravity-cli", "settings.json"), JSON.stringify({ modelProvider: "gemini" }));
};
const geminiText = (requests) => JSON.stringify(requests.map((q) => [q.body?.systemInstruction, q.body?.contents]));

// The design Claude uses, through the real agy: the client's tools and environment reach the model,
// the client runs the tool on its own computer, and agy's own tools stay off.
test("real agy (Gemini API key, fake API): harness turn: lazy schema read, the client's tool on the client, result back to agy", { skip: !AGY && "set KOMPUTER_TEST_REAL_AGY", timeout: 180_000 }, async (t) => {
  const alias = `k_0_${createHash("sha256").update(JSON.stringify("Bash")).digest("hex").slice(0, 12)}`;
  let home;
  const gemini = await fakeGemini(t, [
    [{ functionCall: { name: "read_file", args: { get Path() { return path.join(home, ".gemini", "antigravity-cli", "mcp", "komputer_use", `${alias}.json`); } } } }],
    [{ functionCall: { name: "call_mcp_tool", args: { ServerName: "komputer_use", ToolName: alias, Arguments: { command: "Get-ChildItem" } } } }],
    [{ text: "done" }],
  ]);
  const f = await apiFixture(t, { env: agyEnv(gemini), prepare: geminiSettings });
  home = path.join(f.dir, "home");
  await f.ready();
  const tools = [{ type: "function", function: { name: "Bash", description: "Run a shell command on the user's computer",
    parameters: { type: "object", properties: { command: { type: "string" } }, required: ["command"] } } }];
  const messages = [
    { role: "system", content: "You are a coding agent.\n<env>\nWorking directory: C:\\Users\\me\\proj\nPlatform: win32\n</env>" },
    { role: "user", content: "list my files" },
  ];
  const opts = { signal: AbortSignal.timeout(90_000) };
  const first = await f.request("/v1/chat/completions", { model: "komputer-agy", messages, tools }, opts);
  assert.equal(first.status, 200, first.text + f.logs());
  assert.equal(first.headers.get("x-komputer-mode"), "harness");
  const choice = first.json().choices[0];
  assert.equal(choice.finish_reason, "tool_calls");
  const [call] = choice.message.tool_calls;
  assert.equal(call.function.name, "Bash");
  assert.deepEqual(JSON.parse(call.function.arguments), { command: "Get-ChildItem" });
  const [schema] = functionResponses(gemini.requests).map((p) => JSON.stringify(p.response));
  assert.match(schema, /Bash: Run a shell command on the user's computer/, "the hook lets agy read its own lazy MCP schema");
  assert.match(geminiText(gemini.requests), /Platform: win32/, "the client's environment reaches the model");

  const next = await f.request("/v1/chat/completions", { model: "komputer-agy", tools,
    messages: [...messages, choice.message, { role: "tool", tool_call_id: call.id, content: "Mode  Name\r\nd---- src" }] }, opts);
  assert.equal(next.status, 200, next.text + f.logs());
  assert.equal(next.json().choices[0].message.content.trim(), "done");
  const responses = functionResponses(gemini.requests).map((p) => JSON.stringify(p.response));
  assert.match(responses.at(-1), /d---- src/, "the client's result reached agy");
});

test("real agy (Gemini API key, fake API): Body turn by default: agy's own tools off, the computer through komputer_use", { skip: (!AGY && "set KOMPUTER_TEST_REAL_AGY") || (!haveBody && "kloud-kode-body dependencies are not installed"), timeout: 180_000 }, async (t) => {
  const body = await realBody(t);
  const gemini = await fakeGemini(t, [
    [{ functionCall: { name: "run_command", args: { CommandLine: "ls", toolSummary: "list", WaitMsBeforeAsync: 5000 } } }],
    [{ functionCall: { name: "call_mcp_tool", args: { ServerName: "komputer_use", ToolName: "run_command", Arguments: { command: "ls" } } } }],
    [{ text: "done" }],
  ]);
  const f = await apiFixture(t, { env: agyEnv(gemini), prepare: (dir) => (fs.writeFileSync(path.join(dir, ".mcp.json"), body.mcpJson), geminiSettings(dir)) });
  await f.ready();
  const r = await f.request("/v1/chat/completions", { model: "komputer-agy", messages: [{ role: "user", content: "list my files" }] },
    { signal: AbortSignal.timeout(90_000) });
  assert.equal(r.status, 200, r.text + f.logs());
  assert.equal(r.json().choices[0].message.content, "done");
  const [own, remote] = functionResponses(gemini.requests).map((p) => JSON.stringify(p.response));
  assert.match(own, /denied by pre-tool hook: run_command is not available in this conversation/);
  assert.match(remote, /only-on-the-users-computer\.txt/, "komputer_use ran the command on the Body");
  assert.match(geminiText(gemini.requests), /You work on the user's computer\..*system_info/, "the shared work note reaches the model");
});
