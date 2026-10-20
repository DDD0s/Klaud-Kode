// Muse Code CLI backend: `muse exec --json --prompt-file <file>`.
//
// Verified against muse 1.4.11 with the offline echo provider (see
// docs/backends.md): the JSONL envelope, run.output.delta / run.usage /
// run.terminal.* records, --session-id create-or-resume and its sequence
// numbering, the workspace-mismatch refusal, the missing-credentials error,
// and MCP injection through -c with ${VAR} values interpolated from the child
// environment. Not verifiable offline: the task_kind of tool steps, and whether
// a real model emits reasoning text in exec JSON.
import os from "node:os";
import path from "node:path";
import { promises as fs } from "node:fs";
import { randomUUID } from "node:crypto";
import { buildChildEnv, MUSE_KEEP } from "../claude-env.mjs";
import { backendError, inlineSystem, jsonLine, promptWithText, readMcpServers, splitAttachments } from "./common.mjs";

const EFFORTS = new Set(["none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"]);
const IMAGE_EXT = { "image/png": "png", "image/jpeg": "jpg", "image/jpg": "jpg", "image/gif": "gif", "image/webp": "webp" };

/**
 * MCP servers → `-c mcp_servers.<name>=<json>` values. URLs and header values
 * stay out of argv: muse interpolates whole-value ${VAR} from its environment.
 */
export function museMcpOverrides(servers) {
  const args = [];
  const env = {};
  servers.forEach((s, i) => {
    if (!s.url) return; // stdio body servers are not translated (documented)
    const urlVar = `KMCP_${i}_URL`;
    env[urlVar] = s.url;
    const headers = {};
    Object.entries(s.headers || {}).forEach(([h, v], j) => {
      const hv = `KMCP_${i}_H${j}`;
      env[hv] = String(v);
      headers[h] = `\${${hv}}`;
    });
    const value = { transport: "streamable_http", url: `\${${urlVar}}`, ...(Object.keys(headers).length ? { headers } : {}), mode: "required" };
    args.push("-c", `mcp_servers.${s.name}=${JSON.stringify(value)}`);
  });
  return { args, env };
}

export function museArgs({ promptFile, workspace, session, model, effort, tools, mcpArgs = [], images = [], extraArgs = [] }) {
  const args = ["exec", "--json", "--prompt-file", promptFile, "--workspace", workspace];
  if (model) args.push("--model", model);
  if (effort) args.push("--reasoning-effort", effort);
  if (session.mode === "none") args.push("--no-session-log");
  else args.push("--session-id", session.id);
  args.push("--disable-approval", "--disable-request-user-input", "--no-foreign-personal-context");
  // Island mode: no shell, file writes or web on the brain host; MCP tools only.
  if (tools === "") args.push("--disable-shell", "--disable-write", "--disable-web-tools");
  args.push(...mcpArgs);
  for (const img of images) args.push("--image", img);
  args.push(...extraArgs);
  return args;
}

export function museUsage(models) {
  if (!Array.isArray(models) || !models.length) return null;
  const sum = (k) => models.reduce((n, m) => n + (Number(m?.[k]) || 0), 0);
  const cached = sum("cached_tokens");
  return {
    input_tokens: Math.max(0, sum("input_tokens") - cached),
    cache_read_input_tokens: cached,
    output_tokens: sum("output_tokens"),
    reasoning_tokens: sum("reasoning_tokens"),
  };
}

/** Parser for one `muse exec --json` run. */
export function museParser({ session, hooks, bail }) {
  const { onText, onToolUse } = hooks;
  const resume = session.mode === "resume";
  let first = true;
  let runId = null;
  let terminal = null;
  let usage = null;
  let streamed = "";
  let afterTool = false;
  let activity = false;
  return {
    get activity() {
      return activity;
    },
    line(line) {
      const r = jsonLine(line);
      if (!r || !r.payload) return;
      const p = r.payload;
      if (r.payload_type === "run.usage") {
        usage = museUsage(p.models);
        return;
      }
      if (r.stream?.kind !== "session") return;
      if (first) {
        first = false;
        // --session-id creates a session when it does not exist; a resumed one
        // continues its sequence numbering.
        if (resume && Number(r.sequence) === 1) {
          bail(`session ${session.id} not found (muse started a new one)`, "session_not_found");
          return;
        }
      }
      const ownRun = (x) => !runId || !x?.run_stream?.id || x.run_stream.id === runId;
      switch (r.payload_type) {
        case "session.run.linked":
          runId ??= p.run_stream?.id || null;
          break;
        case "run.output.delta":
          if (!ownRun(p) || typeof p.text !== "string" || !p.text) break;
          activity = true;
          if (afterTool && streamed) {
            streamed += "\n\n";
            onText?.("\n\n");
          }
          afterTool = false;
          streamed += p.text;
          onText?.(p.text);
          break;
        case "task.lifecycle.proposed": {
          const kind = String(p.event?.task_kind || "");
          if (!ownRun(p) || kind.startsWith("reminder.")) break;
          activity = true;
          if (kind.startsWith("model.")) break;
          afterTool = true;
          onToolUse?.(p.event?.tool_name || p.event?.name || kind.replace(/^tool\./, "") || "tool");
          break;
        }
        case "run.terminal.completed":
        case "run.terminal.failed":
        case "run.terminal.cancelled":
          if (ownRun(p)) terminal = { type: r.payload_type, text: p.text, reason: p.reason };
          break;
        default:
      }
    },
    stderr(text) {
      if (/refusing to resume in workspace/i.test(text)) bail("session belongs to a different workspace", "session_not_found");
    },
    finish(code, sig, stderr) {
      if (terminal?.type === "run.terminal.completed") {
        return {
          ok: true,
          value: { text: String(terminal.text ?? streamed).trim() || streamed.trim(), thinking: "", session_id: session.id || null, usage },
        };
      }
      if (terminal) return { ok: false, message: `muse ${terminal.type.split(".").pop()}: ${terminal.reason || ""} ${stderr.slice(-400)}`.trim() };
      return { ok: false, message: `muse exit ${code}${sig ? ` signal=${sig}` : ""}: ${stderr.slice(-800)}` };
    },
  };
}

/**
 * @param {{bin: string, cwd: string, mcpConfig: string, modelId: string, models: string[], defaultModel: string,
 *          defaultEffort: string, taskModel: string, extraArgs: string[]}} cfg
 */
export function museBackend(cfg) {
  const ids = [cfg.modelId, ...cfg.models.filter((m) => m !== cfg.modelId)];
  return {
    name: "muse",
    label: "Muse Code CLI",
    bin: cfg.bin,
    cwd: cfg.cwd,
    ids,
    defaultId: cfg.modelId,
    taskModel: cfg.taskModel,
    /** muse exec --session-id <uuid> creates or resumes that session. */
    assignsSessionId: false,
    newSessionId: () => randomUUID(),
    inlinesSystem: true,
    owns(id) {
      return ids.includes(id) || /^(komputer-)?muse(-|$)/i.test(id);
    },
    resolveModel(requested) {
      const id = String(requested || "").trim();
      if (!id || id === cfg.modelId) return cfg.defaultModel;
      return id.replace(/^komputer-/i, "") || cfg.defaultModel;
    },
    resolveEffort(body) {
      let e = String(body.reasoning_effort ?? body.effort ?? body.output_config?.effort ?? "").toLowerCase().trim();
      if (!e && body.thinking?.type === "enabled") {
        const budget = Number(body.thinking.budget_tokens) || 0;
        e = budget >= 32000 ? "max" : budget >= 16000 ? "xhigh" : "high";
      }
      return EFFORTS.has(e) ? e : cfg.defaultEffort;
    },
    childEnv(source, canonicalCwd) {
      return buildChildEnv(source, canonicalCwd, MUSE_KEEP);
    },
    async init() {
      await fs.mkdir(cfg.cwd, { recursive: true });
    },
    async prepare({ session, prompt, attachments = [], systemPrompt, includeSystem = true, model, effort, tools, mcp, mcpServer, workDir }) {
      const { text, binary } = splitAttachments(attachments);
      const files = [];
      const cleanup = () => Promise.all(files.map((f) => fs.unlink(f).catch(() => {})));
      try {
        const images = [];
        for (const b of binary) {
          const ext = b.type === "image" && b.source?.type === "base64" ? IMAGE_EXT[String(b.source.media_type).toLowerCase()] : null;
          if (!ext) throw backendError(400, "This model accepts base64 PNG, JPEG, GIF or WebP images only", "invalid_request_error");
          const f = path.join(os.tmpdir(), `komputer-muse-img-${randomUUID()}.${ext}`);
          files.push(f);
          await fs.writeFile(f, Buffer.from(b.source.data, "base64"), { mode: 0o600 });
          images.push(f);
        }
        const body = promptWithText(prompt, text);
        const promptFile = path.join(os.tmpdir(), `komputer-muse-${randomUUID()}.txt`);
        files.push(promptFile);
        await fs.writeFile(promptFile, includeSystem ? inlineSystem(systemPrompt, body) : body, { mode: 0o600 });
        const servers = mcpServer ? [mcpServer] : mcp === false ? [] : await readMcpServers(cfg.mcpConfig);
        const mcpOverrides = museMcpOverrides(servers);
        return {
          args: museArgs({
            promptFile, workspace: mcpServer ? workDir : cfg.cwd, session, model, effort,
            tools: mcpServer ? "" : tools, mcpArgs: mcpOverrides.args, images, extraArgs: cfg.extraArgs,
          }),
          stdin: "",
          cwd: mcpServer ? workDir : cfg.cwd,
          env: mcpOverrides.env,
          cleanup,
        };
      } catch (e) {
        await cleanup();
        throw e;
      }
    },
    parser: museParser,
    classify(message) {
      if (/missing meta credentials|muse login|META_API_KEY/i.test(message)) return "upstream_auth_error";
      if (/Required MCP server .* failed during startup/i.test(message)) return "body_unavailable";
      return null;
    },
  };
}
