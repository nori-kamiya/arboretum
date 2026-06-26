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
   - ~~`--memory`/`--cpus` acceptance~~ — VERIFIED. `--memory 512m` accepted;
     limits visibly applied (`container ls` showed 512 MB / 1 CPU vs default
     1024 MB / 4). **Bug fixed:** Apple `container --cpus` takes whole CPUs only
     (rejected `0.5`), so `cpuLimit` now rounds fractional compose limits up
     (`0.5` → `1`), never under-provisioning. `trimFloat` removed.
   - **Service-name DNS — root-caused; recipe verified (drives #5 below).**
     Findings on container 1.0.0 (cross-checked with apple/container docs):
     - The embedded DNS *works* (resolves external names) and container↔container
       *IP* connectivity works; only **name records** were missing.
     - Apple registers a container under its **literal name**; resolution needs a
       *local DNS domain* (`sudo container system dns create <domain>`, admin).
       The intended path is a **default domain** system property (`[dns] domain`)
       so a container named `web` auto-registers as `web.<domain>` — but
       `container system property` has **no `set`** in 1.0.0, so it can't be set
       via CLI. `--dns-domain` only writes the container's resolv.conf; it does
       NOT register the record.
     - **Verified workaround that needs no default-domain property:** name the
       container `<service>.<domain>` (so it registers) AND pass
       `--dns-domain <domain>` to peers (search domain) → a peer resolves the
       **bare `<service>`**. Confirmed both container→container and host→container.
     - Implication for orchard: deliver compose DNS by setting container name =
       `<service>.<domain>` + `--dns-domain <domain>`, with `<domain>` = project
       name (also solves cross-project isolation). Domain creation is one-time
       sudo per domain → preflight + instruct (can't automate). Implement in #5.
2. **Foreground `up` log multiplexing** — interleave `container logs -f` per
   service with colored `name |` prefixes; Ctrl-C → stop all. (Currently Logs
   tails services sequentially — see `orch.Logs` TODO.)
3. **`depends_on` healthcheck conditions** (`condition: service_healthy`) — poll
   `container inspect`/exec until healthy before starting dependents.
4. ~~**`exec`** subcommand~~ — DONE (`orch.Exec`; `container exec --tty
   --interactive` by default, `-T` to disable). Verified against real
   `container exec` (env passthrough + command execution work).
5. **Cross-project safety + service-name DNS** (verified design, see item 1).
   Name containers `<service>.<project>`, run with `--dns-domain <project>`, and
   require a one-time `sudo container system dns create <project>` (preflight +
   instruct; orchard can't sudo). Bare `<service>` then resolves within the
   project, and distinct project domains remove cross-project collisions — fixing
   the one-project-at-a-time caveat in one move. Fall back to bare names (today's
   behavior) when the domain is absent, so non-networked stacks still work.
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
