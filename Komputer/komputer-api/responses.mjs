// OpenAI Responses API (POST /v1/responses) translated onto the chat shapes the
// server already runs. previous_response_id chains are kept in a local store
// and mapped onto CLI sessions by server.mjs.
import { promises as fs } from "node:fs";
import { randomUUID, createHash } from "node:crypto";

const invalid = (message, param) => Object.assign(new Error(message), { statusCode: 400, type: "invalid_request_error", param });
const hex = () => randomUUID().replace(/-/g, "");
const object = (v) => v !== null && typeof v === "object" && !Array.isArray(v);

function partsToChat(content) {
  if (content == null) return "";
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) throw invalid("content must be a string or an array of content parts", "input");
  return content.map((p) => {
    switch (p?.type) {
      case "input_text":
      case "output_text":
      case "text":
        return { type: "text", text: String(p.text ?? "") };
      case "refusal":
        return { type: "text", text: String(p.refusal ?? "") };
      case "input_image": {
        const url = typeof p.image_url === "string" ? p.image_url : p.image_url?.url;
        if (!url) throw invalid("input_image needs image_url; file_id is not supported", "input");
        return { type: "image_url", image_url: { url } };
      }
      case "input_file":
        if (typeof p.file_data !== "string" || !p.file_data) throw invalid("input_file needs file_data; file_id and file_url are not supported", "input");
        return { type: "file", file: { file_data: p.file_data, filename: p.filename } };
      default:
        throw invalid(`Unsupported content part type: ${p?.type}`, "input");
    }
  });
}

function outputToChat(output) {
  if (output == null) return "";
  if (typeof output === "string") return output;
  if (!Array.isArray(output)) return JSON.stringify(output);
  return output.map((p) => {
    if (p?.type === "input_text" || p?.type === "output_text") return { type: "text", text: String(p.text ?? "") };
    if (p?.type === "input_image") return { type: "image_url", image_url: { url: typeof p.image_url === "string" ? p.image_url : p.image_url?.url } };
    throw invalid("function_call_output supports text and images only", "input");
  });
}

/** Responses input (string or item list) → chat messages. */
export function inputToMessages(input) {
  if (typeof input === "string") return [{ role: "user", content: input }];
  if (!Array.isArray(input)) throw invalid("input must be a string or an array of items", "input");
  const out = [];
  for (const item of input) {
    if (!object(item)) throw invalid("input items must be objects", "input");
    const type = item.type ?? (item.role ? "message" : undefined);
    if (type === "message") {
      if (!["user", "assistant", "system", "developer"].includes(item.role)) throw invalid(`Unsupported message role: ${item.role}`, "input");
      out.push({ role: item.role, content: partsToChat(item.content) });
    } else if (type === "function_call") {
      if (typeof item.call_id !== "string" || !item.call_id || typeof item.name !== "string") throw invalid("function_call items need call_id and name", "input");
      const call = {
        id: item.call_id,
        type: "function",
        function: { name: item.name, arguments: typeof item.arguments === "string" ? item.arguments : JSON.stringify(item.arguments ?? {}) },
      };
      const prev = out.at(-1);
      if (prev?.role === "assistant") (prev.tool_calls ||= []).push(call);
      else out.push({ role: "assistant", content: "", tool_calls: [call] });
    } else if (type === "function_call_output") {
      if (typeof item.call_id !== "string" || !item.call_id) throw invalid("function_call_output items need call_id", "input");
      out.push({ role: "tool", tool_call_id: item.call_id, content: outputToChat(item.output) });
    } else if (type === "reasoning") {
      // Earlier reasoning is never replayed into a CLI.
    } else {
      throw invalid(`Unsupported input item type: ${type}`, "input");
    }
  }
  return out;
}

export function toolsToChat(tools) {
  if (tools === undefined || tools === null) return undefined;
  if (!Array.isArray(tools)) throw invalid("tools must be an array", "tools");
  return tools.map((t) => {
    if (t?.type !== "function") throw invalid(`Only function tools are supported (got ${t?.type})`, "tools");
    return { type: "function", function: { name: t.name, description: t.description ?? undefined, parameters: t.parameters ?? undefined } };
  });
}

function toolChoiceToChat(choice) {
  if (choice === undefined || choice === null) return undefined;
  if (typeof choice === "string") return choice;
  if (choice?.type === "function" && typeof choice.name === "string") return { type: "function", function: { name: choice.name } };
  throw invalid("Unsupported tool_choice", "tool_choice");
}

function storedContent(content) {
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  return content
    .map((p) => (p?.type === "text" ? p.text : p?.type === "image_url" ? "[image]" : p?.type === "file" ? `[file${p.file?.filename ? `: ${p.file.filename}` : ""}]` : ""))
    .filter(Boolean)
    .join("\n");
}

/** Text-only copy for the store: attachments are never persisted. */
export function storedMessages(messages) {
  return messages.map((m) => ({
    role: m.role,
    content: storedContent(m.content),
    ...(m.tool_calls ? { tool_calls: m.tool_calls } : {}),
    ...(m.tool_call_id ? { tool_call_id: m.tool_call_id } : {}),
  }));
}

/**
 * Validate a Responses request and translate it to a chat body.
 * @returns {{chatBody: object, newMessages: object[], previous: object|null, fallbackKey: string|null}}
 */
export function responsesRequest(body, store) {
  if (body.background === true) throw invalid("background responses are not supported", "background");
  if (body.input === undefined || body.input === null || body.input === "" || (Array.isArray(body.input) && !body.input.length)) {
    throw invalid("input required", "input");
  }
  if (body.text?.format && body.text.format.type !== "text") throw invalid("Only the text output format is supported", "text.format");
  if (body.instructions != null && typeof body.instructions !== "string") throw invalid("instructions must be a string", "instructions");
  if (body.previous_response_id != null && typeof body.previous_response_id !== "string") throw invalid("previous_response_id must be a string", "previous_response_id");
  if (body.previous_response_id && body.conversation) throw invalid("conversation and previous_response_id cannot be used together", "conversation");
  if (body.reasoning != null && !object(body.reasoning)) throw invalid("reasoning must be an object", "reasoning");
  let previous = null;
  let history = [];
  if (body.previous_response_id) {
    previous = store.get(body.previous_response_id);
    if (!previous) throw invalid(`Previous response with id '${body.previous_response_id}' not found.`, "previous_response_id");
    history = store.history(previous.id);
  }
  const newMessages = inputToMessages(body.input);
  const messages = [...history, ...newMessages];
  if (!messages.some((m) => m.role === "user" || m.role === "tool")) throw invalid("input needs a user message or a function_call_output", "input");
  let tools = toolsToChat(body.tools);
  let toolChoice = toolChoiceToChat(body.tool_choice);
  if (!tools?.length) {
    tools = undefined;
    // Many clients always send tool_choice "auto"; without tools that is ordinary chat.
    if (toolChoice === "auto" || toolChoice === "none") toolChoice = undefined;
  }
  const conversation = typeof body.conversation === "string" ? body.conversation : body.conversation?.id;
  const chatBody = {
    model: body.model,
    messages,
    stream: body.stream === true,
    ...(body.instructions ? { system: body.instructions } : {}),
    ...(tools ? { tools } : {}),
    ...(toolChoice !== undefined ? { tool_choice: toolChoice } : {}),
    ...(body.parallel_tool_calls !== undefined ? { parallel_tool_calls: body.parallel_tool_calls } : {}),
    ...(body.reasoning?.effort ? { reasoning_effort: body.reasoning.effort } : {}),
    ...(body.user ? { user: body.user } : {}),
    ...(object(body.metadata) ? { metadata: body.metadata } : {}),
    ...(conversation ? { conversation_id: conversation } : {}),
  };
  // A chain without its own conversation id continues on a session named after
  // its first stored link, so turn 3+ resumes instead of replaying history.
  const fallbackKey = previous ? previous.key || `resp-${previous.id}` : null;
  return { chatBody, newMessages, previous, fallbackKey };
}

export function responsesUsage(u) {
  const cached = Number(u?.cache_read_input_tokens) || 0;
  const input = (Number(u?.input_tokens) || 0) + cached + (Number(u?.cache_creation_input_tokens) || 0);
  const output = Number(u?.output_tokens) || 0;
  return {
    input_tokens: input,
    input_tokens_details: { cached_tokens: cached },
    output_tokens: output,
    output_tokens_details: { reasoning_tokens: Number(u?.reasoning_tokens) || 0 },
    total_tokens: input + output,
  };
}

export function responseObject({ id, createdAt, model, request, status = "in_progress", output = [], usage = null, error = null }) {
  return {
    id,
    object: "response",
    created_at: createdAt,
    status,
    background: false,
    error,
    incomplete_details: null,
    instructions: request.instructions ?? null,
    max_output_tokens: request.max_output_tokens ?? null,
    model,
    output,
    parallel_tool_calls: request.parallel_tool_calls ?? true,
    previous_response_id: request.previous_response_id ?? null,
    reasoning: { effort: request.reasoning?.effort ?? null, summary: request.reasoning?.summary ?? null },
    store: request.store !== false,
    temperature: request.temperature ?? null,
    text: { format: { type: "text" } },
    tool_choice: request.tool_choice ?? "auto",
    tools: Array.isArray(request.tools) ? request.tools : [],
    top_p: request.top_p ?? null,
    truncation: "disabled",
    usage,
    user: request.user ?? null,
    metadata: object(request.metadata) ? request.metadata : {},
  };
}

const fcId = (callId) => `fc_${createHash("sha256").update(String(callId)).digest("hex").slice(0, 32)}`;

function messageItem(id, text, status = "completed") {
  return { id, type: "message", status, role: "assistant", content: [{ type: "output_text", text, annotations: [], logprobs: [] }] };
}

function functionCallItem(call, status = "completed") {
  return { id: fcId(call.call_id), type: "function_call", status, call_id: call.call_id, name: call.name, arguments: call.arguments };
}

/** Non-streaming output items. toolCalls: [{call_id, name, arguments: string}]. */
export function responseOutput({ text, thinking, toolCalls = [] }) {
  const out = [];
  if (thinking) out.push({ id: `rs_${hex()}`, type: "reasoning", summary: [{ type: "summary_text", text: thinking }] });
  if (text || !toolCalls.length) out.push(messageItem(`msg_${hex()}`, text || ""));
  for (const call of toolCalls) out.push(functionCallItem(call));
  return out;
}

/** Emits the Responses streaming event sequence and builds the final output list. */
export class ResponsesStreamWriter {
  constructor(write, response) {
    this.write = write;
    this.response = response;
    this.seq = 0;
    this.output = [];
    this.current = null;
    this.started = false;
  }

  event(type, data) {
    this.write(type, { type, sequence_number: this.seq++, ...data });
  }

  start() {
    if (this.started) return;
    this.started = true;
    const response = { ...this.response, status: "in_progress", output: [], usage: null };
    this.event("response.created", { response });
    this.event("response.in_progress", { response });
  }

  open(kind) {
    if (this.current?.kind === kind) return this.current;
    this.close();
    const index = this.output.length;
    if (kind === "text") {
      const id = `msg_${hex()}`;
      this.event("response.output_item.added", { output_index: index, item: { id, type: "message", status: "in_progress", role: "assistant", content: [] } });
      this.event("response.content_part.added", { item_id: id, output_index: index, content_index: 0, part: { type: "output_text", text: "", annotations: [], logprobs: [] } });
      this.current = { kind, id, index, text: "" };
    } else {
      const id = `rs_${hex()}`;
      this.event("response.output_item.added", { output_index: index, item: { id, type: "reasoning", summary: [] } });
      this.event("response.reasoning_summary_part.added", { item_id: id, output_index: index, summary_index: 0, part: { type: "summary_text", text: "" } });
      this.current = { kind, id, index, text: "" };
    }
    this.output.push(null);
    return this.current;
  }

  text(delta) {
    this.start();
    const c = this.open("text");
    c.text += delta;
    this.event("response.output_text.delta", { item_id: c.id, output_index: c.index, content_index: 0, delta, logprobs: [] });
  }

  reasoning(delta) {
    this.start();
    const c = this.open("reasoning");
    c.text += delta;
    this.event("response.reasoning_summary_text.delta", { item_id: c.id, output_index: c.index, summary_index: 0, delta });
  }

  close() {
    const c = this.current;
    if (!c) return;
    this.current = null;
    let item;
    if (c.kind === "text") {
      const part = { type: "output_text", text: c.text, annotations: [], logprobs: [] };
      this.event("response.output_text.done", { item_id: c.id, output_index: c.index, content_index: 0, text: c.text, logprobs: [] });
      this.event("response.content_part.done", { item_id: c.id, output_index: c.index, content_index: 0, part });
      item = messageItem(c.id, c.text);
    } else {
      this.event("response.reasoning_summary_text.done", { item_id: c.id, output_index: c.index, summary_index: 0, text: c.text });
      this.event("response.reasoning_summary_part.done", { item_id: c.id, output_index: c.index, summary_index: 0, part: { type: "summary_text", text: c.text } });
      item = { id: c.id, type: "reasoning", summary: [{ type: "summary_text", text: c.text }] };
    }
    this.output[c.index] = item;
    this.event("response.output_item.done", { output_index: c.index, item });
  }

  functionCall(call) {
    this.start();
    this.close();
    const index = this.output.length;
    const item = functionCallItem(call);
    this.output.push(item);
    this.event("response.output_item.added", { output_index: index, item: { ...item, status: "in_progress", arguments: "" } });
    this.event("response.function_call_arguments.delta", { item_id: item.id, output_index: index, delta: call.arguments });
    this.event("response.function_call_arguments.done", { item_id: item.id, output_index: index, arguments: call.arguments });
    this.event("response.output_item.done", { output_index: index, item });
  }

  complete(usage) {
    this.start();
    if (!this.output.some((i) => i?.type === "message" || i?.type === "function_call") && this.current?.kind !== "text") this.open("text");
    this.close();
    const response = { ...this.response, status: "completed", output: this.output.filter(Boolean), usage };
    this.event("response.completed", { response });
    return response;
  }

  fail(error) {
    this.start();
    this.close();
    const response = { ...this.response, status: "failed", output: this.output.filter(Boolean), error };
    this.event("response.failed", { response });
    return response;
  }
}

/** Stored responses for previous_response_id and GET /v1/responses/{id}. */
export class ResponseStore {
  constructor({ file, max = 1000, log = () => {} }) {
    this.file = file;
    this.max = max;
    this.log = log;
    this.map = new Map();
    this.chain = Promise.resolve();
  }

  async load() {
    if (!this.file) return;
    try {
      const data = JSON.parse(await fs.readFile(this.file, "utf8"));
      for (const rec of Array.isArray(data.responses) ? data.responses : []) if (rec?.id) this.map.set(rec.id, rec);
      this.log(`loaded ${this.map.size} stored response(s)`);
    } catch (e) {
      if (e?.code !== "ENOENT") this.log(`warn: failed to load responses store: ${e.message}`);
    }
  }

  get(id) {
    return this.map.get(id) || null;
  }

  /** Full message history up to and including a stored response (oldest first). */
  history(id) {
    const chain = [];
    const seen = new Set();
    for (let rec = this.get(id); rec && !seen.has(rec.id); rec = rec.previous_response_id ? this.get(rec.previous_response_id) : null) {
      seen.add(rec.id);
      chain.unshift(rec);
    }
    return chain.flatMap((r) => [...(r.input || []), ...(r.output || [])]);
  }

  put(rec) {
    this.map.delete(rec.id);
    this.map.set(rec.id, rec);
    while (this.map.size > this.max) this.map.delete(this.map.keys().next().value);
    return this.save();
  }

  async delete(id) {
    if (!this.map.delete(id)) return false;
    await this.save();
    return true;
  }

  save() {
    if (!this.file) return Promise.resolve();
    this.chain = this.chain
      .catch(() => {})
      .then(async () => {
        const tmp = `${this.file}.${process.pid}.tmp`;
        await fs.writeFile(tmp, JSON.stringify({ updated_at: new Date().toISOString(), responses: [...this.map.values()] }), { mode: 0o600 });
        await fs.rename(tmp, this.file);
      });
    return this.chain;
  }
}
