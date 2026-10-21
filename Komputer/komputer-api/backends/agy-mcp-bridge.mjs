#!/usr/bin/env node
// stdio MCP server that the service registers in agy's global
// ~/.gemini/config/mcp_config.json (`agy mcp add <name> -- node <this> <name>`).
//
// agy has no per-workspace or per-process MCP configuration, but it starts
// stdio servers with its own environment and cwd. Each agy process the
// service spawns names a private target file in KOMPUTER_AGY_MCP_FILE, so one
// stable global entry reaches the right server for that turn: the Body MCP,
// a harness run's client-tool server, or nothing. Without a target (task
// turns, or someone running agy by hand) it is an MCP server with no tools.
import fs from "node:fs";
import http from "node:http";
import https from "node:https";
import readline from "node:readline";
import { spawn } from "node:child_process";

const name = process.argv[2] || "";
const write = (m) => process.stdout.write(JSON.stringify(m) + "\n");
const fail = (id, message) => write({ jsonrpc: "2.0", id, error: { code: -32603, message: `komputer MCP bridge: ${message}` } });

function loadTarget() {
  const file = process.env.KOMPUTER_AGY_MCP_FILE;
  if (!file) return null;
  try {
    return JSON.parse(fs.readFileSync(file, "utf8"))?.servers?.[name] || null;
  } catch {
    return null;
  }
}

function emptyServer() {
  readline.createInterface({ input: process.stdin }).on("line", (line) => {
    let msg;
    try {
      msg = JSON.parse(line);
    } catch {
      return;
    }
    if (msg?.id === undefined || !msg.method) return;
    if (msg.method === "initialize") {
      write({ jsonrpc: "2.0", id: msg.id, result: { protocolVersion: msg.params?.protocolVersion || "2025-06-18", capabilities: { tools: {} }, serverInfo: { name: "komputer", version: "0" } } });
    } else if (msg.method === "tools/list") write({ jsonrpc: "2.0", id: msg.id, result: { tools: [] } });
    else if (msg.method === "ping") write({ jsonrpc: "2.0", id: msg.id, result: {} });
    else write({ jsonrpc: "2.0", id: msg.id, error: { code: -32601, message: "Method not found" } });
  });
}

function stdioTarget(target) {
  const env = { ...process.env, ...(target.env || {}) };
  delete env.KOMPUTER_AGY_MCP_FILE;
  const child = spawn(target.command, target.args || [], { stdio: ["pipe", "pipe", "inherit"], env });
  process.stdin.pipe(child.stdin);
  child.stdout.pipe(process.stdout);
  child.on("error", () => process.exit(1));
  child.on("exit", (code, sig) => process.exit(code ?? (sig ? 1 : 0)));
}

/** Streamable HTTP client side: one POST per message, JSON or SSE replies. */
function httpTarget(target) {
  const url = new URL(target.url);
  const lib = url.protocol === "https:" ? https : http;
  let sessionId = null;
  let protocol = null;
  let ready = Promise.resolve();
  const pending = new Set();

  const post = (msg) =>
    new Promise((resolve, reject) => {
      const body = Buffer.from(JSON.stringify(msg));
      const headers = { ...(target.headers || {}), "content-type": "application/json", accept: "application/json, text/event-stream", "content-length": body.length };
      if (sessionId) headers["mcp-session-id"] = sessionId;
      if (protocol) headers["mcp-protocol-version"] = protocol;
      const req = lib.request(url, { method: "POST", headers }, (res) => {
        const sid = res.headers["mcp-session-id"];
        if (sid) sessionId = String(sid);
        const type = String(res.headers["content-type"] || "");
        const deliver = (m) => {
          if (msg.method === "initialize" && m?.id === msg.id && m.result?.protocolVersion) protocol = m.result.protocolVersion;
          write(m);
        };
        if (res.statusCode >= 400) {
          res.resume();
          res.on("end", () => reject(new Error(`HTTP ${res.statusCode}`)));
          return;
        }
        res.setEncoding("utf8");
        if (type.includes("text/event-stream")) {
          let buf = "";
          const flush = (block) => {
            const data = block.split(/\r?\n/).filter((l) => l.startsWith("data:")).map((l) => l.slice(5).replace(/^ /, "")).join("\n");
            if (!data) return;
            try {
              for (const m of [].concat(JSON.parse(data))) deliver(m);
            } catch {
              /* not JSON-RPC */
            }
          };
          res.on("data", (d) => {
            buf += d;
            let i;
            while ((i = buf.search(/\r?\n\r?\n/)) >= 0) {
              flush(buf.slice(0, i));
              buf = buf.slice(i).replace(/^\r?\n\r?\n/, "");
            }
          });
          res.on("end", () => {
            if (buf.trim()) flush(buf);
            resolve();
          });
        } else {
          let raw = "";
          res.on("data", (d) => (raw += d));
          res.on("end", () => {
            if (raw.trim() && type.includes("json")) {
              try {
                for (const m of [].concat(JSON.parse(raw))) deliver(m);
              } catch {
                return reject(new Error("invalid JSON reply"));
              }
            }
            resolve();
          });
        }
      });
      req.on("error", reject);
      req.end(body);
    });

  const rl = readline.createInterface({ input: process.stdin });
  rl.on("line", (line) => {
    let msg;
    try {
      msg = JSON.parse(line);
    } catch {
      return;
    }
    const isRequest = msg?.id !== undefined && Boolean(msg.method);
    const send = () => post(msg).catch((e) => isRequest && fail(msg.id, e.message));
    // Later messages need the session id that initialize returns; everything
    // else runs concurrently, because a client tool call can wait for minutes.
    const p = msg?.method === "initialize" ? (ready = ready.then(send)) : ready.then(send);
    pending.add(p);
    p.finally(() => pending.delete(p));
  });
  rl.on("close", async () => {
    await Promise.allSettled([...pending]);
    process.exit(0);
  });
}

const target = loadTarget();
if (target?.command) stdioTarget(target);
else if (target?.url) httpTarget(target);
else emptyServer();
