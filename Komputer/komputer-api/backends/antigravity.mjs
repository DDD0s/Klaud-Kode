// Antigravity CLI backend: `agy --input-format stream-json --output-format stream-json`.
//
// Verified against agy 1.4.3 (see docs/backends.md): flags, the stdin user
// event, init / step_update / result events, model and effort rules, the
// unauthenticated result, the "conversation not found" warning, and MCP
// loading: only the global ~/.gemini/config/mcp_config.json (or plugins) is
// read, and stdio servers inherit agy's environment and cwd. MCP servers
// therefore reach agy through agy-mcp-bridge.mjs, registered once with
// `agy mcp add`, and each turn points the bridge at its own target.
//
// agy's own tools run on the user's computer in Body turns (remote tools):
// agy runs run_command as `bash -c` found on PATH, so a `bash` shim first on
// its PATH runs the command on the Body (agy-bash-shim.mjs); its file tools
// work in-process, so the workspace is the Body root mounted over FUSE
// (komputer-fs.py). A PreToolUse hook (agy-hook.mjs) keeps paths inside that
// workspace and denies the tools that would still act on this host.
import path from "node:path";
import os from "node:os";
import { execFile, spawn } from "node:child_process";
import { randomUUID } from "node:crypto";
import { fileURLToPath } from "node:url";
import { existsSync, promises as fs } from "node:fs";
import { buildChildEnv, AGY_KEEP } from "../claude-env.mjs";
import { backendError, inlineSystem, jsonLine, promptWithText, readMcpServers, splitAttachments } from "./common.mjs";
import { AGY_HOST_FREE, decide } from "./agy-hook.mjs";
import { mcpHttpClient } from "./mcp-call.mjs";
import { environmentNote, isWindowsPath, windowsMountDir } from "./windows.mjs";

const EFFORT_SUFFIX = /-(low|medium|high|xhigh|max)$/i;
const ORDER = ["low", "medium", "high", "xhigh", "max"];
const HERE = path.dirname(fileURLToPath(import.meta.url));
export const AGY_BRIDGE = path.join(HERE, "agy-mcp-bridge.mjs");
export const AGY_HOOK = path.join(HERE, "agy-hook.mjs");
export const AGY_SHIM = path.join(HERE, "agy-bash-shim.mjs");
export const AGY_FS = path.join(HERE, "komputer-fs.py");
export const agyMcpConfigFile = (home) => path.join(home, ".gemini", "config", "mcp_config.json");
export const agyHooksFile = (home) => path.join(home, ".gemini", "config", "hooks.json");
const agySettingsFile = (home) => path.join(home, ".gemini", "antigravity-cli", "settings.json");

const shq = (s) => `'${String(s).replace(/'/g, `'\\''`)}'`;

/** The "komputer" entry of ~/.gemini/config/hooks.json: agy-hook.mjs before every tool call. */
export const hookEntry = (node = process.execPath) => ({
  PreToolUse: [{ matcher: ".*", hooks: [{ type: "command", command: `${shq(node)} ${shq(AGY_HOOK)}`, timeout: 10 }] }],
});
const isHook = (entry) => JSON.stringify(entry) === JSON.stringify(hookEntry());

/** The `bash` agy finds first on PATH in remote turns; anything else runs the real bash. */
export const bashWrapper = (realBash, node = process.execPath) =>
  `#!/bin/sh\nif [ -n "$KOMPUTER_AGY_MCP_FILE" ]; then KOMPUTER_AGY_REAL_BASH=${shq(realBash)} exec ${shq(node)} ${shq(AGY_SHIM)} "$@"; fi\nexec ${shq(realBash)} "$@"\n`;

function findOnPath(name, skip = []) {
  for (const dir of String(process.env.PATH || "").split(path.delimiter)) {
    if (!dir || skip.includes(path.resolve(dir))) continue;
    const p = path.join(dir, name);
    if (existsSync(p)) return p;
  }
  return null;
}

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

async function readJson(file) {
  try {
    return { ok: true, value: JSON.parse(await fs.readFile(file, "utf8")) };
  } catch (e) {
    return { ok: e.code === "ENOENT", value: null };
  }
}

function runAgy(bin, args, env) {
  const viaNode = /\.(c|m)?js$/i.test(bin);
  return new Promise((resolve) => {
    execFile(viaNode ? process.execPath : bin, viaNode ? [bin, ...args] : args, { env, timeout: 30_000 }, (err, stdout, stderr) =>
      resolve({ ok: !err, out: `${stdout}${stderr}`.trim() }));
  });
}

const execOk = (cmd, args) =>
  new Promise((resolve) => execFile(cmd, args, { timeout: 15_000 }, (err) => resolve(!err)));

/** What this host lacks for the FUSE mount of the Body's files (empty: ready). */
export async function fuseMissing(python) {
  const missing = [];
  if (!existsSync("/dev/fuse")) missing.push("/dev/fuse");
  if (!findOnPath("fusermount3") && !findOnPath("fusermount")) missing.push("fusermount3 (package fuse3)");
  // python-fuse is also imported as "fuse"; fusepy is the one with fuse.FUSE.
  if (!(await execOk(python, ["-c", "try:\n import fusepy\nexcept ImportError:\n from fuse import FUSE"]))) missing.push(`fusepy for ${python} (package python3-fusepy)`);
  return missing;
}

const unmountLazy = (dir) =>
  new Promise((resolve) => {
    const tool = findOnPath("fusermount3") ? "fusermount3" : "fusermount";
    execFile(tool, ["-u", "-z", dir], { timeout: 10_000 }, () => resolve());
  });

/**
 * One FUSE mount of the Body root (komputer-fs.py), shared by every remote turn.
 * Started on first use, restarted when the Body address, token or mount directory
 * changes, and retried at most every 30 s after a failure. The daemon unmounts when
 * its stdin closes, so the mount goes away with this process.
 */
function fuseMount({ python, configFile, log }) {
  let child = null;
  let key = "";
  let pending = null;
  let failedAt = 0;
  let current = null;
  const alive = () => child && child.exitCode === null && child.signalCode === null;
  process.once("exit", () => child?.kill("SIGTERM"));

  async function start(server, dir) {
    if (alive()) {
      const old = child;
      await new Promise((r) => {
        old.once("exit", r);
        old.kill("SIGTERM");
      });
    }
    if (current && current !== dir) await unmountLazy(current);
    current = dir;
    // A mount left behind by a crashed run reports "Transport endpoint is not connected".
    await unmountLazy(dir);
    await fs.mkdir(dir, { recursive: true });
    await fs.writeFile(configFile, JSON.stringify({ url: server.url, headers: server.headers || {} }), { mode: 0o600 });
    const c = spawn(python, [AGY_FS, dir, configFile], { stdio: ["pipe", "pipe", "pipe"] });
    child = c;
    let err = "";
    c.stderr.on("data", (d) => (err = (err + d).slice(-2000)));
    c.stdin.on("error", () => {});
    const ok = await new Promise((resolve) => {
      const timer = setTimeout(() => resolve(false), 15_000);
      const done = (v) => {
        clearTimeout(timer);
        resolve(v);
      };
      c.stdout.on("data", (d) => String(d).includes("mounted") && done(true));
      c.once("exit", () => done(false));
      c.once("error", (e) => {
        err += e.message;
        done(false);
      });
    });
    const reason = () => err.trim().split("\n").slice(-2).join(" ").slice(-300);
    if (!ok) {
      c.kill("SIGTERM");
      throw new Error(reason() || "the mount did not come up within 15 s");
    }
    c.once("exit", (code, sig) => {
      if (child === c) child = null;
      log(`agy remote files: the mount at ${dir} ended (${sig || `exit ${code}`}) ${reason()}`.trim());
    });
  }

  return {
    get dir() {
      return current;
    },
    /** The mount directory, or null while the Body's files cannot be mounted. */
    async ensure(server, dir) {
      const k = JSON.stringify([server.url, server.headers || {}, dir]);
      if (k === key && alive() && !pending) return dir;
      if (k === key && pending) return pending;
      if (k === key && Date.now() - failedAt < 30_000) return null;
      key = k;
      pending = start(server, dir).then(
        () => {
          log(`agy remote files: the Body root is mounted at ${dir}`);
          return dir;
        },
        (e) => {
          failedAt = Date.now();
          log(`agy remote files: could not mount the Body root at ${dir}: ${e.message}`);
          return null;
        }
      );
      try {
        return await pending;
      } finally {
        pending = null;
      }
    },
    /** Unmounts (the daemon exits on SIGTERM), waiting at most 3 s. */
    stop() {
      const c = child;
      if (!alive()) return Promise.resolve();
      return new Promise((resolve) => {
        const timer = setTimeout(resolve, 3_000);
        c.once("exit", () => {
          clearTimeout(timer);
          resolve();
        });
        c.kill("SIGTERM");
      });
    },
  };
}

const bodyInfoCache = new Map();
/** The Body's root, platform, OS and shells from system_info (cached; nulls while it is unreachable). */
async function bodyInfo(server) {
  const key = JSON.stringify([server.url, server.headers || {}]);
  const hit = bodyInfoCache.get(key);
  if (hit && Date.now() - hit.at < (hit.value.root ? 300_000 : 15_000)) return hit.value;
  let value = { root: null, platform: null, shells: [] };
  try {
    const r = await mcpHttpClient(server, { timeoutMs: 5_000 }).call("system_info", {});
    const str = (v) => (typeof v === "string" ? v : null);
    value = { root: str(r?.cwdRoot?.path), platform: str(r?.platform), osVersion: str(r?.osVersion), release: str(r?.release), arch: str(r?.arch),
      // Bodies before 1.3 do not list their shells; Windows always has these two.
      shells: Array.isArray(r?.availableShells) ? r.availableShells : r?.platform === "win32" ? ["cmd", "powershell"] : [] };
  } catch {
    /* the turn itself reports an unreachable Body */
  }
  bodyInfoCache.set(key, { at: Date.now(), value });
  return value;
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
  // agy's input_tokens already excludes cache reads (total_tokens = input + output).
  return {
    input_tokens: Number(u.input_tokens) || 0,
    cache_read_input_tokens: Number(u.cache_read_tokens) || 0,
    output_tokens: Number(u.output_tokens) || 0,
    reasoning_tokens: Number(u.thinking_tokens) || 0,
  };
}

/**
 * agy tools that neither touch the host it runs on nor reach the network.
 * Everything else in init.tools (run_command, view_file, list_dir, browser_*,
 * search_web, subagents, ...) acts on the service host unless the turn
 * redirects it to the user's computer.
 */
export const AGY_HOST_FREE_TOOLS = AGY_HOST_FREE;

function toolName(st) {
  const info = st.tool_info || {};
  const p = info.parameters || {};
  const server = p.ServerName ?? p.server_name ?? p.serverName;
  const tool = p.ToolName ?? p.tool_name ?? p.toolName;
  if ((st.tool_name || info.name) === "call_mcp_tool" && server && tool) return `mcp__${server}__${tool}`;
  return st.tool_name || info.name || "tool";
}

let warnedAlwaysProceed = false;
const squash = (s) => String(s).replace(/\s+/g, " ").trim();
const object = (v) => v && typeof v === "object" && !Array.isArray(v);

/**
 * Parser for one agy run; unknown events are ignored. bail() aborts the child before it does more work.
 * options.policy: the turn's hook policy (see agy-hook.mjs; options.island is { mode: "island" });
 * options.guard: "stop" | "warn" | "off" for a tool step the policy denies that ran anyway;
 * options.hooked: the PreToolUse hook enforces the policy, so a denied step ends in ERROR, not DONE.
 */
export function agyParser({ session, hooks, bail, options = {}, log = () => {} }) {
  const { onText, onToolUse } = hooks;
  const requested = session.mode === "resume" ? session.id : null;
  const policy = options.policy || (options.island ? { mode: "island" } : null);
  const guard = policy && policy.mode !== "local" ? options.guard || "warn" : "off";
  let conversation = requested;
  let result = null;
  let streamed = "";
  let lastTextStep = null;
  let afterTool = false;
  let activity = false;
  let alwaysProceed = false;
  const announced = new Set();
  const flagged = new Set();
  const hostTool = (st, name) => {
    if (guard === "off" || flagged.has(st.step_index)) return;
    // Under always-proceed an ACTIVE step is already running, unless the hook still gets to deny it.
    if (st.state !== "DONE" && !(alwaysProceed && !options.hooked)) return;
    const raw = st.tool_name || st.tool_info?.name || "";
    const call = { toolCall: { name: raw, args: st.tool_info?.parameters || {} }, conversationId: st.conversation_id || conversation };
    if (decide(call, policy)?.decision !== "deny") return;
    flagged.add(st.step_index);
    const message = `agy ran its built-in tool ${name} on the service host (permission_mode ${alwaysProceed ? "always-proceed" : "unknown"})`;
    if (guard === "stop") bail(message, "builtin_tool_blocked");
    else log(`WARNING: ${message}`);
  };
  return {
    get activity() {
      return activity;
    },
    line(line) {
      const ev = jsonLine(line);
      if (!ev) return;
      if (ev.event === "init") {
        const init = object(ev.init) ? ev.init : {};
        const id = ev.conversation_id || init.conversation_id || null;
        // agy starts a fresh conversation when --conversation is unknown.
        if (requested && id && id !== requested) {
          bail(`conversation "${requested}" not found (agy started ${id})`, "session_not_found");
          return;
        }
        if (id) conversation = id;
        alwaysProceed = init.permission_mode === "always-proceed";
        if (alwaysProceed && !options.skip && !warnedAlwaysProceed) {
          warnedAlwaysProceed = true;
          log("WARNING: agy reports permission_mode always-proceed without --dangerously-skip-permissions: its built-in tools run on this host without asking");
        }
        return;
      }
      if (ev.event === "step_update") {
        // agy 1.4.3 nests the step under step_update; accept the flat shape too.
        const st = object(ev.step_update) ? ev.step_update : ev;
        if (st.step_type === "agent_response" || st.step_type === "tool") activity = true;
        if (st.step_type === "agent_response" && typeof st.text_delta === "string" && st.text_delta) {
          if (streamed && (afterTool || (lastTextStep !== null && st.step_index !== lastTextStep))) {
            streamed += "\n\n";
            onText?.("\n\n");
          }
          afterTool = false;
          lastTextStep = st.step_index;
          streamed += st.text_delta;
          onText?.(st.text_delta);
        } else if (st.step_type === "tool") {
          const name = toolName(st);
          const key = st.step_index ?? announced.size;
          if (!announced.has(key)) {
            announced.add(key);
            afterTool = true;
            onToolUse?.(name);
          }
          hostTool(st, name);
        }
        return;
      }
      if (ev.event === "result" && object(ev.result)) {
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

const ISLAND = { mode: "island" };
const LOCAL = { mode: "local" };

/**
 * @param {{bin: string, cwd: string, mcpConfig: string, bodyMcp: string, modelId: string, models: string[], defaultModel: string,
 *          defaultEffort: string, efforts: string[], taskModel: string, permissions: "settings"|"skip", mcpSetup: "auto"|"off",
 *          builtinGuard: "stop"|"warn"|"off", remoteTools: "auto"|"all"|"commands"|"off", remoteShell: string, remoteMount: string,
 *          posixFallback: "auto"|"bash"|"translate"|"off", python: string, extraArgs: string[], log?: (msg: string) => void}} cfg
 */
export function antigravityBackend(cfg) {
  const ids = [cfg.modelId, ...cfg.models.filter((m) => m !== cfg.modelId)];
  const efforts = cfg.efforts.length ? cfg.efforts : ["low", "medium", "high"];
  const log = cfg.log || (() => {});
  const bodyDir = path.join(cfg.cwd, "body");
  const plainDir = path.join(cfg.cwd, "plain");
  const targetDir = path.join(cfg.cwd, "mcp");
  const shimDir = path.join(cfg.cwd, "shim");
  const remoteTools = cfg.remoteTools || "off";
  const mountBase = cfg.remoteMount || path.join(cfg.cwd, "remote");
  const mount = fuseMount({ python: cfg.python || "python3", configFile: path.join(targetDir, "komputer-fs.json"), log });
  /** Windows roots mount at <base>/c/Users/..., so agy's paths read like WSL's /mnt/c/... */
  const mountDirFor = (info) => (info.platform === "win32" && isWindowsPath(info.root) ? windowsMountDir(mountBase, info.root) : mountBase);
  const shellFor = (info) => cfg.remoteShell || (info.platform === "win32" ? (info.shells.includes("pwsh") ? "pwsh" : "powershell") : "bash");
  let files = remoteTools === "all";
  let fuseGaps = [];
  let hooked = false;
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
      if (remoteTools !== "off") {
        await fs.mkdir(shimDir, { recursive: true });
        const realBash = findOnPath("bash", [path.resolve(shimDir)]) || "/bin/bash";
        const wrapper = path.join(shimDir, "bash");
        await fs.writeFile(wrapper, bashWrapper(realBash), { mode: 0o755 });
        await fs.chmod(wrapper, 0o755);
        if (remoteTools !== "commands") fuseGaps = await fuseMissing(cfg.python || "python3");
        files = remoteTools === "all" || (remoteTools === "auto" && !fuseGaps.length);
      }
      const home = os.homedir();
      const hooksFile = agyHooksFile(home);
      const hooks = await readJson(hooksFile);
      hooked = isHook(hooks.value?.komputer);
      if (cfg.mcpSetup !== "auto") return;
      if (!hooked && hooks.ok) {
        await fs.mkdir(path.dirname(hooksFile), { recursive: true });
        await fs.writeFile(hooksFile, JSON.stringify({ ...(hooks.value || {}), komputer: hookEntry() }, null, 2) + "\n");
        hooked = true;
        setupNotes.push(`agy hooks: registered the "komputer" PreToolUse hook in ${agyHooksFile("~")} (keeps agy's own tools on the user's computer or off)`);
      }
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
    async prepare({ session, prompt, attachments = [], systemPrompt, includeSystem = true, model, effort, tools, mcp, mcpServer, workDir }) {
      const { text, binary } = splitAttachments(attachments);
      if (binary.length) throw backendError(400, "This model does not accept image or PDF attachments", "invalid_request_error");
      let cwd;
      let servers = [];
      // Harness and task turns never use agy's own tools; Body turns use them on the user's computer
      // when remote tools are on, or on this host when KOMPUTER_BUILTIN_TOOLS asks for that.
      let policy = ISLAND;
      let remote = null;
      let note = "";
      if (mcpServer) {
        cwd = path.join(workDir, "agy");
        servers = [mcpServer];
      } else if (mcp === false) {
        cwd = plainDir;
      } else {
        cwd = bodyDir;
        servers = await readMcpServers(cfg.mcpConfig);
        const target = servers.find((s) => s.name === cfg.bodyMcp && s.url);
        if (tools) policy = LOCAL;
        else if (remoteTools !== "off" && target) {
          const server = { url: target.url, headers: target.headers || {} };
          const info = await bodyInfo(server);
          const mounted = files && info.root ? await mount.ensure(server, mountDirFor(info)) : null;
          if (remoteTools === "all" && !mounted) {
            throw Object.assign(backendError(503, "The computer is unavailable. Please retry later.", "body_unavailable"),
              { detail: "agy remote files: the Body root is not mounted" });
          }
          cwd = mounted || bodyDir;
          await fs.mkdir(cwd, { recursive: true });
          const workspace = await fs.realpath(cwd).catch(() => cwd);
          const shell = shellFor(info);
          const bash = info.shells.includes("bash");
          remote = { server, mount: workspace, root: info.root, platform: info.platform, shell, posixFallback: cfg.posixFallback || "auto", bash };
          policy = { mode: "remote", mount: workspace, root: info.root, platform: info.platform, files: Boolean(mounted), commands: true };
          note = environmentNote({ ...info, mount: workspace, shell, files: Boolean(mounted), posixFallback: remote.posixFallback, bash });
        }
      }
      await fs.mkdir(cwd, { recursive: true });
      const body = promptWithText(prompt, text);
      // Every remote turn: it corrects what agy's own system prompt says about this host, and survives compaction.
      const turnText = [note, includeSystem ? inlineSystem(systemPrompt, body) : body].filter(Boolean).join("\n\n");
      const prepared = {
        args: agyArgs({ session, model, effort, permissions: cfg.permissions, extraArgs: cfg.extraArgs }),
        stdin: agyUserEvent(turnText),
        cwd,
        parserOptions: { policy, guard: cfg.builtinGuard, skip: cfg.permissions === "skip", hooked },
      };
      if (!servers.length && policy === LOCAL) return prepared;
      // Outside agy's workspace, so its file tools do not list it next to the user's work.
      const file = path.join(mcpServer ? workDir : targetDir, `agy-mcp-${randomUUID()}.json`);
      await fs.writeFile(file, JSON.stringify({ ...agyBridgeTargets(servers), policy, ...(remote ? { remote } : {}) }), { mode: 0o600 });
      const env = { KOMPUTER_AGY_MCP_FILE: file, ...(remote ? { PATH: [shimDir, process.env.PATH].filter(Boolean).join(path.delimiter) } : {}) };
      return { ...prepared, env, cleanup: () => fs.rm(file, { force: true }).catch(() => {}) };
    },
    close() {
      return mount.stop();
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
      const hooks = await readJson(agyHooksFile(home));
      const hookOk = isHook(hooks.value?.komputer);
      if (!hookOk) {
        notes.push(`agy hooks: the "komputer" PreToolUse hook is missing from ~/.gemini/config/hooks.json, so nothing stops agy's own tools ` +
          `from acting on this host; add: "komputer": ${JSON.stringify(hookEntry())}`);
      }
      if (remoteTools !== "off") {
        notes.push(`agy remote tools (${remoteTools}): run_command runs on the user's computer through ${path.join(shimDir, "bash")}; ` +
          (files ? `file tools use the Body root mounted at ${mount.dir || `${mountBase} (a Windows root such as C:\\Users\\me mounts at ${path.join(mountBase, "c", "Users", "me")})`}` : `file tools are denied (the model uses run_command instead)${fuseGaps.length ? `; for the mount install: ${fuseGaps.join(", ")}` : ""}`));
        if (files && !existsSync("/etc/mtab")) notes.push("agy remote files: /etc/mtab is missing, so fusermount3 cannot unmount; link it: ln -s /proc/self/mounts /etc/mtab");
      }
      if (cfg.permissions === "skip") {
        notes.push(hookOk
          ? "KOMPUTER_AGY_PERMISSIONS=skip passes --dangerously-skip-permissions: agy's own tools run unprompted, limited by the komputer hook"
          : "KOMPUTER_AGY_PERMISSIONS=skip passes --dangerously-skip-permissions: agy's built-in shell and file tools run unprompted on this host");
        return notes;
      }
      const listed = (await readJson(agySettingsFile(home))).value?.permissions?.allow;
      const allow = Array.isArray(listed) ? listed : [];
      if (remoteTools !== "off" && !allow.some((r) => String(r) === "command(*)")) {
        notes.push(`agy permissions: add "command(*)" to permissions.allow in ~/.gemini/antigravity-cli/settings.json (or set KOMPUTER_AGY_PERMISSIONS=skip), ` +
          "otherwise headless agy denies run_command and remote commands never reach the user's computer");
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
