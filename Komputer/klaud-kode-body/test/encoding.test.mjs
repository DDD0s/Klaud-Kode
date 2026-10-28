import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { decodeOutput, readTextWindow } from "../server.mjs";

const bytes = Buffer.from([0x93, 0xfa, 0x96, 0x7b, 0x8c, 0xea]);

test("invalid UTF-8 on a UTF-8 console uses a reversible, accurately labelled fallback", () => {
  const result = decodeOutput(bytes, "utf-8");
  assert.equal(result.encoding, "latin1");
  assert.deepEqual(Buffer.from(result.text, "latin1"), bytes);
  assert.equal(result.text.includes("\ufffd"), false);
});

test("known console encodings still decode correctly and valid UTF-8 wins", () => {
  assert.deepEqual(decodeOutput(bytes, "shift_jis"), { text: "日本語", encoding: "shift_jis" });
  assert.deepEqual(decodeOutput(Buffer.from("中文"), "shift_jis"), { text: "中文", encoding: "utf-8" });
});

test("windowed reads use the same reversible fallback, including C1 bytes", async (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "klaud-encoding-test-"));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const file = path.join(dir, "raw.txt");
  fs.writeFileSync(file, Buffer.concat([Buffer.from("skip\n"), bytes, Buffer.from("\nend")]));
  const result = await readTextWindow(file, 100, 2, 1, "utf-8");
  assert.equal(result.encoding, "latin1");
  assert.equal(result.totalLines, 3);
  assert.deepEqual(Buffer.from(result.content, "latin1"), bytes);
});
