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
		return compose.Load(ctx, files, projectName)
	}

	root := &cobra.Command{
		Use:           "orchard",
		Short:         "docker-compose, backed by Apple's container runtime",
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

	root.AddCommand(up, down, ps, logs)
	return root
}
