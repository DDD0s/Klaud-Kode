#!/usr/bin/env node
// Test double for the Antigravity CLI (`agy`), modelled on agy 1.4.3:
// --input-format stream-json on stdin, init / step_update / result events on
// stdout, a warning (not an error) for an unknown --conversation, and the
// single ERROR result line it prints when not signed in. Prompt keywords
// drive behaviour, like fake-claude.mjs.
import fs from "node:fs";
import path from "node:path";
import { randomUUID } from "node:crypto";

const args = process.argv.slice(2);
const stateDir = process.env.FAKE_AGY_STATE;
const argVal = (flag) => {
  const i = args.indexOf(flag);
  return i >= 0 ? args[i + 1] : undefined;
};
const out = (o) => process.stdout.write(JSON.stringify(o) + "\n");
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const usage = { input_tokens: 20, output_tokens: 9, thinking_tokens: 4, cache_read_tokens: 5, total_tokens: 33 };

if (argVal("--input-format") === "stream-json" && argVal("--output-format") !== "stream-json") {
  process.stderr.write("Error: --input-format=stream-json requires --output-format=stream-json\n");
  process.exit(2);
}

let stdin = "";
process.stdin.setEncoding("utf8");
process.stdin.on("data", (d) => (stdin += d));
process.stdin.on("end", () => main().catch((e) => {
  process.stderr.write(`fake agy crashed: ${e.stack}\n`);
  process.exit(3);
}));

function readMcpConfig() {
  const p = path.join(process.cwd(), ".agents", "mcp_config.json");
  return fs.existsSync(p) ? { path: p, mode: fs.statSync(p).mode & 0o777, config: JSON.parse(fs.readFileSync(p, "utf8")) } : null;
}

async function main() {
  const line = stdin.trim().split("\n")[0] || "";
  let msg;
  try {
    msg = JSON.parse(line);
  } catch {
    process.stderr.write("Error: invalid stream-json input\n");
    process.exit(2);
  }
  const blocks = msg?.message?.content || [];
  if (msg.event !== "user" || blocks.some((b) => b.type !== "text")) {
    process.stderr.write("Error: only text content blocks are supported\n");
    process.exit(2);
  }
  const prompt = blocks.map((b) => b.text).join("");
  const mcp = readMcpConfig();
  fs.appendFileSync(
    path.join(stateDir, "agy-calls.jsonl"),
    JSON.stringify({ args, prompt, cwd: process.cwd(), envKeys: Object.keys(process.env).sort(), mcp,
      noProxy: process.env.NO_PROXY || null, gitCeiling: process.env.GIT_CEILING_DIRECTORIES || null }) + "\n"
  );

  if (prompt.includes("FAIL_AUTH")) {
    process.stderr.write("Error: authentication required. Run 'agy' to log in, then retry.\n");
    out({ event: "result", result: { conversation_id: "", status: "ERROR", response: "", error: "authentication failed or timed out",
      duration_seconds: 0, num_turns: 0, usage: { input_tokens: 0, output_tokens: 0, thinking_tokens: 0, cache_read_tokens: 0, total_tokens: 0 } } });
    process.exit(1);
  }

  let id = argVal("--conversation");
  let previous = "";
  if (id) {
    const f = path.join(stateDir, `agy-${id}.txt`);
    if (fs.existsSync(f) && !prompt.includes("LOSE_CONVERSATION")) previous = fs.readFileSync(f, "utf8");
    else {
      process.stderr.write(`warning: conversation "${id}" not found\n`);
      id = null;
    }
  }
  const resumed = Boolean(id);
  id ||= randomUUID();
  fs.writeFileSync(path.join(stateDir, `agy-${id}.txt`), prompt);
  out({ event: "init", conversation_id: id, init: { cwd: process.cwd(), tools: ["view_file", "run_command", "call_mcp_tool"],
    permission_mode: args.includes("--dangerously-skip-permissions") ? "bypass" : "default", model: argVal("--model") || "default" } });
  out({ event: "step_update", conversation_id: id, step_index: 0, state: "DONE", step_type: "user_input" });

  if (prompt.includes("FAIL_LIMIT")) {
    out({ event: "result", result: { conversation_id: id, status: "ERROR", response: "", error: "RESOURCE_EXHAUSTED: quota exceeded", num_turns: 1, usage } });
    process.exit(1);
  }
  if (prompt.includes("CLIENT_TOOL")) return await clientTools(id, prompt);

  let step = 1;
  const say = async (text) => {
    for (const piece of [text.slice(0, 4), text.slice(4)].filter(Boolean)) {
      out({ event: "step_update", conversation_id: id, step_index: step, state: "ACTIVE", step_type: "agent_response", text_delta: piece });
      if (prompt.includes("SLOW")) await sleep(300);
    }
    out({ event: "step_update", conversation_id: id, step_index: step, state: "DONE", step_type: "agent_response" });
    step++;
  };
  if (prompt.includes("TOOL")) {
    await say("checking");
    const info = { name: "call_mcp_tool", parameters: { ServerName: "komputer_use", ToolName: "run_command" } };
    out({ event: "step_update", conversation_id: id, step_index: step, state: "ACTIVE", step_type: "tool", tool_name: "call_mcp_tool", tool_info: info });
    out({ event: "step_update", conversation_id: id, step_index: step, state: "DONE", step_type: "tool", tool_name: "call_mcp_tool", tool_info: { ...info, output: "ok" } });
    step++;
  }
  const reply = resumed ? `prev=${previous}|now=${prompt}` : `agy:${prompt}`;
  await say(reply);
  out({ event: "result", result: { conversation_id: id, status: "SUCCESS", response: reply, error: "", duration_seconds: 1, num_turns: 1, usage } });
}

/** Acts as the MCP client agy would be for the workspace .agents/mcp_config.json. */
async function clientTools(id, prompt) {
  const server = readMcpConfig()?.config?.mcpServers?.komputer_use;
  let rpcId = 0;
  const rpc = async (method, params) => {
    const r = await fetch(server.serverUrl, {
      method: "POST", headers: { ...server.headers, "Content-Type": "application/json", Accept: "application/json, text/event-stream" },
      body: JSON.stringify({ jsonrpc: "2.0", id: ++rpcId, method, params }),
    });
    if (!r.ok) throw new Error(`MCP HTTP ${r.status}`);
    const response = await r.json();
    if (response.error) throw new Error(response.error.message);
    return response.result;
  };
  await rpc("initialize", { protocolVersion: "2025-06-18", capabilities: {}, clientInfo: { name: "fake-agy", version: "1" } });
  const { tools } = await rpc("tools/list");
  const forced = /For your first tool call, use ([a-z0-9_]+)/.exec(prompt)?.[1];
  const tool = tools.find((t) => t.name === forced) || tools[0];
  const info = { name: "call_mcp_tool", parameters: { ServerName: "komputer_use", ToolName: tool.name } };
  out({ event: "step_update", conversation_id: id, step_index: 1, state: "ACTIVE", step_type: "tool", tool_name: "call_mcp_tool", tool_info: info });
  const result = await rpc("tools/call", { name: tool.name, arguments: { query: "from agy" } });
  out({ event: "step_update", conversation_id: id, step_index: 1, state: "DONE", step_type: "tool", tool_name: "call_mcp_tool", tool_info: { ...info, output: result } });
  const reply = `agy tool result:${result.content?.[0]?.text ?? ""}`;
  out({ event: "step_update", conversation_id: id, step_index: 2, state: "ACTIVE", step_type: "agent_response", text_delta: reply });
  out({ event: "result", result: { conversation_id: id, status: "SUCCESS", response: reply, error: "", num_turns: 1, usage } });
}
