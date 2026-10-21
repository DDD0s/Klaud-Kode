import { promises as fs } from "node:fs";

export const backendError = (statusCode, message, type) => Object.assign(new Error(message), { statusCode, type });

/** CLIs without a system-prompt flag get the instructions at the top of the user turn. */
export function inlineSystem(system, prompt) {
  if (!system) return prompt;
  return `<system_instructions>\n${system}\n</system_instructions>\n\n${prompt}`;
}

/**
 * Attachment blocks (Anthropic shape, see extractAttachments in server.mjs)
 * split into text that can be appended to the prompt and binary blocks.
 */
export function splitAttachments(attachments = []) {
  const text = attachments.filter((b) => b.type === "text").map((b) => b.text);
  const binary = attachments.filter((b) => b.type !== "text");
  return { text, binary };
}

export function promptWithText(prompt, texts) {
  return texts.length ? [prompt, ...texts].join("\n\n") : prompt;
}

/**
 * The body MCP config is written for Claude Code (`{mcpServers: {name: {type, url, headers}}}`).
 * Other CLIs get the same servers translated into their own format.
 */
export async function readMcpServers(file) {
  let parsed;
  try {
    parsed = JSON.parse(await fs.readFile(file, "utf8"));
  } catch (e) {
    throw Object.assign(backendError(503, "The computer is unavailable. Please retry later.", "body_unavailable"), {
      detail: `MCP config unreadable: ${e.code || e.message}`,
    });
  }
  const out = [];
  for (const [name, server] of Object.entries(parsed?.mcpServers || {})) {
    if (!/^[A-Za-z0-9_-]{1,64}$/.test(name) || !server || typeof server !== "object") continue;
    if (typeof server.url === "string") out.push({ name, url: server.url, headers: server.headers || {} });
    else if (typeof server.command === "string") out.push({ name, command: server.command, args: server.args || [], env: server.env || {} });
  }
  return out;
}

/** Splits a stdout stream into JSON lines; unparseable lines are ignored like Claude's. */
export function jsonLine(line) {
  try {
    const v = JSON.parse(line);
    return v && typeof v === "object" ? v : null;
  } catch {
    return null;
  }
}

export function splitArgs(s) {
  return String(s || "").split(/\s+/).filter(Boolean);
}
