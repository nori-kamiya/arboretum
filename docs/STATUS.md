# arboretum — Status & Resume Notes

Last updated: 2026-06-25. Read this first when resuming.

## What this is

`docker-compose`-compatible CLI that drives Apple's `container` runtime (one
lightweight VM per container, per-service memory/CPU limits). Built because
colima reserves a fixed VM (4 GiB) for its whole lifetime; Apple `container`
sizes per service and frees memory on stop. See `README.md` for the pitch.

## Current state (Phase 1 / MVP — DONE; Phase 2 in progress)

- Commands: `up` (`-d`, per-service ✔ summary), `down` (`--prune-builder`),
  `ps` (table SERVICE/NAME/STATE/PORTS, `-q`, `--format json`), `logs`
  (`--follow`), `exec` (`-d`/`-T`/`-e`/`-w`/`-u`, args pass-through via
  non-interspersed flags), `stop`/`start`/`restart` (operate on existing
  containers by label), `config` (`--services`, `--format json`), and
  `builder` (`status`/`start`/`stop`/`delete`) wrapping `container builder`.
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
  `arboretum.project=<name>` (not by name), so `down`/`ps` filter on labels.
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
     - Implication for arboretum: deliver compose DNS by setting container name =
       `<service>.<domain>` + `--dns-domain <domain>`, with `<domain>` = project
       name (also solves cross-project isolation). Domain creation is one-time
       sudo per domain → preflight + instruct (can't automate). Implement in #5.
2. ~~**Foreground `up` log multiplexing**~~ — DONE. `orch.Logs` now streams every
   service's `container logs -f` concurrently through colored, aligned `name | `
   prefixes (`backend.Stream` seam + `prefixWriter`/`syncWriter`); non-follow and
   dry-run stay sequential/deterministic. Ctrl-C cancels the command context
   (`signal.NotifyContext` in main) which kills the children. Verified on the
   real runtime: live interleaved output and clean SIGINT shutdown.
3. ~~**`depends_on` healthcheck conditions**~~ — DONE. container 1.0.0 has no
   native healthchecks (no run flags, no health in inspect), so `orch.waitHealthy`
   runs the compose `healthcheck.test` via `container exec` (CMD / CMD-SHELL /
   legacy), polling with the compose interval/retries/start_period before
   starting a `service_healthy` dependent. Verified on the real runtime: a
   dependent waited for its dependency's healthcheck to pass before starting.
   (`service_completed_successfully` not yet handled.)
4. ~~**`exec`** subcommand~~ — DONE (`orch.Exec`; `container exec --tty
   --interactive` by default, `-T` to disable). Verified against real
   `container exec` (env passthrough + command execution work).
5. ~~**Cross-project safety + service-name DNS**~~ — DONE. Containers are named
   `<service>.<project>` (`containerName`) and run with `--dns-domain <project>`.
   This makes names unique per project (no collisions between stacks) AND, once
   the user runs the one-time `sudo container system dns create <project>`,
   registers each container so peers resolve the bare `<service>` via their shared
   search domain. Safe without the domain (the flag/name are no-ops for DNS then);
   `up` prints a hint (`hintServiceDNS`) telling multi-service projects how to
   create the domain. Verified on the real runtime: `web` resolved bare `db`, and
   names are project-scoped. (`service_completed_successfully` aside, this closes
   the one-project-at-a-time caveat.)
6. **`restart` policy** — ~~translate~~ NOT translatable: container 1.0.0 has no
   `--restart` and arboretum is not a supervising daemon. `orch.restartPolicy`
   detects `restart:`/`deploy.restart_policy` and Up prints a one-line warning
   that it's ignored (verified), rather than silently dropping it. Revisit if the
   runtime adds restart support.
7. ~~profiles, `compose.override.yaml`~~ — DONE. `--profile` (repeatable) flag →
   `cli.WithDefaultProfiles` (also honors `COMPOSE_PROFILES`); profiled services
   are excluded unless active. Override/multi-`-f` merging is handled by
   compose-go already. Both verified on the real CLI.

## Known caveats (carried)

- Service-name DNS needs a one-time `sudo container system dns create <project>`
  (Apple requires admin to create the local domain; arboretum can't automate it).
- Bind-mount I/O performance for large codebases is unverified (orthogonal to
  arboretum; the original colima concern).
- `container` is young (v1.0) — keep using `--format json` and tolerant parsing.

## Packaging (later)

Pure Go single binary → `buildGoModule` in the nix-darwin flake
(`nori-kamiya/nix-darwin`), optionally `home.shellAliases.docker-compose =
"arboretum"`. Not in nixpkgs; self-host in the flake.
