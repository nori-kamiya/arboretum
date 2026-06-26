# arboretum

*[日本語版 README](README.ja.md)*

`docker-compose`, backed by Apple's [`container`](https://github.com/apple/container)
runtime. Parses compose files with the official compose-spec parser and
translates them into `container` CLI calls — so each service runs in its own
lightweight VM with **per-service memory/CPU limits** and zero idle footprint.

Motivation: colima reserves a fixed VM (e.g. 4 GiB) for its whole lifetime.
Apple `container` runs one VM per container, sized per service (`--memory 256m
--cpus 1`) and freed on stop. arboretum brings the `docker compose` ergonomics on
top of that model.

## Scope

arboretum targets **local development & testing** — spin your compose stack up on
your Mac, develop against it, tear it down. Production typically runs on Linux
(k8s, plain Docker, …), so arboretum doesn't try to be a production orchestrator;
it aims for *enough* `docker compose` fidelity to develop and verify locally.

See [What works / caveats / not yet](#what-works--caveats--not-yet) for the exact
feature coverage, and `docs/STATUS.md` for implementation notes.

Use `--dry-run` to print the exact `container` commands without executing them:

```sh
arboretum up --dry-run -f examples/compose.yaml
```

## Requirements

- macOS 26 (Tahoe) for full container-to-container networking / DNS.
- Apple `container` installed (`container` on PATH). arboretum never needs Docker.
  If it is missing, arboretum prints step-by-step install guidance (and `--dry-run`
  works without it).

## Install

### Script (latest release)

```sh
curl -fsSL https://raw.githubusercontent.com/nori-kamiya/arboretum/main/install.sh | sh
```

Installs into `~/.local/bin` (override with `BINDIR=...`) and verifies the
release checksum.

### Manual download

Grab the archive for your Mac from the
[releases page](https://github.com/nori-kamiya/arboretum/releases), then:

```sh
tar -xzf arboretum_*_darwin_arm64.tar.gz
xattr -d com.apple.quarantine ./arboretum   # binaries are not notarized yet
sudo mv arboretum /usr/local/bin/
```

### From source

```sh
go install github.com/nori-kamiya/arboretum@latest   # needs Go 1.26+
# or, from a clone, with version metadata baked in:
make install
```

Verify with `arboretum version`.

## Usage

`arbo` is a shorthand for `arboretum` — the two are interchangeable (e.g.
`arbo up -d`). The examples below use the long form.

```sh
arboretum up -d                # build, create network/volumes, start in dep order
arboretum up --force-recreate  # recreate even if config is unchanged
arboretum ps                   # table: SERVICE / NAME / STATE / PORTS
arboretum ps -q                # names only;  ps --format json for scripting
arboretum logs --follow        # tail logs (colored, per-service prefixes)
arboretum exec db psql         # run a command in a running service container
arboretum run web sh           # one-off throwaway container for a service
arboretum stop|start|restart   # operate on existing containers (no teardown)
arboretum build | pull         # build images / pull images, without starting
arboretum config               # print the resolved compose (--services, --format json)
arboretum down -v              # stop + remove containers, network, and volumes
```

`up` is idempotent: unchanged containers are left running, stopped ones are
restarted, and a service whose config changed is recreated automatically (its
config-hash differs); `--force-recreate` recreates regardless. `down` keeps
containers for services no longer in the file unless you pass `--remove-orphans`.

Flags: `-f/--file` (repeatable), `-p/--project-name`, `--profile` (repeatable),
`--dry-run`.

### Service networking & DNS (one-time setup per project)

Containers are named `<service>.<project>`. To reach a service **by name** —
from the host or from another container — you must first create the project's
local DNS domain **once** (this needs `sudo`; it's an Apple `container`
requirement, since it writes `/etc/resolver/<project>`):

```sh
sudo container system dns create foo     # do this once per project name
arboretum up -d -f foo/compose.yaml
```

Then reach each service at `http://<service>.<project>:<port>`, hitting the
container's own port directly (no `ports:` publishing, no `localhost` collision
between projects):

```sh
# project "foo" and project "bar", both serving on :3000, at the same time:
curl http://web.foo:3000
curl http://api.foo:3000
curl http://web.bar:3000
```

Notes:

- It's **per project name**, and `sudo` is needed only for `dns create`/`delete`
  — normal `up`/`down`/`ps`/… never need sudo. `up` prints the exact
  `dns create` command when the domain is missing.
- Without the domain, containers still start and can talk **by IP**; only
  name resolution is unavailable.
- The address is `<service>.<project>` (e.g. `web.foo`), **not** the bare project
  name (`foo` alone does not resolve).
- Don't want DNS/sudo? Publish ports on distinct host ports instead and use
  `localhost` (e.g. `3000:3000` and `3001:3000` → `localhost:3000` / `:3001`).

### Compose file discovery

With no `-f`, arboretum auto-discovers, in order, `compose.yaml`, `compose.yml`,
`docker-compose.yml`, `docker-compose.yaml` in the working directory, and merges
the matching `*.override.{yml,yaml}` on top. Pass `-f` (repeatable) to use
specific files; multiple `-f` are merged left-to-right, as in Docker Compose.

### Builds (Dockerfile)

A service's `build:` is run with `container build` and the resulting image is
used for `run`, so the Dockerfile's `ENTRYPOINT`/`CMD`/`ENV`/etc. apply. The
compose build options `dockerfile`, `target`, `args`, and `labels` are forwarded
(`-f` / `--target` / `--build-arg` / `--label`).

### Resources (CPU / memory)

Each container is its own VM, so sizing matters. arboretum resolves CPU/memory in
order: `deploy.resources.limits` → legacy `mem_limit`/`cpus` → `deploy.resources.
reservations`/`mem_reservation`, e.g.

```yaml
services:
  db:
    image: postgres:16
    deploy:
      resources:
        limits: { cpus: "2", memory: 1g }   # or, legacy:  cpus: 2 / mem_limit: 1g
```

When a service sets none (e.g. a plain image or Dockerfile build), arboretum
passes no `--memory`/`--cpus` and `container` uses its own defaults (the
`[container]` system property). Note `container` allocates **whole CPUs**, so a
fractional `cpus` is rounded up.

Shell completion is built in (cobra): `arboretum completion zsh > ...` (also
`bash`, `fish`, `powershell`).

### Builder management

Apple `container` keeps a long-lived helper container (a BuildKit-based builder)
running after the first image build — it isn't part of any compose project, so
`down` leaves it alone (matching compose semantics). Manage it explicitly:

```sh
arboretum builder status   # show the builder's state
arboretum builder stop     # stop it (frees its ~2 GB)
arboretum builder start    # start it again
arboretum builder delete   # remove it entirely

arboretum down --prune-builder   # tear down the project AND stop the builder
```

These live in their own namespace — like `docker compose` vs `docker builder` —
so adding them keeps arboretum a strict superset of the compose CLI surface.

## Automation / AI agents

The CLI is automation-friendly (`--dry-run`, `--format json`, clear exit codes).
Driving it from an agent or script? See [`docs/AGENT_USAGE.md`](docs/AGENT_USAGE.md).
Contributing with a coding agent? See [`AGENTS.md`](AGENTS.md).

## Development

TDD/BDD, **100% statement coverage** is the standard for this repo.

```sh
go test ./... -cover                       # per-package coverage (expect 100%)
go test ./... -coverpkg=./... -coverprofile=cover.out && go tool cover -func=cover.out
```

Layout:

- `internal/compose` — load compose files into `*types.Project` (compose-go).
- `internal/orch` — translate a Project into `container` commands (the core).
- `internal/backend` — thin, seam-injectable wrapper around the `container` CLI.
- `main.go` — cobra CLI wiring (`run()` is the testable entrypoint).

Testability seams (vars, swapped in tests): `backend.Bin`, `backend.DryRun`,
`backend.Stdout`, `backend.SetExecForTest`, `osExit`.

Common tasks live in the `Makefile` (`make build`, `make cover`, `make snapshot`).

## Releasing

Releases are cut by [GoReleaser](https://goreleaser.com) and GitHub Actions:

1. Land your changes on `main` (CI enforces `go vet` + the 100% coverage gate).
2. Tag and push: `git tag v0.1.0 && git push origin v0.1.0`.
3. `.github/workflows/release.yml` runs `goreleaser release --clean`, which
   cross-compiles the macOS `arm64`/`amd64` binaries, generates checksums and a
   changelog, and publishes a GitHub Release — all with the default
   `GITHUB_TOKEN`, no extra secrets.

Dry-run the whole pipeline locally first: `make snapshot` (writes `./dist`,
uploads nothing). Validate the config with `make release-check`.

Optional Homebrew tap publishing (`brew install nori-kamiya/tap/arboretum`) is
pre-wired but commented out in `.goreleaser.yaml`; enable it once you've created
a `homebrew-tap` repo and a `HOMEBREW_TAP_GITHUB_TOKEN` secret.

## License

[MIT](LICENSE).

## What works / caveats / not yet

Verified end-to-end against Apple `container` 1.0.0 on macOS 26 (arm64).

### ✅ Works (docker-compose-like)

- **Commands**: `up` (`-d`, `--force-recreate`), `down` (`-v`, `--remove-orphans`,
  `--prune-builder`), `ps` (`-q`, `--format json`), `logs` (`--follow`), `exec`,
  `run`, `start`/`stop`/`restart`, `build`, `pull`, `config`, `builder`, `version`.
- **Files**: `compose.yaml`/`.yml` & `docker-compose.yml`/`.yaml` discovery,
  `*.override.*` merge, multiple `-f`, `--profile`, `env_file`, `.env`.
- **Services**: `image`; `build` (context, dockerfile, target, args, labels);
  `environment`; `ports` (published); `volumes` (named + bind); `depends_on`
  (start order + `service_healthy` + `service_completed_successfully`);
  CPU/memory (`deploy.resources.limits` → `mem_limit`/`cpus` → `reservations`);
  `working_dir`, `user`, `entrypoint`, service `labels`.
- **Behavior**: idempotent `up` with config-hash auto-recreate; per-project
  network; service-name DNS; cross-project name isolation; concurrent colored
  logs with Ctrl-C teardown.

### ⚠️ Caveats (from the young Apple `container` runtime, not arboretum)

- **macOS 26 (Tahoe) + Apple silicon only.**
- **Service-name DNS needs a one-time `sudo container system dns create <project>`**
  (admin-only; `up` hints when missing). Without it, containers run and reach each
  other by IP but not by name.
- **No native healthchecks** — arboretum emulates `service_healthy` by exec-polling
  the compose `healthcheck.test`.
- **No restart policies** — `restart:` is reported as unsupported (arboretum is a
  CLI, not a supervising daemon).
- **`service_completed_successfully`** confirms the dependency exited, but not that
  it exited `0` (the runtime doesn't expose the exit code).

### ❌ Not yet supported

These compose features are currently **ignored** — `up` prints a warning when it
sees them so your local stack doesn't silently differ from a real Docker setup:

- **Multiple networks per service** (`networks: [a, b]`) — everything joins one
  per-project network. No network aliases / external / custom subnets.
- **`secrets` and `configs`.**
- **Scaling** (`up --scale`, `deploy.replicas`).
- Advanced `ports` (protocol, ranges, host IP), `extra_hosts`, `cap_add/drop`,
  `devices`, `sysctls`, `tmpfs`, `extends`.
- Subcommands `cp`, `top`, `kill`, `pause`/`unpause`, `events`.

## Nix

Pure Go single binary, so it can live in a flake via `buildGoModule` (unlike the
Swift-based alternatives) and stay under declarative management.
