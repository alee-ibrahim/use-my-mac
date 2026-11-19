# CLAUDE.md

Guidance for AI coding agents working in this repository.

## What this is

use-my-mac: a router (`cmd/umm-server`) and a macOS agent (`cmd/umm-agent`) that turn Apple Silicon
Macs into a compute pool, plus a CLI (`cmd/umm`), a Go SDK (`pkg/client`) and a React dashboard
(`dashboard/`). Read `README.md`, then `docs/design.md` for scheduling, yield policy, storage, security and
failure handling.

## Layout

- `pkg/api`: wire types shared by everything. Change carefully; it is the public API.
- `internal/sched`: pure scoring/filters/backoff. No I/O.
- `internal/engine`: job/task state machine on top of `internal/store`. All transitions happen under
  `Engine.mu`.
- `internal/store`: SQL for SQLite and Postgres. Write queries with `?`; `rebind` converts them for
  Postgres. Times are Unix milliseconds, 0 = unset.
- `internal/storage`: blob backends (local with HMAC-signed URLs, S3 with hand-written SigV4 presign).
- `internal/agent`: agent loop, yield state machine, argv/env building. `internal/agent/telemetry` holds
  macOS collectors. Parsers are pure and tested with fixtures; only `default_darwin.go` /
  `default_other.go` are build-tagged.
- `internal/hostprep`: root-side Mac preparation (work volume, pf anchor), run by `umm-agent prepare` and
  the updater. Commands go through an injectable runner; tests fake `diskutil`/`pfctl`.
- `internal/server`: HTTP handlers, auth middleware, OIDC login.
- `internal/integration`: router + real agents + real processes with simulated telemetry.

## Commands

```sh
go test ./...                          # all tests (integration ~8 s)
GOOS=darwin GOARCH=arm64 go vet ./...  # macOS build check
cd dashboard && npm run lint && npm run build
```

## Rules

- Keep the core generic: no workload-specific logic outside `examples/`.
- Never put secrets in argv, config files, logs or test fixtures that look real.
- Agents must never need inbound ports. They hold no long-lived storage credentials.
- A logged-in user alone must never stop work. Only the user's own sustained load (ours excluded) or
  machine distress may.
- Dashboard: official shadcn/ui components only, added with the shadcn CLI. `src/components/ui` is vendored
  registry code.
- Keep everything organisation-neutral: no company names, hostnames or IPs. Use `example.com`.
