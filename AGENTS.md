# AGENTS.md — working on arboretum

Guidance for AI agents (and humans) contributing to this repo. For driving the
`arboretum` CLI itself, see [`docs/AGENT_USAGE.md`](docs/AGENT_USAGE.md).

## What this is

A `docker-compose`-compatible CLI that translates a compose project into Apple
`container` CLI calls. Go module `github.com/nori-kamiya/arboretum`. Target:
local dev/test on macOS 26 + Apple silicon. See `README.md` and `docs/STATUS.md`.

## Build / test / run

```sh
make build          # builds ./arboretum (+ ./arbo symlink)
go vet ./...
go test ./... -coverpkg=./... -coverprofile=cover.out && go tool cover -func=cover.out | tail -1
go run . up --dry-run -f examples/compose.yaml   # exercise without the runtime
```

## Non-negotiable conventions

- **100% statement coverage.** CI fails below `100.0%` (`.github/workflows/ci.yml`).
  Every new branch needs a test. Check locally before committing.
- **`go vet` clean**, `go test -race` clean.
- **TDD / table-driven tests.** Write the behavior test first.
- **`--dry-run` is the acceptance oracle.** Behavior is verified by asserting the
  exact `container` argv that arboretum emits — never require a real runtime in
  tests.
- **Never reimplement the compose schema** — load via compose-go
  (`internal/compose`). Interpolation, `.env`, `env_file`, profiles, override
  merge, and discovery are all compose-go's job.
- **Every runtime touch goes through `internal/backend`** so tests can inject
  behavior via seams.

## Architecture

| Path | Responsibility |
|------|----------------|
| `internal/compose/load.go` | compose files → `*types.Project` (compose-go). |
| `internal/orch/orch.go` | **Core**: Project → `container` argv; up/down/ps/logs/exec/run/start/stop/restart/build/pull/config/builder, depends_on waits, resource sizing, log multiplexing, config-hash recreate. |
| `internal/backend/container.go` | Thin `container` CLI wrapper + JSON parsing (nested Apple schema). |
| `main.go` | cobra wiring; `run(args, out, err) int` is the testable entrypoint. |

## Test seams (swap in tests, restore with the returned/captured func)

- `backend.Bin` — binary name (point at a stub script, or a bogus name).
- `backend.DryRun`, `backend.Stdout` — capture/echo.
- `backend.SetExecForTest(fn)` — fake `container <args>` exec (returns bytes/err).
- `backend.SetStreamForTest(fn)` — fake streaming exec (logs).
- `orch.sleepFn` — health/completed poll delay (set to no-op).
- `orch.LogColor` — disable ANSI for deterministic log assertions.
- `orch.marshalProject` — force a `config` marshal error.
- `osExit` (main) — cover `main()`.

## Adding a compose→container translation

Most translation lives in `orch.runSpec`/`runArgs` (the `container run` argv). Add
the field, then a `--dry-run`/`runArgs` assertion. Remember:

- `container run` takes **whole CPUs** (`--cpus` integer; round fractional up).
- A `arboretum.config-hash` label is appended so `up` can detect config drift —
  keep `configHash` and `runArgs` consistent (both go through `runSpec`).
- If a compose feature can't be honored, **warn** (see `warnUnsupported` /
  `restartPolicy`) rather than silently dropping it.

## Runtime reality (so tests stay hermetic)

Apple `container` 1.0.0 quirks already handled — don't regress: JSON nests under
`configuration`/`status`; no native healthchecks, restart policies, exit codes;
service DNS needs an admin-created domain. None of these may be required by the
test suite — use seams and `--dry-run`.

## Commits / PRs

- Conventional, imperative subject (e.g. `orchard: …` history; use `arboretum:`).
- Keep coverage at 100% and CI green. Tag releases `vX.Y.Z` (GoReleaser publishes).
