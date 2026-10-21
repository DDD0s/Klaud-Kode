# Kloud Kode feature inventory (main branch)

What Kloud Kode does today on `main`, as the owner built it. It was assembled by
reading every tracked file on `main` at `90753b7` (source, docs, configs, scripts,
tests, CI) and all 14 commit messages. Nothing here describes PR #1 except the last
section, which lists where PR #1's assumptions disagree with this design.

Versions on main: API `kloud-kode-api` 0.9.0, workstation `kloud-kode-body` 1.2.1.
Product name "Kloud Kode by Kosmolopic"; internal name `komputer` (earlier "AgentVR",
retired by an explicit, tested rename); the Body was earlier "cloudcode-body".

## 1. What it is, in one paragraph

A single-user service that exposes the **official, unmodified Claude Code CLI**, running
on a Linux host (the "brain"), as an OpenAI Chat Completions and Anthropic Messages
compatible HTTP API. Every model request is made by the real `claude` binary using its
own login; the service never reads, forwards or imitates Claude credentials. The work
itself happens on the user's own computer: either the user's local agent client (the
"harness") executes tools and returns results through the API, or a small MCP server
on the user's computer (the "Body") executes them for Claude Code over a private link.

The owner's two stated goals (`DEVELOPING.md`, review follow-up):

1. Feel like using the computer locally: files, commands and applications really run on
   the target computer, and paths, shell, exit codes and output are that computer's.
2. Keep the cloud host out of the model's context as far as possible ("the island"):
   built-in tools off, a neutral working directory, a filtered child environment. Not an
   OS sandbox, but "cannot be 100%" is not a reason to stop tightening it.

Hard rules that appear throughout: no token proxy or account pool; single user (all keys
are the same owner); public access only over TLS with strong keys; no real hosts, keys,
certificates, deployment targets or tokenised MCP files in the repo; internal errors are
never passed to clients verbatim.

## 2. Components

| Piece | Where it runs | What it does |
| --- | --- | --- |
| `komputer-api/server.mjs` | brain host | HTTP API, spawns `claude` per turn, Body sessions, SSE, error mapping |
| `komputer-api/client-tools.mjs` | brain host | Harness mode: per-run loopback MCP server that hands Claude's tool calls back to the client |
| `komputer-api/public-http.mjs` | brain host | TLS, CORS, failed-auth limiter, public-listener rules |
| `komputer-api/claude-env.mjs` | brain host | Allowlisted environment for the `claude` child |
| `kloud-kode-body/server.mjs` | user's computer | MCP server: shell, files, processes, nested local MCP servers |
| `komputer/tunnel-up.sh` | brain host | SSH local forward brain `127.0.0.1:18787` to Body `127.0.0.1:8787` (Body mode only) |
| `komputer-session/` | template | `.mcp.json` pointing Claude at the Body (`komputer_use`, Bearer token placeholder) and `settings.local.json` enabling it |
| `deploy/` | operator's machine | Update an existing brain install from `origin/main`, with rollback |

Data flow (`komputer/TRANSPORT.md`): harness on the user's computer, over HTTPS or a
private network, to `komputer-api` on the brain, which spawns `claude`, which talks to
Anthropic. In harness mode the tool calls come back down the same HTTP connection, so the
cloud never connects to the user's machine. In Body mode Claude reaches the Body through
the SSH tunnel or a private network such as Tailscale.

## 3. The three ways a request runs

The mode is chosen per request (`planTurn` in `server.mjs`):

- **Harness mode** (the daily path): the request has non-empty `tools`, an explicit
  `tool_choice`, or carries tool results. Claude gets only the client's tools, no
  built-ins, and no session persistence; the client supplies history.
- **Body mode**: no client tools. Claude gets the Body's MCP tools from `.mcp.json` and a
  persistent Claude session mapped to the client's conversation id.
- **Task mode**: the latest user message starts with `### Task:` (Open WebUI titles, tags,
  follow-ups). It runs once on `KOMPUTER_TASK_MODEL` (default `haiku`) with no tools, no
  MCP and no session.
- Body-mode requests with no conversation id, or with `X-Komputer-Ephemeral` /
  `komputer_ephemeral`, run **stateless**: one-shot, full history in the prompt, no
  mapping (`KOMPUTER_NO_KEY_MODE=stateless`, or `auto-session` to derive a key).

Responses carry `X-Komputer-Mode` (`harness`, `session`, `stateless` or `task`), plus
`X-Komputer-Run` in harness mode and `X-Komputer-Session`,
`X-Komputer-Claude-Session` and optionally `X-Komputer-Session-Recovered` in session
mode.

## 4. How the CLI is driven

One `claude` process per turn (`claudeArgs`, `runClaudeTurn`):

- Always: `-p --dangerously-skip-permissions --strict-mcp-config --output-format stream-json
  --verbose --include-partial-messages --append-system-prompt-file <0600 temp file>`.
- `--mcp-config <file>` unless the turn has no MCP (task mode).
- `--tools ""` by default (island mode), any other value of `KOMPUTER_BUILTIN_TOOLS`
  passed through; `default` omits the flag and restores Claude Code's normal tools.
- `--system-prompt-file` only when `KOMPUTER_SYSTEM_PROMPT_FILE` is set (experimental
  full replacement; missing, unreadable or directory paths fail at startup; relative
  paths are resolved before the cwd change).
- `--input-format stream-json` when the latest message has attachments.
- `--model`, `--effort`, and one of `--resume <uuid>`, `--session-id <uuid>` or
  `--no-session-persistence`.
- The prompt is written to stdin (no argv limit, not visible in `ps`, can never be parsed
  as a flag). A `.js`/`.mjs` `CLAUDE_BIN` is run with node.
- Timeout `KOMPUTER_CLAUDE_TIMEOUT_MS` (10 minutes), then SIGTERM, then SIGKILL 5 s later.
- The stream-json output is parsed for text deltas, thinking deltas, tool-use starts,
  the final result, usage and error text.

Appended system text:

- Body mode: `DEFAULT_WORK_NOTE`, "You work on ${BODY_NAME}. Your shell, files, processes
  and applications are all there, and the mcp__komputer_use__* tools are how you reach
  them … At the start of a new conversation, call system_info to learn the OS, shell,
  date, time zone, working roots and limits." `KOMPUTER_SYSTEM_PROMPT` replaces it, and
  an empty value sends none. `KOMPUTER_BODY_NAME` defaults to "the user's computer".
- Harness mode: "You work on ${BODY_NAME}. Use the available tools to work with the
  computer. Wait for tool results … Use the environment and shell conventions supplied
  in the conversation and tool descriptions …", then the client's own system text, then
  an optional tool-choice line ("Do not call any tools.", "Use at least one available
  tool…", or "For your first tool call, use k_… (name).").

Child process environment (`claude-env.mjs`): an allowlist (PATH, HOME, locale,
certificates, Windows login paths, `CLAUDE_CONFIG_DIR`, `CLAUDE_CODE_OAUTH_TOKEN`, MCP
timeouts, shell and terminal settings, telemetry, proxies, existing `ANTHROPIC_API_KEY` /
`ANTHROPIC_AUTH_TOKEN`), plus exact names from `KOMPUTER_CHILD_ENV_EXTRA`. Names starting
with `KOMPUTER_`, `BODY_` or `KLOUD_KODE_`, and launcher settings, are always excluded.
It also sets `CI=1` and `GIT_CEILING_DIRECTORIES` to the parent of the canonical cwd, so
git cannot discover a repository above it. Upper- and lowercase proxy variables are
normalised (uppercase wins), no proxy is invented, and harness runs get `127.0.0.1` added
to `NO_PROXY` for the loopback MCP server. An `ANTHROPIC_API_KEY` in the API's own
environment logs a billing warning.

Working directory: `~/.komputer/workspaces/<first 16 hex of sha256(session dir)>`, outside
the checkout (`KOMPUTER_CLAUDE_CWD` overrides). This keeps the project name and parent
repository out of Claude's `<env>` block.

## 5. "Redirecting built-in tools to the user's machine": what main actually does

**Main does not redirect Claude Code's built-in tools. It removes them.** `--tools ""`
turns off Bash, Read, Write, Edit, Glob, Grep, Task and web tools, and the model works
only through MCP tools that execute on the user's computer:

- in Body mode, the Body's `mcp__komputer_use__*` tools (shell, files, processes, nested MCP);
- in harness mode, the client's own tools, re-exposed as `mcp__komputer_use__k_<i>_<hash>`
  and executed by the client.

`docs/tool-redirection.md` (2026-09-26) is the owner's evaluation of real redirection. It
keeps `--tools ""` plus `komputer_use` and ships no prototype, with these findings:

- a shell wrapper misses Read/Write/Edit/Glob/Grep, and cwd, quoting, cancel, exit codes,
  background processes and Windows shell semantics would all need adapting;
- PreToolUse can rewrite or block a call, but rewriting arguments does not swap the tool's
  implementation, and a denial is failure feedback, not a successful result;
- PostToolUse runs after the side effect already happened on the brain;
- a network filesystem mount still searches with the brain's CPU and moves file contents,
  contrary to "compute also happens on B";
- path mapping only makes sense on top of real execution forwarding.

The suggested next step there is to research a shell wrapper in an isolated instance with
only Bash enabled and native file tools kept off, and to accept it only by checking side
effects on both machines, not by what the model claims. The review follow-up rejects A2
("restore all native tools by default") because native tools would run on the CLI host,
and `komputer-migration.md` says the project "will not move operations back onto the
execution host just to look native".

## 6. Windows and the "brain in a vat"

What exists on main is **island mode L2** (`cfa0a59`, API 0.7.0 / Body 1.2.0; "Island mode
(L2) is on by default" in `env.example`). Its aim is that the model sees exactly one
computer, the one it operates. The mechanisms are:

- **Removal, not faking:** no built-in tools, so nothing can act on the brain.
- **Single-machine voice:** model-visible text (system note, MCP server and tool names,
  descriptions, outputs) describes one computer and never narrates the split (`3a11b52`
  "Stop narrating the plumbing"; `body_info` became `system_info`; nested MCP servers are
  "additional tool providers"; a Body test asserts no tool text mentions a second machine
  and `system_info` says nothing about the wrapper).
- **Environment from the real machine:** `system_info` reports the Body's host, platform,
  release, OS version, arch, UTC time, local date, time zone and offset, shell, default
  shell and shell list, console encoding, roots and limits. The work note tells the model
  to call it first and use its results for commands and paths.
- **Windows-aware Body:** default shell is `cmd.exe` on Windows, `run_command` can pick
  `powershell`, `pwsh`, `cmd`, `bash` or `sh` (Body 1.1.0), descriptions warn about
  prompting cmd built-ins such as bare `date`, output is decoded with the console code page
  (cp932, gbk and others) when it is not UTF-8, process trees are killed with `taskkill`.
- **Brain hygiene:** neutral cwd, git ceiling, filtered environment, no `CLAUDE.md` or
  memo auto-loaded from the checkout, and the advice to run the service as a separate
  plain user so the brain's `~/.claude` carries no personal skills or memory.

What main does **not** do: it does not spoof the OS, does not rewrite Claude Code's base
prompt, does not map Linux paths to Windows paths, has no hooks, no shell shim, no mount
and no built-in-tool guard. The known residual is documented: Claude Code's own `<env>`
block still says `Platform: linux` and shows the brain's cwd. The only remedies are the
experimental `KOMPUTER_SYSTEM_PROMPT_FILE` full replacement (which must re-teach tool use)
or running the brain on the same OS. `--exclude-dynamic-system-prompt-sections` is noted as
moving, not hiding, that information.

## 7. Body (`kloud-kode-body` 1.2.1)

An MCP server (server name `komputer`; the brain's `.mcp.json` names it `komputer_use`).

| Group | Tools |
| --- | --- |
| Information | `system_info` |
| Shell | `run_command` (command, shell, cwd, timeoutMs, root; stdin closed; stdout/stderr separate; exit code, signal, timedOut, duration, encoding, dropped bytes) |
| Processes | `start_process`, `read_process` (byte cursors, reports missed bytes), `write_process`, `stop_process` (kills the tree), `list_processes` |
| Files | `read_file` (whole file or line window, windowed reads work past the size limit), `write_file` (refuses to clobber unless `overwrite`), `edit_file` (exact text must occur once), `list_directory`, `find_files` (glob), `search_files` (text or regex, context lines, skips binaries), `stat_path`, `create_directory`, `delete_path` (refuses the root; directories need `recursive`), `move_path` |
| Nested MCP | `list_mcp_servers`, `list_mcp_tools`, `call_mcp_tool` (stdio MCP servers from config, connected lazily; their descriptions are flagged as untrusted) |

Behaviour and limits: output is kept head 30% and tail 70% with a "bytes dropped from the
middle" marker (256 KiB), files up to 8 MiB, 30 s default command timeout (max 10 min),
common build and VCS directories skipped when walking. Strict UTF-8 is tried first; on
failure the console code page is used, or byte-preserving latin1 labelled as such.
`roots` (default: home) confine only the file tools and the cwd, not what a shell command
can touch. There are no per-call approvals. Commands get the full environment minus the
Body's own token and config variables. `files`, `shell` and `processes` can be switched off.

Transport: HTTP on `127.0.0.1:8787` by default, `/healthz` without auth (service and
version only), `/mcp` or `/mcp/<token>` with the token in the path or a Bearer header,
compared as SHA-256 digests in constant time, 1 s delay on failure, POST only, 16 MB body
limit, a fresh stateless MCP server per request while processes and nested clients live in
shared state. HTTP mode refuses to start without a token of at least 32 characters, and
warns when listening beyond loopback. `stdio` mode is also available. Config lives in
`~/.kloud-kode-body/config.json` (`KLOUD_KODE_BODY_CONFIG`), token via
`KLOUD_KODE_BODY_TOKEN`.

Body reachability from the API: `ensureTunnel` probes `KOMPUTER_HEALTH_URL`; on failure
it runs `tunnel-up.sh` once (single-flight). Success is cached 15 s, failure 30 s
(`KOMPUTER_TUNNEL_FAIL_CACHE_MS`), and requests during the backoff get 503
`body_unavailable` with a matching `Retry-After`. `KOMPUTER_SKIP_TUNNEL=1` skips it.

## 8. HTTP API

| Endpoint | Notes |
| --- | --- |
| `GET /healthz` (`/health`) | Anonymous: `{ok, service}` only. With a key: model ids, sessions, live sessions, in-flight and queued turns, limits, auth failures, `builtin_tools`, cached `body_ok` / checked-at / skipped, harness `client_tools` stats, `no_key_mode`, listen addresses |
| `GET /v1/models` (`/models`) | `komputer-claude` plus `komputer-opus`, `-sonnet`, `-haiku`, `-fable` (`KOMPUTER_MODELS`), `owned_by: komputer` |
| `POST /v1/chat/completions` | OpenAI format, JSON or SSE |
| `POST /v1/messages` | Anthropic format, JSON or SSE |
| `POST /v1/messages/count_tokens` | Estimate: characters / 3.5 |
| `POST/GET /v1/sessions`, `GET/DELETE /v1/sessions/:id` | Create, list, inspect and forget Body session mappings |
| `DELETE /v1/tool-runs/:id` | Cancel a pending harness run |

The models, chat completions, messages and count_tokens paths also work without the
`/v1` prefix. Every response has `Cache-Control: no-store` and
`X-Content-Type-Options: nosniff`.

Models and effort: `komputer-<alias>` runs `claude --model <alias>`; `claude-*` ids and the
names opus, sonnet, haiku, fable and mythos pass through; unknown ids use the default.
Effort comes from `reasoning_effort`, `effort` or `output_config.effort` (low to max;
minimal and none become low), or from `thinking.budget_tokens` (32k or more is max, 16k
or more is xhigh, otherwise high).

Attachments: OpenAI `image_url` and PDF `file`, Anthropic `image` and `document` blocks are
sent as stream-json content blocks. Text files are decoded into the prompt. Only the latest
user message's attachments are sent; earlier ones become text placeholders.

Streaming: the 200 is held back until Claude produces output or 25 s pass
(`KOMPUTER_SSE_OPEN_MS`), so early failures get a real HTTP status. After that, errors are
reported inside the stream. Keepalive comments every 15 s. OpenAI streams text deltas,
`reasoning_content` for thinking, `[tool] name` progress notes in Body mode, usage and
`[DONE]`. Anthropic streams the standard event sequence. Harness-mode Messages never emits
thinking blocks, because no verifiable signature is available.

Errors: mapped to neutral public messages with the real status and type, for example
upstream login failure 502 `upstream_auth_error`, usage limit 429 `rate_limit_error` with
`Retry-After: 60`, overloaded 503, session conflict 409, Body unavailable 503 with
`Retry-After`, timeout 504, bad JSON 400. Diagnostics stay in the server log with Bearer
tokens, `/mcp/<token>` URLs and `sk-` keys redacted.

### Harness tool protocol (`client-tools.mjs`, `docs/harness-api.md`)

- OpenAI `tools[].function` and Anthropic `tools[]` with object JSON Schemas (draft-07 or
  2020-12, validated with Ajv). Names are 1 to 64 letters, digits, `_` or `-`; duplicates
  and invalid schemas are rejected before any process starts.
- A per-run HTTP MCP server on `127.0.0.1:<random port>` named `komputer_use`, with a
  random Bearer token, Host and Origin checks and a 1 MB request limit. Tools appear as
  `k_<index>_<12 hex of sha256(JSON name)>` with the original name as title.
- Claude's MCP `tools/call` is held open; the API returns it as `tool_calls`
  (`finish_reason: tool_calls`) or `tool_use` (`stop_reason: tool_use`). The client runs it
  and posts the results; the same Claude process continues.
- `tool_choice` auto, none, required or any, or a named tool; `parallel_tool_calls: false`
  and `disable_parallel_tool_use` deliver one call per response. A batch must be answered
  completely; results can be text or base64 images.
- While waiting, the run gives up its compute slot (so a nested harness call cannot
  deadlock). Defaults: 5 min wait, 16 runs, 4 MB output, 8 invalid calls before failing.
- Retries of the same request or the same results replay stable ids without a second model
  run. A conflicting result returns 409; an expired or unknown run returns 410. State is in
  memory only.
- Not supported: Responses API, legacy `functions`, provider server tools, `n > 1`,
  external schema `$ref`, async validation.

## 9. Sessions and reliability (Body mode)

- `sessions.json` (mode 0600, atomic writes) maps a client key to a Claude session UUID.
- The key is taken from headers `X-Conversation-Id`, `X-Chat-Id`, `X-OpenWebUI-Chat-Id`,
  `X-Open-WebUI-Chat-Id`, `X-LibreChat-Conversation-Id`, `X-Komputer-Session(-Id)` or
  `X-Session-Id`; or from body fields `conversation_id`, `chat_id`, `thread_id`,
  `metadata.*`, `session_id`, `komputer_session` or `komputer_conversation_id`; or from
  `user` / `metadata.user_id`.
- First turn uses `--session-id`, later turns `--resume`. A new conversation that arrives
  with history gets that history (up to 60,000 characters) in its first prompt.
- One FIFO lock per conversation; at most 4 turns computing at once (`KOMPUTER_MAX_IN_FLIGHT`);
  unlimited mappings by default; 45 minutes idle only stops counting a session as live.
- Regenerate and edit detection compares a 160-character tail of the last reply; a
  divergent history starts a fresh Claude session seeded from the client history. Clients
  that send only the newest message keep resuming.
- A "no conversation found" error before any output or tool activity reseeds once from
  client history (`X-Komputer-Session-Recovered: client-history` or `empty`); after output
  or a tool call it is never replayed. A failed first turn rotates the UUID.
- Turn registry: a dropped client does not kill the turn; a retry attaches to the running
  turn or receives the cached result for 10 minutes. The turn key hashes the full
  messages, system text, tools, MCP and task flag, so different contexts never merge.

## 10. Auth and public HTTP

- Keys in `KEYS.txt` (`api-key:` lines, legacy prefixed lines, `key=` or bare `sk-…`),
  compared as SHA-256 digests in constant time, accepted as `Authorization: Bearer` or
  `x-api-key`. `SIGHUP` reloads them. Each failure is delayed 1 s and failures are limited
  to 20 per minute per socket address (`X-Forwarded-For` is ignored).
- Public mode (`KOMPUTER_PUBLIC_API`, a TLS certificate, or any non-private listen address)
  requires keys of at least 32 characters. Public or wildcard listeners need TLS
  (`KOMPUTER_TLS_CERT_FILE` / `KEY_FILE` / `KEY_PASSPHRASE`, TLS 1.2 minimum) unless
  `KOMPUTER_ALLOW_INSECURE_HTTP=1` declares a protected proxy in front. Invalid TLS files
  fail startup before any listener opens.
- CORS only for exact origins in `KOMPUTER_CORS_ORIGINS` (no `*`, no cookies); preflight
  does not bypass auth.
- Several listen addresses (`KOMPUTER_API_HOST=a,b:port`); if any bind fails the process
  exits 1 rather than run half-live. 512 connections maximum, 15 s header and 30 s request
  timeouts, 50 MB body limit.

## 11. Deployment and operations

- `start.sh` sources `komputer-api/env`, finds node (`NODE_BIN`), applies defaults
  (127.0.0.1:18888, 4 in flight), tries the tunnel once, starts `server.mjs` with nohup,
  writes `komputer-api.pid` and logs to `logs/komputer-api.log`. `stop.sh` sends TERM, then
  KILL. There is no systemd unit.
- `deploy/deploy.sh` (from Git Bash or Linux, targets in git-ignored `deploy/target`)
  refuses uncommitted changes or a HEAD different from `origin/main`, runs the API tests,
  then streams `remote-swap.sh` to the brain over ssh.
- `remote-swap.sh` fetches `origin/main`, refuses revisions without the komputer layout,
  runs `node --check` on the new server, waits up to 5 minutes for in-flight turns to
  finish, records `.deploy-prev`, resets to the new commit while preserving the real
  tokenised `komputer-session/.mcp.json` (kept `skip-worktree`), runs `npm ci`, restarts
  and health-checks (authenticated `/healthz` must contain `builtin_tools`;
  `KOMPUTER_DEPLOY_HEALTH_URL` is required with TLS). On failure it goes back to the
  previous commit. `deploy.sh rollback` runs `remote-rollback.sh` on demand.
- `docs/komputer-migration.md` covers the one-time move from the old layout and names,
  which the scripts deliberately refuse to cross.
- `SETUP.md` is the manual install guide (brain, Body, SSH, tokens, MCP file); there is no
  installer.

## 12. Tests and CI

GitHub Actions on Linux, Node 22 and 24: `npm ci` and `npm test` for the API and the Body.
The API tests use a fake `claude` (`test/fake-claude.mjs`, driven by prompt keywords, which
records argv, prompt, system file, attachments, cwd and leaked environment names) but real
HTTP, MCP, HTTPS and processes. What they assert:

- `server.test.mjs`: anonymous health reveals nothing; key checks; the island flag set
  (`--session-id`, stdin prompt, system file, `--tools ""`); resume; history seeding; both
  SSE formats, keepalives and thinking; error mapping and UUID rotation; retry attach and
  cached replay; real status before streaming; `Retry-After`; a single shared Body probe;
  model and effort mapping; stateless, ephemeral and task modes; regenerate detection;
  attachments; count_tokens; a second listener; the child sees no wrapper environment and
  gets a git ceiling; `KOMPUTER_BUILTIN_TOOLS=default` and `KOMPUTER_SYSTEM_PROMPT_FILE`.
- `client-tools.test.mjs`: harness round trips in both formats, streaming tool-call deltas,
  parallel batches, single-call delivery, cancellation, stable retry ids, no deadlock at
  concurrency 1, tool choice, Unicode chunking, expiry and schema changes, invalid tools.
- `reliability.test.mjs`: the git ceiling really blocks discovery, startup validation,
  failed secondary bind exits, neutral cwd, naming in metadata, missing-session reseed
  rules, error redaction in JSON and both SSE formats, no merging of different contexts,
  tunnel backoff.
- `public-http.test.mjs`: insecure public HTTP refused, per-socket failure limits, CORS,
  malformed targets, bad TLS files, short keys, a full harness round trip over verified HTTPS.
- `claude-env.test.mjs` (allowlist, extras, proxies) and `naming.test.mjs` (no retired
  name anywhere, scripts parse in bash, deploy scripts refuse pre-migration revisions).
- Body `body.test.mjs` and `encoding.test.mjs`: token handling, a real MCP SDK client,
  tool list, "never narrate a second machine", command output and timeouts, code-page
  decoding, head-and-tail truncation, file tools and root escapes, search, processes with
  cursors, `system_info`, nested MCP errors, shell selection, prompting built-ins failing fast.

Not covered by CI: real Claude, a real Windows Body, real SSH or Tailscale, systemd,
public domains, specific harness clients.

## 13. Unfinished or TODO (ROADMAP, README, review follow-up)

- Integration with real harness clients (permissions, long tasks, reconnects, cancel).
- A one-click installer for brain and client; today it is manual.
- A full end-to-end test with real public HTTPS, real Claude Code and local tools.
- Process supervision (systemd or similar); only PID files exist. A separate tunnel
  service was suggested but not done.
- GUI, browser and application control on the user's computer, and in which user session
  they run; there is no desktop-control toolset.
- Choosing between several user computers, with permissions and no mixing of tools or history.
- Cloud leaks that remain: Claude Code's own prompt (`Platform: linux`, cwd), user config,
  memory and skills on the brain.
- Harness state survives only in memory; a restart loses pending tool calls.
- Body permissions: roots do not confine the shell and there are no approvals; Claude is
  always started with `--dangerously-skip-permissions`, and permission modes are not
  synchronised with the client.
- No cleanup policy for session mappings and Claude transcripts.
- Native tool redirection stays a research item (section 5).
- Verify output, encodings, paths and long processes on real machines, and pin the CLI
  version to verify system-prompt updates on resume, Windows paths, timeouts and recovery.
- The root README's title block currently reads "CAPTURED / KERNEL SENSOR: EMURATED..."
  (commit `90753b7`), which looks like placeholder text.

## 14. History on main

| Commit | Change |
| --- | --- |
| `fc9ca7b` | Initial review snapshot (AgentVR naming; built-in tools still default) |
| `5a410a9`, `5e07c08` | Credit Claude; executable bits and ignores |
| `3a11b52` | Stop narrating the plumbing to the model |
| `55fd7fb` | Body 1.1.0: `run_command` picks its shell |
| `318fc17` | MCP server named `komputer_use` |
| `9af6caa` | Deploy by having the brain pull `origin/main` |
| `cfa0a59` | Island mode: remove brain-local tools, seal the wrapper (API 0.7.0, Body 1.2.0) |
| `8a75a39` | `fable` as a first-class model |
| `97ac574` | Harness tool bridge and public HTTPS (API 0.9.0), komputer rename, review fixes |
| `9b5d4ba` | POSIX process state and lossless encoding fallback (Body 1.2.1) |
| `1d79fb9`, `176ecf9`, `90753b7` | README rewrites |

## 15. Where PR #1 disagreed with this design

PR #1 (multi-backend `/v1/responses`, Antigravity `agy` and Muse backends) was built partly
on the premise that main already makes the CLI believe it runs on the user's Windows
machine by faking the OS and redirecting built-in tools. **Main does neither.** Main removes
the built-ins and gives the model the Body's (or the client's) MCP tools, describes one
computer, reports the real OS through `system_info`, and accepts the `Platform: linux`
residual. With that baseline, these PR #1 assumptions contradict the design:

1. **The built-in-tool guard.** `KOMPUTER_AGY_BUILTIN_GUARD` (`stop` / `warn` / `off`) and
   the `builtin_tool_blocked` 502 error are new mechanisms. Main has no guard because it
   has nothing to guard: isolation is structural (`--tools ""` plus `--strict-mcp-config`).
   For agy the equivalent is to not offer its built-ins at all, where the CLI allows it.
2. **"MCP-only tools" treated as a limitation.** PR #1 treated the model using MCP tools
   instead of the CLI's own tools as a gap to close. On main it is the design: the Body's
   MCP tools (or the client's) are the hands, and `tool-redirection.md` explains why.
3. **Redirecting built-ins** (the agy bash shim, the `komputer-fs.py` FUSE mount, path
   mapping, the PreToolUse hook). `tool-redirection.md` evaluated exactly these (shell
   wrapper, PreToolUse, mount, path mapping) and did not ship them; review item A2 was
   rejected and the migration notes refuse to move operations back to the brain to look
   native. In PR #1 this is now opt-in (`KOMPUTER_AGY_REMOTE_TOOLS=off` by default), but it
   is still a parallel mechanism the owner declined.
4. **Windows faking.** The per-turn `<user_computer>` note, PowerShell translation and
   `C:\` path mapping that PR #1 prototyped (and later reverted) have no counterpart on
   main, which does not spoof the OS. Body shell selection was already on main (Body
   1.1.0); it was not new.
5. **Narrating the split to the model.** The agy hook's deny reasons say tools "would act
   on the service host" and paths are "on the service host". Those messages reach the
   model, and main's rule is that model-visible text never mentions a second machine.
   The client-facing `builtin_tool_blocked` message also refers to the service host.
6. **Prompt wrapping.** PR #1 wraps system text in `<system_instructions>` for backends
   without a system flag. That is neutral in wording, but it is new model-visible framing
   worth checking against the single-machine voice.
7. **Responses API.** `docs/harness-api.md` lists the Responses API as unsupported. PR #1
   adds `/v1/responses`. That is an extension, not a conflict, but main's docs then need
   updating.

Consistent with the design: reusing island mode, the work note, `system_info` and the Body
for agy and Muse; keeping Claude's behaviour byte-identical by default
(`KOMPUTER_BACKENDS=claude`); and Body 1.3.0's fidelity work (shell detection, `wsl`,
PowerShell UTF-8), which makes the real computer report itself more accurately rather than
faking anything on the brain.
