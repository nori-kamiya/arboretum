// Package orch translates a compose Project into Apple `container` CLI calls
// and drives the lifecycle (up / down / ps / logs).
package orch

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/nori-kamiya/arboretum/internal/backend"
)

func networkName(p *types.Project) string { return p.Name + "_default" }

// Up builds (when needed), creates the network/volumes and starts every service
// in dependency order. When detach is false it tails logs afterwards.
func Up(ctx context.Context, p *types.Project, detach, forceRecreate bool) error {
	warnUnsupported(p)
	hintServiceDNS(ctx, p)
	if err := backend.EnsureNetwork(ctx, networkName(p), p.Name); err != nil {
		return err
	}
	for _, name := range sortedKeys(p.Volumes) {
		if err := backend.EnsureVolume(ctx, name, p.Name); err != nil {
			return err
		}
	}

	// Existing containers make `up` idempotent: an unchanged running one is left
	// alone, a stopped one is restarted, and one whose config changed (different
	// config-hash) — or all, under --force-recreate — is recreated.
	existing, err := backend.ListByProject(ctx, p.Name)
	if err != nil {
		return err
	}
	state := make(map[string]string, len(existing))
	hashes := make(map[string]string, len(existing))
	for _, c := range existing {
		state[c.Name] = c.State
		hashes[c.Name] = c.Labels[backend.LabelConfigHash]
	}

	order, err := topoSort(p)
	if err != nil {
		return err
	}
	summary := make(map[string]string, len(order))
	for _, name := range order {
		svc := p.Services[name]
		cname := containerName(p, name)
		recreated := false
		if st, ok := state[cname]; ok {
			if !forceRecreate && hashes[cname] == configHash(p, svc) {
				if st != "running" {
					if err := backend.Run(ctx, "start", cname); err != nil {
						return fmt.Errorf("start %s: %w", name, err)
					}
					summary[name] = "started"
				} else {
					summary[name] = "running"
				}
				continue
			}
			// Config changed (or forced): drop the old container, recreate below.
			_ = backend.Run(ctx, "stop", cname)
			_ = backend.Run(ctx, "rm", cname)
			recreated = true
		}
		// Honor depends_on conditions before creating svc. Topological order
		// guarantees the dependency was started first.
		for _, dep := range sortedKeys(svc.DependsOn) {
			switch svc.DependsOn[dep].Condition {
			case types.ServiceConditionHealthy:
				if err := waitHealthy(ctx, p, dep); err != nil {
					return fmt.Errorf("waiting for %s to be healthy: %w", dep, err)
				}
			case types.ServiceConditionCompletedSuccessfully:
				if err := waitCompleted(ctx, p, dep); err != nil {
					return fmt.Errorf("waiting for %s to complete: %w", dep, err)
				}
			}
		}
		if pol := restartPolicy(svc); pol != "" {
			// Apple container has no restart policy and arboretum is not a daemon,
			// so we can't honor it — warn rather than silently drop it.
			fmt.Fprintf(backend.Stdout, "arboretum: warning: service %q requests restart %q, which Apple container does not support; ignoring\n", name, pol)
		}
		if svc.Image == "" && svc.Build != nil {
			if err := build(ctx, p, svc); err != nil {
				return fmt.Errorf("build %s: %w", name, err)
			}
		}
		if err := backend.Run(ctx, runArgs(p, svc)...); err != nil {
			return fmt.Errorf("start %s: %w", name, err)
		}
		if recreated {
			summary[name] = "recreated"
		} else {
			summary[name] = "started"
		}
	}
	// Real-run summary (dry-run already printed the exact commands).
	if !backend.DryRun {
		for _, name := range order {
			fmt.Fprintf(backend.Stdout, "✔ %s %s\n", name, summary[name])
		}
	}
	if !detach {
		return Logs(ctx, p, true)
	}
	return nil
}

// DownOptions controls teardown.
type DownOptions struct {
	Volumes       bool // -v: also remove the project's named volumes
	RemoveOrphans bool // remove containers whose service is no longer in the file
}

// Down stops and removes this project's containers, then deletes the network.
// Containers whose service is no longer defined are kept (with a warning) unless
// RemoveOrphans is set; named volumes are removed only when Volumes is set.
func Down(ctx context.Context, p *types.Project, opts DownOptions) error {
	cs, err := backend.ListByProject(ctx, p.Name)
	if err != nil {
		return err
	}
	for _, c := range cs {
		if _, defined := p.Services[c.Labels[backend.LabelService]]; !defined && !opts.RemoveOrphans {
			fmt.Fprintf(backend.Stdout, "arboretum: warning: orphan container %s is not in the compose file; pass --remove-orphans to remove it\n", c.Name)
			continue
		}
		_ = backend.Run(ctx, "stop", c.Name)
		_ = backend.Run(ctx, "rm", c.Name)
	}
	_ = backend.Run(ctx, "network", "delete", networkName(p))
	if opts.Volumes {
		for _, name := range sortedKeys(p.Volumes) {
			_ = backend.Run(ctx, "volume", "delete", name)
		}
	}
	return nil
}

// BuildAll builds images for every service that declares a build section.
func BuildAll(ctx context.Context, p *types.Project) error {
	for _, name := range sortedServiceNames(p) {
		svc := p.Services[name]
		if svc.Build == nil {
			continue
		}
		if err := build(ctx, p, svc); err != nil {
			return fmt.Errorf("build %s: %w", name, err)
		}
	}
	return nil
}

// Pull pulls the image for every service that references one.
func Pull(ctx context.Context, p *types.Project) error {
	for _, name := range sortedServiceNames(p) {
		svc := p.Services[name]
		if svc.Image == "" {
			continue
		}
		if err := backend.Run(ctx, "image", "pull", svc.Image); err != nil {
			return fmt.Errorf("pull %s: %w", name, err)
		}
	}
	return nil
}

// RunOptions controls a one-off `run`.
type RunOptions struct {
	Detach bool
	NoTTY  bool
	Env    []string
}

// RunOneOff starts a throwaway container for a service (compose's `run`):
// attached to the project network, --rm, with an optional command override.
func RunOneOff(ctx context.Context, p *types.Project, service string, opts RunOptions, cmd ...string) error {
	svc, ok := p.Services[service]
	if !ok {
		return fmt.Errorf("no such service: %s", service)
	}
	if err := backend.EnsureNetwork(ctx, networkName(p), p.Name); err != nil {
		return err
	}
	args := []string{"run", "--rm"}
	if opts.Detach {
		args = append(args, "--detach")
	} else if !opts.NoTTY {
		args = append(args, "--tty", "--interactive")
	}
	args = append(args, "--network", networkName(p), "--dns-domain", p.Name)
	for _, k := range sortedEnvKeys(svc.Environment) {
		if v := svc.Environment[k]; v != nil {
			args = append(args, "--env", k+"="+*v)
		} else {
			args = append(args, "--env", k)
		}
	}
	for _, e := range opts.Env {
		args = append(args, "--env", e)
	}
	args = append(args, imageRef(p, svc))
	if len(cmd) > 0 {
		args = append(args, cmd...)
	} else {
		args = append(args, svc.Command...)
	}
	return backend.Run(ctx, args...)
}

// Stop stops this project's containers without removing them.
func Stop(ctx context.Context, p *types.Project) error {
	return forEachContainer(ctx, p, "stop")
}

// Start (re)starts this project's existing containers.
func Start(ctx context.Context, p *types.Project) error {
	return forEachContainer(ctx, p, "start")
}

// Restart stops then starts this project's containers (`container` has no
// `restart` subcommand).
func Restart(ctx context.Context, p *types.Project) error {
	if err := Stop(ctx, p); err != nil {
		return err
	}
	return Start(ctx, p)
}

// forEachContainer runs a single `container <action> <name>` for every container
// owned by the project, in a stable order.
func forEachContainer(ctx context.Context, p *types.Project, action string) error {
	cs, err := backend.ListByProject(ctx, p.Name)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(cs))
	for _, c := range cs {
		names = append(names, c.Name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := backend.Run(ctx, action, name); err != nil {
			return fmt.Errorf("%s %s: %w", action, name, err)
		}
	}
	return nil
}

// PsOptions controls ps output.
type PsOptions struct {
	Quiet  bool   // -q: print only container names
	Format string // "" (table) or "json"
}

type psRow struct {
	Service string `json:"service"`
	Name    string `json:"name"`
	State   string `json:"state"`
	Ports   string `json:"ports"`
}

// Ps lists this project's containers as an aligned table (SERVICE/NAME/STATE/
// PORTS), or as names only (Quiet) / JSON (Format).
func Ps(ctx context.Context, p *types.Project, out io.Writer, opts PsOptions) error {
	cs, err := backend.ListByProject(ctx, p.Name)
	if err != nil {
		return err
	}
	rows := make([]psRow, 0, len(cs))
	for _, c := range cs {
		state := c.State
		if state == "" {
			state = "-"
		}
		svc := c.Labels[backend.LabelService]
		rows = append(rows, psRow{Service: svc, Name: c.Name, State: state, Ports: portsFor(p, svc)})
	}

	switch {
	case opts.Quiet:
		for _, r := range rows {
			fmt.Fprintln(out, r.Name)
		}
		return nil
	case opts.Format == "json":
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	default:
		if len(rows) == 0 {
			fmt.Fprintf(out, "(no containers for project %s)\n", p.Name)
			return nil
		}
		tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "SERVICE\tNAME\tSTATE\tPORTS")
		for _, r := range rows {
			ports := r.Ports
			if ports == "" {
				ports = "-"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Service, r.Name, r.State, ports)
		}
		return tw.Flush()
	}
}

// portsFor renders a service's published port mappings as "published->target".
func portsFor(p *types.Project, service string) string {
	svc, ok := p.Services[service]
	if !ok {
		return ""
	}
	var parts []string
	for _, port := range svc.Ports {
		if port.Published != "" {
			parts = append(parts, fmt.Sprintf("%s->%d", port.Published, port.Target))
		}
	}
	return strings.Join(parts, ", ")
}

// LogColor toggles ANSI coloring of per-service log prefixes. Tests disable it
// for deterministic output.
var LogColor = true

var logColors = []string{"36", "32", "33", "35", "34", "31", "96", "92"}

// Logs streams each service's logs with a "name | " prefix. With follow, every
// service's `container logs -f` runs concurrently and their lines are
// interleaved (Ctrl-C cancels ctx, which kills the children). Without follow —
// and always under dry-run, to keep output deterministic — services are printed
// sequentially.
func Logs(ctx context.Context, p *types.Project, follow bool) error {
	names := sortedServiceNames(p)
	width := 0
	for _, n := range names {
		if len(n) > width {
			width = len(n)
		}
	}
	out := &syncWriter{w: backend.Stdout}

	if !follow || backend.DryRun {
		for i, name := range names {
			if err := streamLogs(ctx, p, name, i, width, follow, out); err != nil {
				return err
			}
		}
		return nil
	}

	var wg sync.WaitGroup
	errCh := make(chan error, len(names))
	for i, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errCh <- streamLogs(ctx, p, name, i, width, true, out)
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		// Ignore errors caused by our own cancellation (Ctrl-C).
		if err != nil && ctx.Err() == nil {
			return err
		}
	}
	return nil
}

func streamLogs(ctx context.Context, p *types.Project, name string, idx, width int, follow bool, out io.Writer) error {
	w := &prefixWriter{w: out, prefix: logPrefix(name, idx, width)}
	args := []string{"logs"}
	if follow {
		args = append(args, "-f")
	}
	args = append(args, containerName(p, name))
	return backend.Stream(ctx, w, args...)
}

func logPrefix(label string, idx, width int) string {
	padded := fmt.Sprintf("%-*s", width, label)
	if !LogColor {
		return padded + " | "
	}
	return "\x1b[" + logColors[idx%len(logColors)] + "m" + padded + "\x1b[0m | "
}

// syncWriter serializes concurrent writes from the per-service prefix writers to
// the shared output.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(b)
}

// prefixWriter writes the prefix at the start of every line, buffering the
// mid-line state across Write calls.
type prefixWriter struct {
	w      io.Writer
	prefix string
	mid    bool
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	var out []byte
	for _, c := range b {
		if !p.mid {
			out = append(out, p.prefix...)
			p.mid = true
		}
		out = append(out, c)
		if c == '\n' {
			p.mid = false
		}
	}
	if _, err := p.w.Write(out); err != nil {
		return 0, err
	}
	return len(b), nil
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

// ConfigOptions controls config output.
type ConfigOptions struct {
	ServicesOnly bool   // --services: print active service names only
	Format       string // "" (yaml) or "json"
}

// Config renders the merged, profile/override-resolved project so users can see
// exactly what arboretum will act on.
func Config(p *types.Project, out io.Writer, opts ConfigOptions) error {
	if opts.ServicesOnly {
		for _, name := range sortedServiceNames(p) {
			fmt.Fprintln(out, name)
		}
		return nil
	}
	b, err := marshalProject(p, opts.Format == "json")
	if err != nil {
		return err
	}
	_, err = out.Write(b)
	return err
}

// marshalProject is the seam for rendering a project (swapped in tests).
var marshalProject = func(p *types.Project, asJSON bool) ([]byte, error) {
	if asJSON {
		return p.MarshalJSON()
	}
	return p.MarshalYAML()
}

// Builder runs a `container builder` management action (status/start/stop/
// delete). The builder is a runtime-managed helper container, not part of any
// compose project, so this is exposed as its own command rather than folded
// into up/down.
func Builder(ctx context.Context, action string) error {
	return backend.Run(ctx, "builder", action)
}

// warnUnsupported flags compose features arboretum currently ignores, so a local
// stack doesn't silently diverge from a real Docker setup.
func warnUnsupported(p *types.Project) {
	warn := func(msg string) { fmt.Fprintln(backend.Stdout, "arboretum: warning: "+msg) }
	if len(p.Secrets) > 0 {
		warn("`secrets` are not supported and will be ignored")
	}
	if len(p.Configs) > 0 {
		warn("`configs` are not supported and will be ignored")
	}
	for _, name := range sortedServiceNames(p) {
		svc := p.Services[name]
		for _, net := range sortedKeys(svc.Networks) {
			if net != "default" {
				warn(fmt.Sprintf("service %q: custom networks are ignored; all services share one project network", name))
				break
			}
		}
		if svc.Deploy != nil && svc.Deploy.Replicas != nil && *svc.Deploy.Replicas > 1 {
			warn(fmt.Sprintf("service %q: deploy.replicas=%d is not supported; running a single instance", name, *svc.Deploy.Replicas))
		}
	}
}

// hintServiceDNS prints a one-time hint when a multi-service project lacks its
// local DNS domain, so the user knows how to enable service-name resolution. It
// is best-effort and silent under dry-run / on any error.
func hintServiceDNS(ctx context.Context, p *types.Project) {
	if backend.DryRun || len(p.Services) < 2 {
		return
	}
	if ok, err := backend.DNSDomainExists(ctx, p.Name); err != nil || ok {
		return
	}
	fmt.Fprintf(backend.Stdout, "arboretum: hint: run `sudo container system dns create %s` to let services reach each other by name\n", p.Name)
}

// restartPolicy returns a human description of a service's requested restart
// policy (from `restart:` or `deploy.restart_policy`), or "" when none/no.
func restartPolicy(svc types.ServiceConfig) string {
	if svc.Restart != "" && svc.Restart != "no" {
		return svc.Restart
	}
	if svc.Deploy != nil && svc.Deploy.RestartPolicy != nil {
		if c := svc.Deploy.RestartPolicy.Condition; c != "" && c != "none" {
			return "deploy.restart_policy: " + c
		}
	}
	return ""
}

// sleepFn is the seam for health-poll delays; tests swap it out.
var sleepFn = time.Sleep

// waitHealthy polls a dependency's compose healthcheck (Apple `container` has no
// native healthchecks) by exec'ing the test command until it succeeds or the
// retry budget is exhausted.
func waitHealthy(ctx context.Context, p *types.Project, name string) error {
	svc := p.Services[name]
	test, shell, ok := healthTest(svc)
	if !ok {
		return fmt.Errorf("service %q has no usable healthcheck", name)
	}
	retries := 3
	if svc.HealthCheck.Retries != nil {
		retries = int(*svc.HealthCheck.Retries)
	}
	interval := durationOr(svc.HealthCheck.Interval, time.Second)
	if sp := durationOr(svc.HealthCheck.StartPeriod, 0); sp > 0 {
		sleepFn(sp)
	}

	cname := containerName(p, name)
	for attempt := 0; attempt <= retries; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		args := []string{"exec", cname}
		if shell {
			args = append(args, "sh", "-c", test[0])
		} else {
			args = append(args, test...)
		}
		if _, err := backend.Output(ctx, args...); err == nil {
			return nil
		}
		if attempt < retries {
			sleepFn(interval)
		}
	}
	return fmt.Errorf("not healthy after %d attempts", retries+1)
}

// waitCompleted blocks until a dependency container has exited (compose's
// `service_completed_successfully`). Note: container 1.0.0's `inspect` does not
// expose the exit code, so we can only confirm it stopped, not that it exited 0.
func waitCompleted(ctx context.Context, p *types.Project, name string) error {
	cname := containerName(p, name)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		st, err := backend.ContainerState(ctx, cname)
		if err != nil {
			return err
		}
		if st != "" && st != "running" {
			return nil
		}
		sleepFn(time.Second)
	}
}

// healthTest resolves a compose healthcheck into a command. shell is true when
// it must run under `sh -c` (CMD-SHELL or the legacy string form).
func healthTest(svc types.ServiceConfig) (cmd []string, shell, ok bool) {
	hc := svc.HealthCheck
	if hc == nil || hc.Disable || len(hc.Test) == 0 {
		return nil, false, false
	}
	switch hc.Test[0] {
	case "NONE":
		return nil, false, false
	case "CMD":
		return hc.Test[1:], false, len(hc.Test) > 1
	case "CMD-SHELL":
		return []string{strings.Join(hc.Test[1:], " ")}, true, len(hc.Test) > 1
	default:
		return []string{strings.Join(hc.Test, " ")}, true, true
	}
}

func durationOr(d *types.Duration, def time.Duration) time.Duration {
	if d == nil {
		return def
	}
	return time.Duration(*d)
}

func build(ctx context.Context, p *types.Project, svc types.ServiceConfig) error {
	ctxDir := svc.Build.Context
	if ctxDir == "" {
		ctxDir = "."
	}
	args := []string{"build", "-t", imageRef(p, svc)}
	if svc.Build.Dockerfile != "" {
		// Compose defines `dockerfile` relative to `context`, but `container
		// build`'s -f resolves relative to the CWD, not the context-dir
		// positional arg — so it must be joined here.
		dockerfile := svc.Build.Dockerfile
		if !filepath.IsAbs(dockerfile) {
			dockerfile = filepath.Join(ctxDir, dockerfile)
		}
		args = append(args, "-f", dockerfile)
	}
	if svc.Build.Target != "" {
		args = append(args, "--target", svc.Build.Target)
	}
	for _, k := range sortedEnvKeys(svc.Build.Args) {
		if v := svc.Build.Args[k]; v != nil {
			args = append(args, "--build-arg", k+"="+*v)
		} else {
			args = append(args, "--build-arg", k)
		}
	}
	for _, k := range sortedKeys(svc.Build.Labels) {
		args = append(args, "--label", k+"="+svc.Build.Labels[k])
	}
	args = append(args, ctxDir)
	return backend.Run(ctx, args...)
}

// runArgs builds the `container run` argv for a service, including a
// config-hash label so a later `up` can detect config changes and recreate.
func runArgs(p *types.Project, svc types.ServiceConfig) []string {
	flags, tail := runSpec(p, svc)
	flags = append(flags, "--label", backend.LabelConfigHash+"="+hashStrings(append(append([]string{}, flags...), tail...)))
	return append(flags, tail...)
}

// configHash is the stable fingerprint of a service's run config (flags +
// image + command), excluding the hash label itself. Up compares it against the
// running container's stored hash to decide whether to recreate.
func configHash(p *types.Project, svc types.ServiceConfig) string {
	flags, tail := runSpec(p, svc)
	return hashStrings(append(flags, tail...))
}

// runSpec returns the `container run` flags (through volumes) and the trailing
// image+command args, split so the config-hash label can be inserted between.
func runSpec(p *types.Project, svc types.ServiceConfig) (flags, tail []string) {
	flags = []string{
		"run", "--detach",
		"--name", containerName(p, svc.Name),
		"--network", networkName(p),
		// Project name as the DNS domain: the container registers as
		// "<service>.<project>" and peers (which share this search domain)
		// resolve the bare "<service>" once the domain is created. Safe even when
		// the domain is absent (the flag is a no-op then).
		"--dns-domain", p.Name,
		"--label", backend.LabelProject + "=" + p.Name,
		"--label", backend.LabelService + "=" + svc.Name,
	}
	if m := memLimit(svc); m != "" {
		flags = append(flags, "--memory", m)
	}
	if c := cpuLimit(svc); c != "" {
		flags = append(flags, "--cpus", c)
	}
	if svc.WorkingDir != "" {
		flags = append(flags, "--workdir", svc.WorkingDir)
	}
	if svc.User != "" {
		flags = append(flags, "--user", svc.User)
	}
	if len(svc.Entrypoint) > 0 {
		flags = append(flags, "--entrypoint", svc.Entrypoint[0])
	}
	for _, k := range sortedKeys(svc.Labels) {
		flags = append(flags, "--label", k+"="+svc.Labels[k])
	}
	for _, k := range sortedEnvKeys(svc.Environment) {
		if v := svc.Environment[k]; v != nil {
			flags = append(flags, "--env", k+"="+*v)
		} else {
			flags = append(flags, "--env", k)
		}
	}
	for _, port := range svc.Ports {
		if port.Published != "" {
			flags = append(flags, "--publish", fmt.Sprintf("%s:%d", port.Published, port.Target))
		}
	}
	for _, vol := range svc.Volumes {
		if vol.Source != "" {
			flags = append(flags, "--volume", vol.Source+":"+vol.Target)
		}
	}
	tail = []string{imageRef(p, svc)}
	if len(svc.Entrypoint) > 1 {
		// container's --entrypoint takes a single executable; the rest of a
		// compose entrypoint list becomes leading arguments.
		tail = append(tail, svc.Entrypoint[1:]...)
	}
	tail = append(tail, svc.Command...)
	return flags, tail
}

func hashStrings(ss []string) string {
	h := fnv.New64a()
	for _, s := range ss {
		_, _ = h.Write([]byte(s))
		_, _ = h.Write([]byte{0})
	}
	return strconv.FormatUint(h.Sum64(), 16)
}

// containerName scopes the container to its project as "<service>.<project>".
// This (a) keeps names unique across projects — no collisions when two stacks
// both define "db" — and (b) registers the container under the project's DNS
// domain so peers resolve the bare "<service>" via their shared search domain
// (requires a one-time `sudo container system dns create <project>`).
func containerName(p *types.Project, service string) string {
	return service + "." + p.Name
}

func imageRef(p *types.Project, svc types.ServiceConfig) string {
	if svc.Image != "" {
		return svc.Image
	}
	return p.Name + "-" + svc.Name // built image tag
}

// --- resource limits -------------------------------------------------------

// Memory/CPU sizing resolves in order: deploy.resources.limits → legacy
// mem_limit/cpus → deploy.resources.reservations (→ mem_reservation). When
// nothing is set we emit no flag, so `container` sizes the VM with its own
// defaults (the `[container]` system property). `container` takes whole CPUs, so
// a fractional value is rounded up; bytes use the smallest k/m/g suffix.

func memLimit(svc types.ServiceConfig) string {
	if b := resourceMem(svc); b > 0 {
		return humanBytes(int64(b))
	}
	return ""
}

func cpuLimit(svc types.ServiceConfig) string {
	if n := resourceCPU(svc); n > 0 {
		return strconv.Itoa(int(math.Ceil(float64(n))))
	}
	return ""
}

func resourceMem(svc types.ServiceConfig) types.UnitBytes {
	if r := limits(svc); r != nil && r.MemoryBytes > 0 {
		return r.MemoryBytes
	}
	if svc.MemLimit > 0 {
		return svc.MemLimit
	}
	if r := reservations(svc); r != nil && r.MemoryBytes > 0 {
		return r.MemoryBytes
	}
	return svc.MemReservation
}

func resourceCPU(svc types.ServiceConfig) types.NanoCPUs {
	if r := limits(svc); r != nil && r.NanoCPUs > 0 {
		return r.NanoCPUs
	}
	if svc.CPUS > 0 {
		return types.NanoCPUs(svc.CPUS)
	}
	if r := reservations(svc); r != nil && r.NanoCPUs > 0 {
		return r.NanoCPUs
	}
	return 0
}

func limits(svc types.ServiceConfig) *types.Resource {
	if svc.Deploy == nil {
		return nil
	}
	return svc.Deploy.Resources.Limits
}

func reservations(svc types.ServiceConfig) *types.Resource {
	if svc.Deploy == nil {
		return nil
	}
	return svc.Deploy.Resources.Reservations
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
