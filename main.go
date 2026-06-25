// orchard — a docker-compose-compatible CLI backed by Apple's `container`
// runtime. Parses compose files with the official compose-spec parser and
// translates them into `container` CLI calls.
package main

import (
	"context"
	"fmt"
	"io"
	"os"

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
	root := newRootCmd(out)
	root.SetArgs(args)
	root.SetOut(out)
	root.SetErr(errOut)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(errOut, "orchard:", err)
		return 1
	}
	return 0
}

func newRootCmd(out io.Writer) *cobra.Command {
	var (
		files       []string
		projectName string
		dryRun      bool
	)

	load := func(ctx context.Context) (*types.Project, error) {
		backend.DryRun = dryRun
		backend.Stdout = out
		// Fail fast with install guidance when the runtime is absent (a no-op
		// under --dry-run, so previews still work without `container`).
		if err := backend.EnsureInstalled(); err != nil {
			return nil, err
		}
		return compose.Load(ctx, files, projectName)
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
			p, err := load(cmd.Context())
			if err != nil {
				return err
			}
			return orch.Down(cmd.Context(), p)
		},
	}

	ps := &cobra.Command{
		Use:   "ps",
		Short: "List containers for this project",
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := load(cmd.Context())
			if err != nil {
				return err
			}
			return orch.Ps(cmd.Context(), p, out)
		},
	}

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

	root.AddCommand(up, down, ps, logs, exec, versionCmd)
	return root
}
