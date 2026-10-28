import { test } from "node:test";
import assert from "node:assert/strict";
import path from "node:path";
import { buildClaudeEnv } from "../claude-env.mjs";

const cwd = path.resolve("test-work", "session");

test("child retains certificates, login paths, timeouts and shell settings without arbitrary deployment variables", () => {
  const essentials = Object.fromEntries([
    "PATH", "HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "HOMEDRIVE", "HOMEPATH",
    "SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS", "CLAUDE_CONFIG_DIR", "CLAUDE_CODE_OAUTH_TOKEN",
    "MCP_TIMEOUT", "MCP_TOOL_TIMEOUT", "DISABLE_AUTOUPDATER", "DISABLE_TELEMETRY", "SHELL", "TERM",
    "ANTHROPIC_DEFAULT_MODEL", "ANTHROPIC_DEFAULT_FABLE_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL",
    "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL",
  ].map((name) => [name, `fixture-${name}`]));
  const out = buildClaudeEnv({ ...essentials,
    KOMPUTER_KEYS_FILE: "private", BODY_SSH: "private", KLAUD_KODE_TOKEN: "private",
    CUSTOM_SECRET: "private", CLAUDE_BIN: "private", NODE_BIN: "private", NODE_OPTIONS: "--inspect",
    CLAUDE_UNKNOWN_SECRET: "private", ANTHROPIC_UNKNOWN_SECRET: "private",
  }, cwd);
  assert.deepEqual(out, { ...essentials, CI: "1", GIT_CEILING_DIRECTORIES: path.dirname(cwd) });
});

test("extra child variables require exact opt-in and cannot override reserved isolation settings", () => {
  const out = buildClaudeEnv({
    KOMPUTER_CHILD_ENV_EXTRA: "CUSTOM_TOOL_CONFIG, CLAUDE_CODE_PROJECT_DIR_NAME, BODY_SSH, KOMPUTER_KEYS_FILE, GIT_CEILING_DIRECTORIES, CI",
    CUSTOM_TOOL_CONFIG: "custom.json", CLAUDE_CODE_PROJECT_DIR_NAME: "project",
    BODY_SSH: "private", KOMPUTER_KEYS_FILE: "private", GIT_CEILING_DIRECTORIES: "wrong", CI: "0",
  }, cwd);
  assert.deepEqual(out, {
    CUSTOM_TOOL_CONFIG: "custom.json", CLAUDE_CODE_PROJECT_DIR_NAME: "project",
    CI: "1", GIT_CEILING_DIRECTORIES: path.dirname(cwd),
  });
});

test("lowercase HTTP, HTTPS, ALL and NO_PROXY survive and uppercase wins on conflicts", () => {
  const out = buildClaudeEnv({
    http_proxy: "http://fixture-http:1", https_proxy: "http://fixture-https:2",
    all_proxy: "socks5://fixture-all:3", no_proxy: "localhost,fixture.invalid",
  }, cwd);
  for (const name of ["HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY"]) assert.equal(out[name], out[name.toLowerCase()]);
  assert.equal(out.HTTP_PROXY, "http://fixture-http:1");
  assert.equal(out.HTTPS_PROXY, "http://fixture-https:2");
  assert.equal(out.ALL_PROXY, "socks5://fixture-all:3");
  assert.equal(out.NO_PROXY, "localhost,fixture.invalid");
  const upper = buildClaudeEnv({ HTTP_PROXY: "http://upper:1", http_proxy: "http://lower:2", NO_PROXY: "", no_proxy: "ignored" }, cwd);
  assert.equal(upper.http_proxy, "http://upper:1");
  assert.equal(upper.https_proxy, "http://upper:1");
  assert.equal(upper.NO_PROXY, "", "an explicitly empty bypass list must be preserved");
});

test("no proxy is invented; NO_PROXY also survives independently", () => {
  const out = buildClaudeEnv({}, cwd);
  assert.equal(out.HTTP_PROXY, undefined);
  assert.equal(out.HTTPS_PROXY, undefined);
  assert.equal(out.ALL_PROXY, undefined);
  assert.equal(out.NO_PROXY, undefined);
  assert.equal(buildClaudeEnv({ no_proxy: "example.invalid" }, cwd).NO_PROXY, "example.invalid");
});

test("explicit existing provider credentials stay compatible, but ANTHROPIC prefix is not a wildcard", () => {
  const source = { ANTHROPIC_API_KEY: "test-only", ANTHROPIC_AUTH_TOKEN: "test-only", ANTHROPIC_BASE_URL: "https://example.invalid" };
  assert.deepEqual(buildClaudeEnv(source, cwd), { ...source, CI: "1", GIT_CEILING_DIRECTORIES: path.dirname(cwd) });
});
