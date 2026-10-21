#!/usr/bin/env node
// PreToolUse hook the service registers in agy's global ~/.gemini/config/hooks.json.
//
// agy runs it before every tool call (stdin: {toolCall:{name,args}, stepIdx,
// conversationId, workspacePaths, artifactDirectoryPath, ...}; stdout:
// {decision, reason, overwrite}), verified with agy 1.4.3. It only has an
// opinion inside the service's own turns, which name a policy in the file
// that KOMPUTER_AGY_MCP_FILE points to; anywhere else it prints nothing and
// agy's own permission settings decide. Verified: empty output is "no
// opinion", while {} without a decision counts as a denial, and "allow" does
// not lift a headless permission denial (it is only used with overwrite).
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { isWindowsPath, toCrlf, windowsToMount } from "./windows.mjs";

/** Tools that act inside agy itself, on MCP servers, or on nothing at all. */
export const AGY_HOST_FREE = new Set(["call_mcp_tool", "ask_question", "ask_permission", "ask_custom_permission", "list_permissions",
  "finish", "wait", "wait_5_seconds", "manage_task", "list_resources", "read_resource"]);
/** run_command and its follow-ups run through the bash shim. */
export const AGY_COMMAND_TOOLS = new Set(["run_command", "command_status", "send_command_input"]);
/** Tools that read or write files in-process (both the signed-in and the Gemini API tool sets). */
export const AGY_FILE_TOOLS = new Set(["view_file", "read_file", "list_dir", "grep_search", "find_by_name", "write_to_file", "write_file",
  "replace_file_content", "multi_replace_file_content", "edit_file", "sed_file", "notebook_edit"]);
/** Tools that stay on the service host however the turn is set up. */
const HOST_ONLY = /^(browser_|capture_browser_|click_browser_|execute_browser_|list_browser_|open_browser_|read_browser_|notebook_execution$)/;
/** Agent-internal and network-only tools that are fine in a remote turn. */
const REMOTE_OK = new Set(["search_web", "read_url_content", "generate_image", "invoke_subagent", "define_subagent", "manage_subagents",
  "send_message", "schedule", "run_workflow", "manage_inbox", "delete_knowledge", "search_marketplace", "list_plugin_accounts"]);

const PATH_KEY = /(path|file|dir|directory|cwd|folder)$/i;

function pathArgs(args) {
  const out = [];
  for (const [k, v] of Object.entries(args || {})) {
    if (!PATH_KEY.test(k)) continue;
    if (typeof v === "string") out.push([k, v]);
    else if (Array.isArray(v) && v.every((x) => typeof x === "string")) out.push([k, v]);
  }
  return out;
}

const within = (p, dir) => p === dir || p.startsWith(dir.endsWith(path.sep) ? dir : dir + path.sep);
const expand = (p, home) => (p === "~" ? home : p.startsWith("~/") ? path.join(home, p.slice(2)) : p);

const ownDirs = (input, home) =>
  [input?.artifactDirectoryPath, input?.conversationId && path.join(home, ".gemini", "antigravity-cli", "brain", input.conversationId)]
    .filter(Boolean).map((d) => path.resolve(d));

/**
 * Decide one PreToolUse call (null: no opinion). policy.mode: "remote" (built-ins run on the
 * user's computer), "island" (only MCP and agy-internal tools), or "local" (no opinion).
 * A remote policy has mount (agy's workspace, standing for the Body root), root (the Body
 * root's own path), platform (the Body's), files (mount is the Body's files over FUSE) and
 * commands (the bash shim). On Windows, C:\... paths inside the root are rewritten to the mount.
 */
export function decide(input, policy, { home = os.homedir() } = {}) {
  const name = String(input?.toolCall?.name || "");
  const args = input?.toolCall?.args || {};
  if (!policy || policy.mode === "local" || AGY_HOST_FREE.has(name)) return null;
  const deny = (reason) => ({ decision: "deny", reason });
  if (policy.mode !== "remote") {
    return deny(`${name} is not available in this conversation: agy's built-in tools would act on the service host. Use the provided tools instead.`);
  }
  if (HOST_ONLY.test(name)) return deny(`${name} would run on the service host, not on the user's computer, so it is not available here.`);
  if (AGY_COMMAND_TOOLS.has(name)) return policy.commands === false ? deny(`${name} is not available here.`) : checkPaths(name, args, policy, input, home);
  if (AGY_FILE_TOOLS.has(name)) {
    if (policy.files) return checkPaths(name, args, policy, input, home);
    // agy's own artifacts (plans, task lists) stay on the service host, where agy reads them back.
    const own = ownDirs(input, home);
    const values = pathArgs(args).flatMap(([, v]) => [].concat(v));
    if (values.length && values.every((v) => (v.startsWith("/") || v.startsWith("~")) && own.some((d) => within(path.resolve(expand(v, home)), d)))) return null;
    return deny(`The user's files are on their computer, which ${name} cannot reach here. Use run_command (it runs on the user's computer), ` +
      (policy.platform === "win32" ? "for example Get-Content, Get-ChildItem or Set-Content, or the komputer_use tools." : "for example cat, ls, sed or a here-document, or the komputer_use tools."));
  }
  if (REMOTE_OK.has(name)) return null;
  return deny(`${name} is not available in this conversation.`);
}

/** Tools whose Target / TargetContent must match the file's text exactly. */
const EDIT_TOOLS = new Set(["edit_file", "replace_file_content", "multi_replace_file_content"]);
const WRITE_TOOLS = new Set(["write_file", "write_to_file"]);
const EDIT_PAIRS = [["Target", "Replacement"], ["TargetContent", "ReplacementContent"]];
const CONTENT_KEYS = ["Content", "CodeContent"];
const MAX_EOL_PROBE = 4 * 1024 * 1024;

function readText(file) {
  try {
    const st = fs.statSync(file);
    return st.isFile() && st.size <= MAX_EOL_PROBE ? fs.readFileSync(file, "utf8") : null;
  } catch {
    return null;
  }
}

/**
 * Line endings for edits of CRLF files: the model writes \n, and agy's edit tools match
 * byte for byte, so an LF Target misses a CRLF file (verified with agy 1.4.3). When only
 * the CRLF form of a Target is in the file, Target and Replacement become CRLF; a whole-file
 * write over a CRLF-only file keeps CRLF. Returns the arguments to overwrite.
 */
export function lineEndingFixes(name, args, file) {
  if (!file || !(EDIT_TOOLS.has(name) || WRITE_TOOLS.has(name))) return {};
  const text = readText(file);
  if (text === null || !text.includes("\r\n")) return {};
  const out = {};
  if (WRITE_TOOLS.has(name)) {
    if (/(^|[^\r])\n/.test(text)) return {};
    for (const k of CONTENT_KEYS) if (typeof args[k] === "string" && /(^|[^\r])\n/.test(args[k])) out[k] = toCrlf(args[k]);
    return out;
  }
  const fixPair = (obj) => {
    let changed = null;
    for (const [t, r] of EDIT_PAIRS) {
      if (typeof obj?.[t] !== "string" || !obj[t].includes("\n") || text.includes(obj[t])) continue;
      const target = toCrlf(obj[t]);
      if (!text.includes(target)) continue;
      changed = { ...(changed || obj), [t]: target, ...(typeof obj[r] === "string" ? { [r]: toCrlf(obj[r]) } : {}) };
    }
    return changed;
  };
  const top = fixPair(args);
  if (top) for (const [t, r] of EDIT_PAIRS) for (const k of [t, r]) if (top[k] !== args[k]) out[k] = top[k];
  for (const [k, v] of Object.entries(args)) {
    if (!Array.isArray(v) || !v.every((x) => x && typeof x === "object")) continue;
    const mapped = v.map((x) => fixPair(x) || x);
    if (mapped.some((x, i) => x !== v[i])) out[k] = mapped;
  }
  return out;
}

function checkPaths(name, args, policy, input, home) {
  const own = ownDirs(input, home);
  const overwrite = {};
  const windows = policy.platform === "win32" && isWindowsPath(policy.root);
  const fix = (raw) => {
    if (!raw) return raw;
    if (windows && isWindowsPath(raw)) return windowsToMount(raw, policy);
    // A relative Windows path (src\app.js): agy would take the backslashes as part of a file name.
    if (windows && policy.mount && !raw.startsWith("/") && !raw.startsWith("~") && raw.includes("\\")) return path.join(policy.mount, ...raw.split("\\"));
    if (!(raw.startsWith("/") || raw.startsWith("~"))) return raw;
    const p = path.resolve(expand(raw, home));
    if (policy.mount && within(p, policy.mount)) return p === raw ? raw : p;
    if (own.some((d) => within(p, d))) return raw;
    // A path the model saw in command output: the same file under the mount.
    if (policy.mount && policy.root?.startsWith("/") && within(p, policy.root)) return path.join(policy.mount, path.relative(policy.root, p));
    return null;
  };
  for (const [k, v] of pathArgs(args)) {
    const values = [].concat(v);
    const fixed = values.map(fix);
    const bad = values.find((_, i) => fixed[i] === null);
    if (bad !== undefined) {
      if (windows && isWindowsPath(bad)) {
        return { decision: "deny", reason: `${bad} is outside the user's workspace ${policy.root}; ${name} can only use paths inside it. ` +
          "run_command reaches the rest of the user's computer." };
      }
      return { decision: "deny", reason: `${bad} is on the service host, outside the user's workspace at ${policy.mount}; ${name} can only use paths inside it.` };
    }
    if (fixed.some((f, i) => f !== values[i])) overwrite[k] = Array.isArray(v) ? fixed : fixed[0];
  }
  if (policy.files) {
    const file = pathArgs({ ...args, ...overwrite }).map(([, v]) => v).find((v) => typeof v === "string" && v.startsWith("/"));
    Object.assign(overwrite, lineEndingFixes(name, args, file));
  }
  return Object.keys(overwrite).length ? { decision: "allow", overwrite } : null;
}

function loadPolicy() {
  const file = process.env.KOMPUTER_AGY_MCP_FILE;
  if (!file) return null;
  try {
    return JSON.parse(fs.readFileSync(file, "utf8"))?.policy || null;
  } catch {
    return null;
  }
}

const isMain = process.argv[1] && path.resolve(process.argv[1]) === path.resolve(new URL(import.meta.url).pathname);
if (isMain) {
  let raw = "";
  process.stdin.setEncoding("utf8");
  process.stdin.on("data", (d) => (raw += d));
  process.stdin.on("end", () => {
    let input = null;
    try {
      input = JSON.parse(raw);
    } catch {
      /* not a hook payload */
    }
    const policy = loadPolicy();
    const verdict = input && policy ? decide(input, policy) : null;
    if (verdict) process.stdout.write(JSON.stringify(verdict));
  });
}
