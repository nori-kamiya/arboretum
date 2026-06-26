// Package orch translates a compose Project into Apple `container` CLI calls
// and drives the lifecycle (up / down / ps / logs).
package orch

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/nori-kamiya/orchard/internal/backend"
)

func networkName(p *types.Project) string { return p.Name + "_default" }

// Up builds (when needed), creates the network/volumes and starts every service
// in dependency order. When detach is false it tails logs afterwards.
func Up(ctx context.Context, p *types.Project, detach bool) error {
	if err := backend.EnsureNetwork(ctx, networkName(p), p.Name); err != nil {
		return err
	}
	for _, name := range sortedKeys(p.Volumes) {
		if err := backend.EnsureVolume(ctx, name, p.Name); err != nil {
			return err
		}
	}

	// Existing containers make `up` idempotent: a running one is left alone and a
	// stopped one is restarted, rather than failing on a name collision. (We do
	// not yet diff config to recreate on change — a known limitation; `down`
	// first to apply edits.)
	existing, err := backend.ListByProject(ctx, p.Name)
	if err != nil {
		return err
	}
	state := make(map[string]string, len(existing))
	for _, c := range existing {
		state[c.Name] = c.State
	}

	order, err := topoSort(p)
	if err != nil {
		return err
	}
	for _, name := range order {
		svc := p.Services[name]
		cname := containerName(p, name)
		if st, ok := state[cname]; ok {
			if st != "running" {
				if err := backend.Run(ctx, "start", cname); err != nil {
					return fmt.Errorf("start %s: %w", name, err)
				}
			}
			continue
		}
		if svc.Image == "" && svc.Build != nil {
			if err := build(ctx, p, svc); err != nil {
				return fmt.Errorf("build %s: %w", name, err)
			}
		}
		if err := backend.Run(ctx, runArgs(p, svc)...); err != nil {
			return fmt.Errorf("start %s: %w", name, err)
		}
	}
	if !detach {
		return Logs(ctx, p, true)
	}
	return nil
}

// Down stops and removes every container we own, then deletes the network.
func Down(ctx context.Context, p *types.Project) error {
	cs, err := backend.ListByProject(ctx, p.Name)
	if err != nil {
		return err
	}
	for _, c := range cs {
		_ = backend.Run(ctx, "stop", c.Name)
		_ = backend.Run(ctx, "rm", c.Name)
	}
	_ = backend.Run(ctx, "network", "delete", networkName(p))
	return nil
}

// Ps lists this project's containers, writing to out.
func Ps(ctx context.Context, p *types.Project, out io.Writer) error {
	cs, err := backend.ListByProject(ctx, p.Name)
	if err != nil {
		return err
	}
	if len(cs) == 0 {
		fmt.Fprintf(out, "(no containers for project %s)\n", p.Name)
		return nil
	}
	for _, c := range cs {
		state := c.State
		if state == "" {
			state = "-"
		}
		fmt.Fprintf(out, "%-24s %-10s %s\n", c.Name, state, c.Labels[backend.LabelService])
	}
	return nil
}

// Logs follows logs for each service. Apple's `container logs -f` attaches to a
// single container, so for now we follow them sequentially; multiplexing with
// colored prefixes is a phase-2 item (see README).
func Logs(ctx context.Context, p *types.Project, follow bool) error {
	for _, name := range sortedServiceNames(p) {
		args := []string{"logs"}
		if follow {
			args = append(args, "-f")
		}
		args = append(args, containerName(p, name))
		if err := backend.Run(ctx, args...); err != nil {
			return err
		}
	}
	return nil
}

// ExecOptions controls how Exec attaches to a running service container,
// mirroring the relevant `docker compose exec` flags.
type ExecOptions struct {
	Detach  bool     // -d: run the command in the background
	NoTTY   bool     // -T: do not allocate a pseudo-TTY
	Env     []string // -e KEY=VALUE (repeatable)
	Workdir string   // -w: working directory inside the container
	User    string   // -u: user[:group] to run as
}

// Exec runs a one-off command in a service's already-running container via
// `container exec`. By default it allocates an interactive TTY (compose-style);
// pass NoTTY to disable it.
func Exec(ctx context.Context, p *types.Project, service string, opts ExecOptions, cmd ...string) error {
	if _, ok := p.Services[service]; !ok {
		return fmt.Errorf("no such service: %s", service)
	}
	if len(cmd) == 0 {
		return fmt.Errorf("exec requires a command to run")
	}
	args := []string{"exec"}
	if opts.Detach {
		args = append(args, "--detach")
	}
	if !opts.NoTTY {
		args = append(args, "--tty", "--interactive")
	}
	for _, e := range opts.Env {
		args = append(args, "--env", e)
	}
	if opts.Workdir != "" {
		args = append(args, "--workdir", opts.Workdir)
	}
	if opts.User != "" {
		args = append(args, "--user", opts.User)
	}
	args = append(args, containerName(p, service))
	args = append(args, cmd...)
	return backend.Run(ctx, args...)
}

func build(ctx context.Context, p *types.Project, svc types.ServiceConfig) error {
	args := []string{"build", "-t", imageRef(p, svc)}
	if svc.Build.Dockerfile != "" {
		args = append(args, "-f", svc.Build.Dockerfile)
	}
	ctxDir := svc.Build.Context
	if ctxDir == "" {
		ctxDir = "."
	}
	args = append(args, ctxDir)
	return backend.Run(ctx, args...)
}

// runArgs builds the `container run` argv for a service. This is the heart of
// the translation: compose fields -> container flags.
func runArgs(p *types.Project, svc types.ServiceConfig) []string {
	a := []string{
		"run", "--detach",
		"--name", containerName(p, svc.Name),
		"--network", networkName(p),
		"--label", backend.LabelProject + "=" + p.Name,
		"--label", backend.LabelService + "=" + svc.Name,
	}
	if m := memLimit(svc); m != "" {
		a = append(a, "--memory", m)
	}
	if c := cpuLimit(svc); c != "" {
		a = append(a, "--cpus", c)
	}
	if svc.WorkingDir != "" {
		a = append(a, "--workdir", svc.WorkingDir)
	}
	if svc.User != "" {
		a = append(a, "--user", svc.User)
	}
	if len(svc.Entrypoint) > 0 {
		a = append(a, "--entrypoint", svc.Entrypoint[0])
	}
	for _, k := range sortedKeys(svc.Labels) {
		a = append(a, "--label", k+"="+svc.Labels[k])
	}
	for _, k := range sortedEnvKeys(svc.Environment) {
		if v := svc.Environment[k]; v != nil {
			a = append(a, "--env", k+"="+*v)
		} else {
			a = append(a, "--env", k)
		}
	}
	for _, port := range svc.Ports {
		if port.Published != "" {
			a = append(a, "--publish", fmt.Sprintf("%s:%d", port.Published, port.Target))
		}
	}
	for _, vol := range svc.Volumes {
		if vol.Source != "" {
			a = append(a, "--volume", vol.Source+":"+vol.Target)
		}
	}
	a = append(a, imageRef(p, svc))
	if len(svc.Entrypoint) > 1 {
		// container's --entrypoint takes a single executable; the rest of a
		// compose entrypoint list becomes leading arguments.
		a = append(a, svc.Entrypoint[1:]...)
	}
	a = append(a, svc.Command...)
	return a
}

// containerName: we name the container after the service so Apple container's
// embedded DNS resolves the short service name (e.g. "db") between containers.
// Caveat: this assumes one project at a time; cross-project name collisions are
// a known limitation tracked in the README.
func containerName(_ *types.Project, service string) string { return service }

func imageRef(p *types.Project, svc types.ServiceConfig) string {
	if svc.Image != "" {
		return svc.Image
	}
	return p.Name + "-" + svc.Name // built image tag
}

// --- resource limits -------------------------------------------------------

func memLimit(svc types.ServiceConfig) string {
	var b types.UnitBytes
	if lim := limits(svc); lim != nil && lim.MemoryBytes > 0 {
		b = lim.MemoryBytes
	} else if svc.MemLimit > 0 {
		b = svc.MemLimit
	}
	if b <= 0 {
		return ""
	}
	return humanBytes(int64(b))
}

func cpuLimit(svc types.ServiceConfig) string {
	if lim := limits(svc); lim != nil && lim.NanoCPUs > 0 {
		return trimFloat(float64(lim.NanoCPUs))
	}
	return ""
}

func limits(svc types.ServiceConfig) *types.Resource {
	if svc.Deploy == nil {
		return nil
	}
	return svc.Deploy.Resources.Limits
}

func humanBytes(b int64) string {
	const (
		gi = 1 << 30
		mi = 1 << 20
		ki = 1 << 10
	)
	switch {
	case b%gi == 0:
		return fmt.Sprintf("%dg", b/gi)
	case b%mi == 0:
		return fmt.Sprintf("%dm", b/mi)
	case b%ki == 0:
		return fmt.Sprintf("%dk", b/ki)
	default:
		return fmt.Sprintf("%d", b)
	}
}

func trimFloat(f float64) string {
	s := fmt.Sprintf("%.3f", f)
	s = strings.TrimRight(s, "0")
	return strings.TrimRight(s, ".")
}

// --- ordering helpers ------------------------------------------------------

func topoSort(p *types.Project) ([]string, error) {
	const (
		unvisited = 0
		visiting  = 1
		done      = 2
	)
	state := map[string]int{}
	var order []string
	var visit func(string) error
	visit = func(n string) error {
		switch state[n] {
		case done:
			return nil
		case visiting:
			return fmt.Errorf("dependency cycle involving %q", n)
		}
		state[n] = visiting
		svc := p.Services[n]
		deps := make([]string, 0, len(svc.DependsOn))
		for d := range svc.DependsOn {
			deps = append(deps, d)
		}
		sort.Strings(deps)
		for _, d := range deps {
			if _, ok := p.Services[d]; ok {
				if err := visit(d); err != nil {
					return err
				}
			}
		}
		state[n] = done
		order = append(order, n)
		return nil
	}
	for _, n := range sortedServiceNames(p) {
		if err := visit(n); err != nil {
			return nil, err
		}
	}
	return order, nil
}

func sortedServiceNames(p *types.Project) []string {
	names := make([]string, 0, len(p.Services))
	for n := range p.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func sortedEnvKeys(m types.MappingWithEquals) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
