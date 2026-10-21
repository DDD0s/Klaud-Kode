import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import http from "node:http";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { apiFixture, haveBody, realBody } from "./helpers.mjs";
import { fuseMissing } from "../backends/antigravity.mjs";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const BRIDGE = path.join(HERE, "..", "backends", "agy-mcp-bridge.mjs");
const HOOK = path.join(HERE, "..", "backends", "agy-hook.mjs");

const NOTE_START = "You work on the user's computer.";
const BASE = ["--input-format", "stream-json", "--output-format", "stream-json"];

/** Streamable HTTP MCP server standing in for the body; answers with SSE like the MCP SDK can. */
async function bodyServer(t) {
  const seen = [];
  const server = http.createServer(async (req, res) => {
    let raw = "";
    for await (const c of req) raw += c;
    const msg = JSON.parse(raw);
    seen.push({ method: msg.method, auth: req.headers.authorization, session: req.headers["mcp-session-id"] || null });
    if (msg.id === undefined) { res.writeHead(202); res.end(); return; }
    const reply = msg.method === "initialize"
      ? { result: { protocolVersion: msg.params.protocolVersion, capabilities: { tools: {} }, serverInfo: { name: "body", version: "1" } } }
      : msg.method === "tools/list" ? { result: { tools: [{ name: "system_info", inputSchema: { type: "object", properties: {} } }] } }
        : { error: { code: -32601, message: "Method not found" } };
    res.writeHead(200, { "content-type": "text/event-stream", "mcp-session-id": "body-session-1" });
    res.end(`event: message\ndata: ${JSON.stringify({ jsonrpc: "2.0", id: msg.id, ...reply })}\n\n`);
  });
  await new Promise((r) => server.listen(0, "127.0.0.1", r));
  t.after(() => new Promise((r) => server.close(r)));
  return { url: `http://127.0.0.1:${server.address().port}/mcp`, seen };
}

async function agyFixture(t, env = {}, { prepare = () => {} } = {}) {
  const body = await bodyServer(t);
  const f = await apiFixture(t, {
    // "commands" keeps these tests off FUSE, whether or not this host could mount.
    env: { KOMPUTER_SKIP_TUNNEL: "1", KOMPUTER_BACKENDS: "claude,antigravity", KOMPUTER_AGY_REMOTE_TOOLS: "commands", ...env },
    prepare: (dir) => {
      fs.writeFileSync(path.join(dir, ".mcp.json"), JSON.stringify({ mcpServers: { komputer_use: { type: "http", url: body.url, headers: { Authorization: "Bearer body-secret" } } } }));
      return prepare(dir);
    },
  });
  await f.ready();
  f.body = body;
  f.globalMcp = () => JSON.parse(fs.readFileSync(path.join(f.dir, "home", ".gemini", "config", "mcp_config.json"), "utf8")).mcpServers;
  return f;
}
const chat = (f, body, headers = {}) => f.request("/v1/chat/completions", body, { headers });
const user = (content) => ({ role: "user", content });

function sse(text) {
  return text.split("\n\n").filter((b) => b.startsWith("data: ") && !b.includes("[DONE]")).map((b) => JSON.parse(b.slice(6)));
}

test("agy: routed by model id, session created then resumed with --conversation", async (t) => {
  const f = await agyFixture(t);
  const ids = (await f.request("/v1/models")).json().data.map((m) => m.id);
  assert.deepEqual(ids, ["komputer-claude", "komputer-opus", "komputer-sonnet", "komputer-haiku", "komputer-fable",
    "komputer-agy", "komputer-gemini-4-argon", "komputer-gemini-3.8-flash"]);

  let r = await chat(f, { model: "komputer-gemini-4-argon", reasoning_effort: "high", messages: [{ role: "system", content: "RULE" }, user("hello")] },
    { "x-conversation-id": "agy-chat" });
  assert.equal(r.status, 200, r.text);
  assert.equal(f.calls().length, 0, "Claude Code is not spawned for agy models");
  let c = f.agyCalls().at(-1);
  assert.deepEqual(c.args, [...BASE, "--model", "gemini-4-argon", "--effort", "high"]);
  assert.ok(c.prompt.startsWith(`<system_instructions>\n${NOTE_START}`), c.prompt);
  assert.ok(c.prompt.endsWith("RULE\n</system_instructions>\n\nhello"), c.prompt);
  assert.equal(c.cwd, fs.realpathSync(path.join(f.dir, "agy-ws", "body")));
  assert.equal(c.gitCeiling, fs.realpathSync(path.join(f.dir, "agy-ws")));
  assert.deepEqual(c.mcpTools, { komputer_use: ["system_info"] }, "body tools reach agy through the global bridge entry");
  const methods = f.body.seen.map((s) => s.method);
  assert.deepEqual(methods.slice(0, 3), ["initialize", "notifications/initialized", "tools/call"], "the service asks the Body for system_info (its root)");
  assert.deepEqual(methods.slice(methods.indexOf("server/discover"), methods.indexOf("server/discover") + 4),
    ["server/discover", "initialize", "notifications/initialized", "tools/list"]);
  const ws = fs.realpathSync(path.join(f.dir, "agy-ws", "body"));
  assert.deepEqual(c.target.policy, { mode: "remote", mount: ws, root: null, files: false, commands: true }, "Body turns run agy's own tools on the user's computer");
  assert.deepEqual(c.target.remote, { server: { url: f.body.url }, mount: ws, root: null, shell: "bash" });
  assert.equal(c.pathHead, path.join(f.dir, "agy-ws", "shim"), "the bash shim comes first on agy's PATH");
  assert.ok(f.body.seen.every((s) => s.auth === "Bearer body-secret"));
  assert.equal(f.body.seen.at(-1).session, "body-session-1", "the bridge keeps the MCP session id");
  assert.equal(c.bridgeFileMode, 0o600);
  assert.equal(path.dirname(c.bridgeFile), path.join(f.dir, "agy-ws", "mcp"));
  assert.equal(fs.existsSync(c.bridgeFile), false, "the per-turn target file is removed after the turn");
  assert.ok(!c.envKeys.some((k) => /^(KOMPUTER_(?!AGY_MCP_FILE$)|CLAUDE_BIN|AGY_BIN|FAKE_CLAUDE_STATE|ANTHROPIC_)/.test(k)), c.envKeys.join(","));
  assert.ok(c.envKeys.includes("FAKE_AGY_STATE"));
  const entry = f.globalMcp().komputer_use;
  assert.deepEqual(entry, { args: [BRIDGE, "komputer_use"], command: process.execPath, disabled: false });
  assert.doesNotMatch(JSON.stringify(f.globalMcp()), /body-secret|127\.0\.0\.1/, "the global agy config holds no URLs or tokens");
  const conversation = r.headers.get("x-komputer-backend-session");
  assert.equal(r.headers.get("x-komputer-backend"), "antigravity");
  assert.equal(r.headers.get("x-komputer-claude-session"), null);
  assert.match(conversation, /^[0-9a-f-]{36}$/);
  const first = r.json().choices[0].message.content;
  assert.ok(first.startsWith("agy:<system_instructions>"));
  assert.deepEqual(r.json().usage, { prompt_tokens: 25, completion_tokens: 9, total_tokens: 34 }, "agy input_tokens exclude its 5 cache reads");

  r = await chat(f, { model: "komputer-gemini-4-argon", messages: [{ role: "system", content: "RULE" }, user("hello"), { role: "assistant", content: first }, user("second")] },
    { "x-conversation-id": "agy-chat" });
  assert.equal(r.status, 200, r.text);
  c = f.agyCalls().at(-1);
  assert.deepEqual(c.args, [...BASE, "--model", "gemini-4-argon", "--conversation", conversation]);
  assert.equal(c.prompt, "second", "unchanged instructions are not resent on resume");
  const second = r.json().choices[0].message.content;
  assert.match(second, /^prev=<system_instructions>[\s\S]*\|now=second$/);

  r = await chat(f, { model: "komputer-gemini-4-argon", messages: [{ role: "system", content: "NEW RULE" }, user("hello"), { role: "assistant", content: first },
    user("second"), { role: "assistant", content: second }, user("third")] }, { "x-conversation-id": "agy-chat" });
  c = f.agyCalls().at(-1);
  assert.ok(c.args.includes("--conversation"));
  assert.ok(c.prompt.includes("NEW RULE\n</system_instructions>\n\nthird"), "changed instructions are sent again");

  const stored = JSON.parse(fs.readFileSync(path.join(f.dir, "sessions.json"), "utf8")).sessions.find((s) => s.client_key === "agy-chat");
  assert.equal(stored.backend, "antigravity");
  assert.equal(stored.backend_session_id, conversation);
  assert.equal(stored.claude_session_id, undefined);
  assert.equal(stored.turn_count, 3);
  const pub = (await f.request("/v1/sessions/agy-chat")).json();
  assert.equal(pub.backend, "antigravity");
  assert.equal(pub.backend_session_id, conversation);
});

test("agy: an unknown --conversation is detected at init and reseeded once from client history", async (t) => {
  const f = await agyFixture(t);
  const h = { "x-conversation-id": "lost" };
  const r1 = await chat(f, { model: "komputer-agy", messages: [user("one")] }, h);
  const a1 = r1.json().choices[0].message.content;
  const r2 = await chat(f, { model: "komputer-agy", messages: [user("one"), { role: "assistant", content: a1 }, user("LOSE_CONVERSATION two")] }, h);
  assert.equal(r2.status, 200, r2.text);
  assert.equal(r2.headers.get("x-komputer-session-recovered"), "client-history");
  const [, lost, retry] = f.agyCalls();
  assert.ok(lost.args.includes("--conversation"));
  assert.ok(!retry.args.includes("--conversation"));
  assert.match(retry.prompt, /Earlier conversation for context:\n\nUser: one\n\nAssistant: agy:/);
  assert.notEqual(r2.headers.get("x-komputer-backend-session"), r1.headers.get("x-komputer-backend-session"));
  assert.equal((await f.request("/v1/sessions/lost")).json().reseeds, 1);
});

test("agy: auth and quota failures map to retryable statuses", async (t) => {
  const f = await agyFixture(t);
  const auth = await chat(f, { model: "komputer-agy", messages: [user("FAIL_AUTH")] });
  assert.equal(auth.status, 502);
  assert.equal(auth.json().error.type, "upstream_auth_error");
  const limit = await chat(f, { model: "komputer-agy", messages: [user("FAIL_LIMIT")] });
  assert.equal(limit.status, 429);
  assert.equal(limit.json().error.type, "rate_limit_error");
  assert.equal(f.agyCalls()[0].args.includes("--conversation"), false);
});

test("agy: streaming text, tool notes and step separators", async (t) => {
  const f = await agyFixture(t);
  const r = await chat(f, { model: "komputer-agy", stream: true, messages: [user("TOOL now")] });
  assert.equal(r.status, 200);
  const chunks = sse(r.text);
  const text = chunks.map((c) => c.choices?.[0]?.delta?.content || "").join("");
  const notes = chunks.map((c) => c.choices?.[0]?.delta?.reasoning_content || "").join("");
  assert.match(text, /^checking\n\nagy:/);
  assert.equal(notes, "\n[tool] mcp__komputer_use__run_command\n");
  assert.equal(chunks.at(-1).choices[0].finish_reason, "stop");
});

test("agy: attachments, task mode, permissions, extra args and effort handling", async (t) => {
  const f = await agyFixture(t, { KOMPUTER_AGY_PERMISSIONS: "skip", KOMPUTER_AGY_EXTRA_ARGS: "--add-dir /nonexistent-test-dir",
    KOMPUTER_AGY_TASK_MODEL: "gemini-3.8-flash" });
  const image = await chat(f, { model: "komputer-agy", messages: [{ role: "user", content: [{ type: "text", text: "see" },
    { type: "image_url", image_url: { url: "data:image/png;base64,iVBORw0KGgo=" } }] }] });
  assert.equal(image.status, 400);
  assert.match(image.json().error.message, /does not accept image/);
  assert.equal(f.agyCalls().length, 0);

  const file = await chat(f, { model: "komputer-agy", messages: [{ role: "user", content: [{ type: "text", text: "read" },
    { type: "file", file: { filename: "a.txt", file_data: `data:text/plain;base64,${Buffer.from("FILE BODY").toString("base64")}` } }] }] });
  assert.equal(file.status, 200, file.text);
  assert.match(f.agyCalls().at(-1).prompt, /read\n\nFile a\.txt:\nFILE BODY$/);

  await chat(f, { model: "komputer-agy", messages: [user("### Task: make a title")] });
  let c = f.agyCalls().at(-1);
  assert.deepEqual(c.args, [...BASE, "--dangerously-skip-permissions", "--model", "gemini-3.8-flash", "--add-dir", "/nonexistent-test-dir"]);
  assert.equal(c.cwd, fs.realpathSync(path.join(f.dir, "agy-ws", "plain")));
  assert.deepEqual(c.target, { policy: { mode: "island" } }, "the target file only carries the hook policy");
  assert.notEqual(c.pathHead, path.join(f.dir, "agy-ws", "shim"));
  assert.deepEqual(c.mcpTools, { komputer_use: [] }, "background tasks get no MCP tools: the bridge has no target");

  await chat(f, { model: "komputer-gemini-4-argon", reasoning_effort: "max", messages: [user("x")] });
  assert.deepEqual(f.agyCalls().at(-1).args.slice(5, 9), ["--model", "gemini-4-argon", "--effort", "high"], "efforts above the configured set clamp down");
  await chat(f, { model: "komputer-gemini-4-argon-medium", reasoning_effort: "low", messages: [user("x")] });
  c = f.agyCalls().at(-1);
  assert.ok(c.args.includes("gemini-4-argon-medium") && !c.args.includes("--effort"), "slugs that carry their effort get no --effort");
  await chat(f, { model: "komputer-agy", reasoning_effort: "minimal", messages: [user("x")] });
  assert.deepEqual(f.agyCalls().at(-1).args.slice(5, 7), ["--effort", "low"]);
});

test("agy: client tools reach agy through the bridge, pointed at the run's own MCP server", async (t) => {
  const f = await agyFixture(t);
  const tools = [{ type: "function", function: { name: "lookup", parameters: { type: "object", properties: { query: { type: "string" } }, required: ["query"] } } }];
  const r = await chat(f, { model: "komputer-agy", tools, messages: [user("CLIENT_TOOL please")] });
  assert.equal(r.status, 200, r.text);
  const call = r.json().choices[0].message.tool_calls[0];
  assert.equal(call.function.name, "lookup");
  assert.deepEqual(JSON.parse(call.function.arguments), { query: "from agy" });
  const c = f.agyCalls().at(-1);
  assert.match(c.cwd, /komputer-tools-[^/]+\/agy$/);
  assert.deepEqual(Object.keys(c.mcpTools), ["komputer_use"]);
  assert.equal(c.mcpTools.komputer_use.length, 1, "only the client tool, not the body's");
  assert.notEqual(c.mcpTools.komputer_use[0], "system_info");
  assert.match(c.bridgeFile, /komputer-tools-[^/]+\/agy-mcp-[0-9a-f-]+\.json$/);
  assert.equal(c.bridgeFileMode, 0o600);
  assert.ok(!f.body.seen.length, "a harness turn never touches the body MCP");
  assert.ok(c.noProxy.split(",").includes("127.0.0.1"));
  assert.deepEqual(c.args, BASE, "harness runs are stateless and island-only");
  const done = await chat(f, { model: "komputer-agy", tools, messages: [{ role: "tool", tool_call_id: call.id, content: "forty-two" }] });
  assert.equal(done.status, 200, done.text);
  assert.equal(done.json().choices[0].message.content, "agy tool result:forty-two");
});

test("agy: a conversation that switches CLIs is reseeded on the new one", async (t) => {
  const f = await agyFixture(t);
  const h = { "x-conversation-id": "switch" };
  const r1 = await chat(f, { model: "komputer-sonnet", messages: [user("on claude")] }, h);
  assert.equal(r1.status, 200);
  assert.ok(r1.headers.get("x-komputer-claude-session"));
  const r2 = await chat(f, { model: "komputer-agy", messages: [user("on claude"), { role: "assistant", content: r1.json().choices[0].message.content }, user("now agy")] }, h);
  assert.equal(r2.status, 200, r2.text);
  assert.equal(r2.headers.get("x-komputer-backend"), "antigravity");
  assert.match(f.agyCalls().at(-1).prompt, /Earlier conversation for context:\n\nUser: on claude\n\nAssistant: echo:on claude\n\n---\n\nnow agy$/);
  const pub = (await f.request("/v1/sessions/switch")).json();
  assert.equal(pub.backend, "antigravity");
  assert.equal(pub.reseeds, 1);
  const r3 = await chat(f, { model: "komputer-sonnet", messages: [user("back")] }, h);
  assert.equal(r3.status, 200);
  assert.ok(f.calls().at(-1).args.includes("--session-id"), "switching back starts a fresh Claude session");
  const stored = JSON.parse(fs.readFileSync(path.join(f.dir, "sessions.json"), "utf8")).sessions.find((s) => s.client_key === "switch");
  assert.equal(stored.backend, undefined);
  assert.ok(stored.claude_session_id);
});

test("agy: KOMPUTER_AGY_MCP_SETUP=off leaves the global config alone and startup says what is missing", async (t) => {
  const f = await agyFixture(t, { KOMPUTER_AGY_MCP_SETUP: "off" }, {
    prepare: (dir) => {
      fs.mkdirSync(path.join(dir, "home", ".gemini", "config"), { recursive: true });
      fs.writeFileSync(path.join(dir, "home", ".gemini", "config", "mcp_config.json"),
        JSON.stringify({ mcpServers: { stray: { command: "true", args: [], disabled: false }, parked: { command: "true", disabled: true } } }));
    },
  });
  assert.deepEqual(Object.keys(f.globalMcp()), ["stray", "parked"]);
  const logs = f.logs();
  assert.match(logs, /"komputer_use" is not the komputer bridge[^\n]*run: agy mcp add komputer_use -- /);
  assert.match(logs, /global server "stray" is enabled for every agy turn/);
  assert.doesNotMatch(logs, /"parked"/);
  assert.match(logs, /permissions\.allow/);
  assert.match(logs, /the "komputer" PreToolUse hook is missing from ~\/\.gemini\/config\/hooks\.json/);
  assert.match(logs, /add "command\(\*\)" to permissions\.allow/, "headless agy denies run_command without it");
  assert.equal(fs.existsSync(path.join(f.dir, "home", ".gemini", "config", "hooks.json")), false);
});

test("agy: final text without deltas is still streamed, and unknown events are ignored", async (t) => {
  const f = await agyFixture(t);
  const r = await chat(f, { model: "komputer-agy", stream: true, messages: [user("NO_DELTAS hi")] });
  assert.equal(r.status, 200);
  const text = sse(r.text).map((c) => c.choices?.[0]?.delta?.content || "").join("");
  assert.match(text, /^agy:<system_instructions>[\s\S]*\n\nNO_DELTAS hi$/);
  const plain = await chat(f, { model: "komputer-agy", messages: [user("TOOL again")] });
  assert.match(plain.json().choices[0].message.content, /^checking\n\nagy:<system_instructions>[\s\S]*TOOL again$/, "non-streaming keeps the text of every step, like the stream");
});

test("agy: non-Gemini models keep the CLI's full effort range; slugs with an effort never get --effort", async (t) => {
  const f = await agyFixture(t, { KOMPUTER_AGY_MODELS: "komputer-gemini-3.8-flash,komputer-agy-claude-opus-5-5" });
  let r = await chat(f, { model: "komputer-agy-claude-opus-5-5", reasoning_effort: "xhigh", messages: [user("x")] });
  assert.equal(r.status, 200, r.text);
  assert.equal(r.headers.get("x-komputer-backend"), "antigravity");
  assert.deepEqual(f.agyCalls().at(-1).args.slice(4), ["--model", "claude-opus-5-5", "--effort", "xhigh"]);
  r = await chat(f, { model: "komputer-gemini-3.8-flash", reasoning_effort: "xhigh", messages: [user("x")] });
  assert.deepEqual(f.agyCalls().at(-1).args.slice(4), ["--model", "gemini-3.8-flash", "--effort", "high"]);
  r = await chat(f, { model: "komputer-gemini-3.8-flash-low", reasoning_effort: "high", messages: [user("x")] });
  assert.equal(r.status, 200, r.text);
  assert.deepEqual(f.agyCalls().at(-1).args.slice(4), ["--model", "gemini-3.8-flash-low"]);
  await chat(f, { model: "komputer-agy", messages: [user("### Task: title")] });
  assert.deepEqual(f.agyCalls().at(-1).args.slice(4), ["--model", "gemini-3.8-flash-low"], "background tasks default to the cheapest Gemini");
});

test("agy: island turns never run agy's own tools: the hook denies them first, the guard (warn, or stop) catches any that ran", async (t) => {
  const own = { PostToolUse: [{ matcher: ".*", hooks: [{ type: "command", command: "true" }] }] };
  const f = await agyFixture(t, { KOMPUTER_AGY_PERMISSIONS: "skip", KOMPUTER_AGY_REMOTE_TOOLS: "off" }, {
    prepare: (dir) => {
      fs.mkdirSync(path.join(dir, "home", ".gemini", "config"), { recursive: true });
      fs.writeFileSync(path.join(dir, "home", ".gemini", "config", "hooks.json"), JSON.stringify({ mine: own }));
    },
  });
  const hooks = JSON.parse(fs.readFileSync(path.join(f.dir, "home", ".gemini", "config", "hooks.json"), "utf8"));
  assert.deepEqual(hooks.mine, own, "the operator's own hooks are kept");
  assert.deepEqual(hooks.komputer.PreToolUse[0].hooks[0].command, `'${process.execPath}' '${HOOK}'`);
  assert.match(f.logs(), /registered the "komputer" PreToolUse hook/);
  const r = await chat(f, { model: "komputer-agy", messages: [user("BUILTIN list files")] });
  assert.equal(r.status, 200, r.text);
  assert.match(f.calls("agy-tools.jsonl").at(-1).error, /^tool call denied by pre-tool hook: run_command is not available in this conversation/);
  assert.deepEqual(f.calls("agy-host-commands.jsonl"), [], "nothing ran on the service host");
  assert.doesNotMatch(f.logs(), /agy ran its built-in tool/);
  assert.equal((await chat(f, { model: "komputer-agy", messages: [user("TOOL via mcp")] })).status, 200, "MCP tool steps are fine");

  // Without a hook that runs, the parser is the backstop: warn by default, stop on request.
  const noHooks = { KOMPUTER_AGY_REMOTE_TOOLS: "off", KOMPUTER_CHILD_ENV_EXTRA: "FAKE_AGY_NO_HOOKS,FAKE_AGY_PERMISSION_MODE", FAKE_AGY_NO_HOOKS: "1" };
  const warn = await agyFixture(t, { ...noHooks, FAKE_AGY_PERMISSION_MODE: "always-proceed" });
  const w = await chat(warn, { model: "komputer-agy", messages: [user("BUILTIN list files")] });
  assert.equal(w.status, 200, w.text);
  assert.match(warn.logs(), /antigravity: WARNING: agy ran its built-in tool run_command on the service host \(permission_mode always-proceed\)/);
  assert.match(warn.logs(), /always-proceed without --dangerously-skip-permissions/);
  const stop = await agyFixture(t, { ...noHooks, KOMPUTER_AGY_PERMISSIONS: "skip", KOMPUTER_AGY_BUILTIN_GUARD: "stop" });
  const s = await chat(stop, { model: "komputer-agy", messages: [user("BUILTIN list files")] });
  assert.equal(s.status, 502, s.text);
  assert.equal(s.json().error.type, "builtin_tool_blocked");
  assert.match(s.json().error.message, /service host instead of your computer/);
  const streamed = sse((await chat(stop, { model: "komputer-agy", stream: true, messages: [user("BUILTIN SLOW again")] })).text);
  assert.equal(streamed.at(-1).error?.type, "builtin_tool_blocked", "after the stream opened, the failure is reported in-stream");

  const open = await agyFixture(t, { KOMPUTER_BUILTIN_TOOLS: "default", KOMPUTER_AGY_PERMISSIONS: "skip" });
  const o = await chat(open, { model: "komputer-agy", messages: [user("BUILTIN<<echo local>>")] });
  assert.equal(o.status, 200, "Body turns that opted into built-in tools on this host are not guarded");
  assert.match(open.calls("agy-tools.jsonl").at(-1).output, /^Command exited with code 0\.\r\n\r\nCommand output:\r\nlocal\r\n$/);
  assert.equal(open.calls("agy-host-commands.jsonl").length, 1);
});

test("agy: in Body turns run_command runs on the user's computer through the bash shim, file tools are kept off this host", { skip: !haveBody && "kloud-kode-body dependencies are not installed" }, async (t) => {
  const body = await realBody(t);
  const f = await agyFixture(t, { KOMPUTER_AGY_PERMISSIONS: "skip" }, { prepare: (dir) => fs.writeFileSync(path.join(dir, ".mcp.json"), body.mcpJson) });
  const ws = fs.realpathSync(path.join(f.dir, "agy-ws", "body"));
  const r = await chat(f, { model: "komputer-agy", messages: [user(`BUILTIN<<ls -la; pwd; cat ${body.root}/only-on-the-users-computer.txt; cd sub && pwd; exit 3>> READ<<${body.root}/sub/note.md>> READ<<${ws}/x>>`)] });
  assert.equal(r.status, 200, r.text);
  assert.equal(f.agyCalls().at(-1).target.policy.root, body.root, "the Body's root comes from system_info");
  const [command, read, readWs] = f.calls("agy-tools.jsonl");
  assert.equal(command.name, "run_command");
  assert.match(command.output, /^Command exited with code 3\./, "the remote exit code reaches agy");
  assert.match(command.output, /only-on-the-users-computer\.txt/, "ls -la lists the user's computer");
  assert.match(command.output, /sub\r\n/);
  assert.match(command.output, /hello from the user's computer/);
  assert.ok(command.output.includes(`\r\n${ws}\r\nhello from the user's computer\r\n${ws}/sub\r\n`), "paths on the user's computer read as agy's workspace");
  assert.ok(!command.output.includes(body.root), command.output);
  assert.equal(f.calls("agy-host-commands.jsonl").length, 1, "agy started bash once ...");
  assert.deepEqual(fs.readdirSync(ws), [], "... and it ran nothing in the service-side workspace");
  assert.match(read.error, /The user's files are on their computer, which read_file cannot reach here\. Use run_command/);
  assert.match(readWs.error, /read_file cannot reach here/, "without the mount, file tools are denied even inside the workspace");
  assert.match(f.logs(), /agy remote tools \(commands\): run_command runs on the user's computer/);
});

const fuseGaps = await fuseMissing("python3");

test("agy: with the FUSE mount, agy's workspace is the user's files: reads, writes and commands all reach the Body", { skip: !haveBody ? "kloud-kode-body dependencies are not installed" : fuseGaps.length ? `no FUSE here: ${fuseGaps.join(", ")}` : false }, async (t) => {
  const body = await realBody(t);
  const f = await agyFixture(t, { KOMPUTER_AGY_PERMISSIONS: "skip", KOMPUTER_AGY_REMOTE_TOOLS: "all" }, { prepare: (dir) => fs.writeFileSync(path.join(dir, ".mcp.json"), body.mcpJson) });
  const mnt = path.join(f.dir, "agy-ws", "remote");
  const r = await chat(f, { model: "komputer-agy", messages: [user(`BUILTIN<<pwd; ls>> READ<<${body.root}/sub/note.md>> READ<<sub/note.md>> WRITE<<${mnt}/from-agy.txt>> READ<</etc/hostname>>`)] });
  assert.equal(r.status, 200, r.text);
  const c = f.agyCalls().at(-1);
  assert.equal(c.cwd, mnt, "agy's workspace is the mount");
  assert.deepEqual(c.target.policy, { mode: "remote", mount: mnt, root: body.root, files: true, commands: true });
  const [command, mapped, relative, host, write] = f.calls("agy-tools.jsonl");
  assert.match(command.output, new RegExp(`Command output:\\r\\n${mnt}\\r\\nonly-on-the-users-computer\\.txt\\r\\nsub\\r\\n`));
  assert.deepEqual(mapped.args, { Path: path.join(mnt, "sub", "note.md") }, "a path from command output is moved onto the mount");
  assert.equal(mapped.output, "# note\n");
  assert.equal(relative.output, "# note\n");
  assert.equal(write.output, "");
  assert.equal(fs.readFileSync(path.join(body.root, "from-agy.txt"), "utf8"), "written by agy\n", "the write landed on the user's computer");
  assert.match(host.error, /\/etc\/hostname is on the service host, outside the user's workspace/);
  assert.ok(fs.readFileSync("/proc/mounts", "utf8").includes(` ${mnt} fuse.komputer `));
  f.child.kill("SIGTERM");
  await f.exited;
  for (let i = 0; i < 50 && fs.readFileSync("/proc/mounts", "utf8").includes(` ${mnt} `); i++) await new Promise((res) => setTimeout(res, 100));
  assert.ok(!fs.readFileSync("/proc/mounts", "utf8").includes(` ${mnt} `), "the mount goes away with the service");
});
