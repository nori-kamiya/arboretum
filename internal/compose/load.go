// Package compose loads docker-compose files into a typed Project using the
// official compose-spec parser. We deliberately do NOT reimplement the compose
// schema, interpolation, .env handling or merging — compose-go (the same
// library Docker Compose v2 uses) gives us a validated *types.Project.
package compose

import (
	"context"
	"fmt"

	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/compose-spec/compose-go/v2/types"
)

// Load parses the given compose files (or the default discovery set when empty)
// into a Project. projectName overrides the name from the file / directory when
// non-empty.
func Load(ctx context.Context, files []string, projectName string) (*types.Project, error) {
	opts := []cli.ProjectOptionsFn{
		cli.WithOsEnv,
		cli.WithDotEnv,
	}
	if len(files) == 0 {
		// Discover compose.yaml/docker-compose.yml in the working dir.
		opts = append(opts, cli.WithDefaultConfigPath)
	}
	if projectName != "" {
		opts = append(opts, cli.WithName(projectName))
	}

	options, err := cli.NewProjectOptions(files, opts...)
	if err != nil {
		return nil, fmt.Errorf("compose options: %w", err)
	}
	project, err := options.LoadProject(ctx)
	if err != nil {
		return nil, fmt.Errorf("load compose project: %w", err)
	}
	return project, nil
}
