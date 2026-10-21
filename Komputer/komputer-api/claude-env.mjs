import path from "node:path";

// Keep runtime/login configuration, not the API process's deployment secrets.
// Deliberately list credential/provider variables: a new ANTHROPIC_* variable
// must not silently become part of the child's environment.
const BASE = [
  "PATH", "HOME", "USER", "LOGNAME", "LANG", "TZ", "TMPDIR", "TEMP", "TMP",
  "SYSTEMROOT", "COMSPEC", "PATHEXT", "OS", "NUMBER_OF_PROCESSORS",
  "USERPROFILE", "APPDATA", "LOCALAPPDATA", "HOMEDRIVE", "HOMEPATH",
  "SHELL", "TERM", "SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS",
];
const KEEP = new Set([
  ...BASE,
  "CLAUDE_CONFIG_DIR", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_USE_POWERSHELL_TOOL",
  "MCP_TIMEOUT", "MCP_TOOL_TIMEOUT", "DISABLE_AUTOUPDATER", "DISABLE_TELEMETRY",
  "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL",
  "ANTHROPIC_DEFAULT_MODEL", "ANTHROPIC_DEFAULT_FABLE_MODEL",
  "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL",
  "ANTHROPIC_SMALL_FAST_MODEL",
  "FAKE_CLAUDE_STATE", // test double only; never populated by the service
]);
/** Antigravity CLI: its own login lives under HOME; API-key / ADC auth is opt-in by the operator. */
export const AGY_KEEP = new Set([
  ...BASE,
  "GEMINI_API_KEY", "GOOGLE_API_KEY", "GOOGLE_APPLICATION_CREDENTIALS", "GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_LOCATION",
  "FAKE_AGY_STATE", // test double only
]);
/** Muse Code CLI: `muse login` state lives under HOME / XDG_CONFIG_HOME; META_API_KEY is the documented alternative. */
export const MUSE_KEEP = new Set([
  ...BASE,
  "META_API_KEY",
  "FAKE_MUSE_STATE", // test double only
]);
const RESERVED = /^(?:KOMPUTER_|BODY_|KLOUD_KODE_)|^(?:CLAUDE_BIN|AGY_BIN|MUSE_BIN|NODE_BIN|CI|GIT_CEILING_DIRECTORIES)$/i;

/** cwd must be the absolute, canonical working directory used by the child. */
export function buildChildEnv(source, cwd, keep) {
  const extra = new Set((source.KOMPUTER_CHILD_ENV_EXTRA || "").split(",").map((k) => k.trim()).filter(Boolean));
  const out = {};
  for (const [k, v] of Object.entries(source)) {
    if (v === undefined || RESERVED.test(k)) continue;
    if (keep.has(k.toUpperCase()) || /^(LC_|XDG_|PROCESSOR_)/.test(k) || extra.has(k)) out[k] = v;
  }
  out.CI = "1";
  // Git ignores a ceiling equal to cwd. Stop before entering its parent.
  // This is not a sandbox: a .git IN cwd is still visible.
  out.GIT_CEILING_DIRECTORIES = path.dirname(cwd);

  const http = source.HTTP_PROXY || source.http_proxy;
  const https = source.HTTPS_PROXY || source.https_proxy;
  const all = source.ALL_PROXY || source.all_proxy;
  if (http || https) {
    out.HTTP_PROXY = out.http_proxy = http || https;
    out.HTTPS_PROXY = out.https_proxy = https || http;
  }
  if (all) out.ALL_PROXY = out.all_proxy = all;
  const noProxy = source.NO_PROXY ?? source.no_proxy ??
    (http || https || all ? "127.0.0.1,localhost,::1,*.ts.net,100.64.0.0/10,10.0.0.0/8" : undefined);
  if (noProxy !== undefined) out.NO_PROXY = out.no_proxy = noProxy;
  return out;
}

export function buildClaudeEnv(source, cwd) {
  return buildChildEnv(source, cwd, KEEP);
}
