# Go rewrite status

Status: merged in `main` and now the deployment default. Releases publish
checksum-verified binaries for Linux, Windows and macOS (amd64/arm64);
`start.sh`, `start.ps1` and the Body launchers download the version named by
`Komputer/VERSION` into `bin/`, so neither host needs a Go toolchain and an
upgrade never overwrites the file a running service holds. `KLAUD_KODE_BUILD=1`
builds from a checkout instead. The JavaScript implementation stays in the
tree as the rollback path (`KOMPUTER_IMPL=node`) and as the behavior oracle
for tests.

What has not happened yet: no signed-in Claude, agy or muse turn has run the
Go code, and no deployment has served a real harness over it. Treat the first
real run as the acceptance test.

## End state

One Go binary with subcommands:

- `klaud-kode api` — the brain-host API service
- `klaud-kode body` — the caller-computer execution layer
- `klaud-kode bridge|hook|shim` — small CLI helper entrypoints (not yet)

Internal layering is protocol-neutral at the center:

- `internal/domain`: canonical messages, events, tool payloads and errors
- `internal/backend`: one official CLI per adapter (argv, stdin, parser)
- `internal/runner`: child process execution, timeout and stream plumbing
- `internal/api`: public protocol adapters and turn/session service
- `internal/session`: the existing sessions.json format
- `internal/childenv`: the child environment allowlist
- `internal/config`: the existing environment variable names and defaults

Public wire protocols (OpenAI Chat, Anthropic Messages, OpenAI Responses)
stay at the edge. None of them is allowed to become the internal model.

## Implemented so far

- Claude, Muse and Antigravity backends behind one registry (exact
  advertised ids, then backend patterns). Muse covers argv and prompt
  files, JSONL parsing with missing-session bail, usage, env-interpolated
  MCP `-c` overrides, base64 images, inline-system session hashing and
  error classification. Antigravity covers stream-json turns with
  conversation resume and missing-conversation bail, the globally
  registered stdio bridge and PreToolUse hook (`bridge` / `hook`
  subcommands), island and remote tool policies, the workspace bash shim
  (`shim`) that runs `run_command` on the caller's computer, and the
  optional FUSE mount of the caller's files (embedded Python helper).
- Claude exact argv shape, stream-json parser, child environment
  allowlist, appended system prompt file, attachments via
  `--input-format stream-json`.
- Chat Completions and Anthropic Messages: streaming (deferred SSE) and
  non-streaming, stateless, task and session turns, divergence reseeding,
  missing-session recovery, duplicate-turn attach/replay, global in-flight
  cap and per-session serialization.
- `GET /v1/models`, `/v1/sessions` CRUD subset, `POST /v1/messages/count_tokens`,
  `GET /healthz`.
- API key loading from the existing KEYS.txt formats, constant-time
  comparison, failed-auth delay and per-address limiter.
- Body transport without SSH, two directions: `KOMPUTER_BODY_URL` points the API at
  the Body's MCP endpoint directly, skips the SSH tunnel, probes the Body for
  health, reports `body_direct` / `body_ok` (token masked) and generates the
  MCP config file from that URL. The Body can bind a private-network address
  through `host` / `KLAUD_KODE_BODY_HOST` and warns when it only binds loopback.
  With `KOMPUTER_BODY_CONNECT=1` the Body instead dials out over one WebSocket
  (`klaud-kode body connect`) and the API proxies MCP over it from its own
  loopback, for computers nobody can dial into; /healthz reports
  `body_connected` and the Body redials forever with backoff.
- Body health probe / tunnel revive, TLS listener support, exact-origin CORS,
  SIGHUP key reload (old keys kept on failure) and the connection cap.
- Existing sessions.json files load; Claude records keep their on-disk shape.
- The Body itself: the same config file, HTTP and stdio MCP transports,
  token auth, health endpoint, roots, files, sync commands, long-lived
  interactive processes, platform shell detection and Windows PowerShell /
  cmd UTF-8 handling, nested stdio MCP forwarding through `call_mcp_tool`.
- `POST /v1/responses` in Body and harness modes, stored responses with
  `previous_response_id` chains mapped onto CLI sessions, `GET`/`DELETE`
  retrieval, text-only persisted history (attachments are never stored),
  `store: false`, and the full streaming event sequence.
- Harness client tools for Chat and Messages: per-run authenticated loopback
  MCP server, JSON Schema validation before a process starts, tool-choice
  and parallel-call policies, batched sealing, compute-lease release while
  waiting, exact-batch result validation, identical-retry replay,
  continuation conflict checks, output limits, explicit run cancellation
  (`DELETE /v1/tool-runs/:id`) and health stats.

## Validation so far

- `go test ./...` passes on Windows (Go 1.27.3), including HTTP
  integration tests that run the existing `test/fake-claude.mjs` double:
  stateless chat, streamed chat, session create and `--resume`, Anthropic
  Messages, auth rejection, child-env non-leakage, plus OpenAI and Anthropic
  harness tool cycles that continue the same fake-Claude process, Muse
  session create/resume and missing-session reseeding, images and errors, an
  explicit tool-run cancellation, Responses non-streaming/streaming,
  previous-response session resume, function tools through the harness,
  image-store privacy and delete, and Antigravity session create/resume
  through the real Go bridge process plus client tools on one continuing
  fake-agy process.
- `go vet ./...` passes for the host and with `GOOS=linux`, and every release
  target cross-compiles in CI, so a tag build cannot fail after merge.
- The v1.0.1 review added regression tests for the launcher checksum path and
  versioned install, the Body token in URL paths and GET streams,
  percent-encoded ids, UTF-16 token estimates, the auth limiter bounds, the
  encrypted-PKCS#8 TLS key path and the work-directory fallback.
- Fake-CLI results only. No signed-in CLI or production deployment has run
  the Go binary yet; the JavaScript suites remain untouched and green.

## Next slices

1. Caller-environment binding and the model-visible leak gate from
   `docs/REMOTE-BRAIN-DESIGN.md`, then a real signed-in acceptance run.

## Known limits

- On a Windows host whose antivirus denies writes from an unseen executable for
  a whole run, the work directory falls back to `komputer-api/work-<backend>` and
  the service says so; set `KOMPUTER_WORKSPACE_ROOT` to choose explicitly.
- The encrypted-key path accepts PBES2 PKCS#8 only (what `openssl genpkey`
  writes); a legacy `openssl rsa -aes256` PEM key is reported, not decrypted.
