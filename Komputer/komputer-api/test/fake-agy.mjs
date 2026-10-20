#!/usr/bin/env node
// Test double for the Antigravity CLI (`agy`), modelled on agy 1.4.3:
// --input-format stream-json on stdin, init / step_update / result events on
// stdout, a warning (not an error) for an unknown --conversation, and the
// single ERROR result line it prints when not signed in.
//
// MCP works like the real CLI: `agy mcp add|remove|list|enable|disable`
// edits $HOME/.gemini/config/mcp_config.json (the only MCP config agy reads),
// and every run starts the enabled stdio servers with agy's own environment
// and cwd, then sends server/discover, falls back to initialize, and lists
// tools. Prompt keywords drive behaviour, like fake-claude.mjs.
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import readline from "node:readline";
import { spawn } from "node:child_process";
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
const configFile = path.join(os.homedir(), ".gemini", "config", "mcp_config.json");
const readConfig = () => (fs.existsSync(configFile) ? JSON.parse(fs.readFileSync(configFile, "utf8")) : { mcpServers: {} });
const writeConfig = (c) => {
  fs.mkdirSync(path.dirname(configFile), { recursive: true });
  fs.writeFileSync(configFile, JSON.stringify(c, null, 2) + "\n");
};

if (args[0] === "mcp") {
  mcpCommand(args.slice(1));
  process.exit(0);
}

if (argVal("--input-format") === "stream-json" && argVal("--output-format") !== "stream-json") {
  process.stderr.write("Error: --input-format=stream-json requires --output-format=stream-json\n");
  process.exit(2);
}
if (args.includes("--effort") && /-(low|medium|high|xhigh|max)$/.test(argVal("--model") || "")) {
  process.stderr.write("Error: --effort cannot be combined with a model that already names its effort\n");
  process.exit(2);
}

let stdin = "";
process.stdin.setEncoding("utf8");
process.stdin.on("data", (d) => (stdin += d));
process.stdin.on("end", () => main().then(() => process.exit(0), (e) => {
  process.stderr.write(`fake agy crashed: ${e.stack}\n`);
  process.exit(3);
}));

function mcpCommand([sub, ...rest]) {
  const c = readConfig();
  if (sub === "add") {
    const headers = {};
    const env = {};
    while (rest[0]?.startsWith("-") && rest[0] !== "--") {
      const flag = rest.shift();
      const value = rest.shift();
      if (flag === "--header" || flag === "-H") headers[value.split(":")[0].trim()] = value.slice(value.indexOf(":") + 1).trim();
      if (flag === "--env" || flag === "-e") env[value.split("=")[0]] = value.slice(value.indexOf("=") + 1);
    }
    const name = rest.shift();
    if (rest[0] === "--") rest.shift();
    const [target, ...targetArgs] = rest;
    c.mcpServers[name] = /^https?:/.test(target)
      ? { disabled: false, headers, serverUrl: target }
      : { args: targetArgs, command: target, disabled: false, ...(Object.keys(env).length ? { env } : {}) };
    writeConfig(c);
    fs.appendFileSync(path.join(stateDir, "agy-mcp-admin.jsonl"), JSON.stringify(["add", name, target, ...targetArgs]) + "\n");
    process.stdout.write(`Added MCP server "${name}" (${/^https?:/.test(target) ? "http" : "stdio"})\n`);
  } else if (sub === "remove") {
    delete c.mcpServers[rest[0]];
    writeConfig(c);
  } else if (sub === "enable" || sub === "disable") {
    if (c.mcpServers[rest[0]]) c.mcpServers[rest[0]].disabled = sub === "disable";
    writeConfig(c);
  } else if (sub === "list") {
    for (const [name, s] of Object.entries(c.mcpServers)) process.stdout.write(`${name} ${s.serverUrl ? "http" : "stdio"} ${s.disabled ? "disabled" : "enabled"}\n`);
  }
}

/** Minimal stdio MCP client, as agy's language server is for each global stdio server. */
function stdioClient(server) {
  const child = spawn(server.command, server.args || [], { cwd: process.cwd(), env: { ...process.env, ...(server.env || {}) }, stdio: ["pipe", "pipe", "inherit"] });
  const waiting = new Map();
  let next = 0;
  readline.createInterface({ input: child.stdout }).on("line", (line) => {
    const m = JSON.parse(line);
    waiting.get(m.id)?.(m);
    waiting.delete(m.id);
  });
  const request = (method, params = {}) => new Promise((resolve) => {
    const id = ++next;
    waiting.set(id, resolve);
    child.stdin.write(JSON.stringify({ jsonrpc: "2.0", id, method, params }) + "\n");
  });
  const notify = (method) => child.stdin.write(JSON.stringify({ jsonrpc: "2.0", method, params: {} }) + "\n");
  return {
    async connect() {
      const discover = await request("server/discover", { _meta: { "io.modelcontextprotocol/protocolVersion": "2026-07-28" } });
      if (discover.error) {
        const init = await request("initialize", { protocolVersion: "2025-11-25", capabilities: {}, clientInfo: { name: "antigravity-client", version: "v1.0.0" } });
        if (init.error) throw new Error(init.error.message);
        notify("notifications/initialized");
      }
      const listed = await request("tools/list");
      return listed.result?.tools || [];
    },
    async call(name, args) {
      const r = await request("tools/call", { name, arguments: args });
      if (r.error) throw new Error(r.error.message);
      return r.result;
    },
    close: () => child.stdin.end(),
  };
}

async function startMcp() {
  const servers = {};
  for (const [name, s] of Object.entries(readConfig().mcpServers || {})) {
    if (s.disabled || !s.command) continue;
    const client = stdioClient(s);
    try {
      servers[name] = { client, tools: await client.connect() };
    } catch (e) {
      servers[name] = { client, tools: [], error: e.message };
    }
  }
  return servers;
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
  const mcp = await startMcp();
  const bridgeFile = process.env.KOMPUTER_AGY_MCP_FILE || null;
  fs.appendFileSync(
    path.join(stateDir, "agy-calls.jsonl"),
    JSON.stringify({ args, prompt, cwd: process.cwd(), envKeys: Object.keys(process.env).sort(),
      mcpTools: Object.fromEntries(Object.entries(mcp).map(([n, s]) => [n, s.error ? `error: ${s.error}` : s.tools.map((t) => t.name)])),
      bridgeFile, bridgeFileMode: bridgeFile && fs.existsSync(bridgeFile) ? fs.statSync(bridgeFile).mode & 0o777 : null,
      noProxy: process.env.NO_PROXY || null, gitCeiling: process.env.GIT_CEILING_DIRECTORIES || null }) + "\n"
  );
  try {
    await turn(prompt, mcp);
  } finally {
    for (const s of Object.values(mcp)) s.client.close();
  }
}

async function turn(prompt, mcp) {
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
  const mcpTools = Object.entries(mcp).flatMap(([server, s]) => s.tools.map((t) => `${server}/${t.name}`));
  out({ event: "init", conversation_id: id, init: { cwd: process.cwd(), tools: ["view_file", "run_command", "call_mcp_tool", ...mcpTools],
    permission_mode: args.includes("--dangerously-skip-permissions") ? "bypass" : "default", model: argVal("--model") || "default" } });
  out({ event: "step_update", conversation_id: id, step_index: 0, state: "DONE", step_type: "user_input" });
  out({ event: "telemetry_hint", detail: "unknown event types must be ignored" });

  if (prompt.includes("FAIL_LIMIT")) {
    out({ event: "result", result: { conversation_id: id, status: "ERROR", response: "", error: "RESOURCE_EXHAUSTED: quota exceeded", num_turns: 1, usage } });
    process.exit(1);
  }
  if (prompt.includes("CLIENT_TOOL")) return await clientTools(id, prompt, mcp);

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
  if (prompt.includes("NO_DELTAS")) {
    out({ event: "result", result: { conversation_id: id, status: "SUCCESS", response: reply, error: "", num_turns: 1, usage } });
    return;
  }
  await say(reply);
  out({ event: "result", result: { conversation_id: id, status: "SUCCESS", response: reply, error: "", duration_seconds: 1, num_turns: 1, usage } });
}

async function clientTools(id, prompt, mcp) {
  const server = mcp.komputer_use;
  if (!server?.tools.length) throw new Error(`no komputer_use tools reached agy: ${JSON.stringify(Object.keys(mcp))}`);
  const forced = /For your first tool call, use ([a-z0-9_]+)/.exec(prompt)?.[1];
  const tool = server.tools.find((t) => t.name === forced) || server.tools[0];
  const info = { name: "call_mcp_tool", parameters: { ServerName: "komputer_use", ToolName: tool.name } };
  out({ event: "step_update", conversation_id: id, step_index: 1, state: "ACTIVE", step_type: "tool", tool_name: "call_mcp_tool", tool_info: info });
  const result = await server.client.call(tool.name, { query: "from agy" });
  out({ event: "step_update", conversation_id: id, step_index: 1, state: "DONE", step_type: "tool", tool_name: "call_mcp_tool", tool_info: { ...info, output: result } });
  const reply = `agy tool result:${result.content?.[0]?.text ?? ""}`;
  out({ event: "step_update", conversation_id: id, step_index: 2, state: "ACTIVE", step_type: "agent_response", text_delta: reply });
  out({ event: "result", result: { conversation_id: id, status: "SUCCESS", response: reply, error: "", num_turns: 1, usage } });
}
