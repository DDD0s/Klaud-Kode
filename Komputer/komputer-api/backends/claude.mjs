// Claude Code backend: `claude -p --output-format stream-json`. The argv,
// stdin and stream parsing here are the pre-abstraction behavior, unchanged;
// test/claude-regression.test.mjs pins them.
import os from "node:os";
import path from "node:path";
import { promises as fs } from "node:fs";
import { randomUUID } from "node:crypto";
import { buildClaudeEnv } from "../claude-env.mjs";
import { jsonLine } from "./common.mjs";

const EFFORT_LEVELS = new Set(["low", "medium", "high", "xhigh", "max"]);

/**
 * @param {{session: {mode: "new"|"resume"|"none", id?: string}, sysFile: string, model?: string,
 *          effort?: string, tools?: string, mcp?: boolean, streamInput?: boolean,
 *          systemPromptFile?: string, mcpConfig?: string}} o
 * @param {string} defaultMcpConfig
 */
export function claudeArgs(o, defaultMcpConfig) {
  // The prompt goes over stdin: no argv size limit, not visible in `ps`, and a
  // message starting with "-" can never be parsed as a CLI flag.
  const args = ["-p", "--dangerously-skip-permissions", "--strict-mcp-config"];
  if (o.mcp !== false) args.push("--mcp-config", o.mcpConfig || defaultMcpConfig);
  args.push(
    "--output-format",
    "stream-json",
    "--verbose",
    "--include-partial-messages",
    "--append-system-prompt-file",
    o.sysFile
  );
  if (o.systemPromptFile) args.push("--system-prompt-file", o.systemPromptFile);
  if (o.streamInput) args.push("--input-format", "stream-json");
  if (o.tools !== undefined && o.tools !== "default") args.push("--tools", o.tools);
  if (o.model) args.push("--model", o.model);
  if (o.effort) args.push("--effort", o.effort);
  if (o.session.mode === "resume") args.push("--resume", o.session.id);
  else if (o.session.mode === "new") args.push("--session-id", o.session.id);
  else args.push("--no-session-persistence");
  return args;
}

/** Stream-json parser for one Claude Code run. */
export function claudeParser({ sessionId, hooks }) {
  const { onText, onThinking, onToolUse } = hooks;
  let result = null;
  let emittedText = false;
  let thinking = "";
  let hadActivity = false;
  return {
    get activity() {
      return hadActivity;
    },
    line(line) {
      const ev = jsonLine(line);
      if (!ev) return;
      // Never automatically replay a turn once inference/tool activity began,
      // including subagent events that are not forwarded to the client UI.
      if (["stream_event", "assistant", "user", "tool_use", "tool_result"].includes(ev.type)) hadActivity = true;
      if (ev.type === "result" && Number(ev.num_turns) > 0) hadActivity = true;
      if (ev.type === "stream_event" && ev.event && !ev.parent_tool_use_id) {
        const e = ev.event;
        if (e.type === "content_block_start") {
          const t = e.content_block?.type;
          if (t === "text" && emittedText) onText?.("\n\n"); // new text block after tool use
          else if (t === "tool_use" || t === "server_tool_use") onToolUse?.(e.content_block.name);
        } else if (e.type === "content_block_delta") {
          if (e.delta?.type === "text_delta" && e.delta.text) {
            emittedText = true;
            onText?.(e.delta.text);
          } else if (e.delta?.type === "thinking_delta" && e.delta.thinking) {
            thinking += e.delta.thinking;
            onThinking?.(e.delta.thinking);
          }
        }
      } else if (ev.type === "result") {
        result = ev;
      }
    },
    stderr() {},
    finish(code, sig, stderr) {
      if (result) {
        if (result.is_error) return { ok: false, message: String(result.result || result.error || "claude is_error") };
        return {
          ok: true,
          value: { text: String(result.result ?? "").trim(), thinking, session_id: result.session_id || sessionId, usage: result.usage || null },
        };
      }
      return { ok: false, message: `claude exit ${code}${sig ? ` signal=${sig}` : ""}: ${stderr.slice(-800)}` };
    },
  };
}

/**
 * @param {{bin: string, cwd: string, mcpConfig: string, modelId: string, extraModels: string[],
 *          defaultModel: string, defaultEffort: string, taskModel: string, systemPromptFile: string}} cfg
 */
export function claudeBackend(cfg) {
  const ids = [cfg.modelId, ...cfg.extraModels.filter((m) => m !== cfg.modelId)];
  return {
    name: "claude",
    label: "Claude Code",
    bin: cfg.bin,
    cwd: cfg.cwd,
    ids,
    defaultId: cfg.modelId,
    taskModel: cfg.taskModel,
    /** Claude Code accepts a caller-chosen --session-id, so the id is known before the first turn. */
    assignsSessionId: false,
    newSessionId: () => randomUUID(),
    inlinesSystem: false,
    owns(id) {
      return ids.includes(id) || /^(claude-|opus|sonnet|haiku|fable|mythos)/i.test(id);
    },
    resolveModel(requested) {
      const id = String(requested || "").trim();
      if (!id || id === cfg.modelId) return cfg.defaultModel;
      const m = /^komputer-(.+)$/i.exec(id);
      if (m) return m[1];
      if (/^(claude-|opus|sonnet|haiku|fable|mythos)/i.test(id)) return id;
      return cfg.defaultModel; // unknown ids (e.g. a client's built-in "gpt-4o") → default
    },
    resolveEffort(body) {
      let e = String(body.reasoning_effort ?? body.effort ?? body.output_config?.effort ?? "")
        .toLowerCase()
        .trim();
      if (e === "minimal" || e === "none") e = "low";
      if (!e && body.thinking?.type === "enabled") {
        const budget = Number(body.thinking.budget_tokens) || 0;
        e = budget >= 32000 ? "max" : budget >= 16000 ? "xhigh" : "high";
      }
      return EFFORT_LEVELS.has(e) ? e : cfg.defaultEffort;
    },
    childEnv(source, canonicalCwd) {
      return buildClaudeEnv(source, canonicalCwd);
    },
    async prepare({ session, prompt, attachments = [], systemPrompt, model, effort, tools, mcp, mcpConfig }) {
      const sysFile = path.join(os.tmpdir(), `komputer-sys-${randomUUID()}.txt`);
      await fs.writeFile(sysFile, systemPrompt, { mode: 0o600 });
      const streamInput = attachments.length > 0;
      const stdin = streamInput
        ? JSON.stringify({
            type: "user",
            message: { role: "user", content: [{ type: "text", text: prompt }, ...attachments] },
            parent_tool_use_id: null,
            session_id: "",
          }) + "\n"
        : prompt;
      return {
        args: claudeArgs({ session, sysFile, model, effort, tools, mcp, mcpConfig, streamInput, systemPromptFile: cfg.systemPromptFile }, cfg.mcpConfig),
        stdin,
        cwd: cfg.cwd,
        cleanup: () => fs.unlink(sysFile).catch(() => {}),
      };
    },
    parser: claudeParser,
    classify(message) {
      if (/No conversation found with session ID:\s*[0-9a-f-]{36}\s*$/i.test(message)) return "session_not_found";
      return null;
    },
  };
}
