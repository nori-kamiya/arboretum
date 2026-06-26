# Driving arboretum from an AI agent / automation

`arboretum` (alias `arbo`) is a `docker-compose`-compatible CLI over Apple's
`container` runtime, for **local dev/test on macOS 26 + Apple silicon**. This
page is a contract for agents and scripts that drive it. For contributing to the
codebase, see [`AGENTS.md`](../AGENTS.md).

## Golden rules

1. **Preview first with `--dry-run`.** Every command accepts it and prints the
   exact `container …` commands without executing — use it to validate intent
   before mutating anything.
2. **Ask for machine-readable output.** `ps --format json`, `config --format json`.
   Avoid screen-scraping the table form.
3. **Never allocate a TTY non-interactively.** For `exec` and `run`, always pass
   `-T` (disable pseudo-TTY) when there's no human terminal, or the call may hang
   or fail.
4. **Check exit codes.** `0` = success. `1` = failure, with a single
   `arboretum: <error>` line on **stderr**. stdout stays parseable.
5. **Be non-interactive.** Use `-f <file>` explicitly, `-d` for `up`, and avoid
   commands that stream forever (`logs --follow`, foreground `up`) unless you
   manage the process/timeout yourself.

## Command cheat sheet

| Command | Purpose | Agent-relevant flags |
|---|---|---|
| `up -d` | create network/volumes, build, start in dep order | `-d` (detach), `--force-recreate`, `--dry-run` |
| `down` | stop+remove containers and network | `-v` (volumes), `--remove-orphans`, `--prune-builder` |
| `ps` | list this project's containers | `-q` (names), `--format json` |
| `logs` | print logs | `--follow` (streams — manage timeout) |
| `exec SERVICE CMD…` | run in a running container | `-T` (no TTY), `-d`, `-e/-w/-u` |
| `run SERVICE [CMD…]` | one-off throwaway container (`--rm`) | `-T` (no TTY), `-d`, `-e` |
| `start`/`stop`/`restart` | operate on existing containers | — |
| `build` / `pull` | build / pull images without starting | `--dry-run` |
| `config` | print the resolved compose | `--services`, `--format json` |
| `version` | build metadata | — |

Global flags: `-f/--file` (repeatable), `-p/--project-name`, `--profile`
(repeatable), `--dry-run`.

## JSON shapes

`arboretum ps --format json` → array of:

```json
[{ "service": "web", "name": "web.myproj", "state": "running", "ports": "8080->3000" }]
```

`arboretum config --format json` → the merged, normalized compose project
(profiles/overrides applied) as JSON.

## Behavior an agent must account for

- **Idempotent `up`.** Re-running is safe: unchanged containers stay, stopped
  ones restart, and a service whose config changed is **recreated automatically**
  (tracked via an `arboretum.config-hash` label). `--force-recreate` forces it.
- **Container naming.** Containers are `<service>.<project>`; everything is
  tracked by label (`arboretum.project` / `arboretum.service`), so projects don't
  collide.
- **Warnings on unsupported features.** `up` prints `arboretum: warning: …` for
  features it ignores — **parse these and surface them**: multiple per-service
  networks, `secrets`, `configs`, `deploy.replicas`. Treat as "this won't behave
  like real Docker."
- **Resource sizing.** `--cpus` is whole-CPU (fractional rounds up) and *soft*
  (oversubscribable). `--memory` is a **hard cap** → exceeding it OOM-kills the
  process. Size memory with headroom.

## Things that need a human (cannot be automated by the agent)

- **Service-name DNS** requires a one-time, admin-only:
  `sudo container system dns create <project>`. Without it, containers run and
  reach each other **by IP**, but `<service>.<project>` name resolution won't
  work. `up` prints the exact command when the domain is missing — relay it to
  the human; do not attempt `sudo`.
- **Runtime not installed / stopped.** If `container` is missing, arboretum exits
  `1` with install guidance. If the runtime is stopped, ask the human to run
  `container system start` (it may prompt for a kernel download on first run).

## Typical agent workflow

```sh
arboretum up -d --dry-run -f compose.yaml      # 1. validate the plan
arboretum up -d -f compose.yaml                # 2. start (detached)
arboretum ps --format json -f compose.yaml     # 3. confirm state (parse JSON)
arboretum exec -T -f compose.yaml db pg_isready # 4. probe (note -T)
arboretum logs -f compose.yaml | head -n 50    # 5. inspect (bounded, no --follow)
arboretum down -v -f compose.yaml              # 6. clean up
```

Prefer `--dry-run` to discover the exact runtime effect, parse `--format json`
for state, and always relay DNS/runtime setup steps (which need `sudo`) to the
human rather than attempting them.
