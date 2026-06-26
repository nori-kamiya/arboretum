# orchard — Status & Resume Notes

Last updated: 2026-06-25. Read this first when resuming.

## What this is

`docker-compose`-compatible CLI that drives Apple's `container` runtime (one
lightweight VM per container, per-service memory/CPU limits). Built because
colima reserves a fixed VM (4 GiB) for its whole lifetime; Apple `container`
sizes per service and frees memory on stop. See `README.md` for the pitch.

## Current state (Phase 1 / MVP — DONE; Phase 2 in progress)

- Commands: `up` (`-d`), `down` (`--prune-builder`), `ps`, `logs` (`--follow`),
  `exec` (`-d`/`-T`/`-e`/`-w`/`-u`, args pass-through via non-interspersed flags),
  and `builder` (`status`/`start`/`stop`/`delete`) wrapping `container builder`.
  `builder` is a deliberate superset (its own namespace, like `docker compose`
  vs `docker builder`) so compose compatibility is preserved; `down` stays
  compose-pure unless `--prune-builder` is passed. Verified on the real runtime.
- Compose features translated: `image`, `build` (context + dockerfile),
  `environment`, `ports`, `volumes` (+ named volumes pre-created), `networks`
  (single per-project network), `depends_on` (topological start order),
  resource limits (`deploy.resources.limits.memory/cpus` and legacy
  `mem_limit`/`cpus`) → `container run --memory/--cpus`, plus `working_dir`
  → `--workdir`, `user` → `--user`, user `labels` → `--label`, and
  `entrypoint` (`[0]` → `--entrypoint`, rest prepended to the command).
- **Preflight**: real (non-dry-run) commands check `container` is on PATH first
  (`backend.EnsureInstalled`) and, when missing, fail fast with a
  `*NotInstalledError` that walks the user through installing the runtime.
  `--dry-run` skips the check so previews work without a runtime.
- `--dry-run` prints the exact `container` commands (used as the acceptance
  oracle in tests). Verified output for `examples/compose.yaml`.
- **Tests: TDD/BDD, 100% statement coverage across all packages, `go vet` clean.**
- **Distribution**: `version`/`--version` (ldflags-injected metadata), GoReleaser
  (`.goreleaser.yaml`, darwin arm64+amd64 archives + checksums), GitHub Actions
  (`ci.yml` = vet + 100% coverage gate + `goreleaser check`; `release.yml` =
  publish on `v*` tag with the default token), `Makefile`, and `install.sh`.
  Verified end-to-end locally via `goreleaser release --snapshot` (produces a
  working arm64 binary with version baked in). Releasing steps in README.
  Open item before a public release: add a `LICENSE` file (and then the
  Homebrew tap block in `.goreleaser.yaml` can be enabled).

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

1. ~~**Real-install verification**~~ — DONE against `container` 1.0.0 on macOS 26
   (arm64). Verified end-to-end: `up` (fresh / idempotent / restart-stopped),
   `build` (custom Dockerfile + RUN layer), `ps` (with state), `exec` (env +
   command), `logs`, `down` (clean removal incl. network), network reuse.
   **Two real bugs found and fixed (with regression tests):**
   - `container ls`/`network list --format json` nest labels/id under
     `configuration` and state under `status` — `backend` now resolves both
     (`resolveConfig`/`nameOf`/`stateOf`). Previously `ps` showed nothing and
     `up` re-created the network.
   - `up` now skips running containers and (re)starts stopped ones instead of
     failing with "container with id X already exists".
   Still to confirm on a multi-service net: service-name DNS resolution, and
   `--memory`/`--cpus` unit acceptance under load (flags emit & containers run).
2. **Foreground `up` log multiplexing** — interleave `container logs -f` per
   service with colored `name |` prefixes; Ctrl-C → stop all. (Currently Logs
   tails services sequentially — see `orch.Logs` TODO.)
3. **`depends_on` healthcheck conditions** (`condition: service_healthy`) — poll
   `container inspect`/exec until healthy before starting dependents.
4. ~~**`exec`** subcommand~~ — DONE (`orch.Exec`; `container exec --tty
   --interactive` by default, `-T` to disable). Verified against real
   `container exec` (env passthrough + command execution work).
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
