# arboretum

`docker-compose`, backed by Apple's [`container`](https://github.com/apple/container)
runtime. Parses compose files with the official compose-spec parser and
translates them into `container` CLI calls — so each service runs in its own
lightweight VM with **per-service memory/CPU limits** and zero idle footprint.

Motivation: colima reserves a fixed VM (e.g. 4 GiB) for its whole lifetime.
Apple `container` runs one VM per container, sized per service (`--memory 256m
--cpus 1`) and freed on stop. arboretum brings the `docker compose` ergonomics on
top of that model.

## Status

Phase 1 (MVP): `up` / `down` / `ps` / `logs`, covering image & build services,
networks, named volumes, env, ports, `depends_on` ordering and **resource
limits** (`deploy.resources.limits` / `mem_limit` / `cpus`).

Phase 2 (in progress): `exec`, plus `working_dir` / `user` / `entrypoint` /
service `labels` translation. See `docs/STATUS.md` for the roadmap.

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

## Status & known gaps

Verified end-to-end against Apple `container` 1.0.0 on macOS 26 (arm64):
`up` (fresh / idempotent / restart-stopped), `build`, `ps`, `exec`, `logs`,
`down`, network reuse, and the `--workdir`/`--user`/`--entrypoint`/`--label`
translations. JSON output is parsed from the real (nested) schema.

Remaining gaps:

- **Service-name DNS** — containers are named `<service>.<project>` and run with
  `--dns-domain <project>`, so they're unique per project (no cross-project
  collisions). For services to reach each other by bare name, create the
  project's local DNS domain once (admin):
  `sudo container system dns create <project>` (`up` prints this hint when it's
  missing). Without it, containers still run and talk by IP.
- **Config changes on `up`** — an existing container is left as-is; `up` does not
  yet diff config to recreate it. Run `down` first to apply compose edits.
- profiles, restart policies — phase 2+.

## Nix

Pure Go single binary, so it can live in a flake via `buildGoModule` (unlike the
Swift-based alternatives) and stay under declarative management.
