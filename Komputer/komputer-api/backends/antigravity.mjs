// Antigravity CLI backend: `agy --input-format stream-json --output-format stream-json`.
//
// Verified against agy 1.4.3 (see docs/backends.md): flags, the stdin user
// event, init / step_update / result events, model and effort rules, the
// unauthenticated result, the "conversation not found" warning, and MCP
// loading: only the global ~/.gemini/config/mcp_config.json (or plugins) is
// read, and stdio servers inherit agy's environment and cwd. MCP servers
// therefore reach agy through agy-mcp-bridge.mjs, registered once with
// `agy mcp add`, and each turn points the bridge at its own target.
import path from "node:path";
import os from "node:os";
import { execFile } from "node:child_process";
import { randomUUID } from "node:crypto";
import { fileURLToPath } from "node:url";
import { promises as fs } from "node:fs";
import { buildChildEnv, AGY_KEEP } from "../claude-env.mjs";
import { backendError, inlineSystem, jsonLine, promptWithText, readMcpServers, splitAttachments } from "./common.mjs";

const EFFORT_SUFFIX = /-(low|medium|high|xhigh|max)$/i;
const ORDER = ["low", "medium", "high", "xhigh", "max"];
export const AGY_BRIDGE = path.join(path.dirname(fileURLToPath(import.meta.url)), "agy-mcp-bridge.mjs");
export const agyMcpConfigFile = (home) => path.join(home, ".gemini", "config", "mcp_config.json");

/** The global entry that `agy mcp add <name> -- node <bridge> <name>` writes. */
export const bridgeEntry = (name, node = process.execPath) => ({ command: node, args: [AGY_BRIDGE, name] });

const isBridge = (entry, name) =>
  entry && !entry.disabled && entry.command && Array.isArray(entry.args) && entry.args.length === 2 &&
  path.basename(String(entry.args[0])) === "agy-mcp-bridge.mjs" && entry.args[1] === name;

/** Per-turn bridge targets, keyed by MCP server name. */
export function agyBridgeTargets(servers) {
  const out = {};
  for (const s of servers) out[s.name] = s.url ? { url: s.url, headers: s.headers || {} } : { command: s.command, args: s.args || [], env: s.env || {} };
  return { servers: out };
}

async function readGlobalMcp(home) {
  try {
    return JSON.parse(await fs.readFile(agyMcpConfigFile(home), "utf8"))?.mcpServers || {};
  } catch {
    return {};
  }
}

function runAgy(bin, args, env) {
  const viaNode = /\.(c|m)?js$/i.test(bin);
  return new Promise((resolve) => {
    execFile(viaNode ? process.execPath : bin, viaNode ? [bin, ...args] : args, { env, timeout: 30_000 }, (err, stdout, stderr) =>
      resolve({ ok: !err, out: `${stdout}${stderr}`.trim() }));
  });
}

export function agyArgs({ session, model, effort, permissions, extraArgs = [] }) {
  const args = ["--input-format", "stream-json", "--output-format", "stream-json"];
  if (permissions === "skip") args.push("--dangerously-skip-permissions");
  if (model) args.push("--model", model);
  // agy rejects a slug that names its effort (gemini-3.8-flash-low) combined with --effort.
  if (effort && !EFFORT_SUFFIX.test(model || "")) args.push("--effort", effort);
  if (session.mode === "resume" && session.id) args.push("--conversation", session.id);
  args.push(...extraArgs);
  return args;
}

export function agyUserEvent(text) {
  return JSON.stringify({ event: "user", message: { content: [{ type: "text", text }] } }) + "\n";
}

export function agyUsage(u) {
  if (!u || typeof u !== "object") return null;
  const cached = Number(u.cache_read_tokens) || 0;
  return {
    // agy reports input including cache reads; keep Claude's split so usage maths stays uniform.
    input_tokens: Math.max(0, (Number(u.input_tokens) || 0) - cached),
    cache_read_input_tokens: cached,
    output_tokens: Number(u.output_tokens) || 0,
    reasoning_tokens: Number(u.thinking_tokens) || 0,
  };
}

function toolName(ev) {
  const info = ev.tool_info || {};
  const p = info.parameters || {};
  if ((info.name || ev.tool_name) === "call_mcp_tool" && p.ServerName && p.ToolName) return `mcp__${p.ServerName}__${p.ToolName}`;
  return ev.tool_name || info.name || "tool";
}

const squash = (s) => String(s).replace(/\s+/g, " ").trim();

/** Parser for one agy run; unknown events are ignored. bail() aborts the child before it does more work. */
export function agyParser({ session, hooks, bail }) {
  const { onText, onToolUse } = hooks;
  const requested = session.mode === "resume" ? session.id : null;
  let conversation = requested;
  let result = null;
  let streamed = "";
  let lastTextStep = null;
  let afterTool = false;
  let activity = false;
  const announced = new Set();
  return {
    get activity() {
      return activity;
    },
    line(line) {
      const ev = jsonLine(line);
      if (!ev) return;
      if (ev.event === "init") {
        const id = ev.conversation_id || ev.init?.conversation_id || null;
        // agy starts a fresh conversation when --conversation is unknown.
        if (requested && id && id !== requested) {
          bail(`conversation "${requested}" not found (agy started ${id})`, "session_not_found");
          return;
        }
        if (id) conversation = id;
        return;
      }
      if (ev.event === "step_update") {
        if (ev.step_type === "agent_response" || ev.step_type === "tool") activity = true;
        if (ev.step_type === "agent_response" && typeof ev.text_delta === "string" && ev.text_delta) {
          if (streamed && (afterTool || (lastTextStep !== null && ev.step_index !== lastTextStep))) {
            streamed += "\n\n";
            onText?.("\n\n");
          }
          afterTool = false;
          lastTextStep = ev.step_index;
          streamed += ev.text_delta;
          onText?.(ev.text_delta);
        } else if (ev.step_type === "tool") {
          const key = ev.step_index ?? announced.size;
          if (!announced.has(key)) {
            announced.add(key);
            afterTool = true;
            onToolUse?.(toolName(ev));
          }
        }
        return;
      }
      if (ev.event === "result" && ev.result && typeof ev.result === "object") {
        result = ev.result;
        if (Number(result.num_turns) > 0) activity = true;
        if (result.conversation_id && !requested) conversation = result.conversation_id;
      }
    },
    stderr(text) {
      const m = /conversation "([^"]+)" not found/i.exec(text);
      if (m && requested) bail(`conversation "${m[1]}" not found`, "session_not_found");
    },
    finish(code, sig, stderr) {
      if (result) {
        if (result.status === "SUCCESS") {
          const response = String(result.response || "");
          let text = streamed;
          // The final response may cover only the last step, or text the deltas never carried.
          if (!streamed.trim() || (response.startsWith(streamed) && response.length > streamed.length)) {
            onText?.(response.slice(streamed.length));
            text = response;
          } else if (!squash(streamed).includes(squash(response))) {
            text = response;
          }
          return {
            ok: true,
            value: {
              text: text.trim(),
              thinking: "",
              session_id: conversation || result.conversation_id || null,
              usage: agyUsage(result.usage),
            },
          };
        }
        return { ok: false, message: `agy ${result.status || "ERROR"}: ${result.error || ""} ${stderr.slice(-400)}`.trim() };
      }
      return { ok: false, message: `agy exit ${code}${sig ? ` signal=${sig}` : ""}: ${stderr.slice(-800)}` };
    },
  };
}

/**
 * @param {{bin: string, cwd: string, mcpConfig: string, modelId: string, models: string[], defaultModel: string,
 *          defaultEffort: string, efforts: string[], taskModel: string, permissions: "settings"|"skip", mcpSetup: "auto"|"off", extraArgs: string[]}} cfg
 */
export function antigravityBackend(cfg) {
  const ids = [cfg.modelId, ...cfg.models.filter((m) => m !== cfg.modelId)];
  const efforts = cfg.efforts.length ? cfg.efforts : ["low", "medium", "high"];
  const bodyDir = path.join(cfg.cwd, "body");
  const plainDir = path.join(cfg.cwd, "plain");
  const targetDir = path.join(cfg.cwd, "mcp");
  let bridgeNames = [];
  const setupNotes = [];
  /** komputer-gemini-4-argon → gemini-4-argon; komputer-agy-claude-opus-5-5 → claude-opus-5-5. */
  const resolveModel = (requested) => {
    const id = String(requested || "").trim();
    if (!id || id === cfg.modelId) return cfg.defaultModel;
    return id.replace(/^komputer-/i, "").replace(/^(agy|antigravity)-/i, "") || cfg.defaultModel;
  };
  const clampEffort = (e) => {
    if (efforts.includes(e)) return e;
    const want = ORDER.indexOf(e);
    if (want < 0) return "";
    const allowed = efforts.filter((x) => ORDER.indexOf(x) >= 0).sort((a, b) => ORDER.indexOf(a) - ORDER.indexOf(b));
    return allowed.filter((x) => ORDER.indexOf(x) <= want).at(-1) || allowed[0] || "";
  };
  return {
    name: "antigravity",
    label: "Antigravity CLI",
    bin: cfg.bin,
    cwd: cfg.cwd,
    ids,
    defaultId: cfg.modelId,
    taskModel: cfg.taskModel,
    /** agy picks the conversation id; it is learned from the init / result event. */
    assignsSessionId: true,
    newSessionId: () => null,
    inlinesSystem: true,
    owns(id) {
      return ids.includes(id) || /^(komputer-)?(agy|antigravity|gemini)(-|$)/i.test(id);
    },
    resolveModel,
    resolveEffort(body) {
      let e = String(body.reasoning_effort ?? body.effort ?? body.output_config?.effort ?? "").toLowerCase().trim();
      if (e === "minimal" || e === "none") e = "low";
      if (!e && body.thinking?.type === "enabled") {
        const budget = Number(body.thinking.budget_tokens) || 0;
        e = budget >= 32000 ? "max" : budget >= 16000 ? "xhigh" : "high";
      }
      if (!e) return cfg.defaultEffort;
      // Gemini models reject xhigh/max; other agy models (and an unknown default) get the CLI's own range.
      const model = resolveModel(body.model) || cfg.defaultModel;
      if (!model || /^gemini/i.test(model)) return clampEffort(e) || cfg.defaultEffort;
      return ORDER.includes(e) ? e : cfg.defaultEffort;
    },
    childEnv(source, canonicalCwd) {
      return buildChildEnv(source, canonicalCwd, AGY_KEEP);
    },
    async init() {
      await fs.mkdir(bodyDir, { recursive: true });
      await fs.mkdir(plainDir, { recursive: true });
      await fs.mkdir(targetDir, { recursive: true, mode: 0o700 });
      bridgeNames = [...new Set([cfg.bodyMcp, "komputer_use", ...(await readMcpServers(cfg.mcpConfig).catch(() => [])).map((s) => s.name)])];
      if (cfg.mcpSetup !== "auto") return;
      const home = os.homedir();
      const current = await readGlobalMcp(home);
      const env = buildChildEnv(process.env, cfg.cwd, AGY_KEEP);
      for (const name of bridgeNames) {
        if (isBridge(current[name], name) && current[name].command === process.execPath && current[name].args[0] === AGY_BRIDGE) continue;
        const { command, args } = bridgeEntry(name);
        const r = await runAgy(cfg.bin, ["mcp", "add", name, "--", command, ...args], env);
        setupNotes.push(r.ok
          ? `agy MCP: registered "${name}" in ${agyMcpConfigFile("~")} (stdio bridge to this service)`
          : `agy MCP: "agy mcp add ${name}" failed: ${r.out.slice(-300)}`);
      }
    },
    async prepare({ session, prompt, attachments = [], systemPrompt, includeSystem = true, model, effort, mcp, mcpServer, workDir }) {
      const { text, binary } = splitAttachments(attachments);
      if (binary.length) throw backendError(400, "This model does not accept image or PDF attachments", "invalid_request_error");
      let cwd;
      let servers = [];
      if (mcpServer) {
        cwd = path.join(workDir, "agy");
        servers = [mcpServer];
      } else if (mcp === false) {
        cwd = plainDir;
      } else {
        cwd = bodyDir;
        servers = await readMcpServers(cfg.mcpConfig);
      }
      await fs.mkdir(cwd, { recursive: true });
      const body = promptWithText(prompt, text);
      const prepared = {
        args: agyArgs({ session, model, effort, permissions: cfg.permissions, extraArgs: cfg.extraArgs }),
        stdin: agyUserEvent(includeSystem ? inlineSystem(systemPrompt, body) : body),
        cwd,
      };
      if (!servers.length) return prepared;
      // Outside agy's workspace, so its file tools do not list it next to the user's work.
      const file = path.join(mcpServer ? workDir : targetDir, `agy-mcp-${randomUUID()}.json`);
      await fs.writeFile(file, JSON.stringify(agyBridgeTargets(servers)), { mode: 0o600 });
      return { ...prepared, env: { KOMPUTER_AGY_MCP_FILE: file }, cleanup: () => fs.rm(file, { force: true }).catch(() => {}) };
    },
    parser: agyParser,
    classify(message) {
      if (/authentication required|authentication failed|run 'agy' to log in|sign in/i.test(message)) return "upstream_auth_error";
      if (/RESOURCE_EXHAUSTED|quota/i.test(message)) return "rate_limit_error";
      return null;
    },
    /** Startup warnings for operator configuration the service must not change itself. */
    async checks(home) {
      const notes = [...setupNotes];
      const names = bridgeNames.length ? bridgeNames : [...new Set([cfg.bodyMcp, "komputer_use"])];
      const global = await readGlobalMcp(home);
      for (const name of names) {
        if (!isBridge(global[name], name)) {
          notes.push(`agy MCP: "${name}" is not the komputer bridge in ~/.gemini/config/mcp_config.json, so agy turns cannot reach it; ` +
            `run: agy mcp add ${name} -- ${process.execPath} ${AGY_BRIDGE} ${name}`);
        }
      }
      for (const [name, entry] of Object.entries(global)) {
        if (!names.includes(name) && !entry?.disabled) {
          notes.push(`agy MCP: global server "${name}" is enabled for every agy turn and runs on this host; disable it with: agy mcp disable ${name}`);
        }
      }
      if (cfg.permissions === "skip") {
        notes.push("KOMPUTER_AGY_PERMISSIONS=skip passes --dangerously-skip-permissions: agy's built-in shell and file tools run unprompted on this host");
        return notes;
      }
      const file = path.join(home, ".gemini", "antigravity-cli", "settings.json");
      let allow = [];
      try {
        allow = JSON.parse(await fs.readFile(file, "utf8"))?.permissions?.allow || [];
      } catch {
        /* missing or unreadable: reported below */
      }
      for (const name of names) {
        if (!allow.some((r) => String(r) === `mcp(${name}/*)` || String(r) === "mcp(*)")) {
          notes.push(`agy permissions: add "mcp(${name}/*)" to permissions.allow in ~/.gemini/antigravity-cli/settings.json, otherwise headless agy soft-denies those MCP tools`);
        }
      }
      return notes;
    },
  };
}
