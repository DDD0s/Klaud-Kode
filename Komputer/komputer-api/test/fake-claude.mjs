#!/usr/bin/env node
// Test double for the `claude` CLI: speaks --output-format stream-json and
// accepts --input-format stream-json. Behaviour is driven by keywords in the prompt.
import fs from "node:fs";
import path from "node:path";

const args = process.argv.slice(2);
const stateDir = process.env.FAKE_CLAUDE_STATE;
const argVal = (flag) => {
  const i = args.indexOf(flag);
  return i >= 0 ? args[i + 1] : undefined;
};
const resume = args.includes("--resume");
const sid = argVal("--resume") || argVal("--session-id") || null;
const sysFile = argVal("--append-system-prompt-file");
const streamInput = argVal("--input-format") === "stream-json";

let stdin = "";
process.stdin.setEncoding("utf8");
process.stdin.on("data", (d) => (stdin += d));
process.stdin.on("end", main);

const out = (o) => process.stdout.write(JSON.stringify(o) + "\n");
const ev = (event) => out({ type: "stream_event", event });
const delta = (text) => ev({ type: "content_block_delta", index: 0, delta: { type: "text_delta", text } });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function main() {
  let prompt = stdin;
  let attachments = [];
  if (streamInput) {
    const msg = JSON.parse(stdin.trim().split("\n")[0]);
    const content = msg.message.content;
    // The server sends the prompt text first, then attachment blocks.
    prompt = content[0].text;
    attachments = content.slice(1).map((b) => ({
      type: b.type,
      source_type: b.source?.type,
      media_type: b.source?.media_type,
      title: b.title,
      text: b.type === "text" ? b.text : undefined,
    }));
  }
  const system = sysFile && fs.existsSync(sysFile) ? fs.readFileSync(sysFile, "utf8") : null;
  const leaked = Object.keys(process.env).filter(
    (k) => /^(KOMPUTER_|BODY_|KLAUD_KODE_|CUSTOM_)/.test(k) || ["CLAUDE_BIN", "NODE_BIN"].includes(k) || /SECRET/.test(k)
  );
  const envSeen = {
    path: Boolean(process.env.PATH || process.env.Path),
    gitCeiling: process.env.GIT_CEILING_DIRECTORIES || null,
    fakeState: Boolean(process.env.FAKE_CLAUDE_STATE),
    loopbackBypass: (process.env.NO_PROXY || "").split(",").includes("127.0.0.1"),
    leaked,
  };
  fs.appendFileSync(
    path.join(stateDir, "calls.jsonl"),
    JSON.stringify({ args, prompt, stdin, system, attachments, pid: process.pid, cwd: process.cwd(), env: envSeen,
      envKeys: Object.keys(process.env).sort() }) + "\n"
  );

  let previous = "";
  if (prompt.includes("ALWAYS_MISSING")) {
    process.stderr.write(`No conversation found with session ID: ${sid}\n`);
    process.exit(1);
  }
  if (sid) {
    const sessFile = path.join(stateDir, `${sid}.txt`);
    if (!resume && fs.existsSync(sessFile)) {
      process.stderr.write(`Error: Session ID ${sid} is already in use.\n`);
      process.exit(1);
    }
    if (resume && !fs.existsSync(sessFile)) {
      process.stderr.write(`No conversation found with session ID: ${sid}\n`);
      process.exit(1);
    }
    previous = resume ? fs.readFileSync(sessFile, "utf8") : "";
    fs.writeFileSync(sessFile, prompt); // Claude persists the session as the turn starts
  }

  out({ type: "system", subtype: "init", session_id: sid });

  if (prompt.includes("SILENT_START")) await sleep(250);
  if (/FAIL_AFTER_(TEXT|TOOL)_MISSING/.test(prompt)) {
    if (prompt.includes("FAIL_AFTER_TEXT")) delta("already started");
    else ev({ type: "content_block_start", index: 0, content_block: { type: "tool_use", name: "run_command" } });
    out({ type: "result", is_error: true, result: `No conversation found with session ID: ${sid}` });
    process.exit(1);
  }
  if (prompt.includes("FAIL_PRIVATE")) {
    if (prompt.includes("AFTER_OUTPUT")) delta("started");
    out({ type: "result", is_error: true, result: "brain host /private/operator tunnel stderr Authorization: Bearer private-test-token sk-testsecret12345" });
    process.exit(1);
  }

  if (prompt.includes("FAIL_AUTH")) {
    const msg = "Failed to authenticate: OAuth session expired and could not be refreshed";
    out({ type: "result", subtype: "success", is_error: true, result: msg, session_id: sid });
    process.exit(1);
  }
  if (prompt.includes("FAIL_LIMIT")) {
    out({
      type: "result",
      subtype: "success",
      is_error: true,
      result: "Claude usage limit reached. Your limit will reset at 3pm.",
      session_id: sid,
    });
    process.exit(1);
  }
  if (prompt.includes("CLIENT_TOOL")) return await clientToolFixture(prompt, system);
  if (prompt.includes("THINK")) {
    ev({ type: "content_block_start", index: 0, content_block: { type: "thinking", thinking: "" } });
    ev({ type: "content_block_delta", index: 0, delta: { type: "thinking_delta", thinking: "pondering" } });
  }
  if (prompt.includes("SLOW")) {
    for (let i = 0; i < 6; i++) {
      delta(`tick${i} `);
      await sleep(500);
    }
    fs.writeFileSync(path.join(stateDir, `finished-${process.pid}`), "1");
  }
  if (prompt.includes("TOOL")) {
    delta("checking");
    ev({ type: "content_block_start", index: 1, content_block: { type: "tool_use", name: "mcp__komputer_use__run_command" } });
    ev({ type: "content_block_start", index: 2, content_block: { type: "text", text: "" } });
  }
  const reply = prompt.includes("ECHO_SYSTEM") ? `system=${system}` : resume ? `prev=${previous}|now=${prompt}` : `echo:${prompt}`;
  ev({ type: "content_block_start", index: 0, content_block: { type: "text", text: "" } });
  delta(reply.slice(0, 5));
  delta(reply.slice(5));
  out({
    type: "result",
    subtype: "success",
    is_error: false,
    result: reply,
    session_id: sid,
    usage: { input_tokens: 7, cache_read_input_tokens: 3, output_tokens: 11 },
  });
}

async function clientToolFixture(prompt, system) {
  const configPath = argVal("--mcp-config");
  const config = JSON.parse(fs.readFileSync(configPath, "utf8")).mcpServers.komputer_use;
  let id = 0;
  const rpc = async (method, params) => {
    const r = await fetch(config.url, {
      method: "POST", headers: { ...config.headers, "Content-Type": "application/json", Accept: "application/json, text/event-stream" },
      body: JSON.stringify({ jsonrpc: "2.0", id: ++id, method, params }),
    });
    if (!r.ok) throw new Error(`MCP HTTP ${r.status}`);
    const response = await r.json();
    if (response.error) throw new Error(response.error.message);
    return response.result;
  };
  await rpc("initialize", { protocolVersion: "2025-06-18", capabilities: {}, clientInfo: { name: "test-claude", version: "1" } });
  const { tools } = await rpc("tools/list");
  const toolRuns = [];
  if (!system.includes("Do not call any tools.") && !prompt.includes("CLIENT_TOOL_NO_CALL")) {
    const forced = /For your first tool call, use ([a-z0-9_]+)/.exec(system)?.[1];
    const selected = prompt.includes("CLIENT_TOOL_IGNORE_CHOICE") ? tools[0] : tools.find((t) => t.name === forced) || tools[0];
    delta("checking ");
    const call = async (tool, args = { query: prompt.includes("CLIENT_TOOL_UNICODE") ? "你好".repeat(20_000) : "hello 世界" }) => {
      ev({ type: "content_block_start", index: 1, content_block: { type: "tool_use", name: `mcp__komputer_use__${tool.name}` } });
      const result = await rpc("tools/call", { name: tool.name, arguments: args });
      toolRuns.push({ name: tool.title, result });
      return result;
    };
    if (prompt.includes("CLIENT_TOOL_BAD_ARGS")) await call(selected, { query: 42 });
    if (prompt.includes("CLIENT_TOOL_PARALLEL")) await Promise.all(tools.slice(0, 2).map((t) => call(t)));
    else await call(selected);
    if (prompt.includes("CLIENT_TOOL_TWO_ROUNDS")) await call(selected);
  }
  fs.appendFileSync(path.join(stateDir, "tool-results.jsonl"), JSON.stringify({ pid: process.pid, results: toolRuns }) + "\n");
  const reply = `tool results:${JSON.stringify(toolRuns)}`;
  ev({ type: "content_block_start", index: 0, content_block: { type: "text", text: "" } });
  delta(reply);
  out({ type: "result", is_error: false, result: reply, usage: { input_tokens: 7, output_tokens: 11 } });
}
