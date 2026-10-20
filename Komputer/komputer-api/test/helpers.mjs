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
  fs.writeFileSync(path.join(dir, "KEYS.txt"), `api-key: ${KEY}\n`);
  await prepare(dir);
  const env = { ...process.env };
  for (const key of Object.keys(env)) if (/^KOMPUTER_/i.test(key)) delete env[key];
  const child = spawn(process.execPath, [path.join(here, "..", "server.mjs")], {
    env: { ...env, KOMPUTER_API_HOST: "127.0.0.1", KOMPUTER_API_PORT: "0",
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
