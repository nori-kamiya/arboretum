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
// non-empty. profiles activates the named compose profiles (also honoring the
// COMPOSE_PROFILES env var); services in non-active profiles are excluded.
//
// compose.override.yaml (and multiple -f files) are merged by compose-go, so no
// extra handling is needed here.
func Load(ctx context.Context, files []string, projectName string, profiles []string) (*types.Project, error) {
	opts := []cli.ProjectOptionsFn{
		cli.WithOsEnv,
		cli.WithDotEnv,
		// After WithOsEnv so COMPOSE_PROFILES is visible; merges flag + env.
		cli.WithDefaultProfiles(profiles...),
	}
	if len(files) == 0 {
		// Discover compose.yaml/docker-compose.yml (+ override) in the working dir.
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
