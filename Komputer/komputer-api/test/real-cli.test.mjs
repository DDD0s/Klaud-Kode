// Opt-in smoke tests against the real CLIs. Skipped unless pointed at a binary:
//   KOMPUTER_TEST_REAL_MUSE=/path/to/muse [KOMPUTER_TEST_REAL_MUSE_HOME=/isolated/home]
//     uses muse's offline echo provider, so no account or network is needed.
//   KOMPUTER_TEST_REAL_AGY=/path/to/agy [KOMPUTER_TEST_REAL_AGY_HOME=/isolated/home]
//     uses a fresh signed-out HOME unless one is given; checks MCP and the auth failure path.
import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import http from "node:http";
import path from "node:path";
import { apiFixture } from "./helpers.mjs";

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
