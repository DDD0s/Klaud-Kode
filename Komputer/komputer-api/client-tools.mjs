// Client-executed tools: the CLI sees an isolated MCP server, while the harness
// receives ordinary tool calls and supplies the results over the next API POST.
// No client-provided command is executed by this module.
import http from "node:http";
import os from "node:os";
import path from "node:path";
import { promises as fs } from "node:fs";
import { randomUUID, createHash, timingSafeEqual } from "node:crypto";
import Ajv from "ajv";
import Ajv2020 from "ajv/dist/2020.js";
import addFormats from "ajv-formats";

export const toolError = (statusCode, message, type = "invalid_request_error") =>
  Object.assign(new Error(message), { statusCode, type });
const object = (v) => v !== null && typeof v === "object" && !Array.isArray(v);
const digest = (v) => createHash("sha256").update(JSON.stringify(v)).digest("hex");

function resultContent(value) {
  if (value == null) return [{ type: "text", text: "" }];
  if (typeof value === "string") return [{ type: "text", text: value }];
  if (!Array.isArray(value)) throw toolError(400, "Tool result content must be text or content blocks");
  return value.map((part) => {
    if (part?.type === "text" && typeof part.text === "string") return { type: "text", text: part.text };
    if (part?.type === "image" && part.source?.type === "base64" && typeof part.source.data === "string") {
      return { type: "image", data: part.source.data, mimeType: part.source.media_type };
    }
    if (part?.type === "image_url") {
      const url = typeof part.image_url === "string" ? part.image_url : part.image_url?.url;
      const match = /^data:(image\/[^;,]+);base64,(.*)$/s.exec(url || "");
      if (match) return { type: "image", mimeType: match[1], data: match[2] };
    }
    throw toolError(400, "Tool results support text and base64 images only");
  });
}

/** Recognize tool mode without changing ordinary chat/Body requests. */
export function clientToolRequest(protocol, body) {
  const messages = body.messages;
  const last = messages?.at(-1);
  let results = [];
  if (protocol === "openai" && last?.role === "tool") {
    for (let i = messages.length - 1; i >= 0 && messages[i]?.role === "tool"; i--) {
      const msg = messages[i];
      results.unshift({ id: msg.tool_call_id, content: resultContent(msg.content), isError: msg.is_error === true });
    }
  } else if (protocol === "anthropic" && last?.role === "user" && Array.isArray(last.content)) {
    const blocks = last.content.filter((b) => b?.type === "tool_result");
    if (blocks.length) {
      if (blocks.length !== last.content.length) throw toolError(400, "Send tool results without a new user message in the same turn");
      results = blocks.map((b) => ({ id: b.tool_use_id, content: resultContent(b.content), isError: b.is_error === true }));
    }
  }
  if (body.functions !== undefined || body.function_call !== undefined) throw toolError(400, "Use tools and tool_choice instead of legacy function fields");
  if (body.tools !== undefined && !Array.isArray(body.tools)) throw toolError(400, "tools must be an array");
  if (!body.tools?.length && body.tool_choice === undefined && !results.length) return null;
  if (body.n !== undefined && body.n !== 1) throw toolError(400, "Client tool mode supports n=1 only");
  let tools;
  if (body.tools !== undefined) {
    if (body.tools.length > 128) throw toolError(400, "At most 128 client tools are supported");
    const seen = new Set();
    tools = body.tools.map((entry, index) => {
      if (protocol === "openai" && entry?.type !== "function") throw toolError(400, "Only function tools are supported");
      if (protocol === "anthropic" && entry?.type && entry.type !== "custom") throw toolError(400, "Only client-defined tools are supported");
      const fn = protocol === "openai" ? entry.function : entry;
      if (!object(fn) || typeof fn.name !== "string" || !/^[A-Za-z0-9_-]{1,64}$/.test(fn.name) || seen.has(fn.name)) {
        throw toolError(400, "Tool names must be unique and contain 1-64 letters, digits, underscores or hyphens");
      }
      seen.add(fn.name);
      const schema = (protocol === "openai" ? fn.parameters : fn.input_schema) ?? { type: "object", properties: {} };
      if (!object(schema) || schema.type !== "object") throw toolError(400, "Tool input schema must have type object");
      if (fn.description !== undefined && typeof fn.description !== "string") throw toolError(400, "Tool description must be a string");
      return { name: fn.name, alias: `k_${index}_${digest(fn.name).slice(0, 12)}`, description: fn.description || "", schema };
    });
  }
  let choice = "auto";
  let name = null;
  let parallel = true;
  const requested = body.tool_choice;
  if (protocol === "openai") {
    if (typeof requested === "string") choice = requested;
    else if (requested !== undefined) {
      if (requested?.type !== "function" || typeof requested.function?.name !== "string") throw toolError(400, "Invalid tool_choice");
      choice = "named";
      name = requested.function.name;
    }
    if (body.parallel_tool_calls !== undefined && typeof body.parallel_tool_calls !== "boolean") throw toolError(400, "parallel_tool_calls must be boolean");
    parallel = body.parallel_tool_calls !== false;
  } else if (requested !== undefined) {
    if (!object(requested)) throw toolError(400, "tool_choice must be an object");
    choice = requested.type === "any" ? "required" : requested.type === "tool" ? "named" : requested.type;
    name = requested.name || null;
    if (requested.disable_parallel_tool_use !== undefined && typeof requested.disable_parallel_tool_use !== "boolean") throw toolError(400, "disable_parallel_tool_use must be boolean");
    parallel = requested.disable_parallel_tool_use !== true;
  }
  if (!["auto", "none", "required", "named"].includes(choice)) throw toolError(400, "Unsupported tool_choice");
  const ids = new Set();
  for (const r of results) {
    if (typeof r.id !== "string" || !r.id || ids.has(r.id)) throw toolError(400, "Tool results require unique tool call IDs");
    ids.add(r.id);
  }
  return { protocol, tools, results, choice, name, parallel };
}

function compileTools(tools) {
  return tools.map((tool) => {
    try {
      const Compiler = tool.schema.$schema?.includes("2020-12") ? Ajv2020 : Ajv;
      const compiler = new Compiler({ strict: false, allErrors: true, logger: false, ownProperties: true });
      addFormats(compiler);
      const validate = compiler.compile(tool.schema);
      if (validate.$async) throw new Error("async schemas are unsupported");
      return { ...tool, validate };
    } catch {
      throw toolError(400, `Invalid or unsupported input schema for tool ${tool.name}`);
    }
  });
}

function validateChoice(spec, tools) {
  if (spec.choice === "named" && !tools.some((t) => t.name === spec.name)) throw toolError(400, "tool_choice names an unavailable tool");
  if (spec.choice === "required" && tools.length === 0) throw toolError(400, "Required tool choice needs at least one tool");
}

function segment(run, spec) {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  promise.catch(() => {}); // can fail before an HTTP subscriber attaches
  return { id: `${run.id}_${run.sequence++}`, events: [], subs: new Set(), calls: [], sealed: false,
    policy: { choice: spec.choice, name: spec.name, parallel: spec.parallel }, promise, resolve, reject, delivered: false };
}

function json(res, status, value) {
  if (res.destroyed || res.writableEnded) return;
  // Explicit chunk framing also works with intermediaries that rewrite lengths.
  res.writeHead(status, { "Content-Type": "application/json", "Transfer-Encoding": "chunked" });
  res.end(JSON.stringify(value));
}

export class ClientToolGateway {
  constructor({ runCli, runClaude, acquire = async () => {}, release = () => {}, timeoutMs = 300_000, cacheMs = 600_000, maxRuns = 16, maxOutputBytes = 4_000_000 }) {
    this.runCli = runCli || runClaude;
    this.acquire = acquire;
    this.release = release;
    this.timeoutMs = timeoutMs;
    this.cacheMs = cacheMs;
    this.maxRuns = maxRuns;
    this.maxOutputBytes = maxOutputBytes;
    this.runs = new Map();
    this.calls = new Map();
    this.initial = new Map();
  }

  stats() {
    const runs = [...this.runs.values()];
    return { active: runs.filter((r) => !r.done).length, pending_tools: runs.reduce((n, r) => n + r.pending.size, 0) };
  }

  sweep() {
    for (const run of this.runs.values()) {
      if (run.done && Date.now() - run.finishedAt > this.cacheMs) this.forget(run);
    }
    while (this.runs.size >= this.maxRuns) {
      const old = [...this.runs.values()].find((r) => r.done);
      if (!old) break;
      this.forget(old);
    }
  }

  forget(run) {
    this.runs.delete(run.id);
    if (this.initial.get(run.key) === run) this.initial.delete(run.key);
    for (const id of run.callIds) this.calls.delete(id);
  }

  /** All continuation validation occurs before releasing even one MCP result. */
  prepare(spec, context) {
    this.sweep();
    let run, current;
    if (spec.results.length) {
      run = this.calls.get(spec.results[0].id);
      if (!run || spec.results.some((r) => this.calls.get(r.id) !== run)) throw toolError(410, "Tool call state is unavailable; do not repeat side effects automatically", "tool_state_expired");
      if (run.protocol !== spec.protocol) throw toolError(409, "Do not change API protocol during a tool cycle");
      if (context.clientKey && run.context.clientKey && context.clientKey !== run.context.clientKey) throw toolError(409, "Tool results belong to a different conversation");
      if (context.modelSpecified && (context.model !== run.context.model || context.backend !== run.context.backend)) throw toolError(409, "Cannot change models while tool calls are pending");
      if (context.effortSpecified && context.effort !== run.context.effort) throw toolError(409, "Cannot change reasoning effort during a tool cycle");
      if (context.systemSpecified && context.systemPrompt !== run.context.systemPrompt) throw toolError(409, "Cannot change system instructions during a tool cycle");
      if (spec.tools && digest(spec.tools) !== run.toolDigest) throw toolError(409, "Cannot change tool definitions during a tool cycle");
      validateChoice(spec, run.tools);
      const ordered = [...spec.results].sort((a, b) => a.id.localeCompare(b.id));
      const receiptKey = digest(ordered.map((r) => r.id));
      const valueHash = digest({ results: ordered, choice: spec.choice, name: spec.name, parallel: spec.parallel });
      const previous = run.receipts.get(receiptKey);
      if (previous) {
        if (previous.valueHash !== valueHash) throw toolError(409, "Conflicting retry of a tool result");
        current = previous.segment;
      } else {
        if (run.done) throw toolError(410, "Tool call state has expired", "tool_state_expired");
        const expected = run.waiting?.calls.map((c) => c.id).sort();
        if (!expected || digest(expected) !== digest(ordered.map((r) => r.id))) throw toolError(400, "Supply exactly one result for every tool call in the last response");
        current = segment(run, spec);
        run.current = current;
        run.waiting = null;
        clearTimeout(run.waitTimer);
        run.receipts.set(receiptKey, { valueHash, segment: current });
        current.events.push(...run.queuedEvents.splice(0));
        // Install the receipt before queuing a compute lease, so simultaneous
        // retries join this continuation instead of releasing results twice.
        run.resuming = true;
        this.continue(run, ordered).catch((e) => this.fail(run, e));
      }
    } else {
      const tools = spec.tools || [];
      validateChoice(spec, tools);
      const key = digest({ protocol: spec.protocol, context: context.key, tools, choice: spec.choice, name: spec.name, parallel: spec.parallel });
      const existing = this.initial.get(key);
      if (existing && (!existing.done || !existing.first.delivered)) {
        run = existing;
        current = run.first;
      } else {
        if (this.runs.size >= this.maxRuns) throw toolError(429, "Too many pending harness runs", "rate_limit_error");
        const compiled = compileTools(tools);
        run = { id: randomUUID().replaceAll("-", ""), key, protocol: spec.protocol, tools: compiled, toolDigest: digest(tools),
          context, sequence: 0, pending: new Map(), callIds: new Set(), queue: [], queuedEvents: [], receipts: new Map(),
          abort: new AbortController(), done: false, outputBytes: 0, invalidCalls: 0 };
        current = segment(run, spec);
        run.current = run.first = current;
        this.runs.set(run.id, run);
        this.initial.set(key, run);
        run.work = this.start(run).catch((e) => this.fail(run, e)).finally(() => this.cleanup(run));
        run.work.catch((e) => console.error("[komputer] tool cleanup failed:", e.code || "error"));
      }
    }
    return {
      id: current.id,
      runId: run.id,
      model: run.context.publicModel,
      wait: async ({ on, gone } = {}) => {
        const sub = { on, gone };
        for (const e of current.events) if (!gone?.aborted) on?.(e.kind, e.value);
        current.subs.add(sub);
        try { return await current.promise; }
        finally {
          current.subs.delete(sub);
          if (!gone?.aborted) current.delivered = true;
        }
      },
    };
  }

  emit(run, kind, value) {
    if (run.done) return;
    run.outputBytes += Buffer.byteLength(value);
    if (run.outputBytes > this.maxOutputBytes) {
      this.fail(run, toolError(502, "Tool cycle output limit exceeded", "tool_output_limit"));
      return;
    }
    const event = { kind, value };
    if (run.current.sealed) { run.queuedEvents.push(event); return; }
    run.current.events.push(event);
    for (const sub of run.current.subs) if (!sub.gone?.aborted) sub.on?.(kind, value);
  }

  seal(run, toolCalls, usage = null) {
    const current = run.current;
    if (current.sealed) return;
    current.sealed = true;
    current.calls = toolCalls;
    if (toolCalls.length) this.releaseLease(run);
    current.resolve({ text: current.events.filter((e) => e.kind === "text").map((e) => e.value).join(""),
      thinking: current.events.filter((e) => e.kind === "thinking").map((e) => e.value).join(""), toolCalls, usage });
  }

  scheduleCalls(run) {
    if (run.done || run.resuming || run.current.sealed || !run.queue.length || run.batchTimer) return;
    // A serial client may change tool_choice while an upstream parallel call
    // is queued. Re-check policy before ever exposing that call to the client.
    const policy = run.current.policy;
    run.queue = run.queue.filter((call) => {
      if (policy.choice !== "none" && (policy.choice !== "named" || call.name === policy.name)) return true;
      run.pending.delete(call.id);
      call.respond({ isError: true, content: [{ type: "text", text: "Tool is not available for this response" }] });
      return false;
    });
    if (!run.queue.length) return;
    run.batchTimer = setTimeout(() => {
      run.batchTimer = null;
      if (run.done || run.current.sealed) return;
      const limit = run.current.policy.parallel ? run.queue.length : 1;
      const batch = run.queue.splice(0, limit);
      run.waiting = run.current;
      this.seal(run, batch.map(({ id, name, arguments: args }) => ({ id, name, arguments: args })));
      run.waitTimer = setTimeout(() => this.fail(run, toolError(504, "Timed out waiting for tool results; do not repeat side effects automatically", "tool_result_timeout")), this.timeoutMs);
      run.waitTimer.unref();
    }, 15);
  }

  async start(run) {
    run.dir = await fs.mkdtemp(path.join(os.tmpdir(), "komputer-tools-"));
    const token = randomUUID() + randomUUID();
    const tokenBytes = Buffer.from(`Bearer ${token}`);
    run.server = http.createServer((req, res) => {
      this.rpc(run, tokenBytes, req, res).catch(() => json(res, 400, { error: "Invalid MCP request" }));
    });
    run.server.requestTimeout = Math.max(this.timeoutMs + 10_000, 60_000);
    await new Promise((resolve, reject) => {
      run.server.once("error", reject);
      run.server.listen(0, "127.0.0.1", resolve);
    });
    const mcpConfig = path.join(run.dir, "tools.json");
    const mcpServer = { name: "komputer_use", url: `http://127.0.0.1:${run.server.address().port}/mcp`, headers: { Authorization: `Bearer ${token}` } };
    await fs.writeFile(mcpConfig, JSON.stringify({ mcpServers: { komputer_use: {
      type: "http", url: mcpServer.url, headers: mcpServer.headers,
    } } }), { mode: 0o600 });
    if (run.abort.signal.aborted) throw run.abort.signal.reason;
    await this.takeLease(run);
    const policy = run.first.policy;
    const forced = run.tools.find((t) => t.name === policy.name);
    const instruction = policy.choice === "none" ? "Do not call any tools."
      : policy.choice === "required" ? "Use at least one available tool before your first answer."
        : forced ? `For your first tool call, use ${forced.alias} (${forced.name}).` : "";
    const out = await this.runCli(run.context, {
      mcpConfig, mcpServer, workDir: run.dir, instruction, signal: run.abort.signal,
      onText: (t) => this.emit(run, "text", t), onThinking: (t) => this.emit(run, "thinking", t),
    });
    if (run.done) return;
    if (run.pending.size) throw toolError(502, "The tool cycle ended before accepting results", "upstream_error");
    if (["required", "named"].includes(run.current.policy.choice)) throw toolError(502, "No tool call was produced for the required tool choice", "tool_choice_error");
    if (!run.current.events.some((e) => e.kind === "text") && out.text) this.emit(run, "text", out.text);
    this.seal(run, [], out.usage);
    run.done = true;
    run.finishedAt = Date.now();
  }

  async rpc(run, token, req, res) {
    const auth = Buffer.from(req.headers.authorization || "");
    if (auth.length !== token.length || !timingSafeEqual(auth, token)) { json(res, 403, { error: "Forbidden" }); return; }
    const expectedHost = `127.0.0.1:${run.server.address()?.port}`;
    if (req.headers.host !== expectedHost || (req.headers.origin && req.headers.origin !== `http://${expectedHost}`)) {
      json(res, 403, { error: "Forbidden origin" }); return;
    }
    if (req.url !== "/mcp") { json(res, 404, { error: "Not found" }); return; }
    if (req.method !== "POST") { res.writeHead(405, { Allow: "POST" }); res.end(); return; }
    const chunks = [];
    let size = 0;
    for await (const chunk of req) {
      size += chunk.length;
      if (size > 1_000_000) { json(res, 413, { error: "MCP request too large" }); return; }
      chunks.push(chunk);
    }
    const msg = JSON.parse(Buffer.concat(chunks).toString("utf8"));
    if (!object(msg) || msg.jsonrpc !== "2.0") { json(res, 400, { error: "Invalid JSON-RPC request" }); return; }
    if (msg.id === undefined) { res.writeHead(202); res.end(); return; }
    const reply = (result) => json(res, 200, { jsonrpc: "2.0", id: msg.id, result });
    const rpcError = (code, message) => json(res, 200, { jsonrpc: "2.0", id: msg.id, error: { code, message } });
    if (msg.method === "initialize") {
      reply({ protocolVersion: ["2024-11-05", "2025-03-26", "2025-06-18"].includes(msg.params?.protocolVersion) ? msg.params.protocolVersion : "2025-06-18",
        capabilities: { tools: {} }, serverInfo: { name: "komputer", version: "0.9.0" } }); return;
    }
    if (msg.method === "ping") { reply({}); return; }
    if (msg.method === "tools/list") {
      reply({ tools: run.tools.map((t) => ({ name: t.alias, title: t.name, description: `${t.name}: ${t.description}`, inputSchema: t.schema })) }); return;
    }
    if (msg.method !== "tools/call") { rpcError(-32601, "Method not found"); return; }
    if (run.done) { rpcError(-32000, "Tool cycle is no longer active"); return; }
    const tool = run.tools.find((t) => t.alias === msg.params?.name);
    const args = msg.params?.arguments ?? {};
    const policy = run.current.policy;
    let problem;
    if (!tool) problem = "Unknown tool";
    else if (policy.choice === "none") problem = "Tools are disabled for this response; answer without calling tools";
    else if (policy.choice === "named" && tool.name !== policy.name) problem = `Only ${run.tools.find((t) => t.name === policy.name)?.alias} may be called for this response`;
    else if (!object(args) || !tool.validate(args)) problem = "Arguments do not match the tool input schema; correct the arguments";
    if (problem) {
      reply({ isError: true, content: [{ type: "text", text: problem }] });
      if (++run.invalidCalls >= 8) this.fail(run, toolError(502, "Too many invalid tool calls", "tool_choice_error"));
      return;
    }
    const id = `${run.protocol === "anthropic" ? "toolu" : "call"}_${randomUUID().replaceAll("-", "")}`;
    run.invalidCalls = 0;
    const call = { id, name: tool.name, arguments: args, respond: reply };
    run.pending.set(id, call);
    run.callIds.add(id);
    this.calls.set(id, run);
    run.queue.push(call);
    res.on("close", () => {
      if (!res.writableEnded && !run.done && run.pending.has(id)) this.fail(run, toolError(502, "Tool transport disconnected", "upstream_error"));
    });
    this.scheduleCalls(run);
  }

  fail(run, error) {
    if (run.done) return;
    run.done = true;
    run.finishedAt = Date.now();
    clearTimeout(run.batchTimer);
    clearTimeout(run.waitTimer);
    if (!run.current.sealed) { run.current.sealed = true; run.current.reject(error); }
    run.abort.abort(error);
    this.releaseLease(run);
    for (const call of run.pending.values()) call.respond({ isError: true, content: [{ type: "text", text: "Tool cycle cancelled" }] });
    run.pending.clear();
  }

  async cleanup(run) {
    this.releaseLease(run);
    clearTimeout(run.batchTimer);
    clearTimeout(run.waitTimer);
    run.server?.closeAllConnections();
    if (run.server?.listening) await new Promise((resolve) => run.server.close(resolve));
    if (run.dir) await fs.rm(run.dir, { recursive: true, force: true, maxRetries: 3, retryDelay: 50 });
  }

  async close() {
    for (const run of this.runs.values()) this.fail(run, toolError(503, "Service is stopping", "server_error"));
    await Promise.allSettled([...this.runs.values()].map((r) => r.work));
  }

  async takeLease(run) {
    if (run.hasLease) return;
    await this.acquire(run.abort.signal);
    if (run.done || run.abort.signal.aborted) {
      this.release();
      throw run.abort.signal.reason || toolError(503, "Tool cycle ended");
    }
    run.hasLease = true;
  }

  releaseLease(run) {
    if (run.hasLease) { run.hasLease = false; this.release(); }
  }

  async continue(run, results) {
    try {
      await this.takeLease(run);
      for (const result of results) {
        const call = run.pending.get(result.id);
        run.pending.delete(result.id);
        call.respond({ content: result.content, ...(result.isError ? { isError: true } : {}) });
      }
    } finally { run.resuming = false; }
    this.scheduleCalls(run);
  }

  async cancel(id) {
    const run = this.runs.get(id);
    if (!run) return false;
    this.fail(run, toolError(409, "Tool cycle cancelled", "tool_cycle_cancelled"));
    await run.work;
    return true;
  }
}
