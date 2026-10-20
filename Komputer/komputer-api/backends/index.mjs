import path from "node:path";
import { claudeBackend } from "./claude.mjs";
import { antigravityBackend } from "./antigravity.mjs";
import { museBackend } from "./muse.mjs";
import { splitArgs } from "./common.mjs";

const list = (v, fallback) => String(v ?? fallback).split(",").map((s) => s.trim()).filter(Boolean);
const ALIASES = { claude: "claude", "claude-code": "claude", agy: "antigravity", antigravity: "antigravity", muse: "muse", "muse-code": "muse" };

/**
 * Backends enabled by KOMPUTER_BACKENDS (default "claude", which keeps the
 * service exactly as it was before backends existed).
 */
export function createBackends(env, { workspaceRoot, instanceId, claudeCwd, mcpConfig, systemPromptFile }) {
  const names = [...new Set(list(env.KOMPUTER_BACKENDS, "claude").map((n) => {
    const name = ALIASES[n.toLowerCase()];
    if (!name) throw new Error(`KOMPUTER_BACKENDS: unknown backend "${n}" (use claude, antigravity, muse)`);
    return name;
  }))];
  if (!names.length) throw new Error("KOMPUTER_BACKENDS must name at least one backend");
  const build = {
    claude: () =>
      claudeBackend({
        bin: env.CLAUDE_BIN || "claude",
        cwd: claudeCwd,
        mcpConfig,
        modelId: env.KOMPUTER_MODEL_ID || "komputer-claude",
        extraModels: list(env.KOMPUTER_MODELS, "komputer-opus,komputer-sonnet,komputer-haiku,komputer-fable"),
        defaultModel: env.KOMPUTER_CLAUDE_MODEL || "",
        defaultEffort: env.KOMPUTER_EFFORT || "",
        taskModel: env.KOMPUTER_TASK_MODEL ?? "haiku",
        systemPromptFile,
      }),
    antigravity: () =>
      antigravityBackend({
        bin: env.AGY_BIN || "agy",
        cwd: path.resolve(env.KOMPUTER_AGY_CWD || path.join(workspaceRoot, `${instanceId}-agy`)),
        mcpConfig,
        bodyMcp: env.KOMPUTER_BODY_MCP || "komputer_use",
        modelId: env.KOMPUTER_AGY_MODEL_ID || "komputer-agy",
        models: list(env.KOMPUTER_AGY_MODELS, "komputer-gemini-4-argon,komputer-gemini-3.8-flash"),
        defaultModel: env.KOMPUTER_AGY_MODEL || "",
        defaultEffort: env.KOMPUTER_AGY_EFFORT || "",
        efforts: list(env.KOMPUTER_AGY_EFFORTS, "low,medium,high"),
        taskModel: env.KOMPUTER_AGY_TASK_MODEL ?? "",
        permissions: (env.KOMPUTER_AGY_PERMISSIONS || "settings").toLowerCase(),
        extraArgs: splitArgs(env.KOMPUTER_AGY_EXTRA_ARGS),
      }),
    muse: () =>
      museBackend({
        bin: env.MUSE_BIN || "muse",
        cwd: path.resolve(env.KOMPUTER_MUSE_CWD || path.join(workspaceRoot, `${instanceId}-muse`)),
        mcpConfig,
        modelId: env.KOMPUTER_MUSE_MODEL_ID || "komputer-muse",
        models: list(env.KOMPUTER_MUSE_MODELS, ""),
        defaultModel: env.KOMPUTER_MUSE_MODEL || "",
        defaultEffort: env.KOMPUTER_MUSE_EFFORT || "",
        taskModel: env.KOMPUTER_MUSE_TASK_MODEL ?? "",
        extraArgs: splitArgs(env.KOMPUTER_MUSE_EXTRA_ARGS),
      }),
  };
  const backends = names.map((n) => build[n]());
  const byName = new Map(backends.map((b) => [b.name, b]));
  const defaultName = ALIASES[String(env.KOMPUTER_DEFAULT_BACKEND || names[0]).toLowerCase()];
  const fallback = byName.get(defaultName);
  if (!fallback) throw new Error("KOMPUTER_DEFAULT_BACKEND must be one of KOMPUTER_BACKENDS");
  if (byName.has("antigravity") && !["settings", "skip"].includes((env.KOMPUTER_AGY_PERMISSIONS || "settings").toLowerCase())) {
    throw new Error("KOMPUTER_AGY_PERMISSIONS must be settings or skip");
  }
  const ids = new Set();
  for (const b of backends) {
    for (const id of b.ids) {
      if (ids.has(id)) throw new Error(`model id ${id} is advertised by two backends`);
      ids.add(id);
    }
  }
  return {
    list: backends,
    byName,
    default: fallback,
    get(name) {
      return byName.get(name || "claude") || null;
    },
    /** Exact advertised ids first, then each backend's own name patterns; anything else → default. */
    route(model) {
      const id = String(model || "").trim();
      if (!id) return fallback;
      for (const b of backends) if (b.ids.includes(id)) return b;
      // Non-Claude patterns are explicit prefixes; check them before Claude's catch-all aliases.
      for (const b of [...backends].sort((a, z) => (a.name === "claude") - (z.name === "claude"))) if (b.owns(id)) return b;
      return fallback;
    },
  };
}
