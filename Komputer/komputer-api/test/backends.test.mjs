import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { createBackends } from "../backends/index.mjs";
import { agyArgs, agyParser, agyUsage } from "../backends/antigravity.mjs";
import { museMcpOverrides, museParser } from "../backends/muse.mjs";
import { buildChildEnv, buildClaudeEnv, AGY_KEEP, MUSE_KEEP } from "../claude-env.mjs";

const here = path.dirname(fileURLToPath(import.meta.url));
const fixture = (name) => fs.readFileSync(path.join(here, "fixtures", name), "utf8").trim().split("\n");
const ctx = { workspaceRoot: "/w", instanceId: "id", claudeCwd: "/w/id", mcpConfig: "/w/.mcp.json", systemPromptFile: "" };

function run(make, session, lines, stderr = "") {
  const seen = { text: "", tools: [], bail: null };
  const p = make({ session, sessionId: session.id || null, hooks: { onText: (t) => (seen.text += t), onToolUse: (n) => seen.tools.push(n) },
    bail: (message, type) => (seen.bail ??= { message, type }) });
  for (const l of lines) p.line(l);
  if (stderr) p.stderr(stderr);
  return { seen, parser: p, result: p.finish(0, null, stderr) };
}

test("routing: exact ids, backend name patterns, default backend", () => {
  const b = createBackends({ KOMPUTER_BACKENDS: "claude,agy,muse" }, ctx);
  assert.deepEqual(b.list.map((x) => x.name), ["claude", "antigravity", "muse"]);
  const route = (id) => b.route(id).name;
  assert.equal(route("komputer-opus"), "claude");
  assert.equal(route("claude-sonnet-5"), "claude");
  assert.equal(route("komputer-gemini-4-argon"), "antigravity");
  assert.equal(route("gemini-3.8-flash"), "antigravity");
  assert.equal(route("komputer-agy"), "antigravity");
  assert.equal(route("komputer-muse"), "muse");
  assert.equal(route("muse-spark"), "muse");
  assert.equal(route("gpt-4o"), "claude");
  assert.equal(route(undefined), "claude");
  assert.equal(b.get("antigravity").resolveModel("komputer-agy-gemini-4-argon-high"), "gemini-4-argon-high");
  assert.equal(b.get("antigravity").resolveModel("komputer-agy"), "");
  assert.equal(b.get("muse").resolveModel("komputer-muse-spark"), "muse-spark");
  assert.equal(b.get("claude").resolveModel("komputer-gemini-4-argon"), "gemini-4-argon", "Claude's own komputer-<alias> rule is unchanged");

  const agyFirst = createBackends({ KOMPUTER_BACKENDS: "claude,antigravity", KOMPUTER_DEFAULT_BACKEND: "agy" }, ctx);
  assert.equal(agyFirst.route("gpt-4o").name, "antigravity");
  assert.equal(agyFirst.route("komputer-sonnet").name, "claude");
  const solo = createBackends({ KOMPUTER_BACKENDS: "muse" }, ctx);
  assert.deepEqual(solo.list.flatMap((x) => x.ids), ["komputer-muse"]);
  assert.equal(solo.route("komputer-opus").name, "muse");

  assert.throws(() => createBackends({ KOMPUTER_BACKENDS: "claude,other" }, ctx), /unknown backend/);
  assert.throws(() => createBackends({ KOMPUTER_BACKENDS: "claude,muse", KOMPUTER_MUSE_MODELS: "komputer-opus" }, ctx), /advertised by two backends/);
  assert.throws(() => createBackends({ KOMPUTER_BACKENDS: "claude", KOMPUTER_DEFAULT_BACKEND: "muse" }, ctx), /DEFAULT_BACKEND/);
  assert.throws(() => createBackends({ KOMPUTER_BACKENDS: "agy", KOMPUTER_AGY_PERMISSIONS: "yolo" }, ctx), /settings or skip/);
});

test("effort mapping per backend", () => {
  const b = createBackends({ KOMPUTER_BACKENDS: "claude,agy,muse", KOMPUTER_AGY_EFFORT: "medium" }, ctx);
  const agy = b.get("antigravity");
  assert.equal(agy.resolveEffort({ reasoning_effort: "xhigh" }), "high");
  assert.equal(agy.resolveEffort({ reasoning_effort: "none" }), "low");
  assert.equal(agy.resolveEffort({ thinking: { type: "enabled", budget_tokens: 2000 } }), "high");
  assert.equal(agy.resolveEffort({}), "medium");
  assert.equal(agy.resolveEffort({ reasoning_effort: "bogus" }), "medium");
  const wide = createBackends({ KOMPUTER_BACKENDS: "agy", KOMPUTER_AGY_EFFORTS: "low,medium,high,xhigh,max" }, ctx).get("antigravity");
  assert.equal(wide.resolveEffort({ reasoning_effort: "max" }), "max");
  const muse = b.get("muse");
  assert.equal(muse.resolveEffort({ reasoning_effort: "minimal" }), "minimal");
  assert.equal(muse.resolveEffort({ reasoning_effort: "ultra" }), "ultra");
  assert.equal(muse.resolveEffort({ thinking: { type: "enabled", budget_tokens: 40000 } }), "max");
  assert.equal(b.get("claude").resolveEffort({ reasoning_effort: "minimal" }), "low");
});

test("agy argv: effort suffix slugs, resume, permissions", () => {
  assert.deepEqual(agyArgs({ session: { mode: "new" }, model: "gemini-4-argon", effort: "high" }),
    ["--input-format", "stream-json", "--output-format", "stream-json", "--model", "gemini-4-argon", "--effort", "high"]);
  assert.deepEqual(agyArgs({ session: { mode: "resume", id: "c1" }, model: "gemini-4-argon-high", effort: "low", permissions: "skip", extraArgs: ["--sandbox"] }),
    ["--input-format", "stream-json", "--output-format", "stream-json", "--dangerously-skip-permissions", "--model", "gemini-4-argon-high", "--conversation", "c1", "--sandbox"]);
});

test("agy parser: steps, tool names, usage, lost conversations and the real unauthenticated result", () => {
  const ev = (o) => JSON.stringify(o);
  const lines = [
    ev({ event: "init", conversation_id: "c1", init: { cwd: "/x", tools: [] } }),
    ev({ event: "step_update", step_index: 0, state: "DONE", step_type: "user_input" }),
    ev({ event: "step_update", step_index: 1, state: "ACTIVE", step_type: "agent_response", text_delta: "Hel" }),
    ev({ event: "step_update", step_index: 1, state: "ACTIVE", step_type: "agent_response", text_delta: "lo" }),
    ev({ event: "step_update", step_index: 2, state: "ACTIVE", step_type: "tool", tool_name: "call_mcp_tool", tool_info: { name: "call_mcp_tool", parameters: { ServerName: "komputer_use", ToolName: "run_command" } } }),
    ev({ event: "step_update", step_index: 2, state: "DONE", step_type: "tool", tool_name: "call_mcp_tool" }),
    ev({ event: "step_update", step_index: 3, state: "ACTIVE", step_type: "tool", tool_name: "view_file" }),
    ev({ event: "step_update", step_index: 4, state: "ACTIVE", step_type: "agent_response", text_delta: "Done" }),
    "not json",
    ev({ event: "result", result: { conversation_id: "c1", status: "SUCCESS", response: "Done", num_turns: 1,
      usage: { input_tokens: 100, output_tokens: 7, thinking_tokens: 3, cache_read_tokens: 40, total_tokens: 110 } } }),
  ];
  const { seen, result, parser } = run(agyParser, { mode: "new" }, lines);
  assert.equal(seen.text, "Hello\n\nDone");
  assert.deepEqual(seen.tools, ["mcp__komputer_use__run_command", "view_file"]);
  assert.equal(parser.activity, true);
  assert.deepEqual(result, { ok: true, value: { text: "Done", thinking: "", session_id: "c1",
    usage: { input_tokens: 60, cache_read_input_tokens: 40, output_tokens: 7, reasoning_tokens: 3 } } });
  assert.equal(agyUsage(null), null);

  const lost = run(agyParser, { mode: "resume", id: "old" }, [ev({ event: "init", conversation_id: "fresh" })]);
  assert.equal(lost.seen.bail.type, "session_not_found");
  assert.equal(lost.parser.activity, false);
  const warned = run(agyParser, { mode: "resume", id: "old" }, [], 'warning: conversation "old" not found\n');
  assert.equal(warned.seen.bail.type, "session_not_found");

  const unauth = run(agyParser, { mode: "new" }, fixture("agy-1.4.3-unauthenticated.jsonl"), "Error: authentication required. Run 'agy' to log in, then retry.\n");
  assert.equal(unauth.result.ok, false);
  assert.match(unauth.result.message, /authentication failed/);
  const agy = createBackends({ KOMPUTER_BACKENDS: "agy" }, ctx).get("antigravity");
  assert.equal(agy.classify(unauth.result.message), "upstream_auth_error");
  assert.equal(agy.classify("agy ERROR: RESOURCE_EXHAUSTED"), "rate_limit_error");
});

test("muse parser: real muse 1.4.11 echo-provider traces (new session and resume)", () => {
  const fresh = run(museParser, { mode: "new", id: "s" }, fixture("muse-1.4.11-echo-new.jsonl"));
  assert.equal(fresh.seen.text, "echo: hello there\n");
  assert.deepEqual(fresh.seen.tools, [], "reminder and model tasks are not tool calls");
  assert.equal(fresh.seen.bail, null);
  assert.deepEqual(fresh.result, { ok: true, value: { text: "echo: hello there", thinking: "", session_id: "s",
    usage: { input_tokens: 0, cache_read_input_tokens: 0, output_tokens: 0, reasoning_tokens: 0 } } });

  const resumed = run(museParser, { mode: "resume", id: "s" }, fixture("muse-1.4.11-echo-resume.jsonl"));
  assert.equal(resumed.seen.bail, null, "a real resume continues the sequence numbering");
  assert.equal(resumed.result.value.text, "echo: second");

  const lost = run(museParser, { mode: "resume", id: "s" }, fixture("muse-1.4.11-echo-new.jsonl"));
  assert.equal(lost.seen.bail.type, "session_not_found", "a resume that starts at sequence 1 is a new session");

  const failed = run(museParser, { mode: "none" }, [JSON.stringify({ stream: { kind: "session", id: "x" }, sequence: 1, payload_type: "run.terminal.failed",
    payload: { terminal: "failed", reason: "invalid run configuration: Required MCP server `komputer_use` failed during startup: unreachable" } })]);
  assert.equal(failed.result.ok, false);
  const muse = createBackends({ KOMPUTER_BACKENDS: "muse" }, ctx).get("muse");
  assert.equal(muse.classify(failed.result.message), "body_unavailable");
  assert.equal(muse.classify("muse exit 1: missing meta credentials: run `muse login` or set META_API_KEY"), "upstream_auth_error");
  const ws = run(museParser, { mode: "resume", id: "s" }, [], "session s was created in workspace /a; refusing to resume in workspace /b\n");
  assert.equal(ws.seen.bail.type, "session_not_found");
});

test("muse MCP overrides keep URLs and header values out of argv", () => {
  const { args, env } = museMcpOverrides([{ name: "komputer_use", url: "http://127.0.0.1:1/mcp/t0ken", headers: { Authorization: "Bearer s3cret", "X-A": "b" } },
    { name: "stdio_server", command: "node" }]);
  assert.deepEqual(args, ["-c", 'mcp_servers.komputer_use={"transport":"streamable_http","url":"${KMCP_0_URL}","headers":{"Authorization":"${KMCP_0_H0}","X-A":"${KMCP_0_H1}"},"mode":"required"}']);
  assert.deepEqual(env, { KMCP_0_URL: "http://127.0.0.1:1/mcp/t0ken", KMCP_0_H0: "Bearer s3cret", KMCP_0_H1: "b" });
});

test("child env allowlists are per backend", () => {
  const source = { PATH: "/bin", HOME: "/h", ANTHROPIC_API_KEY: "a", CLAUDE_CODE_OAUTH_TOKEN: "c", GEMINI_API_KEY: "g", GOOGLE_APPLICATION_CREDENTIALS: "/g.json",
    META_API_KEY: "m", KOMPUTER_X: "k", AGY_BIN: "/agy", MUSE_BIN: "/muse", SECRET_TOKEN: "s", XDG_CONFIG_HOME: "/x" };
  const keys = (e) => Object.keys(e).sort();
  assert.deepEqual(keys(buildClaudeEnv(source, "/w/c")), ["ANTHROPIC_API_KEY", "CI", "CLAUDE_CODE_OAUTH_TOKEN", "GIT_CEILING_DIRECTORIES", "HOME", "PATH", "XDG_CONFIG_HOME"]);
  assert.deepEqual(keys(buildChildEnv(source, "/w/a", AGY_KEEP)), ["CI", "GEMINI_API_KEY", "GIT_CEILING_DIRECTORIES", "GOOGLE_APPLICATION_CREDENTIALS", "HOME", "PATH", "XDG_CONFIG_HOME"]);
  assert.deepEqual(keys(buildChildEnv(source, "/w/m", MUSE_KEEP)), ["CI", "GIT_CEILING_DIRECTORIES", "HOME", "META_API_KEY", "PATH", "XDG_CONFIG_HOME"]);
});
