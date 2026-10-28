import { test } from "node:test";
import assert from "node:assert/strict";
import { spawn, spawnSync } from "node:child_process";
import { once } from "node:events";
import http from "node:http";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const KEY = "r".repeat(40);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function fixture(t, extra = {}, prepare = () => {}) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "klaud-reliability-"));
  fs.mkdirSync(path.join(dir, "state"));
  fs.writeFileSync(path.join(dir, "KEYS.txt"), `api-key: ${KEY}\n`);
  prepare(dir);
  const reservation = http.createServer();
  await new Promise((r) => reservation.listen(0, "127.0.0.1", r));
  const port = reservation.address().port;
  await new Promise((r) => reservation.close(r));
  const childEnv = { ...process.env };
  for (const k of Object.keys(childEnv)) if (/^KOMPUTER_/i.test(k)) delete childEnv[k];
  const child = spawn(process.execPath, [path.join(here, "..", "server.mjs")], {
    env: {
      ...childEnv,
      KOMPUTER_API_HOST: "127.0.0.1",
      KOMPUTER_API_PORT: String(port),
      KOMPUTER_KEYS_FILE: path.join(dir, "KEYS.txt"),
      KOMPUTER_SESSION_DIR: dir,
      KOMPUTER_CLAUDE_CWD: dir,
      KOMPUTER_SESSIONS_FILE: path.join(dir, "sessions.json"),
      KOMPUTER_SKIP_TUNNEL: "1",
      KOMPUTER_AUTH_FAIL_DELAY_MS: "1",
      CLAUDE_BIN: path.join(here, "fake-claude.mjs"),
      FAKE_CLAUDE_STATE: path.join(dir, "state"),
      ...Object.fromEntries(Object.entries(extra).map(([k, v]) => [k, typeof v === "function" ? v(dir) : v])),
    },
    stdio: ["ignore", "pipe", "pipe"],
  });
  let logs = "";
  child.stdout.on("data", (d) => (logs += d));
  child.stderr.on("data", (d) => (logs += d));
  const exited = once(child, "exit");
  t.after(async () => {
    if (child.exitCode === null && child.signalCode === null) child.kill();
    await exited;
    fs.rmSync(dir, { recursive: true, force: true });
  });
  const ready = async () => {
    for (let i = 0; i < 200; i++) {
      if (logs.includes("listening on")) return;
      if (child.exitCode !== null) throw new Error(logs);
      await sleep(25);
    }
    throw new Error(`startup timeout: ${logs}`);
  };
  async function request(endpoint, body, headers = {}) {
    const res = await fetch(`http://127.0.0.1:${port}${endpoint}`, {
      method: body ? "POST" : "GET",
      headers: { authorization: `Bearer ${KEY}`, "content-type": "application/json", ...headers },
      body: body ? JSON.stringify(body) : undefined,
      signal: AbortSignal.timeout(8000),
    });
    const text = await res.text();
    return { status: res.status, headers: res.headers, text, json: () => JSON.parse(text) };
  }
  return {
    dir, child, exited, ready, request,
    logs: () => logs,
    calls: () => {
      const f = path.join(dir, "state", "calls.jsonl");
      return fs.existsSync(f) ? fs.readFileSync(f, "utf8").trim().split("\n").filter(Boolean).map(JSON.parse) : [];
    },
    chat: (content, opts = {}, headers = {}) => request("/v1/chat/completions", { messages: [{ role: "user", content }], ...opts }, headers),
  };
}

test("git ceiling actually blocks parent checkout discovery", async (t) => {
  const f = await fixture(t, {
    KOMPUTER_CLAUDE_CWD: (dir) => path.join(dir, "repo", "session"),
  }, (dir) => {
    fs.mkdirSync(path.join(dir, "repo", "session"), { recursive: true });
    const init = spawnSync("git", ["init", "--quiet", path.join(dir, "repo")], { encoding: "utf8" });
    assert.equal(init.status, 0, init.stderr);
  });
  await f.ready();
  assert.equal((await f.chat("git environment")).status, 200);
  const cwd = path.join(f.dir, "repo", "session");
  const gitEnv = { ...process.env };
  for (const k of Object.keys(gitEnv)) if (/^GIT_/i.test(k)) delete gitEnv[k];
  const probe = (ceiling) => spawnSync("git", ["rev-parse", "--show-toplevel"], {
    cwd, encoding: "utf8", env: { ...gitEnv, GIT_CEILING_DIRECTORIES: ceiling },
  });
  assert.equal(probe(cwd).status, 0, "control: cwd ceiling reproduces the old leak");
  const bounded = probe(f.calls()[0].env.gitCeiling);
  assert.notEqual(bounded.status, 0, "Claude's real git environment must not discover the parent repo");
  assert.match(bounded.stderr, /not a git repository/i);
});

test("missing or non-file system prompt and invalid cwd fail at startup", async (t) => {
  for (const kind of ["missing", "directory", "cwd-file"]) await t.test(kind, async (t) => {
    const f = await fixture(t, kind === "cwd-file"
      ? { KOMPUTER_CLAUDE_CWD: (dir) => path.join(dir, "KEYS.txt") }
      : { KOMPUTER_SYSTEM_PROMPT_FILE: (dir) => kind === "missing" ? path.join(dir, "missing.txt") : dir });
    const exit = await Promise.race([f.exited, sleep(1000).then(() => null)]);
    assert.ok(exit, "invalid startup configuration should terminate without serving requests");
    assert.notEqual(exit[0], 0);
    assert.doesNotMatch(f.logs(), /listening on/);
  });
});

test("a failed secondary bind exits nonzero instead of leaving a half-live API", async (t) => {
  const occupied = http.createServer();
  await new Promise((r) => occupied.listen(0, "127.0.0.1", r));
  t.after(() => new Promise((r) => occupied.close(r)));
  const f = await fixture(t, { KOMPUTER_API_HOST: `127.0.0.1,127.0.0.1:${occupied.address().port}` });
  const exit = await Promise.race([f.exited, sleep(1000).then(() => null)]);
  assert.ok(exit, "API must exit when a required listen address fails");
  assert.notEqual(exit[0], 0);
  assert.match(f.logs(), /EADDRINUSE/);
});

test("relative system prompt is resolved before changing the Claude working directory", async (t) => {
  const f = await fixture(t, {
    KOMPUTER_SYSTEM_PROMPT_FILE: (dir) => path.relative(process.cwd(), path.join(dir, "prompt.txt")),
  }, (dir) => fs.writeFileSync(path.join(dir, "prompt.txt"), "test prompt"));
  await f.ready();
  assert.equal((await f.chat("relative prompt")).status, 200);
  const args = f.calls()[0].args;
  assert.equal(args[args.indexOf("--system-prompt-file") + 1], path.join(f.dir, "prompt.txt"));
});

test("default working directory is neutral and outside the application checkout", async (t) => {
  const f = await fixture(t, {
    KOMPUTER_CLAUDE_CWD: "",
    HOME: (dir) => dir,
    USERPROFILE: (dir) => dir,
  });
  await f.ready();
  assert.equal((await f.chat("default working directory")).status, 200);
  const cwd = f.calls()[0].cwd;
  const parent = path.join(fs.realpathSync(f.dir), ".komputer", "workspaces");
  assert.equal(path.dirname(cwd), parent);
  assert.match(path.basename(cwd), /^[a-f0-9]{16}$/);
  assert.equal(fs.existsSync(path.join(cwd, ".git")), false);
  assert.equal(f.calls()[0].env.gitCeiling, parent);
});

test("komputer names are used for models, response metadata and both session input forms", async (t) => {
  const f = await fixture(t);
  await f.ready();
  const models = (await f.request("/v1/models")).json().data;
  assert.ok(models.every((m) => m.id.startsWith("komputer-") && m.owned_by === "komputer"));
  assert.equal((await f.request("/healthz")).json().service, "komputer-api");
  const first = await f.chat("name check", { model: "komputer-sonnet" }, { "x-komputer-session": "named-chat" });
  assert.equal(first.status, 200);
  assert.equal(first.headers.get("x-komputer-session"), "named-chat");
  assert.equal(first.json().komputer_session, "named-chat");
  assert.equal(first.json().komputer_conversation_id, "named-chat");
  const second = await f.chat("next", { komputer_session: "named-chat" });
  assert.equal(second.status, 200);
  assert.ok(f.calls().at(-1).args.includes("--resume"));
  const once = await f.chat("ephemeral", { komputer_ephemeral: true, komputer_session: "named-chat" });
  assert.equal(once.headers.get("x-komputer-mode"), "stateless");
  const forbidden = new RegExp(String.fromCharCode(97, 103, 101, 110, 116, 118, 114), "i");
  assert.doesNotMatch(JSON.stringify({ models, body: first.json(), headers: [...first.headers], logs: f.logs() }), forbidden);
});

test("missing resume reseeds once with supplied history and consistent session headers", async (t) => {
  const f = await fixture(t);
  await f.ready();
  const headers = { "x-conversation-id": "lost-history" };
  const first = await f.chat("remember this", {}, headers);
  assert.equal(first.status, 200);
  const oldId = first.headers.get("x-komputer-claude-session");
  fs.unlinkSync(path.join(f.dir, "state", `${oldId}.txt`));
  const reply = await f.chat("continue", { messages: [
    { role: "user", content: "remember this" },
    { role: "assistant", content: first.json().choices[0].message.content },
    { role: "user", content: "continue" },
  ] }, headers);
  assert.equal(reply.status, 200, reply.text);
  assert.equal(f.calls().length, 3, "one resume failure and exactly one fresh run");
  assert.match(f.calls().at(-1).prompt, /User: remember this/);
  const session = (await f.request("/v1/sessions/lost-history")).json();
  assert.notEqual(session.claude_session_id, oldId);
  assert.equal(session.reseeds, 1);
  assert.equal(session.turn_count, 2);
  assert.equal(reply.headers.get("x-komputer-claude-session"), session.claude_session_id);
  assert.equal((await f.chat("next", { messages: [
    { role: "user", content: "remember this" },
    { role: "assistant", content: first.json().choices[0].message.content },
    { role: "user", content: "continue" },
    { role: "assistant", content: reply.json().choices[0].message.content },
    { role: "user", content: "next" },
  ] }, headers)).status, 200);
  assert.ok(f.calls().at(-1).args.includes("--resume"));
});

test("a missing-session message after output or a tool call is never automatically replayed", async (t) => {
  for (const kind of ["TEXT", "TOOL"]) await t.test(kind, async (t) => {
    const f = await fixture(t);
    await f.ready();
    const headers = { "x-conversation-id": `active-${kind}` };
    await f.chat("start", {}, headers);
    const reply = await f.chat(`FAIL_AFTER_${kind}_MISSING`, {}, headers);
    assert.equal(reply.status, 502, reply.text);
    assert.equal(f.calls().length, 2, "retry could execute side effects twice");
    assert.equal((await f.request(`/v1/sessions/active-${kind}`)).json().reseeds, 0);
  });
});

test("missing history without a transcript is explicit; repeated missing errors are retried only once", async (t) => {
  const f = await fixture(t);
  await f.ready();
  const headers = { "x-conversation-id": "bare-recovery" };
  await f.chat("first", {}, headers);
  const r = await f.chat("ALWAYS_MISSING", {}, headers);
  assert.equal(r.status, 502);
  assert.equal(f.calls().length, 3, "one initial turn, one failed resume and one failed reseed");
  assert.match(f.calls().at(-1).prompt, /Earlier conversation context is unavailable/);
  const fresh = await f.chat("retry normally", {}, headers);
  assert.equal(fresh.status, 200, fresh.text);
  assert.ok(f.calls().at(-1).args.includes("--session-id"), "a failed reseed must not be persisted as started");
  const id = fresh.headers.get("x-komputer-claude-session");
  fs.unlinkSync(path.join(f.dir, "state", `${id}.txt`));
  const recovered = await f.chat("newest only", {}, headers);
  assert.equal(recovered.status, 200, recovered.text);
  assert.equal(recovered.headers.get("x-komputer-session-recovered"), "empty");
});

test("JSON and both SSE protocols withhold internal errors and redact private diagnostics", async (t) => {
  const f = await fixture(t);
  await f.ready();
  for (const endpoint of ["/v1/chat/completions", "/v1/messages"]) {
    for (const stream of [false, true]) {
      const r = await f.request(endpoint, { stream, messages: [{ role: "user", content: `FAIL_PRIVATE ${stream ? "AFTER_OUTPUT" : ""}` }] });
      assert.equal(r.status, stream ? 200 : 502);
      assert.doesNotMatch(r.text, /brain host|private\/operator|tunnel stderr|private-test-token|sk-testsecret12345/);
      assert.match(r.text, /upstream_error/);
    }
  }
  assert.match(f.logs(), /private\/operator/, "operators retain diagnostic context");
  assert.doesNotMatch(f.logs(), /private-test-token|sk-testsecret12345/);
});

test("an early SSE heartbeat does not suppress protocol start events", async (t) => {
  const f = await fixture(t, { KOMPUTER_SSE_OPEN_MS: "20", KOMPUTER_HEARTBEAT_MS: "20" });
  await f.ready();
  const openai = await f.chat("SILENT_START openai", { stream: true });
  assert.match(openai.text, /: keepalive/);
  const chunks = openai.text.split("\n").filter((l) => l.startsWith("data: {")).map((l) => JSON.parse(l.slice(6)));
  assert.equal(chunks[0].choices[0].delta.role, "assistant");
  assert.equal(chunks.filter((c) => c.choices[0].delta.role).length, 1);
  const anthropic = await f.request("/v1/messages", { stream: true, messages: [{ role: "user", content: "SILENT_START anthropic" }] });
  const events = anthropic.text.split("\n").filter((l) => l.startsWith("event: "));
  assert.equal(events[0], "event: message_start");
  assert.equal(events.filter((e) => e === "event: message_start").length, 1);
});

test("same user text with different system context is not coalesced", async (t) => {
  const f = await fixture(t);
  await f.ready();
  const [a, b] = await Promise.all([
    f.chat("SILENT_START ECHO_SYSTEM", { system: "context-alpha" }),
    f.chat("SILENT_START ECHO_SYSTEM", { system: "context-beta" }),
  ]);
  assert.equal(a.status, 200);
  assert.equal(b.status, 200);
  assert.equal(f.calls().length, 2);
  assert.match(a.json().choices[0].message.content, /context-alpha/);
  assert.match(b.json().choices[0].message.content, /context-beta/);
});

test("failed tunnel probes are backed off, remain observable, and recover after expiry", async (t) => {
  let hits = 0;
  let online = false;
  const health = http.createServer((_req, res) => { hits++; res.writeHead(online ? 200 : 503); res.end(); });
  await new Promise((r) => health.listen(0, "127.0.0.1", r));
  t.after(() => new Promise((r) => health.close(r)));
  const f = await fixture(t, {
    KOMPUTER_SKIP_TUNNEL: "0",
    KOMPUTER_HEALTH_URL: `http://127.0.0.1:${health.address().port}/healthz`,
    KOMPUTER_TUNNEL_UP: (dir) => path.join(dir, "no-tunnel-script.sh"),
    KOMPUTER_TUNNEL_FAIL_CACHE_MS: "1000",
  });
  await f.ready();
  const initial = (await f.request("/healthz")).json();
  assert.equal(initial.body_ok, null, "unprobed is unknown, not healthy");
  assert.equal(hits, 0, "health GET must not start SSH");
  assert.equal((await f.chat("offline")).status, 503);
  const probes = hits;
  const retry = await f.chat("offline again");
  assert.equal(retry.status, 503);
  assert.ok(Number(retry.headers.get("retry-after")) >= 1);
  assert.equal(hits, probes);
  assert.equal((f.logs().match(/running tunnel-up/g) || []).length, 1);
  assert.equal((await f.request("/healthz")).json().body_ok, false);
  online = true;
  await sleep(1100);
  assert.equal((await f.chat("online now")).status, 200);
  assert.equal((await f.request("/healthz")).json().body_ok, true);
  assert.equal(hits, probes + 1);
});
