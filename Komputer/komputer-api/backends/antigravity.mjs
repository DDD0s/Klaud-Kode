// Antigravity CLI backend: `agy --input-format stream-json --output-format stream-json`.
//
// Verified against agy 1.4.3 (see docs/backends.md): flags, the stdin user
// event, the init / step_update / result event shapes, the unauthenticated
// result, and the "conversation not found" warning on an unknown
// --conversation. Not verifiable without a signed-in account and therefore
// configurable: workspace MCP pickup from <cwd>/.agents/mcp_config.json,
// --model naming, and how text_delta / response behave across tool steps.
import path from "node:path";
import { promises as fs } from "node:fs";
import { buildChildEnv, AGY_KEEP } from "../claude-env.mjs";
import { backendError, inlineSystem, jsonLine, promptWithText, readMcpServers, splitAttachments } from "./common.mjs";

const EFFORT_SUFFIX = /-(low|medium|high|xhigh|max)$/i;
const ORDER = ["low", "medium", "high", "xhigh", "max"];

/** Claude-format MCP servers → agy's mcp_config.json (the format `agy mcp add` writes). */
export function agyMcpConfig(servers) {
  const mcpServers = {};
  for (const s of servers) {
    mcpServers[s.name] = s.url
      ? { serverUrl: s.url, headers: s.headers || {}, disabled: false }
      : { command: s.command, args: s.args || [], env: s.env || {}, disabled: false };
  }
  return { mcpServers };
}

async function writePrivateJson(file, value) {
  await fs.mkdir(path.dirname(file), { recursive: true, mode: 0o700 });
  const data = JSON.stringify(value, null, 2);
  const current = await fs.readFile(file, "utf8").catch(() => null);
  if (current === data) return;
  const tmp = `${file}.${process.pid}.tmp`;
  await fs.writeFile(tmp, data, { mode: 0o600 });
  await fs.rename(tmp, file);
}

export function agyArgs({ session, model, effort, permissions, extraArgs = [] }) {
  const args = ["--input-format", "stream-json", "--output-format", "stream-json"];
  if (permissions === "skip") args.push("--dangerously-skip-permissions");
  if (model) args.push("--model", model);
  // A model slug like gemini-4-argon-high already names its effort.
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

/** Parser for one agy run. bail() aborts the child before it does more work. */
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
          return {
            ok: true,
            value: {
              text: String(result.response || streamed).trim(),
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
 *          defaultEffort: string, efforts: string[], taskModel: string, permissions: "settings"|"skip", extraArgs: string[]}} cfg
 */
export function antigravityBackend(cfg) {
  const ids = [cfg.modelId, ...cfg.models.filter((m) => m !== cfg.modelId)];
  const efforts = cfg.efforts.length ? cfg.efforts : ["low", "medium", "high"];
  const bodyDir = path.join(cfg.cwd, "body");
  const plainDir = path.join(cfg.cwd, "plain");
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
    resolveModel(requested) {
      const id = String(requested || "").trim();
      if (!id || id === cfg.modelId) return cfg.defaultModel;
      return id.replace(/^komputer-/i, "").replace(/^(agy|antigravity)-/i, "") || cfg.defaultModel;
    },
    resolveEffort(body) {
      let e = String(body.reasoning_effort ?? body.effort ?? body.output_config?.effort ?? "").toLowerCase().trim();
      if (e === "minimal" || e === "none") e = "low";
      if (!e && body.thinking?.type === "enabled") {
        const budget = Number(body.thinking.budget_tokens) || 0;
        e = budget >= 32000 ? "max" : budget >= 16000 ? "xhigh" : "high";
      }
      return (e && clampEffort(e)) || cfg.defaultEffort;
    },
    childEnv(source, canonicalCwd) {
      return buildChildEnv(source, canonicalCwd, AGY_KEEP);
    },
    async init() {
      await fs.mkdir(bodyDir, { recursive: true });
      await fs.mkdir(plainDir, { recursive: true });
    },
    async prepare({ session, prompt, attachments = [], systemPrompt, includeSystem = true, model, effort, mcp, mcpServer, workDir }) {
      const { text, binary } = splitAttachments(attachments);
      if (binary.length) throw backendError(400, "This model does not accept image or PDF attachments", "invalid_request_error");
      let cwd;
      if (mcpServer) {
        cwd = path.join(workDir, "agy");
        await writePrivateJson(path.join(cwd, ".agents", "mcp_config.json"), agyMcpConfig([mcpServer]));
      } else if (mcp === false) {
        cwd = plainDir;
      } else {
        cwd = bodyDir;
        await writePrivateJson(path.join(cwd, ".agents", "mcp_config.json"), agyMcpConfig(await readMcpServers(cfg.mcpConfig)));
      }
      const body = promptWithText(prompt, text);
      return {
        args: agyArgs({ session, model, effort, permissions: cfg.permissions, extraArgs: cfg.extraArgs }),
        stdin: agyUserEvent(includeSystem ? inlineSystem(systemPrompt, body) : body),
        cwd,
      };
    },
    parser: agyParser,
    classify(message) {
      if (/authentication required|authentication failed|run 'agy' to log in|sign in/i.test(message)) return "upstream_auth_error";
      if (/RESOURCE_EXHAUSTED|quota/i.test(message)) return "rate_limit_error";
      return null;
    },
    /** Startup warnings for operator configuration the service must not change itself. */
    async checks(home) {
      const notes = [];
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
      const names = [...new Set([cfg.bodyMcp, "komputer_use"])];
      for (const name of names) {
        if (!allow.some((r) => String(r) === `mcp(${name}/*)` || String(r) === "mcp(*)")) {
          notes.push(`agy permissions: add "mcp(${name}/*)" to permissions.allow in ~/.gemini/antigravity-cli/settings.json, otherwise headless agy soft-denies those MCP tools`);
        }
      }
      return notes;
    },
  };
}
