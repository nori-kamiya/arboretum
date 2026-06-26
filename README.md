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

## Install

### Script (latest release)

```sh
curl -fsSL https://raw.githubusercontent.com/nori-kamiya/orchard/main/install.sh | sh
```

Installs into `~/.local/bin` (override with `BINDIR=...`) and verifies the
release checksum.

### Manual download

Grab the archive for your Mac from the
[releases page](https://github.com/nori-kamiya/orchard/releases), then:

```sh
tar -xzf orchard_*_darwin_arm64.tar.gz
xattr -d com.apple.quarantine ./orchard   # binaries are not notarized yet
sudo mv orchard /usr/local/bin/
```

### From source

```sh
go install github.com/nori-kamiya/orchard@latest   # needs Go 1.26+
# or, from a clone, with version metadata baked in:
make install
```

Verify with `orchard version`.

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

Optional Homebrew tap publishing (`brew install nori-kamiya/tap/orchard`) is
pre-wired but commented out in `.goreleaser.yaml`; enable it once you've created
a `homebrew-tap` repo and a `HOMEBREW_TAP_GITHUB_TOKEN` secret.

> **Note:** a `LICENSE` file isn't in the repo yet — add one before a public
> release (Homebrew formulae and most downstreams expect it).

## Status & known gaps

Verified end-to-end against Apple `container` 1.0.0 on macOS 26 (arm64):
`up` (fresh / idempotent / restart-stopped), `build`, `ps`, `exec`, `logs`,
`down`, network reuse, and the `--workdir`/`--user`/`--entrypoint`/`--label`
translations. JSON output is parsed from the real (nested) schema.

Remaining gaps:

- **Service-name DNS** — relies on naming the container after the service so
  Apple's embedded DNS resolves short names. Assumes one project at a time;
  cross-project name collisions are a known limitation.
- **Config changes on `up`** — an existing container is left as-is; `up` does not
  yet diff config to recreate it. Run `down` first to apply compose edits.
- Foreground `up` log multiplexing, `depends_on` healthcheck conditions,
  profiles, restart policies — phase 2+.

## Nix

Pure Go single binary, so it can live in a flake via `buildGoModule` (unlike the
Swift-based alternatives) and stay under declarative management.
