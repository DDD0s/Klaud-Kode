#!/usr/bin/env node
// `bash` for agy turns whose commands belong on the user's computer.
//
// agy 1.4.3 runs every run_command as `bash -c <CommandLine>`, looking bash
// up on PATH (verified with the real binary). The service puts a directory
// holding a `bash` wrapper first on agy's PATH; the wrapper runs this file
// when the turn has a remote target, so the command runs on the Body instead:
// started with start_process, streamed back with read_process, stdin passed
// through write_process, stopped with stop_process when agy terminates it.
// Anything that is not `bash [-opts] -c <command>` runs the real bash.
import fs from "node:fs";
import path from "node:path";
import { spawnSync } from "node:child_process";
import { mcpHttpClient } from "./mcp-call.mjs";

const SIGNALS = { SIGHUP: 1, SIGINT: 2, SIGQUIT: 3, SIGKILL: 9, SIGTERM: 15 };

/** The command of `bash [options] -c <command> [name [args...]]`, or null for any other invocation. */
export function commandOf(argv) {
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a === "--") return null;
    if (/^-[A-Za-z]*c[A-Za-z]*$/.test(a)) return i + 1 < argv.length ? argv[i + 1] : null;
    if (/^(--[a-z-]+|[-+][A-Za-z]+)$/.test(a)) {
      if (a === "-o" || a === "+o" || a === "-O" || a === "+O" || a === "--rcfile" || a === "--init-file") i++;
      continue;
    }
    return null;
  }
  return null;
}

const posixRoot = (root) => typeof root === "string" && root.startsWith("/");

/** Rewrite mount paths in a command into the user's paths (POSIX roots only). */
export function toRemote(text, remote) {
  if (!remote.mount || !posixRoot(remote.root) || remote.mount === remote.root) return text;
  return text.split(remote.mount).join(remote.root);
}

/** Directory on the Body, relative to its root, for a local working directory. */
export function remoteCwd(local, remote) {
  if (!remote.mount) return ".";
  const rel = path.relative(remote.mount, local);
  if (!rel) return ".";
  if (rel.startsWith("..") || path.isAbsolute(rel)) return ".";
  return rel.split(path.sep).join("/");
}

/** Streams remote output back with the user's root shown as the mount, across chunk boundaries. */
export function outputMapper(remote) {
  if (!remote.mount || !posixRoot(remote.root) || remote.mount === remote.root) return { push: (s) => s, end: () => "" };
  const from = remote.root;
  let carry = "";
  return {
    push(s) {
      const text = carry + s;
      let cut = text.length;
      for (let k = Math.min(from.length - 1, text.length); k > 0; k--) {
        if (from.startsWith(text.slice(-k))) {
          cut = text.length - k;
          break;
        }
      }
      carry = text.slice(cut);
      return text.slice(0, cut).split(from).join(remote.mount);
    },
    end() {
      const rest = carry;
      carry = "";
      return rest.split(from).join(remote.mount);
    },
  };
}

function loadRemote() {
  const file = process.env.KOMPUTER_AGY_MCP_FILE;
  if (!file) return null;
  try {
    return JSON.parse(fs.readFileSync(file, "utf8"))?.remote || null;
  } catch {
    return null;
  }
}

function passthrough(argv) {
  const real = process.env.KOMPUTER_AGY_REAL_BASH || "/bin/bash";
  const r = spawnSync(real, argv, { stdio: "inherit" });
  if (r.error) {
    process.stderr.write(`bash: ${r.error.message}\n`);
    process.exit(127);
  }
  process.exit(r.status ?? 128 + (SIGNALS[r.signal] || 1));
}

const exitCodeOf = (r) => (Number.isInteger(r.exitCode) ? r.exitCode : r.signal ? 128 + (SIGNALS[r.signal] || 1) : 1);

async function run(command, remote) {
  const client = mcpHttpClient(remote.server);
  const shell = remote.shell || "bash";
  const cwd = remoteCwd(process.cwd(), remote);
  const text = toRemote(command, remote);
  const out = outputMapper(remote);
  const err = outputMapper(remote);
  let started;
  try {
    started = await client.call("start_process", { command: text, shell, cwd });
  } catch (e) {
    if (e.toolError || /not found|unknown tool/i.test(e.message)) return runOnce(client, { command: text, shell, cwd }, remote);
    throw e;
  }
  const id = started.processId;
  let stopping = false;
  const stop = (sig) => {
    if (stopping) return;
    stopping = true;
    client.call("stop_process", { processId: id }).catch(() => {}).finally(() => process.exit(128 + (SIGNALS[sig] || 15)));
  };
  for (const sig of ["SIGTERM", "SIGINT", "SIGHUP"]) process.on(sig, () => stop(sig));
  process.stdin.on("data", (d) => {
    client.call("write_process", { processId: id, input: d.toString("utf8") }).catch(() => {});
  });
  process.stdin.on("error", () => {});
  let cursors = { stdoutCursor: 0, stderrCursor: 0 };
  let delay = 100;
  for (;;) {
    const r = await client.call("read_process", { processId: id, ...cursors });
    if (r.stdout) process.stdout.write(out.push(r.stdout));
    if (r.stderr) process.stderr.write(err.push(r.stderr));
    cursors = { stdoutCursor: r.stdoutCursor, stderrCursor: r.stderrCursor };
    if (!r.running) {
      process.stdout.write(out.end());
      process.stderr.write(err.end());
      return exitCodeOf(r);
    }
    delay = r.stdout || r.stderr ? 100 : Math.min(delay * 1.5, 500);
    await new Promise((res) => setTimeout(res, delay));
  }
}

async function runOnce(client, args, remote) {
  const r = await client.call("run_command", { ...args, ...(remote.timeoutMs ? { timeoutMs: remote.timeoutMs } : {}) });
  const out = outputMapper(remote);
  const err = outputMapper(remote);
  process.stdout.write(out.push(r.stdout || "") + out.end());
  process.stderr.write(err.push(r.stderr || "") + err.end());
  if (r.timedOut) process.stderr.write("\n[timed out on the user's computer]\n");
  return exitCodeOf(r);
}

const isMain = process.argv[1] && path.resolve(process.argv[1]) === path.resolve(new URL(import.meta.url).pathname);
if (isMain) {
  const argv = process.argv.slice(2);
  const remote = loadRemote();
  const command = commandOf(argv);
  if (!remote?.server?.url || command === null) passthrough(argv);
  else {
    run(command, remote).then(
      (code) => process.exit(code),
      (e) => {
        process.stderr.write(`bash: could not run the command on the user's computer: ${e.message}\n`);
        process.exit(126);
      }
    );
  }
}
