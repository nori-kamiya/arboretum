// orchard — a docker-compose-compatible CLI backed by Apple's `container`
// runtime. Parses compose files with the official compose-spec parser and
// translates them into `container` CLI calls.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/nori-kamiya/orchard/internal/backend"
	"github.com/nori-kamiya/orchard/internal/compose"
	"github.com/nori-kamiya/orchard/internal/orch"
	"github.com/spf13/cobra"
)

// osExit is a seam so main() can be covered in tests.
var osExit = os.Exit

// Build metadata. Overridden at release time via -ldflags
// (see .goreleaser.yaml / Makefile); "dev" for local builds.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	osExit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run wires up the CLI and returns a process exit code. Extracted from main so
// it can be exercised end-to-end in tests.
func run(args []string, out, errOut io.Writer) int {
	// Ctrl-C cancels the command context so foreground `logs -f` (and the
	// container child processes started via exec.CommandContext) shut down.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	root := newRootCmd(out)
	root.SetArgs(args)
	root.SetOut(out)
	root.SetErr(errOut)
	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(errOut, "orchard:", err)
		return 1
	}
	return 0
}

func newRootCmd(out io.Writer) *cobra.Command {
	var (
		files       []string
		projectName string
		profiles    []string
		dryRun      bool
	)

	// prep wires backend globals and fails fast with install guidance when the
	// runtime is absent (a no-op under --dry-run, so previews work without it).
	prep := func() error {
		backend.DryRun = dryRun
		backend.Stdout = out
		return backend.EnsureInstalled()
	}
	load := func(ctx context.Context) (*types.Project, error) {
		if err := prep(); err != nil {
			return nil, err
		}
		return compose.Load(ctx, files, projectName, profiles)
	}

	root := &cobra.Command{
		Use:           "orchard",
		Short:         "docker-compose, backed by Apple's container runtime",
		Version:       version, // enables `orchard --version`
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	pf := root.PersistentFlags()
	pf.StringArrayVarP(&files, "file", "f", nil, "compose file (repeatable)")
	pf.StringArrayVar(&profiles, "profile", nil, "enable a compose profile (repeatable)")
	pf.StringVarP(&projectName, "project-name", "p", "", "project name (default: from file/dir)")
	pf.BoolVar(&dryRun, "dry-run", false, "print the container commands instead of running them")

	up := &cobra.Command{
		Use:   "up",
		Short: "Create and start services",
		RunE: func(cmd *cobra.Command, _ []string) error {
			detach, _ := cmd.Flags().GetBool("detach")
			p, err := load(cmd.Context())
			if err != nil {
				return err
			}
			return orch.Up(cmd.Context(), p, detach)
		},
	}
	up.Flags().BoolP("detach", "d", false, "run in the background")

	down := &cobra.Command{
		Use:   "down",
		Short: "Stop and remove containers and the network",
		RunE: func(cmd *cobra.Command, _ []string) error {
			prune, _ := cmd.Flags().GetBool("prune-builder")
			p, err := load(cmd.Context())
			if err != nil {
				return err
			}
			if err := orch.Down(cmd.Context(), p); err != nil || !prune {
				return err
			}
			return orch.Builder(cmd.Context(), "stop")
		},
	}
	down.Flags().Bool("prune-builder", false, "also stop the shared image builder after teardown")

	ps := &cobra.Command{
		Use:   "ps",
		Short: "List containers for this project",
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts := orch.PsOptions{}
			opts.Quiet, _ = cmd.Flags().GetBool("quiet")
			opts.Format, _ = cmd.Flags().GetString("format")
			p, err := load(cmd.Context())
			if err != nil {
				return err
			}
			return orch.Ps(cmd.Context(), p, out, opts)
		},
	}
	ps.Flags().BoolP("quiet", "q", false, "only display container names")
	ps.Flags().String("format", "", "output format: table (default) or json")

	mkLifecycle := func(use, short string, fn func(context.Context, *types.Project) error) *cobra.Command {
		return &cobra.Command{
			Use:   use,
			Short: short,
			RunE: func(cmd *cobra.Command, _ []string) error {
				p, err := load(cmd.Context())
				if err != nil {
					return err
				}
				return fn(cmd.Context(), p)
			},
		}
	}
	stop := mkLifecycle("stop", "Stop containers without removing them", orch.Stop)
	start := mkLifecycle("start", "Start existing containers", orch.Start)
	restart := mkLifecycle("restart", "Restart containers", orch.Restart)

	config := &cobra.Command{
		Use:   "config",
		Short: "Render the resolved compose configuration",
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts := orch.ConfigOptions{}
			opts.ServicesOnly, _ = cmd.Flags().GetBool("services")
			opts.Format, _ = cmd.Flags().GetString("format")
			p, err := load(cmd.Context())
			if err != nil {
				return err
			}
			return orch.Config(p, out, opts)
		},
	}
	config.Flags().Bool("services", false, "print active service names only")
	config.Flags().String("format", "", "output format: yaml (default) or json")

	logs := &cobra.Command{
		Use:   "logs",
		Short: "View output from containers",
		RunE: func(cmd *cobra.Command, _ []string) error {
			follow, _ := cmd.Flags().GetBool("follow")
			p, err := load(cmd.Context())
			if err != nil {
				return err
			}
			return orch.Logs(cmd.Context(), p, follow)
		},
	}
	logs.Flags().Bool("follow", false, "follow log output") // no -f: reserved for --file

	exec := &cobra.Command{
		Use:   "exec [flags] SERVICE COMMAND [ARG...]",
		Short: "Run a command in a running service container",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts := orch.ExecOptions{}
			opts.Detach, _ = cmd.Flags().GetBool("detach")
			opts.NoTTY, _ = cmd.Flags().GetBool("no-TTY")
			opts.Env, _ = cmd.Flags().GetStringArray("env")
			opts.Workdir, _ = cmd.Flags().GetString("workdir")
			opts.User, _ = cmd.Flags().GetString("user")
			p, err := load(cmd.Context())
			if err != nil {
				return err
			}
			return orch.Exec(cmd.Context(), p, args[0], opts, args[1:]...)
		},
	}
	// Stop flag parsing at the first positional (the service name) so flags that
	// belong to the executed command (e.g. `exec db psql -U postgres`) pass
	// through untouched instead of being claimed by orchard.
	exec.Flags().SetInterspersed(false)
	ef := exec.Flags()
	ef.BoolP("detach", "d", false, "run the command in the background")
	ef.BoolP("no-TTY", "T", false, "disable pseudo-TTY allocation")
	ef.StringArrayP("env", "e", nil, "set environment variables (repeatable)")
	ef.StringP("workdir", "w", "", "working directory inside the container")
	ef.StringP("user", "u", "", "run as the given user[:group]")

	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Print detailed version information",
		RunE: func(*cobra.Command, []string) error {
			fmt.Fprintf(out, "orchard %s (commit %s, built %s)\n", version, commit, date)
			return nil
		},
	}

	// builder manages the shared image builder that `container` spins up for
	// builds. This is outside docker-compose's vocabulary (compose itself never
	// touches the builder), so it lives in its own namespace — mirroring the
	// `docker compose` vs `docker builder` split — and keeps `down` compose-pure.
	builder := &cobra.Command{
		Use:   "builder",
		Short: "Manage the shared image builder used by build/up",
	}
	mkBuilder := func(action, short string) *cobra.Command {
		return &cobra.Command{
			Use:   action,
			Short: short,
			RunE: func(cmd *cobra.Command, _ []string) error {
				if err := prep(); err != nil {
					return err
				}
				return orch.Builder(cmd.Context(), action)
			},
		}
	}
	builder.AddCommand(
		mkBuilder("status", "Show the builder status"),
		mkBuilder("start", "Start the builder"),
		mkBuilder("stop", "Stop the builder"),
		mkBuilder("delete", "Delete the builder"),
	)

	root.AddCommand(up, down, ps, logs, exec, stop, start, restart, config, builder, versionCmd)
	return root
}
