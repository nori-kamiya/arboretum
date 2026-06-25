# orchard

`docker-compose`, backed by Apple's [`container`](https://github.com/apple/container)
runtime. Parses compose files with the official compose-spec parser and
translates them into `container` CLI calls — so each service runs in its own
lightweight VM with **per-service memory/CPU limits** and zero idle footprint.

Motivation: colima reserves a fixed VM (e.g. 4 GiB) for its whole lifetime.
Apple `container` runs one VM per container, sized per service (`--memory 256m
--cpus 1`) and freed on stop. orchard brings the `docker compose` ergonomics on
top of that model.

## Status

Phase 1 (MVP): `up` / `down` / `ps` / `logs`, covering image & build services,
networks, named volumes, env, ports, `depends_on` ordering and **resource
limits** (`deploy.resources.limits` / `mem_limit` / `cpus`).

Phase 2 (in progress): `exec`, plus `working_dir` / `user` / `entrypoint` /
service `labels` translation. See `docs/STATUS.md` for the roadmap.

Use `--dry-run` to print the exact `container` commands without executing them:

```sh
orchard up --dry-run -f examples/compose.yaml
```

## Requirements

- macOS 26 (Tahoe) for full container-to-container networking / DNS.
- Apple `container` installed (`container` on PATH). orchard never needs Docker.
  If it is missing, orchard prints step-by-step install guidance (and `--dry-run`
  works without it).

## Usage

```sh
orchard up -d            # build, create network/volumes, start in dep order
orchard ps               # list this project's containers
orchard logs --follow    # tail logs
orchard exec db psql     # run a command in a running service container
orchard down             # stop + remove containers and the network
```

Flags: `-f/--file` (repeatable), `-p/--project-name`, `--dry-run`.

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

## Known gaps / to verify against a real install

These are translated but not yet validated on hardware (tracked for phase 2):

- **`container ls --format json` schema** — parsed defensively; pin field names.
- **Service-name DNS** — relies on naming the container after the service so
  Apple's embedded DNS resolves short names. Assumes one project at a time;
  cross-project name collisions are a known limitation.
- **`--memory` / `--cpus` units** — emitted as `512m` / `0.5`; confirm accepted.
- **New `run`/`exec` flags** (`--workdir`, `--user`, `--entrypoint`, `--label`,
  `exec --tty --interactive`) — translated but not yet validated on hardware.
- Foreground `up` log multiplexing, `depends_on` healthcheck conditions,
  profiles, restart policies — phase 2+.

## Nix

Pure Go single binary, so it can live in a flake via `buildGoModule` (unlike the
Swift-based alternatives) and stay under declarative management.
