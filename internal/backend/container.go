// Package backend is a thin wrapper around Apple's `container` CLI.
//
// Everything that touches the host container runtime goes through here so the
// orchestration layer stays free of os/exec details. When DryRun is set, no
// real command is executed: Run prints what it *would* run and Output behaves
// as if the runtime returned nothing. That lets the whole translation layer be
// exercised without `container` installed (e.g. on macOS < 26 or in CI).
package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Bin is the Apple container CLI executable name. It is a var (not const) so
// tests can point it at a stub binary.
var Bin = "container"

// DryRun, when true, prints commands instead of executing them.
var DryRun bool

// Stdout is where dry-run command echoes are written. Overridable in tests.
var Stdout io.Writer = os.Stdout

// execFn is the seam used by Run/Output to reach the runtime. Tests swap it to
// avoid spawning real processes.
var execFn = runExec

// SetExecForTest swaps the exec seam and returns a restore function. Intended
// only for tests in this module (the package lives under internal/).
func SetExecForTest(fn func(ctx context.Context, stream bool, args ...string) ([]byte, error)) func() {
	prev := execFn
	execFn = fn
	return func() { execFn = prev }
}

func runExec(ctx context.Context, stream bool, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, Bin, args...)
	if stream {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return nil, cmd.Run()
	}
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if ok := asExitError(err, &ee); ok {
			return out, fmt.Errorf("%s %s: %w: %s", Bin, strings.Join(args, " "), err, strings.TrimSpace(string(ee.Stderr)))
		}
		return out, fmt.Errorf("%s %s: %w", Bin, strings.Join(args, " "), err)
	}
	return out, nil
}

func asExitError(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}

// Run executes `container <args>`, streaming stdout/stderr to the user.
func Run(ctx context.Context, args ...string) error {
	if DryRun {
		fmt.Fprintln(Stdout, Bin, strings.Join(args, " "))
		return nil
	}
	_, err := execFn(ctx, true, args...)
	return err
}

// Output executes `container <args>` and captures stdout. Under DryRun it
// returns no output so callers behave as if nothing exists yet.
func Output(ctx context.Context, args ...string) ([]byte, error) {
	if DryRun {
		return nil, nil
	}
	return execFn(ctx, false, args...)
}

// NotInstalledError is returned by EnsureInstalled when the container CLI is
// missing from PATH. Its message walks the user through installing it.
type NotInstalledError struct{ Bin string }

func (e *NotInstalledError) Error() string {
	b := e.Bin
	return "Apple `" + b + "` is not installed (not found on PATH).\n\n" +
		"orchard drives Apple's container runtime, so it must be installed first:\n\n" +
		"  1. Requires macOS 26 (Tahoe) or later on Apple silicon.\n" +
		"  2. Install the latest release:\n" +
		"       - download the signed .pkg from\n" +
		"         https://github.com/apple/container/releases\n" +
		"       - or, with Homebrew:  brew install --cask container\n" +
		"  3. Start the runtime once:  " + b + " system start\n\n" +
		"Then re-run your command. To preview commands without a runtime, add --dry-run."
}

// EnsureInstalled verifies the container CLI is reachable on PATH, returning a
// guidance-bearing *NotInstalledError otherwise. It is a no-op under DryRun so
// the translation layer can be exercised without a runtime installed.
func EnsureInstalled() error {
	if DryRun {
		return nil
	}
	if _, err := exec.LookPath(Bin); err != nil {
		return &NotInstalledError{Bin: Bin}
	}
	return nil
}

// Container is a tolerant view over an item of `container ls --format json`.
// The exact schema of Apple container's JSON output still needs to be pinned
// against a real install (see README "未検証"); we therefore parse defensively.
type Container struct {
	Name   string
	Labels map[string]string
	Raw    map[string]any
}

// ListByProject returns all containers (running or not) tagged with our project
// label. Used by `down`/`ps` so we never rely on name conventions for cleanup.
func ListByProject(ctx context.Context, project string) ([]Container, error) {
	out, err := Output(ctx, "ls", "--all", "--format", "json")
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	var raw []map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse `container ls --format json`: %w", err)
	}
	var res []Container
	for _, m := range raw {
		labels := extractLabels(m)
		if labels[LabelProject] != project {
			continue
		}
		res = append(res, Container{
			Name:   firstString(m, "name", "Name", "id", "ID"),
			Labels: labels,
			Raw:    m,
		})
	}
	return res, nil
}

// EnsureNetwork creates the project network if it does not already exist.
func EnsureNetwork(ctx context.Context, name, project string) error {
	out, err := Output(ctx, "network", "list", "--format", "json")
	if err == nil && len(out) > 0 {
		var nets []map[string]any
		if json.Unmarshal(out, &nets) == nil {
			for _, n := range nets {
				if firstString(n, "name", "Name") == name {
					return nil
				}
			}
		}
	}
	return Run(ctx, "network", "create", "--label", LabelProject+"="+project, name)
}

// EnsureVolume creates a named volume if missing (best-effort/idempotent).
func EnsureVolume(ctx context.Context, name, project string) error {
	return Run(ctx, "volume", "create", "--label", LabelProject+"="+project, name)
}

// Label keys used to track resources we own.
const (
	LabelProject = "orchard.project"
	LabelService = "orchard.service"
)

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

func extractLabels(m map[string]any) map[string]string {
	out := map[string]string{}
	v, ok := m["labels"]
	if !ok {
		v, ok = m["Labels"]
	}
	if !ok {
		return out
	}
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if s, ok := val.(string); ok {
				out[k] = s
			}
		}
	case []any: // ["k=v", ...]
		for _, item := range t {
			if s, ok := item.(string); ok {
				if k, val, found := strings.Cut(s, "="); found {
					out[k] = val
				}
			}
		}
	}
	return out
}
