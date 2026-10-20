// Golden argv / stdin / environment for every Claude Code invocation mode.
// These pin the exact contract with the official CLI; a backend refactor must
// not change a single argument, byte of stdin or child environment name.
import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { apiFixture } from "./helpers.mjs";

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

function normalize(f, args) {
  return args.map((a, i) => {
    if (UUID.test(a)) return "<UUID>";
    if (args[i - 1] === "--append-system-prompt-file") return /komputer-sys-[0-9a-f-]{36}\.txt$/.test(a) ? "<SYS>" : a;
    if (args[i - 1] === "--mcp-config") {
      if (a === path.join(f.dir, ".mcp.json")) return "<BODY_MCP>";
      if (/komputer-tools-[^/\\]+[/\\]tools\.json$/.test(a)) return "<HARNESS_MCP>";
    }
    return a;
  });
}

// Frozen copy of the original child-environment allowlist (claude-env.mjs at 0.9.0).
const KEEP = new Set([
  "PATH", "HOME", "USER", "LOGNAME", "LANG", "TZ", "TMPDIR", "TEMP", "TMP",
  "SYSTEMROOT", "COMSPEC", "PATHEXT", "OS", "NUMBER_OF_PROCESSORS",
  "USERPROFILE", "APPDATA", "LOCALAPPDATA", "HOMEDRIVE", "HOMEPATH",
  "SHELL", "TERM", "SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS",
  "CLAUDE_CONFIG_DIR", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_USE_POWERSHELL_TOOL",
  "MCP_TIMEOUT", "MCP_TOOL_TIMEOUT", "DISABLE_AUTOUPDATER", "DISABLE_TELEMETRY",
  "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL",
  "ANTHROPIC_DEFAULT_MODEL", "ANTHROPIC_DEFAULT_FABLE_MODEL",
  "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL",
  "ANTHROPIC_SMALL_FAST_MODEL", "FAKE_CLAUDE_STATE",
]);
function expectedEnvKeys(source, { privateMcp = false } = {}) {
  const keys = new Set(["CI", "GIT_CEILING_DIRECTORIES"]);
  for (const [k, v] of Object.entries(source)) {
    if (v === undefined || /^(?:KOMPUTER_|BODY_|KLOUD_KODE_)|^(?:CLAUDE_BIN|NODE_BIN|CI|GIT_CEILING_DIRECTORIES)$/i.test(k)) continue;
    if (KEEP.has(k.toUpperCase()) || /^(LC_|XDG_|PROCESSOR_)/.test(k)) keys.add(k);
  }
  const proxy = ["HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy"].some((k) => source[k]);
  if (proxy) for (const k of ["HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy"]) keys.add(k);
  if (source.ALL_PROXY || source.all_proxy) { keys.add("ALL_PROXY"); keys.add("all_proxy"); }
  const all = proxy || source.ALL_PROXY || source.all_proxy;
  if (privateMcp || source.NO_PROXY !== undefined || source.no_proxy !== undefined || all) { keys.add("NO_PROXY"); keys.add("no_proxy"); }
  return [...keys].sort();
}
const serverSource = (f) => {
  const env = { ...process.env };
  for (const k of Object.keys(env)) if (/^KOMPUTER_/i.test(k)) delete env[k];
  return { ...env, FAKE_CLAUDE_STATE: path.join(f.dir, "state") };
};

const COMMON = ["-p", "--dangerously-skip-permissions", "--strict-mcp-config"];
const STREAM = ["--output-format", "stream-json", "--verbose", "--include-partial-messages", "--append-system-prompt-file", "<SYS>"];
const NOTE = "You work on the user's computer. Your shell, files, processes and applications are all there, and the mcp__komputer_use__* tools are how you reach them: run_command for shell commands, the file tools for reading and writing, start_process for anything long-running. At the start of a new conversation, call system_info to learn the OS, shell, date, time zone, working roots and limits. Use those results for your commands and paths.";

test("Claude Code argv, stdin and environment are unchanged for every invocation mode", async (t) => {
  const f = await apiFixture(t, { env: { KOMPUTER_SKIP_TUNNEL: "1" } });
  await f.ready();
  const chat = (body, headers = {}) => f.request("/v1/chat/completions", body, { headers });
  const last = () => f.calls().at(-1);

  // Session, first turn, with client system text, model alias and effort.
  let r = await chat({ model: "komputer-opus", reasoning_effort: "high", messages: [
    { role: "system", content: "CLIENT RULE" }, { role: "user", content: "first" }] }, { "x-conversation-id": "golden" });
  assert.equal(r.status, 200, r.text);
  let c = last();
  assert.deepEqual(normalize(f, c.args), [...COMMON, "--mcp-config", "<BODY_MCP>", ...STREAM, "--tools", "", "--model", "opus", "--effort", "high", "--session-id", "<UUID>"]);
  assert.equal(c.stdin, "first");
  assert.equal(c.system, `${NOTE}\n\nCLIENT RULE`);
  assert.deepEqual(c.envKeys, expectedEnvKeys(serverSource(f)));
  const sid = c.args[c.args.indexOf("--session-id") + 1];

  // Session resume sends only the newest user message.
  r = await chat({ messages: [{ role: "user", content: "first" }, { role: "assistant", content: r.json().choices[0].message.content },
    { role: "user", content: "second" }] }, { "x-conversation-id": "golden" });
  c = last();
  assert.deepEqual(normalize(f, c.args), [...COMMON, "--mcp-config", "<BODY_MCP>", ...STREAM, "--tools", "", "--resume", "<UUID>"]);
  assert.equal(c.args.at(-1), sid);
  assert.equal(c.stdin, "second");
  assert.equal(c.system, NOTE);

  // Stateless one-shot with history.
  await chat({ messages: [{ role: "user", content: "a" }, { role: "assistant", content: "b" }, { role: "user", content: "c" }] });
  c = last();
  assert.deepEqual(normalize(f, c.args), [...COMMON, "--mcp-config", "<BODY_MCP>", ...STREAM, "--tools", "", "--no-session-persistence"]);
  assert.equal(c.stdin, "Earlier conversation for context:\n\nUser: a\n\nAssistant: b\n\n---\n\nc");

  // Background task: task model, no MCP, no note.
  await chat({ messages: [{ role: "user", content: "### Task:\nTitle this." }] }, { "x-conversation-id": "golden" });
  c = last();
  assert.deepEqual(normalize(f, c.args), [...COMMON, ...STREAM, "--tools", "", "--model", "haiku", "--no-session-persistence"]);
  assert.equal(c.system, "");

  // Attachments switch to stream-json input with exact bytes.
  await chat({ messages: [{ role: "user", content: [{ type: "text", text: "look" },
    { type: "image_url", image_url: { url: "data:image/png;base64,QUJD" } }] }] }, { "x-conversation-id": "golden-img" });
  c = last();
  assert.deepEqual(normalize(f, c.args), [...COMMON, "--mcp-config", "<BODY_MCP>", ...STREAM, "--input-format", "stream-json", "--tools", "", "--session-id", "<UUID>"]);
  assert.equal(c.stdin, JSON.stringify({ type: "user", message: { role: "user", content: [{ type: "text", text: "look" },
    { type: "image", source: { type: "base64", media_type: "image/png", data: "QUJD" } }] }, parent_tool_use_id: null, session_id: "" }) + "\n");

  // Anthropic Messages share the same invocation.
  await f.request("/v1/messages", { model: "claude-sonnet-5", system: "S", messages: [{ role: "user", content: "m" }] });
  c = last();
  assert.deepEqual(normalize(f, c.args), [...COMMON, "--mcp-config", "<BODY_MCP>", ...STREAM, "--tools", "", "--model", "claude-sonnet-5", "--no-session-persistence"]);
  assert.equal(c.system, `${NOTE}\n\nS`);

  // Harness: private MCP config, no built-in tools, stateless, loopback proxy bypass.
  const tools = [{ type: "function", function: { name: "lookup", parameters: { type: "object", properties: { query: { type: "string" } } } } }];
  r = await chat({ model: "komputer-sonnet", tools, messages: [{ role: "user", content: "CLIENT_TOOL" }] });
  assert.equal(r.status, 200, r.text);
  c = last();
  assert.deepEqual(normalize(f, c.args), [...COMMON, "--mcp-config", "<HARNESS_MCP>", ...STREAM, "--tools", "", "--model", "sonnet", "--no-session-persistence"]);
  assert.equal(c.stdin, "user: CLIENT_TOOL");
  assert.deepEqual(c.envKeys, expectedEnvKeys(serverSource(f), { privateMcp: true }));
  const call = r.json().choices[0].message.tool_calls[0];
  const done = await chat({ model: "komputer-sonnet", tools, messages: [{ role: "tool", tool_call_id: call.id, content: "ok" }] });
  assert.equal(done.status, 200, done.text);
  assert.equal(f.calls().length, 7);

  // The persisted session map keeps its original field set.
  const stored = JSON.parse(fs.readFileSync(path.join(f.dir, "sessions.json"), "utf8")).sessions.find((s) => s.client_key === "golden");
  assert.deepEqual(Object.keys(stored), ["client_key", "conversation_id", "claude_session_id", "created_at", "last_used_at", "turn_count", "started", "history_seen", "last_reply_tail", "id"]);
  const pub = (await f.request("/v1/sessions/golden")).json();
  assert.deepEqual(Object.keys(pub), ["conversation_id", "client_key", "id", "created_at", "last_used_at", "turn_count", "started", "reseeds", "live", "label", "claude_session_id"]);
  assert.equal(pub.claude_session_id, sid);
});

test("legacy Claude defaults: tools restored and system prompt file are passed exactly", async (t) => {
  const f = await apiFixture(t, { env: { KOMPUTER_SKIP_TUNNEL: "1", KOMPUTER_BUILTIN_TOOLS: "default",
    KOMPUTER_SYSTEM_PROMPT_FILE: (dir) => path.join(dir, "prompt.txt") }, prepare: (dir) => fs.writeFileSync(path.join(dir, "prompt.txt"), "p") });
  await f.ready();
  await f.request("/v1/chat/completions", { messages: [{ role: "user", content: "x" }] });
  const c = f.calls().at(-1);
  assert.deepEqual(normalize(f, c.args), [...COMMON, "--mcp-config", "<BODY_MCP>", ...STREAM, "--system-prompt-file", path.join(f.dir, "prompt.txt"), "--no-session-persistence"]);
  const models = (await f.request("/v1/models")).json().data.map((m) => m.id);
  assert.deepEqual(models, ["komputer-claude", "komputer-opus", "komputer-sonnet", "komputer-haiku", "komputer-fable"]);
});
