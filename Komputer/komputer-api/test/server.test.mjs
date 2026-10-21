// End-to-end tests: real server.mjs process + fake claude + fake body health endpoint.
// Run from komputer-api/: npm test
import { test, before, after } from "node:test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import http from "node:http";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const KEY = "k".repeat(40);
let tmp;
let base;
let server;
let healthSrv;
let healthHits = 0;

function calls() {
  const f = path.join(tmp, "state", "calls.jsonl");
  if (!fs.existsSync(f)) return [];
  return fs
    .readFileSync(f, "utf8")
    .trim()
    .split("\n")
    .filter(Boolean)
    .map((l) => JSON.parse(l));
}
const argVal = (args, flag) => args[args.indexOf(flag) + 1];

async function api(p, { method = "GET", key = KEY, body, headers = {} } = {}) {
  const res = await fetch(base + p, {
    method,
    headers: {
      ...(key ? { authorization: `Bearer ${key}` } : {}),
      ...(body ? { "content-type": "application/json" } : {}),
      ...headers,
    },
    body: body ? JSON.stringify(body) : undefined,
  });
  const text = await res.text();
  let json = null;
  try {
    json = JSON.parse(text);
  } catch {
    /* not json */
  }
  return { status: res.status, headers: res.headers, text, json };
}

function sseEvents(text) {
  return text
    .split("\n\n")
    .map((b) => b.trim())
    .filter(Boolean);
}

const listenable = (port) => new Promise((resolve) => {
  const s = http.createServer();
  s.once("error", () => resolve(false));
  s.listen(port, "127.0.0.1", () => s.close(() => resolve(true)));
});

/** Two consecutive free ports, below Linux's ephemeral range so other tests' listen(0) cannot take them. */
async function freePortPair() {
  for (let i = 0; i < 50; i++) {
    const port = 20000 + Math.floor(Math.random() * 12000);
    if ((await listenable(port)) && (await listenable(port + 1))) return port;
  }
  throw new Error("no free port pair");
}

before(async () => {
  tmp = fs.mkdtempSync(path.join(os.tmpdir(), "komputer-test-"));
  fs.mkdirSync(path.join(tmp, "state"));
  fs.writeFileSync(path.join(tmp, "KEYS.txt"), `api-key: ${KEY}\n`);
  healthSrv = http.createServer((req, res) => {
    healthHits += 1;
    setTimeout(() => res.end('{"ok":true}'), 200);
  });
  await new Promise((r) => healthSrv.listen(0, "127.0.0.1", r));
  const port = await freePortPair();
  base = `http://127.0.0.1:${port}`;
  server = spawn(process.execPath, [path.join(here, "..", "server.mjs")], {
    env: {
      ...process.env,
      KOMPUTER_API_PORT: String(port),
      KOMPUTER_API_HOST: `127.0.0.1,127.0.0.1:${port + 1}`,
      KOMPUTER_KEYS_FILE: path.join(tmp, "KEYS.txt"),
      KOMPUTER_SESSIONS_FILE: path.join(tmp, "sessions.json"),
      KOMPUTER_SESSION_DIR: tmp,
      KOMPUTER_CLAUDE_CWD: tmp,
      KOMPUTER_MCP_CONFIG: path.join(tmp, "mcp.json"),
      KOMPUTER_HEALTH_URL: `http://127.0.0.1:${healthSrv.address().port}/healthz`,
      KOMPUTER_AUTH_FAIL_DELAY_MS: "50",
      KOMPUTER_HEARTBEAT_MS: "200",
      KOMPUTER_BODY_NAME: "the island PC",
      BODY_SSH: "body-secret-marker",
      CUSTOM_LEAK_MARKER: "x",
      CLAUDE_BIN: path.join(here, "fake-claude.mjs"),
      FAKE_CLAUDE_STATE: path.join(tmp, "state"),
    },
    stdio: ["ignore", "pipe", "pipe"],
  });
  let log = "";
  server.stderr.on("data", (d) => (log += d));
  await new Promise((resolve, reject) => {
    const t = setTimeout(() => reject(new Error("server did not start: " + log)), 8000);
    server.stdout.on("data", (d) => {
      log += d;
      if (log.includes("listening on")) {
        clearTimeout(t);
        resolve();
      }
    });
  });
});

after(() => {
  server?.kill();
  healthSrv?.close();
});

test("anonymous /healthz reveals nothing beyond ok", async () => {
  const r = await api("/healthz", { key: null });
  assert.equal(r.status, 200);
  assert.deepEqual(r.json, { ok: true, service: "komputer-api" });
  const r2 = await api("/healthz");
  assert.equal(r2.json.builtin_tools, "");
  assert.ok("in_flight" in r2.json);
});

test("bad key is rejected, good key accepted", async () => {
  assert.equal((await api("/v1/models", { key: "wrong" })).status, 401);
  assert.equal((await api("/v1/models", { key: null })).status, 401);
  const ok = await api("/v1/models");
  assert.equal(ok.status, 200);
  assert.equal(ok.json.data[0].id, "komputer-claude");
});

test("first turn uses --session-id, prompt via stdin, system via file, island tool set", async () => {
  const r = await api("/v1/chat/completions", {
    method: "POST",
    headers: { "x-conversation-id": "conv-A" },
    body: {
      model: "komputer-claude",
      messages: [
        { role: "system", content: "CLIENT-SYSTEM-RULE" },
        { role: "user", content: "--help me please" },
      ],
    },
  });
  assert.equal(r.status, 200, r.text);
  assert.equal(r.json.choices[0].message.content, "echo:--help me please");
  assert.equal(r.headers.get("x-conversation-id"), "conv-A");
  assert.deepEqual(r.json.usage, { prompt_tokens: 10, completion_tokens: 11, total_tokens: 21 });
  const c = calls().at(-1);
  assert.equal(c.prompt, "--help me please");
  assert.ok(!c.args.includes("--help me please"), "prompt must not be in argv");
  assert.ok(c.args.includes("--session-id"));
  assert.equal(argVal(c.args, "--tools"), "", "island mode removes all built-in tools; MCP is unaffected");
  assert.equal(argVal(c.args, "--output-format"), "stream-json");
  assert.ok(c.system.includes("mcp__komputer_use__* tools"), c.system);
  assert.ok(c.system.includes("system_info"), c.system);
  assert.ok(c.system.includes("the island PC"), c.system);
  assert.doesNotMatch(
    c.system,
    /brain|body|remote|local|machine|launcher|tunnel|plumbing/i,
    "the note must describe exactly one computer"
  );
  assert.match(c.system, /CLIENT-SYSTEM-RULE/);
});

test("second turn on same conversation resumes the same Claude session", async () => {
  const first = calls()
    .filter((c) => c.prompt === "--help me please")
    .at(-1);
  const sid = argVal(first.args, "--session-id");
  const r = await api("/v1/chat/completions", {
    method: "POST",
    headers: { "x-conversation-id": "conv-A" },
    body: {
      messages: [
        { role: "user", content: "--help me please" },
        { role: "assistant", content: "echo:--help me please" },
        { role: "user", content: "again" },
      ],
    },
  });
  assert.equal(r.status, 200, r.text);
  assert.equal(r.json.choices[0].message.content, "prev=--help me please|now=again");
  const c = calls().at(-1);
  assert.equal(argVal(c.args, "--resume"), sid);
  assert.equal(c.prompt, "again", "resumed turns send only the newest user message");
});

test("new conversation that arrives with history carries it into the first turn", async () => {
  const r = await api("/v1/chat/completions", {
    method: "POST",
    body: {
      user: "conv-history",
      messages: [
        { role: "user", content: "old question" },
        { role: "assistant", content: "old answer" },
        { role: "user", content: "new question" },
      ],
    },
  });
  assert.equal(r.status, 200, r.text);
  const c = calls().at(-1);
  assert.match(c.prompt, /User: old question/);
  assert.match(c.prompt, /Assistant: old answer/);
  assert.match(c.prompt, /new question$/);
});

test("OpenAI streaming sends incremental deltas, tool notes, usage and [DONE]", async () => {
  const r = await api("/v1/chat/completions", {
    method: "POST",
    headers: { "x-conversation-id": "conv-stream" },
    body: { stream: true, messages: [{ role: "user", content: "TOOL go" }] },
  });
  assert.equal(r.status, 200);
  assert.match(r.headers.get("content-type"), /text\/event-stream/);
  const events = sseEvents(r.text).filter((e) => e.startsWith("data: "));
  assert.equal(events.at(-1), "data: [DONE]");
  const chunks = events.slice(0, -1).map((e) => JSON.parse(e.slice(6)));
  const content = chunks.map((c) => c.choices[0].delta.content || "").join("");
  assert.ok(content.startsWith("checking\n\n"), content);
  assert.ok(content.endsWith("echo:TOOL go"), content);
  assert.ok(
    chunks.some((c) => c.choices[0].delta.reasoning_content === "\n[tool] mcp__komputer_use__run_command\n")
  );
  const last = chunks.at(-1);
  assert.equal(last.choices[0].finish_reason, "stop");
  assert.equal(last.usage.completion_tokens, 11);
  assert.ok(chunks.filter((c) => c.choices[0].delta.content).length >= 3, "should be several deltas");
});

test("SSE keepalive comments are sent while a turn is running", async () => {
  const r = await api("/v1/chat/completions", {
    method: "POST",
    headers: { "x-conversation-id": "conv-slow" },
    body: { stream: true, messages: [{ role: "user", content: "SLOW" }] },
  });
  assert.ok(r.text.includes(": keepalive"), "expected heartbeat comments");
  assert.ok(r.text.includes("tick5"));
});

test("Anthropic /v1/messages streaming event order", async () => {
  const r = await api("/v1/messages", {
    method: "POST",
    headers: { "x-conversation-id": "conv-anth" },
    body: {
      stream: true,
      model: "x",
      max_tokens: 100,
      system: [{ type: "text", text: "S" }],
      messages: [{ role: "user", content: [{ type: "text", text: "hi" }] }],
    },
  });
  assert.equal(r.status, 200);
  const names = sseEvents(r.text)
    .filter((e) => e.startsWith("event:"))
    .map((e) => e.split("\n")[0].slice(7));
  assert.deepEqual(
    [names[0], names[1], names.at(-3), names.at(-2), names.at(-1)],
    ["message_start", "content_block_start", "content_block_stop", "message_delta", "message_stop"]
  );
  const text = sseEvents(r.text)
    .filter((e) => e.startsWith("event: content_block_delta"))
    .map((e) => JSON.parse(e.split("\n")[1].slice(6)).delta.text)
    .join("");
  assert.equal(text, "echo:hi");
});

test("Anthropic /v1/messages non-stream", async () => {
  const r = await api("/v1/messages", {
    method: "POST",
    headers: { "x-conversation-id": "conv-anth2" },
    body: { messages: [{ role: "user", content: "yo" }] },
  });
  assert.equal(r.status, 200, r.text);
  assert.equal(r.json.content[0].text, "echo:yo");
  assert.equal(r.json.usage.output_tokens, 11);
});

test("expired brain login maps to 502 upstream_auth_error and first-turn uuid is rotated", async () => {
  const r = await api("/v1/chat/completions", {
    method: "POST",
    headers: { "x-conversation-id": "conv-auth" },
    body: { messages: [{ role: "user", content: "FAIL_AUTH" }] },
  });
  assert.equal(r.status, 502);
  assert.equal(r.json.error.type, "upstream_auth_error");
  assert.match(r.json.error.message, /sign in/i);
  assert.doesNotMatch(r.json.error.message, /brain host|OAuth session expired/);
  const failedSid = argVal(calls().at(-1).args, "--session-id");
  const r2 = await api("/v1/chat/completions", {
    method: "POST",
    headers: { "x-conversation-id": "conv-auth" },
    body: { messages: [{ role: "user", content: "retry" }] },
  });
  assert.equal(r2.status, 200, r2.text);
  const c = calls().at(-1);
  assert.ok(c.args.includes("--session-id"));
  assert.notEqual(argVal(c.args, "--session-id"), failedSid);
});

test("a dropped connection does not kill the turn; the retry gets the cached answer free", async () => {
  const headers = {
    authorization: `Bearer ${KEY}`,
    "content-type": "application/json",
    "x-conversation-id": "conv-abort",
  };
  const payload = JSON.stringify({ stream: true, messages: [{ role: "user", content: "SLOW abort" }] });
  const ac = new AbortController();
  const dropped = fetch(base + "/v1/chat/completions", { method: "POST", signal: ac.signal, headers, body: payload })
    .then((r) => r.text())
    .catch(() => null);
  await new Promise((r) => setTimeout(r, 900));
  ac.abort();
  await dropped;
  const runs = () => calls().filter((c) => c.prompt === "SLOW abort").length;
  assert.equal(runs(), 1);
  const pid = calls()
    .filter((c) => c.prompt === "SLOW abort")
    .at(-1).pid;

  // The turn keeps going without a listener and finishes normally.
  await new Promise((r) => setTimeout(r, 3500));
  assert.ok(fs.existsSync(path.join(tmp, "state", `finished-${pid}`)), "claude should have run to completion");

  // Same message again: served from the finished turn, no second claude run.
  const retry = await fetch(base + "/v1/chat/completions", { method: "POST", headers, body: payload });
  const text = await retry.text();
  assert.equal(retry.status, 200);
  assert.ok(text.includes("tick5"), "retry should replay the whole answer");
  assert.equal(runs(), 1, "the retry must not start a second claude run");

  // A third send of the same message is a genuine new request and does run again.
  await fetch(base + "/v1/chat/completions", { method: "POST", headers, body: payload }).then((r) => r.text());
  assert.equal(runs(), 2);
});

test("a retry while the turn is still running attaches and replays what it missed", async () => {
  const headers = {
    authorization: `Bearer ${KEY}`,
    "content-type": "application/json",
    "x-conversation-id": "conv-attach",
  };
  const payload = JSON.stringify({ stream: true, messages: [{ role: "user", content: "SLOW attach" }] });
  const ac = new AbortController();
  const dropped = fetch(base + "/v1/chat/completions", { method: "POST", signal: ac.signal, headers, body: payload })
    .then((r) => r.text())
    .catch(() => null);
  await new Promise((r) => setTimeout(r, 700));
  ac.abort();
  await dropped;

  // Rejoin while the first run is mid-flight.
  const rejoined = await fetch(base + "/v1/chat/completions", { method: "POST", headers, body: payload });
  const text = await rejoined.text();
  assert.equal(rejoined.status, 200);
  for (const t of ["tick0", "tick1", "tick5"]) assert.ok(text.includes(t), `missing ${t}`);
  assert.equal(calls().filter((c) => c.prompt === "SLOW attach").length, 1, "only one claude run");
});

test("a streaming request that fails before any output returns a real status, not a 200 stream", async () => {
  const r = await api("/v1/chat/completions", {
    method: "POST",
    headers: { "x-conversation-id": "conv-stream-fail" },
    body: { stream: true, messages: [{ role: "user", content: "FAIL_AUTH" }] },
  });
  assert.equal(r.status, 502);
  assert.match(r.headers.get("content-type"), /application\/json/);
  assert.equal(r.json.error.type, "upstream_auth_error");
});

test("rate limits and body outages carry Retry-After", async () => {
  const limited = await api("/v1/chat/completions", {
    method: "POST",
    headers: { "x-conversation-id": "conv-limit" },
    body: { messages: [{ role: "user", content: "FAIL_LIMIT" }] },
  });
  assert.equal(limited.status, 429);
  assert.equal(limited.headers.get("retry-after"), "60");
});

test("concurrent requests share a single body health probe", async () => {
  await new Promise((r) => setTimeout(r, 15_500)); // let the 15s ok-cache expire
  const hitsBefore = healthHits;
  const results = await Promise.all(
    [1, 2, 3].map((i) =>
      api("/v1/chat/completions", {
        method: "POST",
        headers: { "x-conversation-id": `conv-par-${i}` },
        body: { messages: [{ role: "user", content: `p${i}` }] },
      })
    )
  );
  for (const r of results) assert.equal(r.status, 200, r.text);
  assert.equal(healthHits - hitsBefore, 1);
});

test("session ids with % are decoded exactly once", async () => {
  const created = await api("/v1/sessions", { method: "POST", body: { conversation_id: "a%b" } });
  assert.equal(created.status, 201);
  const got = await api("/v1/sessions/" + encodeURIComponent("a%b"));
  assert.equal(got.status, 200);
  assert.equal(got.json.client_key, "a%b");
  assert.equal((await api("/v1/sessions/%E0%A4%A")).status, 400);
  assert.equal(
    (await api("/v1/sessions/" + encodeURIComponent("a%b"), { method: "DELETE" })).status,
    200
  );
});

test("invalid JSON body returns 400, not 500", async () => {
  const res = await fetch(base + "/v1/chat/completions", {
    method: "POST",
    headers: { authorization: `Bearer ${KEY}`, "content-type": "application/json" },
    body: "{nope",
  });
  assert.equal(res.status, 400);
});

// ------------------------------------------------------------ 0.5 features

const chat = (headers, body) => api("/v1/chat/completions", { method: "POST", headers, body });
const lastCall = () => calls().at(-1);

test("model ids map to claude --model; unknown ids fall back to default; effort passes through", async () => {
  await chat(
    { "x-conversation-id": "conv-model" },
    { model: "komputer-opus", reasoning_effort: "high", messages: [{ role: "user", content: "m1" }] }
  );
  let c = lastCall();
  assert.equal(argVal(c.args, "--model"), "opus");
  assert.equal(argVal(c.args, "--effort"), "high");
  await chat({ "x-conversation-id": "conv-model" }, { model: "gpt-4o", messages: [{ role: "user", content: "m2" }] });
  c = lastCall();
  assert.ok(!c.args.includes("--model"));
  assert.ok(!c.args.includes("--effort"));
  await chat(
    { "x-conversation-id": "conv-model" },
    { model: "claude-sonnet-5", reasoning_effort: "minimal", messages: [{ role: "user", content: "m3" }] }
  );
  c = lastCall();
  assert.equal(argVal(c.args, "--model"), "claude-sonnet-5");
  assert.equal(argVal(c.args, "--effort"), "low");
  await chat(
    { "x-conversation-id": "conv-model" },
    { model: "komputer-fable", messages: [{ role: "user", content: "m4" }] }
  );
  c = lastCall();
  assert.equal(argVal(c.args, "--model"), "fable");
  const models = await api("/v1/models");
  assert.deepEqual(
    models.json.data.map((m) => m.id),
    ["komputer-claude", "komputer-opus", "komputer-sonnet", "komputer-haiku", "komputer-fable"]
  );
});

test("requests without any conversation id run stateless with full history and no mapping", async () => {
  const before = (await api("/v1/sessions")).json.data.length;
  const r = await chat(
    {},
    {
      messages: [
        { role: "user", content: "hello" },
        { role: "assistant", content: "hi there" },
        { role: "user", content: "stateless q" },
      ],
    }
  );
  assert.equal(r.status, 200, r.text);
  assert.equal(r.headers.get("x-komputer-mode"), "stateless");
  const c = lastCall();
  assert.ok(c.args.includes("--no-session-persistence"));
  assert.ok(!c.args.includes("--session-id") && !c.args.includes("--resume"));
  assert.ok(c.args.includes("--mcp-config"));
  assert.match(c.prompt, /Assistant: hi there/);
  assert.equal((await api("/v1/sessions")).json.data.length, before);
});

test("X-Komputer-Ephemeral forces a one-shot turn even with a conversation id", async () => {
  const r = await chat(
    { "x-conversation-id": "conv-eph", "x-komputer-ephemeral": "1" },
    { messages: [{ role: "user", content: "eph" }] }
  );
  assert.equal(r.headers.get("x-komputer-mode"), "stateless");
  assert.ok(lastCall().args.includes("--no-session-persistence"));
  assert.equal((await api("/v1/sessions/conv-eph")).status, 404);
});

test("Open WebUI background tasks run on the task model without tools, MCP or session", async () => {
  await chat({ "x-conversation-id": "conv-task" }, { messages: [{ role: "user", content: "real question" }] });
  const turnsBefore = (await api("/v1/sessions/conv-task")).json.turn_count;
  const r = await chat(
    { "x-conversation-id": "conv-task" },
    { messages: [{ role: "user", content: "### Task:\nGenerate a concise title.\n### Chat History:\n..." }] }
  );
  assert.equal(r.status, 200, r.text);
  assert.equal(r.headers.get("x-komputer-mode"), "task");
  const c = lastCall();
  assert.equal(argVal(c.args, "--model"), "haiku");
  assert.equal(argVal(c.args, "--tools"), "");
  assert.ok(!c.args.includes("--mcp-config"));
  assert.ok(c.args.includes("--no-session-persistence"));
  assert.doesNotMatch(c.system || "", /mcp__komputer_use__/);
  assert.equal((await api("/v1/sessions/conv-task")).json.turn_count, turnsBefore);
});

test("regenerate / edit is detected and reseeds a fresh Claude session from client history", async () => {
  const h = { "x-conversation-id": "conv-regen" };
  const r1 = await chat(h, { messages: [{ role: "user", content: "q1" }] });
  const a1 = r1.json.choices[0].message.content;
  const sid1 = argVal(lastCall().args, "--session-id");

  // Normal continuation: previous assistant message matches our last reply → resume.
  await chat(h, {
    messages: [
      { role: "user", content: "q1" },
      { role: "assistant", content: a1 },
      { role: "user", content: "q2" },
    ],
  });
  assert.equal(argVal(lastCall().args, "--resume"), sid1);

  // Regenerate q2: client drops our q2 answer and resends → history no longer matches → reseed.
  const r3 = await chat(h, {
    messages: [
      { role: "user", content: "q1" },
      { role: "assistant", content: a1 },
      { role: "user", content: "q2" },
    ],
  });
  let c = lastCall();
  const sid2 = argVal(c.args, "--session-id");
  assert.ok(sid2 && sid2 !== sid1, "expected a fresh session id");
  assert.match(c.prompt, /User: q1/);
  assert.ok(c.prompt.includes(`Assistant: ${a1}`));
  assert.match(c.prompt, /q2$/);
  assert.equal((await api("/v1/sessions/conv-regen")).json.reseeds, 1);

  // Continue after the regenerated answer → resume the new session with just the new message.
  await chat(h, {
    messages: [
      { role: "user", content: "q1" },
      { role: "assistant", content: a1 },
      { role: "user", content: "q2" },
      { role: "assistant", content: r3.json.choices[0].message.content },
      { role: "user", content: "q3" },
    ],
  });
  c = lastCall();
  assert.equal(argVal(c.args, "--resume"), sid2);
  assert.equal(c.prompt, "q3");
});

test("clients that only send the newest message keep resuming (no false regenerate)", async () => {
  const h = { "x-conversation-id": "conv-bare" };
  await chat(h, { messages: [{ role: "user", content: "b1" }] });
  const sid = argVal(lastCall().args, "--session-id");
  await chat(h, { messages: [{ role: "user", content: "b2" }] });
  await chat(h, { messages: [{ role: "user", content: "b3" }] });
  assert.equal(argVal(lastCall().args, "--resume"), sid);
  assert.equal((await api("/v1/sessions/conv-bare")).json.reseeds, 0);
});

test("streamed replies are fingerprinted as streamed (tool preamble included)", async () => {
  const h = { "x-conversation-id": "conv-streamfp" };
  const r = await chat(h, { stream: true, messages: [{ role: "user", content: "TOOL x" }] });
  const streamedText = sseEvents(r.text)
    .filter((e) => e.startsWith("data: {"))
    .map((e) => JSON.parse(e.slice(6)).choices[0].delta.content || "")
    .join("");
  const sid = argVal(lastCall().args, "--session-id");
  await chat(h, {
    messages: [
      { role: "user", content: "TOOL x" },
      { role: "assistant", content: streamedText },
      { role: "user", content: "next" },
    ],
  });
  assert.equal(argVal(lastCall().args, "--resume"), sid);
});

test("image and PDF attachments are sent to Claude Code as stream-json content blocks", async () => {
  const png = Buffer.from("fakepng").toString("base64");
  const pdf = Buffer.from("%PDF-1.4 fake").toString("base64");
  const r = await chat(
    { "x-conversation-id": "conv-img" },
    {
      messages: [
        {
          role: "user",
          content: [
            { type: "text", text: "what is in these?" },
            { type: "image_url", image_url: { url: `data:image/png;base64,${png}` } },
            { type: "image_url", image_url: { url: "https://example.com/cat.jpg" } },
            { type: "file", file: { filename: "spec.pdf", file_data: `data:application/pdf;base64,${pdf}` } },
          ],
        },
      ],
    }
  );
  assert.equal(r.status, 200, r.text);
  const c = lastCall();
  assert.equal(argVal(c.args, "--input-format"), "stream-json");
  assert.equal(c.prompt, "what is in these?");
  assert.deepEqual(
    c.attachments.map((a) => [a.type, a.source_type, a.media_type ?? null]),
    [
      ["image", "base64", "image/png"],
      ["image", "url", null],
      ["document", "base64", "application/pdf"],
    ]
  );
  assert.equal(c.attachments[2].title, "spec.pdf");
  // Plain text turns keep using a plain stdin prompt.
  await chat({ "x-conversation-id": "conv-img" }, { messages: [{ role: "user", content: "plain" }] });
  assert.ok(!lastCall().args.includes("--input-format"));
});

test("Anthropic image blocks pass through unchanged", async () => {
  const r = await api("/v1/messages", {
    method: "POST",
    headers: { "x-conversation-id": "conv-anth-img" },
    body: {
      messages: [
        {
          role: "user",
          content: [
            { type: "image", source: { type: "base64", media_type: "image/jpeg", data: "abcd" } },
            { type: "text", text: "describe" },
          ],
        },
      ],
    },
  });
  assert.equal(r.status, 200, r.text);
  const c = lastCall();
  assert.equal(c.prompt, "describe");
  assert.deepEqual(
    c.attachments.map((a) => a.media_type),
    ["image/jpeg"]
  );
});

test("thinking streams as reasoning_content (OpenAI) and a thinking block (Anthropic)", async () => {
  const r = await chat({ "x-conversation-id": "conv-think" }, { stream: true, messages: [{ role: "user", content: "THINK a" }] });
  const chunks = sseEvents(r.text)
    .filter((e) => e.startsWith("data: {"))
    .map((e) => JSON.parse(e.slice(6)));
  assert.ok(chunks.some((c) => c.choices[0].delta.reasoning_content === "pondering"));
  const r2 = await api("/v1/messages", {
    method: "POST",
    headers: { "x-conversation-id": "conv-think2" },
    body: { stream: true, messages: [{ role: "user", content: "THINK b" }] },
  });
  const starts = sseEvents(r2.text)
    .filter((e) => e.startsWith("event: content_block_start"))
    .map((e) => JSON.parse(e.split("\n")[1].slice(6)));
  assert.deepEqual(
    starts.map((s) => [s.index, s.content_block.type]),
    [
      [0, "thinking"],
      [1, "text"],
    ]
  );
  const nonStream = await chat({ "x-conversation-id": "conv-think3" }, { messages: [{ role: "user", content: "THINK c" }] });
  assert.equal(nonStream.json.choices[0].message.reasoning_content, "pondering");
});

test("no cap on live conversations by default", async () => {
  for (let i = 0; i < 8; i++) {
    const r = await api("/v1/sessions", { method: "POST", body: { conversation_id: `bulk-${i}` } });
    assert.equal(r.status, 201, r.text);
  }
  assert.equal((await api("/healthz")).json.max_sessions, "unlimited");
});

test("count_tokens returns an estimate", async () => {
  const r = await api("/v1/messages/count_tokens", {
    method: "POST",
    body: { messages: [{ role: "user", content: "x".repeat(350) }] },
  });
  assert.equal(r.status, 200);
  assert.equal(r.json.input_tokens, 100);
});

test("a second host:port listen address serves the same API", async () => {
  const alt = base.replace(/:(\d+)$/, (_, p) => `:${Number(p) + 1}`);
  const r = await fetch(`${alt}/v1/models`, { headers: { authorization: `Bearer ${KEY}` } });
  assert.equal(r.status, 200);
  const h = await api("/healthz");
  assert.equal(h.json.listen.length, 2);
});

// ------------------------------------------------------------ island (L2) mode

test("the island child sees no wrapper env, keeps PATH, and gets a git ceiling", async () => {
  await chat({ "x-conversation-id": "conv-island-env" }, { messages: [{ role: "user", content: "env check" }] });
  const c = lastCall();
  assert.deepEqual(c.env.leaked, [], `wrapper env leaked into the island: ${c.env.leaked}`);
  assert.equal(c.env.path, true, "PATH must survive the allowlist");
  assert.equal(c.env.fakeState, true, "the test seam must survive the allowlist");
  assert.equal(c.env.gitCeiling, path.dirname(fs.realpathSync(tmp)), "git discovery must stop before the parent of cwd");
});

test("KOMPUTER_BUILTIN_TOOLS=default restores local tools; KOMPUTER_SYSTEM_PROMPT_FILE passes through", async () => {
  const sysFile = path.join(tmp, "custom-system.txt");
  fs.writeFileSync(sysFile, "custom island persona");
  const port = Number(new URL(base).port) + 2; // the main server holds port and port+1
  const lite = spawn(process.execPath, [path.join(here, "..", "server.mjs")], {
    env: {
      ...process.env,
      KOMPUTER_API_PORT: String(port),
      KOMPUTER_API_HOST: "127.0.0.1",
      KOMPUTER_KEYS_FILE: path.join(tmp, "KEYS.txt"),
      KOMPUTER_SESSIONS_FILE: path.join(tmp, "sessions-escape.json"),
      KOMPUTER_SESSION_DIR: tmp,
      KOMPUTER_CLAUDE_CWD: tmp,
      KOMPUTER_MCP_CONFIG: path.join(tmp, "mcp.json"),
      KOMPUTER_HEALTH_URL: `http://127.0.0.1:${healthSrv.address().port}/healthz`,
      KOMPUTER_AUTH_FAIL_DELAY_MS: "50",
      KOMPUTER_BUILTIN_TOOLS: "default",
      KOMPUTER_SYSTEM_PROMPT_FILE: sysFile,
      CLAUDE_BIN: path.join(here, "fake-claude.mjs"),
      FAKE_CLAUDE_STATE: path.join(tmp, "state"),
    },
    stdio: ["ignore", "pipe", "pipe"],
  });
  try {
    let log = "";
    await new Promise((resolve, reject) => {
      const t = setTimeout(() => reject(new Error("escape server did not start: " + log)), 8000);
      lite.stdout.on("data", (d) => {
        log += d;
        if (log.includes("listening on")) {
          clearTimeout(t);
          resolve();
        }
      });
    });
    const before = calls().length;
    const res = await fetch(`http://127.0.0.1:${port}/v1/chat/completions`, {
      method: "POST",
      headers: { authorization: `Bearer ${KEY}`, "content-type": "application/json", "x-conversation-id": "conv-escape" },
      body: JSON.stringify({ messages: [{ role: "user", content: "escape check" }] }),
    });
    assert.equal(res.status, 200, await res.text());
    const c = calls().slice(before).at(-1);
    assert.ok(!c.args.includes("--tools"), "legacy default must not restrict tools");
    assert.equal(argVal(c.args, "--system-prompt-file"), sysFile);
  } finally {
    lite.kill();
  }
});
