import { spawn } from "node:child_process";
import { once } from "node:events";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
export const KEY = "h".repeat(40);
export const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
export async function apiFixture(t, { env: extra = {}, prepare = () => {} } = {}) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "komputer-harness-test-"));
  fs.mkdirSync(path.join(dir, "state"));
  fs.mkdirSync(path.join(dir, "home"));
  fs.writeFileSync(path.join(dir, "KEYS.txt"), `api-key: ${KEY}\n`);
  await prepare(dir);
  const env = { ...process.env };
  for (const key of Object.keys(env)) if (/^KOMPUTER_/i.test(key)) delete env[key];
  const homeDir = path.join(dir, "home");
  // A private home keeps CLI config writes (agy mcp add) out of the developer's own.
  // os.homedir() reads HOME on POSIX but USERPROFILE on Windows, so set both.
  const homeEnv = process.platform === "win32"
    ? { HOME: homeDir, USERPROFILE: homeDir, HOMEDRIVE: path.parse(homeDir).root.slice(0, 2), HOMEPATH: homeDir.slice(2) }
    : { HOME: homeDir };
  const child = spawn(process.execPath, [path.join(here, "..", "server.mjs")], {
    env: { ...env, ...homeEnv, KOMPUTER_API_HOST: "127.0.0.1", KOMPUTER_API_PORT: "0",
      KOMPUTER_KEYS_FILE: path.join(dir, "KEYS.txt"), KOMPUTER_SESSIONS_FILE: path.join(dir, "sessions.json"),
      KOMPUTER_SESSION_DIR: dir, KOMPUTER_CLAUDE_CWD: dir, KOMPUTER_AUTH_FAIL_DELAY_MS: "1",
      KOMPUTER_HEALTH_URL: "http://127.0.0.1:9/healthz", KOMPUTER_TUNNEL_UP: path.join(dir, "disabled.sh"),
      CLAUDE_BIN: path.join(here, "fake-claude.mjs"), FAKE_CLAUDE_STATE: path.join(dir, "state"),
      AGY_BIN: path.join(here, "fake-agy.mjs"), FAKE_AGY_STATE: path.join(dir, "state"), KOMPUTER_AGY_CWD: path.join(dir, "agy-ws"),
      MUSE_BIN: path.join(here, "fake-muse.mjs"), FAKE_MUSE_STATE: path.join(dir, "state"), KOMPUTER_MUSE_CWD: path.join(dir, "muse-ws"),
      KOMPUTER_RESPONSES_FILE: path.join(dir, "responses.json"),
      ...Object.fromEntries(Object.entries(extra).map(([k, v]) => [k, typeof v === "function" ? v(dir) : v])),
    }, stdio: ["ignore", "pipe", "pipe"],
  });
  let logs = "", base;
  const runs = new Set();
  child.stdout.on("data", (d) => { logs += d; base = /listening on (https?:\/\/127\.0\.0\.1:\d+)/.exec(logs)?.[1]; });
  child.stderr.on("data", (d) => (logs += d));
  const exited = once(child, "exit");
  t.after(async () => {
    if (child.exitCode === null && child.signalCode === null) {
      if (base?.startsWith("http:")) for (const id of runs) {
        await fetch(`${base}/v1/tool-runs/${id}`, { method: "DELETE", headers: { authorization: `Bearer ${KEY}` }, signal: AbortSignal.timeout(3000) }).then((r) => r.text()).catch(() => {});
      }
      child.kill();
    }
    await exited;
    fs.rmSync(dir, { recursive: true, force: true });
  });
  return {
    dir, child, exited, logs: () => logs, base: () => base,
    async ready() {
      for (let i = 0; i < 200; i++) {
        if (base) return base;
        if (child.exitCode !== null) throw new Error(logs);
        await sleep(25);
      }
      throw new Error(`startup timeout: ${logs}`);
    },
    calls(file = "calls.jsonl") {
      const p = path.join(dir, "state", file);
      return fs.existsSync(p) ? fs.readFileSync(p, "utf8").trim().split("\n").filter(Boolean).map(JSON.parse) : [];
    },
    agyCalls() {
      return this.calls("agy-calls.jsonl");
    },
    museCalls() {
      return this.calls("muse-calls.jsonl");
    },
    async request(endpoint, body, options = {}) {
      const r = await fetch(base + endpoint, {
        method: options.method || (body ? "POST" : "GET"),
        headers: { authorization: `Bearer ${KEY}`, "content-type": "application/json", ...options.headers },
        body: body ? JSON.stringify(body) : undefined, signal: options.signal || AbortSignal.timeout(8000),
      });
      if (r.headers.get("x-komputer-run")) runs.add(r.headers.get("x-komputer-run"));
      const text = await r.text();
      return { status: r.status, headers: r.headers, text, json: () => JSON.parse(text) };
    },
  };
}

const BODY_PKG = path.join(here, "..", "..", "kloud-kode-body");
export const haveBody = fs.existsSync(path.join(BODY_PKG, "node_modules", "@modelcontextprotocol", "sdk"));

/** A real kloud-kode-body on a temporary root, standing in for the user's computer. */
export async function realBody(t) {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "komputer-user-pc-"));
  const root = path.join(tmp, "root");
  fs.mkdirSync(path.join(root, "sub"), { recursive: true });
  fs.writeFileSync(path.join(root, "only-on-the-users-computer.txt"), "hello from the user's computer\n");
  fs.writeFileSync(path.join(root, "sub", "note.md"), "# note\n");
  const token = "u".repeat(48);
  const port = 21000 + Math.floor(Math.random() * 15000);
  fs.writeFileSync(path.join(tmp, "config.json"), JSON.stringify({ port, host: "127.0.0.1", roots: { project: root }, defaultRoot: "project", token }));
  const env = { ...process.env, KLOUD_KODE_BODY_CONFIG: path.join(tmp, "config.json") };
  delete env.KLOUD_KODE_BODY_TOKEN;
  const proc = spawn(process.execPath, [path.join(BODY_PKG, "server.mjs"), "http"], { env, stdio: ["ignore", "pipe", "pipe"] });
  t.after(() => {
    proc.kill();
    fs.rmSync(tmp, { recursive: true, force: true });
  });
  let log = "";
  await new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error(`body did not start: ${log}`)), 10_000);
    const seen = (d) => {
      log += d;
      if (log.includes("/mcp/")) {
        clearTimeout(timer);
        resolve();
      }
    };
    proc.stdout.on("data", seen);
    proc.stderr.on("data", seen);
  });
  const mcpJson = JSON.stringify({ mcpServers: { komputer_use: { type: "http", url: `http://127.0.0.1:${port}/mcp`, headers: { Authorization: `Bearer ${token}` } } } });
  return { root: fs.realpathSync(root), mcpJson };
}
