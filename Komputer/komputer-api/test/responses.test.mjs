import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { apiFixture } from "./helpers.mjs";
import { inputToMessages, ResponsesStreamWriter, responsesRequest, ResponseStore } from "../responses.mjs";

const BODY_MCP = { mcpServers: { komputer_use: { type: "http", url: "http://127.0.0.1:9/mcp", headers: {} } } };
const TOOLS = [{ type: "function", name: "lookup", description: "Look up", parameters: { type: "object", properties: { query: { type: "string" } }, required: ["query"] } }];

async function fixture(t, env = {}) {
  const f = await apiFixture(t, {
    env: { KOMPUTER_SKIP_TUNNEL: "1", ...env },
    prepare: (dir) => fs.writeFileSync(path.join(dir, ".mcp.json"), JSON.stringify(BODY_MCP)),
  });
  await f.ready();
  return f;
}
const responses = (f, body, headers = {}) => f.request("/v1/responses", body, { headers });

function events(text) {
  return text.split("\n\n").filter((b) => b.startsWith("event: ")).map((b) => {
    const [head, data] = b.split("\n");
    const parsed = JSON.parse(data.slice(6));
    assert.equal(head.slice(7), parsed.type);
    return parsed;
  });
}

test("responses: non-streaming text, instructions, effort, usage and retrieval", async (t) => {
  const f = await fixture(t);
  const r = await responses(f, { model: "komputer-sonnet", input: "hi", instructions: "BE BRIEF", reasoning: { effort: "high" }, metadata: { tag: "x" } });
  assert.equal(r.status, 200, r.text);
  const body = r.json();
  assert.match(body.id, /^resp_[0-9a-f]{32}$/);
  assert.equal(body.object, "response");
  assert.equal(body.status, "completed");
  assert.equal(body.model, "komputer-sonnet");
  assert.equal(body.instructions, "BE BRIEF");
  assert.deepEqual(body.reasoning, { effort: "high", summary: null });
  assert.deepEqual(body.metadata, { tag: "x" });
  assert.equal(body.output.length, 1);
  assert.equal(body.output[0].type, "message");
  assert.equal(body.output[0].role, "assistant");
  assert.deepEqual(body.output[0].content, [{ type: "output_text", text: "echo:hi", annotations: [], logprobs: [] }]);
  assert.deepEqual(body.usage, { input_tokens: 10, input_tokens_details: { cached_tokens: 3 }, output_tokens: 11,
    output_tokens_details: { reasoning_tokens: 0 }, total_tokens: 21 });
  const c = f.calls().at(-1);
  assert.deepEqual(c.args.slice(-5), ["--model", "sonnet", "--effort", "high", "--no-session-persistence"]);
  assert.ok(c.system.endsWith("\n\nBE BRIEF"));
  assert.equal((await f.request(`/v1/responses/${body.id}`)).json().id, body.id);
  const stored = fs.statSync(path.join(f.dir, "responses.json"));
  if (process.platform !== "win32") assert.equal(stored.mode & 0o777, 0o600);
});

test("responses: previous_response_id continues on one CLI session; forks reseed", async (t) => {
  const f = await fixture(t);
  const r1 = (await responses(f, { model: "komputer-opus", input: "hi" })).json();
  const r2res = await responses(f, { model: "komputer-opus", input: "second", previous_response_id: r1.id });
  assert.equal(r2res.status, 200, r2res.text);
  const r2 = r2res.json();
  assert.equal(r2.previous_response_id, r1.id);
  assert.equal(r2res.headers.get("x-komputer-session"), `resp-${r1.id}`);
  let c = f.calls().at(-1);
  assert.ok(c.args.includes("--session-id"));
  assert.equal(c.stdin, "Earlier conversation for context:\n\nUser: hi\n\nAssistant: echo:hi\n\n---\n\nsecond");
  const sid = c.args[c.args.indexOf("--session-id") + 1];

  const r3res = await responses(f, { model: "komputer-opus", input: [{ role: "user", content: [{ type: "input_text", text: "third" }] }], previous_response_id: r2.id });
  assert.equal(r3res.status, 200, r3res.text);
  c = f.calls().at(-1);
  assert.deepEqual(c.args.slice(-2), ["--resume", sid]);
  assert.equal(c.stdin, "third");
  assert.match(r3res.json().output[0].content[0].text, /^prev=.*\|now=third$/s);

  const fork = await responses(f, { model: "komputer-opus", input: "other branch", previous_response_id: r2.id });
  assert.equal(fork.status, 200, fork.text);
  c = f.calls().at(-1);
  assert.ok(c.args.includes("--session-id") && !c.args.includes(sid), "a fork from an earlier response starts a new CLI session");
  assert.match(c.stdin, /User: second\n\nAssistant: echo:Earlier/);
  assert.equal(f.calls().length, 4);

  const missing = await responses(f, { model: "komputer-opus", input: "x", previous_response_id: "resp_missing" });
  assert.equal(missing.status, 400);
  assert.match(missing.json().error.message, /Previous response with id 'resp_missing' not found/);
});

test("responses: streaming event sequence with reasoning and text", async (t) => {
  const f = await fixture(t);
  const r = await responses(f, { model: "komputer-claude", input: "THINK please", stream: true });
  assert.equal(r.status, 200);
  assert.match(r.headers.get("content-type"), /text\/event-stream/);
  assert.ok(!r.text.includes("[DONE]"));
  const ev = events(r.text);
  ev.forEach((e, i) => assert.equal(e.sequence_number, i));
  const types = ev.map((e) => e.type).filter((t, i, a) => t !== a[i - 1]);
  assert.deepEqual(types, [
    "response.created", "response.in_progress",
    "response.output_item.added", "response.reasoning_summary_part.added", "response.reasoning_summary_text.delta",
    "response.reasoning_summary_text.done", "response.reasoning_summary_part.done", "response.output_item.done",
    "response.output_item.added", "response.content_part.added", "response.output_text.delta",
    "response.output_text.done", "response.content_part.done", "response.output_item.done", "response.completed",
  ]);
  const done = ev.at(-1).response;
  assert.equal(done.status, "completed");
  assert.deepEqual(done.output.map((o) => o.type), ["reasoning", "message"]);
  assert.equal(done.output[0].summary[0].text, "pondering");
  const text = ev.filter((e) => e.type === "response.output_text.delta").map((e) => e.delta).join("");
  assert.equal(text, "echo:THINK please");
  assert.equal(done.output[1].content[0].text, text);
  assert.equal(ev[0].response.status, "in_progress");
  assert.equal(done.usage.output_tokens, 11);
});

test("responses: failures before output are real HTTP errors; unsupported features are 400", async (t) => {
  const f = await fixture(t);
  const limit = await responses(f, { model: "komputer-claude", input: "FAIL_LIMIT", stream: true });
  assert.equal(limit.status, 429);
  assert.equal(limit.json().error.type, "rate_limit_error");
  const bad = [
    { input: "x", tools: [{ type: "web_search" }] },
    { input: "x", text: { format: { type: "json_schema", name: "s", schema: {} } } },
    { input: "x", background: true },
    { input: [] },
    { input: [{ type: "item_reference", id: "msg_1" }] },
  ];
  for (const body of bad) {
    const r = await responses(f, { model: "komputer-claude", ...body });
    assert.equal(r.status, 400, JSON.stringify(body));
    assert.equal(r.json().error.type, "invalid_request_error");
  }
  const fail = await responses(f, { model: "komputer-claude", input: "FAIL_PRIVATE AFTER_OUTPUT", stream: true });
  assert.equal(fail.status, 200);
  const last = events(fail.text).at(-1);
  assert.equal(last.type, "response.failed");
  assert.equal(last.response.status, "failed");
  assert.ok(!fail.text.includes("private-test-token"));
});

test("responses: function tools round-trip through previous_response_id and inline history", async (t) => {
  const f = await fixture(t);
  const r = await responses(f, { model: "komputer-sonnet", input: "CLIENT_TOOL find it", tools: TOOLS, tool_choice: "required" });
  assert.equal(r.status, 200, r.text);
  const first = r.json();
  assert.match(first.id, /^resp_[0-9a-f]{32}_0$/);
  const call = first.output.find((o) => o.type === "function_call");
  assert.equal(call.name, "lookup");
  assert.match(call.call_id, /^call_[0-9a-f]{32}$/);
  assert.match(call.id, /^fc_/);
  assert.deepEqual(JSON.parse(call.arguments), { query: "hello 世界" });
  assert.equal(r.headers.get("x-komputer-mode"), "harness");

  const done = await responses(f, { model: "komputer-sonnet", tools: TOOLS, previous_response_id: first.id,
    input: [{ type: "function_call_output", call_id: call.call_id, output: "forty-two" }] });
  assert.equal(done.status, 200, done.text);
  const final = done.json();
  assert.equal(final.output.at(-1).type, "message");
  assert.match(final.output.at(-1).content[0].text, /forty-two/);
  assert.equal(f.calls().length, 1, "the tool result resumes the same CLI run");

  // Stateless clients resend the whole item list instead (store: false).
  const s = await responses(f, { model: "komputer-sonnet", input: "CLIENT_TOOL again", tools: TOOLS, store: false, stream: true });
  const ev = events(s.text);
  const added = ev.find((e) => e.type === "response.output_item.added" && e.item.type === "function_call");
  const argsDone = ev.find((e) => e.type === "response.function_call_arguments.done");
  assert.equal(argsDone.item_id, added.item.id);
  const fc = ev.at(-1).response.output.find((o) => o.type === "function_call");
  assert.equal(argsDone.arguments, fc.arguments);
  assert.equal((await f.request(`/v1/responses/${ev.at(-1).response.id}`)).status, 404, "store:false responses are not retrievable");
  const resumed = await responses(f, { model: "komputer-sonnet", tools: TOOLS, store: false, input: [
    { role: "user", content: "CLIENT_TOOL again" },
    { type: "function_call", call_id: fc.call_id, name: fc.name, arguments: fc.arguments },
    { type: "function_call_output", call_id: fc.call_id, output: [{ type: "input_text", text: "from history" }] },
  ] });
  assert.equal(resumed.status, 200, resumed.text);
  assert.match(resumed.json().output.at(-1).content[0].text, /from history/);
});

test("responses: store keeps text only, honours store:false and supports delete", async (t) => {
  const f = await fixture(t);
  const png = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==";
  const r = await responses(f, { model: "komputer-claude", input: [{ role: "user", content: [{ type: "input_text", text: "what is this" },
    { type: "input_image", image_url: `data:image/png;base64,${png}` }] }] });
  assert.equal(r.status, 200, r.text);
  assert.deepEqual(f.calls().at(-1).attachments.map((a) => a.type), ["image"]);
  const raw = fs.readFileSync(path.join(f.dir, "responses.json"), "utf8");
  assert.ok(!raw.includes(png), "attachments are not persisted");
  assert.ok(raw.includes("[image]"));
  const off = await responses(f, { model: "komputer-claude", input: "not stored", store: false });
  assert.equal(off.json().store, false);
  const cont = await responses(f, { model: "komputer-claude", input: "x", previous_response_id: off.json().id });
  assert.equal(cont.status, 400);
  const id = r.json().id;
  const del = await f.request(`/v1/responses/${id}`, null, { method: "DELETE" });
  assert.deepEqual(del.json(), { id, object: "response", deleted: true });
  assert.equal((await f.request(`/v1/responses/${id}`)).status, 404);
});

test("responses: routed to agy and muse with effort, chained onto their sessions", async (t) => {
  const f = await fixture(t, { KOMPUTER_BACKENDS: "claude,antigravity,muse" });
  const a1 = (await responses(f, { model: "komputer-gemini-3.8-flash", input: "hi", reasoning: { effort: "low" } })).json();
  assert.equal(a1.status, "completed");
  assert.deepEqual(f.agyCalls().at(-1).args.slice(4), ["--model", "gemini-3.8-flash", "--effort", "low"]);
  assert.equal(a1.usage.input_tokens, 25, "Responses input_tokens include the cached part");
  assert.equal(a1.usage.input_tokens_details.cached_tokens, 5);
  assert.equal(a1.usage.output_tokens_details.reasoning_tokens, 4);
  const a2 = (await responses(f, { model: "komputer-gemini-3.8-flash", input: "two", previous_response_id: a1.id })).json();
  const a3 = await responses(f, { model: "komputer-gemini-3.8-flash", input: "three", previous_response_id: a2.id, stream: true });
  assert.equal(a3.status, 200);
  const agy = f.agyCalls();
  assert.ok(!agy[1].args.includes("--conversation") && agy[2].args.includes("--conversation"));
  assert.match(events(a3.text).at(-1).response.output[0].content[0].text, /\|now=three$/);
  assert.equal(a3.headers.get("x-komputer-backend"), "antigravity");

  const m1 = (await responses(f, { model: "komputer-muse", input: "hi", reasoning: { effort: "xhigh" } })).json();
  const muse = f.museCalls().at(-1);
  assert.equal(muse.args[muse.args.indexOf("--reasoning-effort") + 1], "xhigh");
  assert.equal(m1.usage.output_tokens_details.reasoning_tokens, 6);
  const m2 = await responses(f, { model: "komputer-muse", input: "two", previous_response_id: m1.id });
  assert.equal(m2.status, 200);
  assert.equal(m2.headers.get("x-komputer-backend"), "muse");
  assert.equal(f.calls().length, 0, "no Claude Code process for other backends");
});

test("responses: input translation and stream writer units", () => {
  assert.deepEqual(inputToMessages("x"), [{ role: "user", content: "x" }]);
  assert.deepEqual(inputToMessages([
    { type: "message", role: "developer", content: "dev" },
    { role: "user", content: [{ type: "input_text", text: "a" }, { type: "input_file", filename: "f.pdf", file_data: "data:application/pdf;base64,AA==" }] },
    { type: "reasoning", id: "rs_1", summary: [] },
    { type: "message", role: "assistant", content: [{ type: "output_text", text: "b" }] },
    { type: "function_call", call_id: "c1", name: "n", arguments: "{}" },
    { type: "function_call", call_id: "c2", name: "n", arguments: { a: 1 } },
    { type: "function_call_output", call_id: "c1", output: "r1" },
  ]), [
    { role: "developer", content: "dev" },
    { role: "user", content: [{ type: "text", text: "a" }, { type: "file", file: { filename: "f.pdf", file_data: "data:application/pdf;base64,AA==" } }] },
    { role: "assistant", content: [{ type: "text", text: "b" }], tool_calls: [
      { id: "c1", type: "function", function: { name: "n", arguments: "{}" } },
      { id: "c2", type: "function", function: { name: "n", arguments: '{"a":1}' } }] },
    { role: "tool", tool_call_id: "c1", content: "r1" },
  ]);
  const store = new ResponseStore({ file: "" });
  const req = responsesRequest({ input: "x", tools: [], tool_choice: "auto", reasoning: { effort: "low" } }, store);
  assert.equal(req.chatBody.tools, undefined);
  assert.equal(req.chatBody.tool_choice, undefined, "tool_choice auto without tools is ordinary chat");
  assert.equal(req.chatBody.reasoning_effort, "low");
  assert.equal(req.fallbackKey, null);

  const seen = [];
  const w = new ResponsesStreamWriter((type, data) => seen.push(data), { id: "resp_x", status: "in_progress", output: [] });
  w.text("a");
  w.reasoning("r");
  w.text("b");
  w.functionCall({ call_id: "call_1", name: "n", arguments: "{}" });
  const final = w.complete(null);
  assert.deepEqual(final.output.map((o) => o.type), ["message", "reasoning", "message", "function_call"]);
  assert.deepEqual(final.output.map((o) => o.content?.[0]?.text ?? o.summary?.[0]?.text ?? o.arguments), ["a", "r", "b", "{}"]);
  const indexes = seen.filter((e) => e.type === "response.output_item.added").map((e) => e.output_index);
  assert.deepEqual(indexes, [0, 1, 2, 3]);
});
