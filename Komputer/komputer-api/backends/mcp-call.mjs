// Minimal Streamable HTTP MCP client for helpers that run inside agy turns
// (the bash shim) and must reach the Body without the SDK: initialize once,
// then tools/call. JSON and SSE replies are both accepted.
import http from "node:http";
import https from "node:https";

/** @param {{url: string, headers?: Record<string,string>}} server */
export function mcpHttpClient(server, { timeoutMs = 0 } = {}) {
  const url = new URL(server.url);
  const lib = url.protocol === "https:" ? https : http;
  let sessionId = null;
  let protocol = null;
  let ready = null;
  let seq = 0;

  const post = (msg) =>
    new Promise((resolve, reject) => {
      const body = Buffer.from(JSON.stringify(msg));
      const headers = { ...(server.headers || {}), "content-type": "application/json", accept: "application/json, text/event-stream", "content-length": body.length };
      if (sessionId) headers["mcp-session-id"] = sessionId;
      if (protocol) headers["mcp-protocol-version"] = protocol;
      const req = lib.request(url, { method: "POST", headers, timeout: timeoutMs || undefined }, (res) => {
        if (res.headers["mcp-session-id"]) sessionId = String(res.headers["mcp-session-id"]);
        let raw = "";
        res.setEncoding("utf8");
        res.on("data", (d) => (raw += d));
        res.on("end", () => {
          if (res.statusCode >= 400) return reject(new Error(`MCP HTTP ${res.statusCode}: ${raw.slice(0, 200)}`));
          if (msg.id === undefined) return resolve(null);
          const type = String(res.headers["content-type"] || "");
          const payloads = type.includes("text/event-stream")
            ? raw.split(/\r?\n\r?\n/).map((b) => b.split(/\r?\n/).filter((l) => l.startsWith("data:")).map((l) => l.slice(5).trim()).join("\n")).filter(Boolean)
            : [raw];
          for (const p of payloads) {
            try {
              const found = [].concat(JSON.parse(p)).find((m) => m?.id === msg.id);
              if (found) return resolve(found);
            } catch {
              /* keep looking */
            }
          }
          reject(new Error("MCP reply without a matching response"));
        });
      });
      req.on("timeout", () => req.destroy(new Error("MCP request timed out")));
      req.on("error", reject);
      req.end(body);
    });

  const ensure = () =>
    (ready ??= (async () => {
      const r = await post({ jsonrpc: "2.0", id: ++seq, method: "initialize",
        params: { protocolVersion: "2025-06-18", capabilities: {}, clientInfo: { name: "komputer-agy", version: "1" } } });
      if (r.error) throw new Error(`MCP initialize: ${r.error.message}`);
      protocol = r.result?.protocolVersion || null;
      await post({ jsonrpc: "2.0", method: "notifications/initialized" });
    })());

  return {
    /** Call a tool; returns its JSON payload (or text), throws on a tool error. */
    async call(name, args = {}) {
      await ensure();
      const r = await post({ jsonrpc: "2.0", id: ++seq, method: "tools/call", params: { name, arguments: args } });
      if (r.error) throw Object.assign(new Error(r.error.message), { code: r.error.code });
      const text = (r.result?.content || []).filter((c) => c.type === "text").map((c) => c.text).join("");
      if (r.result?.isError) throw Object.assign(new Error(text || `${name} failed`), { toolError: true });
      try {
        return JSON.parse(text);
      } catch {
        return text;
      }
    },
  };
}
