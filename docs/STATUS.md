# orchard — Status & Resume Notes

Last updated: 2026-06-24. Read this first when resuming.

## What this is

`docker-compose`-compatible CLI that drives Apple's `container` runtime (one
lightweight VM per container, per-service memory/CPU limits). Built because
colima reserves a fixed VM (4 GiB) for its whole lifetime; Apple `container`
sizes per service and frees memory on stop. See `README.md` for the pitch.

## Current state (Phase 1 / MVP — DONE)

- Commands: `up` (`-d`), `down`, `ps`, `logs` (`--follow`).
- Compose features translated: `image`, `build` (context + dockerfile),
  `environment`, `ports`, `volumes` (+ named volumes pre-created), `networks`
  (single per-project network), `depends_on` (topological start order), and
  resource limits (`deploy.resources.limits.memory/cpus` and legacy
  `mem_limit`/`cpus`) → `container run --memory/--cpus`.
- `--dry-run` prints the exact `container` commands (used as the acceptance
  oracle in tests). Verified output for `examples/compose.yaml`.
- **Tests: TDD/BDD, 100% statement coverage across all packages, `go vet` clean.**

Sanity check after pulling:

```sh
go build ./...
go test ./... -coverpkg=./... -coverprofile=cover.out && go tool cover -func=cover.out | tail -1   # expect 100.0%
go run . up --dry-run -f examples/compose.yaml
```

## Architecture (where things live)

| Path | Responsibility |
|------|----------------|
| `internal/compose/load.go` | Load compose files → `*types.Project` (compose-go). |
| `internal/orch/orch.go`    | **Core**: `Project` → `container` commands; Up/Down/Ps/Logs, topoSort, runArgs, resource mapping. |
| `internal/backend/container.go` | Thin `container` CLI wrapper. Seams: `Bin`, `DryRun`, `Stdout`, `execFn`/`SetExecForTest`. |
| `main.go` | cobra wiring. `run(args, out, err) int` is the testable entrypoint; `osExit` seam covers `main()`. |

Design choices to keep:
- Container is **named after the service** (not prefixed) so Apple's embedded DNS
  resolves short names (`db`). Tracking/cleanup is by **label**
  `orchard.project=<name>` (not by name), so `down`/`ps` filter on labels.
- Never reimplement compose schema — lean on compose-go.
- Every runtime touch goes through `backend` so tests inject behavior.

## Phase 2 — next work (TDD: write the behavior test first)

Priority order:

1. **Real-install verification** (blocking for trust). Install `container`, then
   pin these against reality and lock with tests:
   - `container ls --format json` actual schema (name/labels keys). Adjust
     `backend.extractLabels`/`firstString`.
   - Service-name DNS actually resolving between containers on the project net.
   - `--memory 512m` / `--cpus 0.5` unit acceptance.
2. **Foreground `up` log multiplexing** — interleave `container logs -f` per
   service with colored `name |` prefixes; Ctrl-C → stop all. (Currently Logs
   tails services sequentially — see `orch.Logs` TODO.)
3. **`depends_on` healthcheck conditions** (`condition: service_healthy`) — poll
   `container inspect`/exec until healthy before starting dependents.
4. **`exec`** subcommand (`container exec -it`).
5. **Cross-project safety** — optional name prefixing + `--network-alias` once we
   confirm alias support, removing the one-project-at-a-time caveat.
6. profiles, `restart` policy, `compose.override.yaml`.

## Known caveats (carried)

- One project at a time (container names are unprefixed for DNS).
- Bind-mount I/O performance for large codebases is unverified (orthogonal to
  orchard; the original colima concern).
- `container` is young (v1.0) — keep using `--format json` and tolerant parsing.

## Packaging (later)

Pure Go single binary → `buildGoModule` in the nix-darwin flake
(`nori-kamiya/nix-darwin`), optionally `home.shellAliases.docker-compose =
"orchard"`. Not in nixpkgs; self-host in the flake.
