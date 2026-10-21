#!/usr/bin/env node
// Test double for the Antigravity CLI (`agy`), modelled on agy 1.4.3 and the
// signed-in trace in fixtures/agy-1.4.3-signed-in-run-command.jsonl:
// --input-format stream-json on stdin; init (cwd, the same 60 tool names,
// permission_mode), step_update events nested under "step_update" with
// text_delta on agent_response steps (the DONE step may carry the last one)
// and tool_name + tool_info.{name,parameters,output} on tool steps, per-step
// usage, and a final result. Also a warning (not an error) for an unknown
// --conversation and the single ERROR result line it prints when not signed in.
//
// MCP works like the real CLI: `agy mcp add|remove|list|enable|disable`
// edits $HOME/.gemini/config/mcp_config.json (the only MCP config agy reads),
// and every run starts the enabled stdio servers with agy's own environment
// and cwd, then sends server/discover, falls back to initialize, and lists
// tools. Built-in tools also work like the real CLI (verified with agy 1.4.3):
// PreToolUse hooks from $HOME/.gemini/config/hooks.json run first,
// run_command is `bash -c` looked up on PATH, and file tools work in-process.
// Prompt keywords drive behaviour, like fake-claude.mjs.
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import readline from "node:readline";
import { spawn, spawnSync } from "node:child_process";
import { randomUUID } from "node:crypto";

const args = process.argv.slice(2);
const stateDir = process.env.FAKE_AGY_STATE;
const argVal = (flag) => {
  const i = args.indexOf(flag);
  return i >= 0 ? args[i + 1] : undefined;
};
const out = (o) => process.stdout.write(JSON.stringify(o) + "\n");
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const usage = { input_tokens: 20, output_tokens: 9, thinking_tokens: 4, cache_read_tokens: 5, total_tokens: 29 };
const trace = path.join(path.dirname(new URL(import.meta.url).pathname), "fixtures", "agy-1.4.3-signed-in-run-command.jsonl");
const BUILTIN_TOOLS = JSON.parse(fs.readFileSync(trace, "utf8").split("\n")[0]).init.tools;
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
      target: bridgeFile && fs.existsSync(bridgeFile) ? (({ policy, remote }) => ({ policy, remote: remote && { ...remote, server: remote.server && { url: remote.server.url } } }))(JSON.parse(fs.readFileSync(bridgeFile, "utf8"))) : null,
      pathHead: String(process.env.PATH || "").split(path.delimiter)[0],
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
  // The trace lists agy's own tools only; MCP tools are reached through call_mcp_tool.
  out({ event: "init", conversation_id: id, init: { cwd: process.cwd(), tools: BUILTIN_TOOLS,
    permission_mode: process.env.FAKE_AGY_PERMISSION_MODE || (args.includes("--dangerously-skip-permissions") ? "always-proceed" : "default") } });
  const stepOut = (o) => out({ event: "step_update", step_update: { conversation_id: id, ...o } });
  stepOut({ step_index: 0, state: "DONE", step_type: "user_input" });
  out({ event: "telemetry_hint", detail: "unknown event types must be ignored" });

  if (prompt.includes("FAIL_LIMIT")) {
    out({ event: "result", result: { conversation_id: id, status: "ERROR", response: "", error: "RESOURCE_EXHAUSTED: quota exceeded", num_turns: 1, usage } });
    process.exit(1);
  }
  if (prompt.includes("CLIENT_TOOL")) return await clientTools(id, prompt, mcp);

  let step = 1;
  const say = async (text) => {
    const pieces = [text.slice(0, 4), text.slice(4)].filter(Boolean);
    for (const [i, piece] of pieces.entries()) {
      const last = i === pieces.length - 1;
      stepOut({ step_index: step, state: last ? "DONE" : "ACTIVE", step_type: "agent_response", text_delta: piece, ...(last ? { usage } : {}) });
      if (prompt.includes("SLOW")) await sleep(300);
    }
    step++;
  };
  const toolStep = async (name, parameters, output) => {
    // A tool turn starts with a text-free agent_response step, as in the trace.
    stepOut({ step_index: step++, state: "DONE", step_type: "agent_response", usage });
    stepOut({ step_index: step, state: "ACTIVE", step_type: "tool", tool_name: name, tool_info: { name, parameters } });
    if (prompt.includes("SLOW")) await sleep(300);
    stepOut({ step_index: step, state: "DONE", step_type: "tool", tool_name: name, tool_info: { name, parameters, output } });
    step++;
  };
  if (prompt.includes("TOOL")) {
    await say("checking");
    await toolStep("call_mcp_tool", { ServerName: "komputer_use", ToolName: "run_command" }, "ok");
  }
  // Built-in tools as agy 1.4.3 runs them: PreToolUse hooks first, then run_command as
  // `bash -c` found on PATH and file tools in-process. BUILTIN<<cmd>>, READ<<path>>, WRITE<<path>>.
  const builtin = async (name, parameters, exec) => {
    stepOut({ step_index: step++, state: "DONE", step_type: "agent_response", usage });
    stepOut({ step_index: step, state: "ACTIVE", step_type: "tool", tool_name: name, tool_info: { name, parameters } });
    const hooked = process.env.FAKE_AGY_NO_HOOKS ? { args: parameters } : preToolHooks(name, parameters, id, step);
    const record = { name, args: hooked.args };
    if (hooked.denied) {
      record.error = hooked.denied;
      stepOut({ step_index: step, state: "ERROR", step_type: "tool", tool_name: name, tool_info: { name, parameters, error: { type: "TOOL_ERROR", message: hooked.denied } } });
    } else {
      record.output = await exec(hooked.args);
      stepOut({ step_index: step, state: "DONE", step_type: "tool", tool_name: name, tool_info: { name, parameters: hooked.args, output: record.output } });
    }
    fs.appendFileSync(path.join(stateDir, "agy-tools.jsonl"), JSON.stringify(record) + "\n");
    step++;
  };
  if (prompt.includes("BUILTIN")) {
    const command = /BUILTIN<<([\s\S]*?)>>/.exec(prompt)?.[1] || "ls -la";
    await builtin("run_command", { CommandLine: command, toolSummary: "run", WaitMsBeforeAsync: 5000 }, async ({ CommandLine }) => {
      if (!args.includes("--dangerously-skip-permissions") && !allowRules().includes("command(*)")) {
        process.stderr.write('a tool required the "command" permission that headless mode cannot prompt for, so it was auto-denied\n');
        return "";
      }
      fs.appendFileSync(path.join(stateDir, "agy-host-commands.jsonl"), JSON.stringify({ command: CommandLine, cwd: process.cwd() }) + "\n");
      const r = spawnSync("bash", ["-c", CommandLine], { cwd: process.cwd(), env: process.env, encoding: "utf8" });
      return `Command exited with code ${r.status ?? 1}.\n\nCommand output:\n${(r.stdout || "") + (r.stderr || "")}`.replace(/\r?\n/g, "\r\n");
    });
  }
  for (const m of prompt.matchAll(/READ<<([^>]*)>>/g)) {
    await builtin("read_file", { Path: m[1] }, async ({ Path }) => {
      try {
        return fs.readFileSync(path.resolve(Path), "utf8");
      } catch (e) {
        return `error: ${e.code}`;
      }
    });
  }
  for (const m of prompt.matchAll(/WRITE<<([^>]*)>>/g)) {
    await builtin("write_file", { Path: m[1], Content: "written by agy\n" }, async ({ Path, Content }) => {
      fs.writeFileSync(path.resolve(Path), Content);
      return "";
    });
  }
  // edit_file as agy 1.4.3 does it: Target must match byte for byte; every occurrence is replaced.
  for (const m of prompt.matchAll(/EDIT<<([^>]*)>>/g)) {
    await builtin("edit_file", { Path: m[1], Target: "line one\nline two", Replacement: "LINE ONE\nLINE TWO" }, async ({ Path, Target, Replacement }) => {
      const text = fs.readFileSync(path.resolve(Path), "utf8");
      if (!text.includes(Target)) return "Encountered error in tool execution: target content not found in the file";
      fs.writeFileSync(path.resolve(Path), text.split(Target).join(Replacement));
      return "edited";
    });
  }
  const reply = resumed ? `prev=${previous}|now=${prompt}` : `agy:${prompt}`;
  if (prompt.includes("NO_DELTAS")) {
    out({ event: "result", result: { conversation_id: id, status: "SUCCESS", response: reply, error: "", num_turns: 1, usage } });
    return;
  }
  await say(reply);
  out({ event: "result", result: { conversation_id: id, status: "SUCCESS", response: reply, error: "", duration_seconds: 1, num_turns: 1, usage } });
}

function allowRules() {
  try {
    const allow = JSON.parse(fs.readFileSync(path.join(os.homedir(), ".gemini", "antigravity-cli", "settings.json"), "utf8"))?.permissions?.allow;
    return Array.isArray(allow) ? allow : [];
  } catch {
    return [];
  }
}

/**
 * ~/.gemini/config/hooks.json PreToolUse hooks, as agy 1.4.3 runs them: `sh -c` in the config
 * directory, the call on stdin; empty output is no opinion, a decision of "deny" (or none at all)
 * denies, "allow" with overwrite merges new arguments.
 */
function preToolHooks(name, args, conversationId, stepIdx) {
  const dir = path.join(os.homedir(), ".gemini", "config");
  let config = {};
  try {
    config = JSON.parse(fs.readFileSync(path.join(dir, "hooks.json"), "utf8"));
  } catch {
    return { args };
  }
  let current = args;
  for (const group of Object.values(config)) {
    for (const rule of group?.PreToolUse || []) {
      if (!new RegExp(rule.matcher || ".*").test(name)) continue;
      for (const hook of rule.hooks || []) {
        const input = JSON.stringify({ toolCall: { name, args: current }, stepIdx, conversationId, workspacePaths: [process.cwd()],
          artifactDirectoryPath: path.join(os.homedir(), ".gemini", "antigravity-cli", "brain", conversationId) });
        const r = spawnSync("sh", ["-c", hook.command], { cwd: dir, input, encoding: "utf8", env: process.env, timeout: (hook.timeout || 10) * 1000 });
        const text = (r.stdout || "").trim();
        if (!text) continue;
        const v = JSON.parse(text);
        if (v.decision === "ask") continue;
        if (v.decision !== "allow") return { args: current, denied: `tool call denied by pre-tool hook: ${v.reason || ""}` };
        if (v.overwrite) current = { ...current, ...v.overwrite };
      }
    }
  }
  return { args: current };
}

async function clientTools(id, prompt, mcp) {
  const server = mcp.komputer_use;
  if (!server?.tools.length) throw new Error(`no komputer_use tools reached agy: ${JSON.stringify(Object.keys(mcp))}`);
  const forced = /For your first tool call, use ([a-z0-9_]+)/.exec(prompt)?.[1];
  const tool = server.tools.find((t) => t.name === forced) || server.tools[0];
  const info = { name: "call_mcp_tool", parameters: { ServerName: "komputer_use", ToolName: tool.name } };
  const stepOut = (o) => out({ event: "step_update", step_update: { conversation_id: id, ...o } });
  stepOut({ step_index: 1, state: "DONE", step_type: "agent_response", usage });
  stepOut({ step_index: 2, state: "ACTIVE", step_type: "tool", tool_name: "call_mcp_tool", tool_info: info });
  const result = await server.client.call(tool.name, { query: "from agy" });
  stepOut({ step_index: 2, state: "DONE", step_type: "tool", tool_name: "call_mcp_tool", tool_info: { ...info, output: result } });
  const reply = `agy tool result:${result.content?.[0]?.text ?? ""}`;
  stepOut({ step_index: 3, state: "DONE", step_type: "agent_response", text_delta: reply, usage });
  out({ event: "result", result: { conversation_id: id, status: "SUCCESS", response: reply, error: "", num_turns: 1, usage } });
}
