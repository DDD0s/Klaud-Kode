import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { apiFixture } from "./helpers.mjs";

const BODY_MCP = { mcpServers: { komputer_use: { type: "http", url: "http://127.0.0.1:9/mcp/body-path-token", headers: { Authorization: "Bearer body-secret" } } } };
const ISLAND = ["--disable-approval", "--disable-request-user-input", "--no-foreign-personal-context", "--disable-shell", "--disable-write", "--disable-web-tools"];
const BODY_OVERRIDE = 'mcp_servers.komputer_use={"transport":"streamable_http","url":"${KMCP_0_URL}","headers":{"Authorization":"${KMCP_0_H0}"},"mode":"required"}';

async function museFixture(t, env = {}) {
  const f = await apiFixture(t, {
    env: { KOMPUTER_SKIP_TUNNEL: "1", KOMPUTER_BACKENDS: "claude,muse", KOMPUTER_MUSE_MODELS: "komputer-muse-spark", ...env },
    prepare: (dir) => fs.writeFileSync(path.join(dir, ".mcp.json"), JSON.stringify(BODY_MCP)),
  });
  await f.ready();
  return f;
}
const chat = (f, body, headers = {}) => f.request("/v1/chat/completions", body, { headers });
const user = (content) => ({ role: "user", content });
const argAfter = (args, flag) => args[args.indexOf(flag) + 1];

test("muse: exec argv, prompt file, MCP via env-interpolated -c, session create and resume", async (t) => {
  const f = await museFixture(t);
  const ids = (await f.request("/v1/models")).json().data.map((m) => m.id);
  assert.deepEqual(ids.slice(-2), ["komputer-muse", "komputer-muse-spark"]);

  const r = await chat(f, { model: "komputer-muse-spark", reasoning_effort: "high", messages: [{ role: "system", content: "RULE" }, user("hello")] },
    { "x-conversation-id": "muse-chat" });
  assert.equal(r.status, 200, r.text);
  assert.equal(f.calls().length, 0);
  const c = f.museCalls().at(-1);
  const workspace = path.join(f.dir, "muse-ws");
  const sid = argAfter(c.args, "--session-id");
  assert.match(sid, /^[0-9a-f-]{36}$/);
  assert.deepEqual(c.args, ["exec", "--json", "--prompt-file", argAfter(c.args, "--prompt-file"), "--workspace", workspace,
    "--model", "muse-spark", "--reasoning-effort", "high", "--session-id", sid, ...ISLAND, "-c", BODY_OVERRIDE]);
  assert.equal(c.promptMode, 0o600);
  assert.equal(fs.existsSync(argAfter(c.args, "--prompt-file")), false, "prompt file is removed after the run");
  assert.match(c.prompt, /^<system_instructions>\nYou work on[\s\S]*RULE\n<\/system_instructions>\n\nhello$/);
  assert.deepEqual(c.mcp.komputer_use.url, BODY_MCP.mcpServers.komputer_use.url);
  assert.deepEqual(c.mcp.komputer_use.headers, { Authorization: "Bearer body-secret" });
  assert.ok(!JSON.stringify(c.args).includes("body-secret") && !JSON.stringify(c.args).includes("body-path-token"), "secrets stay out of argv");
  assert.ok(c.envKeys.includes("KMCP_0_URL") && c.envKeys.includes("FAKE_MUSE_STATE"));
  assert.ok(!c.envKeys.some((k) => /^(KOMPUTER_|CLAUDE_BIN|MUSE_BIN|FAKE_CLAUDE_STATE|ANTHROPIC_)/.test(k)), c.envKeys.join(","));
  assert.equal(r.headers.get("x-komputer-backend"), "muse");
  assert.equal(r.headers.get("x-komputer-backend-session"), sid);
  assert.deepEqual(r.json().usage, { prompt_tokens: 30, completion_tokens: 12, total_tokens: 42 });
  const first = r.json().choices[0].message.content;

  const r2 = await chat(f, { model: "komputer-muse-spark", messages: [{ role: "system", content: "RULE" }, user("hello"), { role: "assistant", content: first }, user("again")] },
    { "x-conversation-id": "muse-chat" });
  assert.equal(r2.status, 200, r2.text);
  const c2 = f.museCalls().at(-1);
  assert.equal(argAfter(c2.args, "--session-id"), sid);
  assert.equal(c2.prompt, "again");
  assert.match(r2.json().choices[0].message.content, /^prev=<system_instructions>[\s\S]*\|now=again$/);
  const stored = JSON.parse(fs.readFileSync(path.join(f.dir, "sessions.json"), "utf8")).sessions.find((s) => s.client_key === "muse-chat");
  assert.equal(stored.backend, "muse");
  assert.equal(stored.backend_session_id, sid);
});

test("muse: a session that comes back new (sequence 1) or from another workspace is reseeded", async (t) => {
  const f = await museFixture(t);
  const h = { "x-conversation-id": "lost" };
  const r1 = await chat(f, { model: "komputer-muse", messages: [user("one")] }, h);
  const sid = r1.headers.get("x-komputer-backend-session");
  fs.rmSync(path.join(f.dir, "state", `muse-${sid}.json`));
  const r2 = await chat(f, { model: "komputer-muse", messages: [user("one"), { role: "assistant", content: r1.json().choices[0].message.content }, user("two")] }, h);
  assert.equal(r2.status, 200, r2.text);
  assert.equal(r2.headers.get("x-komputer-session-recovered"), "client-history");
  const sid2 = r2.headers.get("x-komputer-backend-session");
  assert.notEqual(sid2, sid);
  assert.match(f.museCalls().at(-1).prompt, /^Earlier conversation for context:\n\nUser: one\n\nAssistant: muse:/m);

  const state = path.join(f.dir, "state", `muse-${sid2}.json`);
  fs.writeFileSync(state, JSON.stringify({ ...JSON.parse(fs.readFileSync(state, "utf8")), workspace: "/elsewhere" }));
  const r3 = await chat(f, { model: "komputer-muse", messages: [user("one"), { role: "assistant", content: r1.json().choices[0].message.content },
    user("two"), { role: "assistant", content: r2.json().choices[0].message.content }, user("three")] }, h);
  assert.equal(r3.status, 200, r3.text);
  assert.equal(r3.headers.get("x-komputer-session-recovered"), "client-history");
  assert.equal((await f.request("/v1/sessions/lost")).json().reseeds, 2);
});

test("muse: credentials, required MCP and rate limits map to client-actionable errors", async (t) => {
  const f = await museFixture(t);
  const auth = await chat(f, { model: "komputer-muse", messages: [user("FAIL_AUTH")] });
  assert.equal(auth.status, 502);
  assert.equal(auth.json().error.type, "upstream_auth_error");
  const body = await chat(f, { model: "komputer-muse", messages: [user("MCP_REQUIRED_DOWN")] });
  assert.equal(body.status, 503);
  assert.equal(body.json().error.type, "body_unavailable");
  assert.ok(body.headers.get("retry-after"));
  const limit = await chat(f, { model: "komputer-muse", messages: [user("FAIL_LIMIT")] });
  assert.equal(limit.status, 429);
  assert.equal(argAfter(f.museCalls()[0].args, "--no-session-log"), "--disable-approval", "stateless runs keep no session log");
});

test("muse: streaming, tool notes, images and task mode", async (t) => {
  const f = await museFixture(t, { KOMPUTER_MUSE_TASK_MODEL: "muse-mini" });
  const s = await chat(f, { model: "komputer-muse", stream: true, messages: [user("TOOL now")] });
  const chunks = s.text.split("\n\n").filter((b) => b.startsWith("data: {")).map((b) => JSON.parse(b.slice(6)));
  assert.match(chunks.map((c) => c.choices[0].delta.content || "").join(""), /^checking\n\nmuse:<system_instructions>[\s\S]*\n\nTOOL now$/);
  assert.equal(chunks.map((c) => c.choices[0].delta.reasoning_content || "").join(""), "\n[tool] mcp__komputer_use__run_command\n");

  const png = Buffer.from("fake png bytes").toString("base64");
  const img = await chat(f, { model: "komputer-muse", messages: [{ role: "user", content: [{ type: "text", text: "look" }, { type: "image_url", image_url: { url: `data:image/png;base64,${png}` } }] }] });
  assert.equal(img.status, 200, img.text);
  const c = f.museCalls().at(-1);
  assert.deepEqual(c.images, [{ ext: ".png", bytes: 14 }]);
  assert.equal(fs.existsSync(argAfter(c.args, "--image")), false, "image temp file is removed");
  const pdf = await chat(f, { model: "komputer-muse", messages: [{ role: "user", content: [{ type: "text", text: "doc" },
    { type: "file", file: { filename: "a.pdf", file_data: "data:application/pdf;base64,JVBERi0=" } }] }] });
  assert.equal(pdf.status, 400);

  await chat(f, { model: "komputer-muse", messages: [user("### Task: title")] });
  const task = f.museCalls().at(-1);
  assert.equal(argAfter(task.args, "--model"), "muse-mini");
  assert.ok(!task.args.includes("-c") && task.args.includes("--no-session-log"), "tasks run without MCP or a session");
});

test("muse: client tools use a private loopback MCP server passed through env", async (t) => {
  const f = await museFixture(t);
  const tools = [{ type: "function", function: { name: "lookup", parameters: { type: "object", properties: { query: { type: "string" } } } } }];
  const r = await chat(f, { model: "komputer-muse", tools, messages: [user("CLIENT_TOOL go")] });
  assert.equal(r.status, 200, r.text);
  const call = r.json().choices[0].message.tool_calls[0];
  assert.deepEqual(JSON.parse(call.function.arguments), { query: "from muse" });
  const c = f.museCalls().at(-1);
  assert.match(c.mcp.komputer_use.url, /^http:\/\/127\.0\.0\.1:\d+\/mcp$/);
  assert.match(c.mcp.komputer_use.headers.Authorization, /^Bearer [0-9a-f-]{72}$/);
  assert.match(argAfter(c.args, "--workspace"), /komputer-tools-/);
  assert.ok(c.args.includes("--no-session-log") && c.args.includes("--disable-shell"));
  const done = await chat(f, { model: "komputer-muse", tools, messages: [{ role: "tool", tool_call_id: call.id, content: "ok!" }] });
  assert.equal(done.status, 200, done.text);
  assert.equal(done.json().choices[0].message.content, "muse tool result:ok!");
});
