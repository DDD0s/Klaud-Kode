# Go rewrite status

Status: in progress on branch `rewrite/go`. The JavaScript implementation
remains the default and the behavior oracle until the Go binary passes the
same contracts slice by slice. Do not switch deployment defaults yet.

## End state

One Go binary with subcommands:

- `kloud-kode api` — the brain-host API service
- `kloud-kode body` — the caller-computer execution layer (not yet)
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

- Claude backend only: exact argv shape, stream-json parser, child
  environment allowlist, appended system prompt file, attachments via
  `--input-format stream-json`.
- `POST /v1/chat/completions` and `POST /v1/messages`:
  streaming (deferred SSE) and non-streaming, stateless, task and session
  turns, divergence reseeding, missing-session recovery, duplicate-turn
  attach/replay, global in-flight cap and per-session serialization.
- `GET /v1/models`, `/v1/sessions` CRUD subset, `POST /v1/messages/count_tokens`,
  `GET /healthz`.
- API key loading from the existing KEYS.txt formats, constant-time
  comparison, failed-auth delay and per-address limiter.
- Body health probe / tunnel revive, TLS listener support, exact-origin CORS.
- Existing sessions.json files load; Claude records keep their on-disk shape.
- Harness client tools for Chat and Messages: per-run authenticated loopback
  MCP server, JSON Schema validation before a process starts, tool-choice
  and parallel-call policies, batched sealing, compute-lease release while
  waiting, exact-batch result validation, identical-retry replay,
  continuation conflict checks, output limits, explicit run cancellation
  (`DELETE /v1/tool-runs/:id`) and health stats.

Explicitly not implemented yet (requests fail with 501 where applicable):

- `POST /v1/responses` and the stored-response API
- Antigravity and Muse backends, all agy helper processes, FUSE
- The Body itself (`kloud-kode body`): the JavaScript Body remains the
  execution layer, reached by Claude through the existing .mcp.json
- Key reload on SIGHUP and the connection cap

## Validation so far

- `go test ./...` passes on Windows (Go 1.27.3), including HTTP
  integration tests that run the existing `test/fake-claude.mjs` double:
  stateless chat, streamed chat, session create and `--resume`, Anthropic
  Messages, auth rejection, child-env non-leakage, plus OpenAI and Anthropic
  harness tool cycles that continue the same fake-Claude process and an
  explicit tool-run cancellation.
- `go vet ./...` passes for the host and with `GOOS=linux`.
- Linux and Windows binaries cross-compile (about 12 MB each).
- Fake-CLI results only. No signed-in CLI or production deployment has run
  the Go binary yet; the JavaScript suites remain untouched and green.

## Next slices

1. Responses (`POST /v1/responses`, stored responses, previous-response chains).
2. `kloud-kode body` against the existing Body MCP contract and shell tests.
3. Antigravity and Muse backends, including their helper subcommands.
4. Caller-environment binding and the model-visible leak gate from
   `docs/REMOTE-BRAIN-DESIGN.md`, then a real signed-in acceptance run.
5. Deployment switch with the JavaScript build kept as the rollback.
