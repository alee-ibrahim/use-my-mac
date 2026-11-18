# Contributing

Thanks for helping. Issues and pull requests are welcome.

## Setup

- Go 1.26+ (the module's `go` directive is the source of truth)
- Node 24+ for the dashboard
- Optional: Docker for Postgres tests, Helm for chart linting

```sh
make vet test          # go vet (linux + darwin) and go test -race
make web               # build the dashboard
make dist-mac          # cross-compile the darwin/arm64 agent bundle
```

Postgres store tests run when `UMM_TEST_POSTGRES_URL` is set, for example:

```sh
docker run -d --rm -p 5432:5432 -e POSTGRES_PASSWORD=dev postgres:17-alpine
UMM_TEST_POSTGRES_URL='postgres://postgres:dev@localhost:5432/postgres?sslmode=disable' go test ./internal/store/
```

## Ground rules

- **Keep the core generic.** No workload-specific code in `internal/`. Put workload helpers in `examples/`.
- **Never degrade the Mac's user.** Changes to the agent or the yield policy need tests in
  `internal/agent/yield_test.go` or the integration suite.
- **No secrets in argv, config files, logs or commits.** Read them from the environment or a 0600 file.
- **macOS code paths:** keep parsing in pure functions with fixture tests; only wiring may be build-tagged.
  If you have tested on real hardware, say which Mac and macOS version in the PR.
- **Dashboard:** use official shadcn/ui components only (`npx shadcn@latest add <component>`). Don't hand-roll
  UI primitives, and don't edit files in `src/components/ui` except by re-adding them from the registry.
- Run `gofmt`, `go vet` and `npm run lint` before pushing. CI runs them too.
- Small, focused PRs with a clear description of behaviour changes.

## Commit messages

Imperative mood, short subject line, body explaining *why*.
