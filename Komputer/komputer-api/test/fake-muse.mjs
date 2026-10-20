#!/usr/bin/env node
// Test double for the Muse Code CLI, modelled on `muse exec --json` 1.4.11:
// the JSONL record envelope, --session-id create-or-resume with continuing
// sequence numbers, the workspace-mismatch refusal, the missing-credentials
// error, and -c mcp_servers.<name>={...} with whole-value ${VAR} interpolation.
import fs from "node:fs";
import path from "node:path";
import { randomUUID } from "node:crypto";

const args = process.argv.slice(2);
const stateDir = process.env.FAKE_MUSE_STATE;
const argVal = (flag) => {
  const i = args.indexOf(flag);
  return i >= 0 ? args[i + 1] : undefined;
};
const argVals = (flag) => args.flatMap((a, i) => (a === flag ? [args[i + 1]] : []));
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

if (args[0] !== "exec" || !args.includes("--json")) {
  process.stderr.write("fake muse: expected `exec --json`\n");
  process.exit(2);
}
const promptFile = argVal("--prompt-file");
if (!promptFile) {
  process.stderr.write("error: a prompt is required\n");
  process.exit(2);
}
const prompt = fs.readFileSync(promptFile, "utf8");
const promptMode = fs.statSync(promptFile).mode & 0o777;
const workspace = argVal("--workspace");

const interpolate = (v) => {
  if (typeof v !== "string") return v;
  const m = /^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$/.exec(v);
  if (m) return process.env[m[1]] ?? "";
  if (/\$\{/.test(v)) {
    process.stderr.write("error: secrets must be whole-value ${VAR} references\n");
    process.exit(2);
  }
  return v;
};
const mcp = {};
for (const c of argVals("-c")) {
  const m = /^mcp_servers\.([A-Za-z0-9_-]+)=(.*)$/s.exec(c);
  if (!m) continue;
  const raw = JSON.parse(m[2]);
  mcp[m[1]] = { raw, url: interpolate(raw.url), headers: Object.fromEntries(Object.entries(raw.headers || {}).map(([k, v]) => [k, interpolate(v)])) };
}

fs.appendFileSync(
  path.join(stateDir, "muse-calls.jsonl"),
  JSON.stringify({ args, prompt, promptMode, workspace, cwd: process.cwd(), mcp, envKeys: Object.keys(process.env).sort(),
    images: argVals("--image").map((f) => ({ ext: path.extname(f), bytes: fs.readFileSync(f).length })) }) + "\n"
);

if (prompt.includes("FAIL_AUTH")) {
  process.stderr.write("missing meta credentials: run `muse login` or set META_API_KEY, or save credentials at ~/.config/muse/auth.json\n");
  process.exit(1);
}

const sessionId = argVal("--session-id") || randomUUID();
const sessFile = path.join(stateDir, `muse-${sessionId}.json`);
let session = { turns: 0, workspace, transcript: "" };
if (argVal("--session-id") && fs.existsSync(sessFile)) {
  session = JSON.parse(fs.readFileSync(sessFile, "utf8"));
  if (session.workspace !== workspace) {
    process.stderr.write(`session ${sessionId} was created in workspace ${session.workspace}; refusing to resume in workspace ${workspace}; pass --workspace ${session.workspace} or --allow-workspace-switch\n`);
    process.exit(1);
  }
}
const previous = session.transcript;
session.turns += 1;

let seq = session.turns;
const runId = randomUUID();
const record = (payloadType, payload, { kind = "session", recordType = "event" } = {}) => {
  process.stdout.write(JSON.stringify({
    schema_version: 1, id: randomUUID(), stream: { kind, id: sessionId }, sequence: kind === "session" ? seq++ : 1,
    record_type: recordType, payload_type: payloadType, payload: { ...payload, run_stream: { id: runId, kind: "run" } },
  }) + "\n");
};
const task = (taskKind, extra = {}) => record("task.lifecycle.proposed", { kind: "task_lifecycle", event: { kind: "proposed", task_id: randomUUID(), task_kind: taskKind, ...extra } });

async function main() {
  record("runtime.command.accepted", { command_kind: "turn.submit", kind: "command_accepted" }, { recordType: "reconciliation" });
  record("session.run.linked", { kind: "session_run_linked" });
  record("turn.input.user", { kind: "turn_input_user", prompt }, { recordType: "status" });
  record("run.lifecycle.started", { kind: "run_started", prompt });
  task("reminder.agent.skill-reminder");

  if (prompt.includes("MCP_REQUIRED_DOWN")) {
    record("run.terminal.failed", { kind: "run_terminal", terminal: "failed", reason: "invalid run configuration: Required MCP server `komputer_use` failed during startup: the process could not be started or the server could not be reached." });
    process.exit(1);
  }
  if (prompt.includes("FAIL_LIMIT")) {
    record("run.terminal.failed", { kind: "run_terminal", terminal: "failed", reason: "provider error: rate limit exceeded (429 Too Many Requests)" });
    process.exit(1);
  }
  let text = "";
  const delta = async (t) => {
    text += t;
    record("run.output.delta", { kind: "run_output_delta", text: t }, { recordType: "status" });
    if (prompt.includes("SLOW")) await sleep(300);
  };
  task("model.meta.response");
  if (prompt.includes("CLIENT_TOOL")) {
    const result = await clientTool();
    await delta(`muse tool result:${result}`);
  } else {
    if (prompt.includes("TOOL")) {
      await delta("checking");
      task("tool.mcp", { tool_name: "mcp__komputer_use__run_command" });
      task("model.meta.response");
    }
    const reply = session.turns > 1 ? `prev=${previous}|now=${prompt}` : `muse:${prompt}`;
    await delta(reply.slice(0, 5));
    await delta(reply.slice(5));
  }
  record("run.usage", { kind: "run_usage", models: [{ input_tokens: 30, output_tokens: 12, reasoning_tokens: 6, cached_tokens: 10, calls: 1, model: null }] },
    { kind: "exec.usage", recordType: "status" });
  const final = text.startsWith("checking") ? text.slice("checking".length) : text;
  record("run.terminal.completed", { kind: "run_terminal", terminal: "completed", reason: null, text: final });
  if (argVal("--session-id")) {
    session.transcript = prompt;
    fs.writeFileSync(sessFile, JSON.stringify(session));
  }
}

async function clientTool() {
  const server = mcp.komputer_use;
  let rpcId = 0;
  const rpc = async (method, params) => {
    const r = await fetch(server.url, {
      method: "POST", headers: { ...server.headers, "Content-Type": "application/json", Accept: "application/json, text/event-stream" },
      body: JSON.stringify({ jsonrpc: "2.0", id: ++rpcId, method, params }),
    });
    if (!r.ok) throw new Error(`MCP HTTP ${r.status}`);
    const response = await r.json();
    if (response.error) throw new Error(response.error.message);
    return response.result;
  };
  await rpc("initialize", { protocolVersion: "2025-06-18", capabilities: {}, clientInfo: { name: "fake-muse", version: "1" } });
  const { tools } = await rpc("tools/list");
  task("tool.mcp", { tool_name: `mcp__komputer_use__${tools[0].name}` });
  const result = await rpc("tools/call", { name: tools[0].name, arguments: { query: "from muse" } });
  return result.content?.[0]?.text ?? "";
}

main().catch((e) => {
  process.stderr.write(`fake muse crashed: ${e.stack}\n`);
  process.exit(3);
});
