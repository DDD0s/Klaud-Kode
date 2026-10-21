// Fake Gemini API for driving the real agy binary without a Google account:
// agy 1.4.3 with settings {"modelProvider": "gemini"}, GEMINI_API_KEY and
// GOOGLE_GEMINI_BASE_URL sends POST /v1beta/models/<model>:streamGenerateContent?alt=sse
// and reads SSE `data:` candidates. Each main-model request gets the next
// scripted turn (a list of parts); the title request (a flash-lite model) gets a name.
import http from "node:http";

/** @param {Array<Array<object>>} script model turns, e.g. [[{functionCall: {name, args}}], [{text: "done"}]] */
export async function fakeGemini(t, script) {
  const requests = [];
  let turn = 0;
  const server = http.createServer((req, res) => {
    let raw = "";
    req.on("data", (d) => (raw += d));
    req.on("end", () => {
      if (req.method === "GET") {
        res.writeHead(200, { "content-type": "application/json" });
        res.end(JSON.stringify({ models: [] }));
        return;
      }
      const naming = /flash-lite/.test(req.url);
      if (!naming) requests.push({ url: req.url, body: raw ? JSON.parse(raw) : null });
      const parts = naming ? [{ text: "Probe" }] : script[Math.min(turn++, script.length - 1)];
      const payload = { candidates: [{ content: { role: "model", parts }, finishReason: "STOP", index: 0 }],
        usageMetadata: { promptTokenCount: 10, candidatesTokenCount: 5, totalTokenCount: 15 } };
      res.writeHead(200, { "content-type": /stream/i.test(req.url) ? "text/event-stream" : "application/json" });
      res.end(/stream/i.test(req.url) ? `data: ${JSON.stringify(payload)}\r\n\r\n` : JSON.stringify(payload));
    });
  });
  await new Promise((r) => server.listen(0, "127.0.0.1", r));
  t.after(() => new Promise((r) => server.close(r)));
  return { url: `http://127.0.0.1:${server.address().port}`, requests };
}

/** The functionResponse parts agy sent back, in order. */
export const functionResponses = (requests) =>
  (requests.at(-1)?.body?.contents || []).flatMap((c) => c.parts || []).filter((p) => p.functionResponse).map((p) => p.functionResponse);
