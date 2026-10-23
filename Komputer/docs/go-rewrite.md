# Go rewrite status

Status: in progress on branch `rewrite/go`. The JavaScript implementation
remains the default and the behavior oracle until the Go binary passes the
same contracts slice by slice. Do not switch deployment defaults yet.

## End state

One Go binary with subcommands:

- `kloud-kode api` — the brain-host API service
- `kloud-kode body` — the caller-computer execution layer
- `kloud-kode bridge|hook|shim` — small CLI helper entrypoints (not yet)

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
- Body health probe / tunnel revive, TLS listener support, exact-origin CORS.
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

Explicitly not implemented yet (requests fail with 501 where applicable):

- Key reload on SIGHUP and the connection cap

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
- `go vet ./...` passes for the host and with `GOOS=linux`.
- Linux and Windows binaries cross-compile (about 12 MB each).
- Fake-CLI results only. No signed-in CLI or production deployment has run
  the Go binary yet; the JavaScript suites remain untouched and green.

## Next slices

1. Caller-environment binding and the model-visible leak gate from
   `docs/REMOTE-BRAIN-DESIGN.md`, then a real signed-in acceptance run.
2. Deployment switch with the JavaScript build kept as the rollback.
