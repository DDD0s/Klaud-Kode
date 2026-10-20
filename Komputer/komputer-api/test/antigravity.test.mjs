import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { apiFixture } from "./helpers.mjs";

const BODY_MCP = { mcpServers: { komputer_use: { type: "http", url: "http://127.0.0.1:9/mcp/body-path-token", headers: { Authorization: "Bearer body-secret" } } } };
const NOTE_START = "You work on the user's computer.";
const BASE = ["--input-format", "stream-json", "--output-format", "stream-json"];

async function agyFixture(t, env = {}) {
  const f = await apiFixture(t, {
    env: { KOMPUTER_SKIP_TUNNEL: "1", KOMPUTER_BACKENDS: "claude,antigravity", ...env },
    prepare: (dir) => fs.writeFileSync(path.join(dir, ".mcp.json"), JSON.stringify(BODY_MCP)),
  });
  await f.ready();
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
  assert.deepEqual(c.mcp.config.mcpServers.komputer_use, { serverUrl: BODY_MCP.mcpServers.komputer_use.url, headers: { Authorization: "Bearer body-secret" }, disabled: false });
  assert.equal(c.mcp.mode, 0o600);
  assert.ok(!c.envKeys.some((k) => /^(KOMPUTER_|CLAUDE_BIN|AGY_BIN|FAKE_CLAUDE_STATE|ANTHROPIC_)/.test(k)), c.envKeys.join(","));
  assert.ok(c.envKeys.includes("FAKE_AGY_STATE"));
  const conversation = r.headers.get("x-komputer-backend-session");
  assert.equal(r.headers.get("x-komputer-backend"), "antigravity");
  assert.equal(r.headers.get("x-komputer-claude-session"), null);
  assert.match(conversation, /^[0-9a-f-]{36}$/);
  const first = r.json().choices[0].message.content;
  assert.ok(first.startsWith("agy:<system_instructions>"));
  assert.deepEqual(r.json().usage, { prompt_tokens: 20, completion_tokens: 9, total_tokens: 29 });

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
  assert.equal(c.mcp, null, "background tasks get no MCP servers");

  await chat(f, { model: "komputer-gemini-4-argon", reasoning_effort: "max", messages: [user("x")] });
  assert.deepEqual(f.agyCalls().at(-1).args.slice(5, 9), ["--model", "gemini-4-argon", "--effort", "high"], "efforts above the configured set clamp down");
  await chat(f, { model: "komputer-gemini-4-argon-medium", reasoning_effort: "low", messages: [user("x")] });
  c = f.agyCalls().at(-1);
  assert.ok(c.args.includes("gemini-4-argon-medium") && !c.args.includes("--effort"), "slugs that carry their effort get no --effort");
  await chat(f, { model: "komputer-agy", reasoning_effort: "minimal", messages: [user("x")] });
  assert.deepEqual(f.agyCalls().at(-1).args.slice(5, 7), ["--effort", "low"]);
});

test("agy: client tools reach agy through a private workspace MCP config", async (t) => {
  const f = await agyFixture(t);
  const tools = [{ type: "function", function: { name: "lookup", parameters: { type: "object", properties: { query: { type: "string" } }, required: ["query"] } } }];
  const r = await chat(f, { model: "komputer-agy", tools, messages: [user("CLIENT_TOOL please")] });
  assert.equal(r.status, 200, r.text);
  const call = r.json().choices[0].message.tool_calls[0];
  assert.equal(call.function.name, "lookup");
  assert.deepEqual(JSON.parse(call.function.arguments), { query: "from agy" });
  const c = f.agyCalls().at(-1);
  assert.match(c.cwd, /komputer-tools-[^/]+\/agy$/);
  assert.match(c.mcp.config.mcpServers.komputer_use.serverUrl, /^http:\/\/127\.0\.0\.1:\d+\/mcp$/);
  assert.equal(c.mcp.mode, 0o600);
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
