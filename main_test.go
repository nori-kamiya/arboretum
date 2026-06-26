package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/nori-kamiya/arboretum/internal/backend"
)

const composeFile = "examples/compose.yaml"

// runCLI executes the CLI with args and returns (exitCode, stdout, stderr).
func runCLI(args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// Feature: bringing a stack up
//
//	Given a compose file describing api/db/redis with per-service limits
//	When the user runs `arboretum up --dry-run`
//	Then arboretum emits the exact `container` commands, in dependency order,
//	     honoring each service's memory/cpu limit.
func TestUp_DryRun_EmitsExpectedCommands(t *testing.T) {
	code, out, errOut := runCLI("up", "--dry-run", "-f", composeFile)
	if code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, errOut)
	}

	mustContain(t, out,
		"container network create --label arboretum.project=demo demo_default",
		"container volume create --label arboretum.project=demo dbdata",
		"--name db.demo --network demo_default --dns-domain demo",
		"--memory 512m --cpus 1", // db limits (0.5 cpus rounds up to whole CPU)
		"--memory 256m",            // redis limit
		"container build -t demo-api -f Dockerfile",
		"--name api.demo --network demo_default --dns-domain demo",
		"--memory 512m --cpus 1", // api limits
		"--publish 8080:3000",
	)

	// Dependency order: db and redis must start before api.
	if idx(out, "--name api.demo ") < idx(out, "--name db.demo ") {
		t.Fatal("api started before its dependency db")
	}
}

func TestUp_Detached(t *testing.T) {
	code, out, _ := runCLI("up", "-d", "--dry-run", "-f", composeFile)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.Contains(out, "container logs") {
		t.Fatal("detached up should not tail logs")
	}
}

func TestDown_DryRun(t *testing.T) {
	code, out, _ := runCLI("down", "--dry-run", "-f", composeFile)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out, "container network delete demo_default") {
		t.Fatalf("down should delete the network, got: %s", out)
	}
}

func TestPs_DryRun_NoContainers(t *testing.T) {
	code, out, _ := runCLI("ps", "--dry-run", "-f", composeFile)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out, "no containers for project demo") {
		t.Fatalf("ps output = %q", out)
	}
}

func TestLogs_DryRun(t *testing.T) {
	code, out, _ := runCLI("logs", "--dry-run", "-f", composeFile)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out, "container logs api.demo") {
		t.Fatalf("logs output = %q", out)
	}
}

func TestExec_DryRun(t *testing.T) {
	code, out, errOut := runCLI("exec", "--dry-run", "-f", composeFile, "db", "psql", "-U", "postgres")
	if code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "container exec --tty --interactive db.demo psql -U postgres") {
		t.Fatalf("exec output = %q", out)
	}
}

func TestExec_DryRun_WithFlags(t *testing.T) {
	code, out, errOut := runCLI("exec", "--dry-run", "-f", composeFile,
		"-d", "-T", "-e", "K=V", "-w", "/app", "-u", "root", "db", "sh")
	if code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "container exec --detach --env K=V --workdir /app --user root db.demo sh") {
		t.Fatalf("exec output = %q", out)
	}
}

func TestExec_MissingArgs(t *testing.T) {
	code, _, errOut := runCLI("exec", "--dry-run", "-f", composeFile, "db")
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (needs SERVICE + COMMAND)", code)
	}
	if !strings.Contains(errOut, "arboretum:") {
		t.Fatalf("stderr = %q", errOut)
	}
}

func TestExec_LoadError(t *testing.T) {
	code, _, errOut := runCLI("exec", "--dry-run", "-f", "no-such-file.yaml", "db", "sh")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut, "arboretum:") {
		t.Fatalf("stderr = %q", errOut)
	}
}

func TestPs_QuietAndJSON_DryRun(t *testing.T) {
	for _, flag := range []string{"--quiet", "--format=json"} {
		code, _, errOut := runCLI("ps", flag, "--dry-run", "-f", composeFile)
		if code != 0 {
			t.Fatalf("ps %s: exit %d, stderr=%s", flag, code, errOut)
		}
	}
}

func TestLifecycleCommands_DryRun(t *testing.T) {
	for _, sub := range []string{"stop", "start", "restart"} {
		code, _, errOut := runCLI(sub, "--dry-run", "-f", composeFile)
		if code != 0 {
			t.Fatalf("%s: exit %d, stderr=%s", sub, code, errOut)
		}
	}
}

func TestConfig_DryRun(t *testing.T) {
	code, out, errOut := runCLI("config", "--dry-run", "-f", composeFile)
	if code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "api") || !strings.Contains(out, "services:") {
		t.Fatalf("config output = %q", out)
	}
	code, out, _ = runCLI("config", "--services", "--dry-run", "-f", composeFile)
	if code != 0 || !strings.Contains(out, "db") {
		t.Fatalf("config --services = %q", out)
	}
}

func TestEachCommand_LoadError(t *testing.T) {
	for _, sub := range []string{"up", "down", "ps", "logs", "stop", "start", "restart", "config"} {
		code, _, errOut := runCLI(sub, "--dry-run", "-f", "no-such-file.yaml")
		if code != 1 {
			t.Errorf("%s: exit = %d, want 1", sub, code)
		}
		if !strings.Contains(errOut, "arboretum:") {
			t.Errorf("%s: stderr = %q", sub, errOut)
		}
	}
}

// Feature: friendly guidance when Apple `container` is not installed.
//
//	Given the container CLI is missing from PATH
//	When the user runs a real (non-dry-run) command
//	Then arboretum fails fast and tells them how to install it.
func TestPreflight_GuidesWhenContainerMissing(t *testing.T) {
	oldBin := backend.Bin
	backend.Bin = "arboretum-no-such-binary-zzz"
	t.Cleanup(func() { backend.Bin = oldBin; backend.DryRun = false })

	code, _, errOut := runCLI("up", "-f", composeFile) // note: no --dry-run
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	for _, want := range []string{"arboretum:", "not installed", "github.com/apple/container"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr missing %q:\n%s", want, errOut)
		}
	}
}

// Dry-run must still work without `container` installed (the check is skipped).
func TestPreflight_DryRunWorksWithoutContainer(t *testing.T) {
	oldBin := backend.Bin
	backend.Bin = "arboretum-no-such-binary-zzz"
	t.Cleanup(func() { backend.Bin = oldBin; backend.DryRun = false })

	code, out, errOut := runCLI("up", "--dry-run", "-f", composeFile)
	if code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "run --detach --name db") {
		t.Fatalf("dry-run output = %q", out)
	}
}

func TestBuilder_Subcommands_DryRun(t *testing.T) {
	for _, action := range []string{"status", "start", "stop", "delete"} {
		code, out, errOut := runCLI("builder", action, "--dry-run")
		if code != 0 {
			t.Fatalf("builder %s: exit %d, stderr=%s", action, code, errOut)
		}
		if !strings.Contains(out, "container builder "+action) {
			t.Fatalf("builder %s output = %q", action, out)
		}
	}
}

func TestBuilder_PreflightGuidesWhenMissing(t *testing.T) {
	oldBin := backend.Bin
	backend.Bin = "arboretum-no-such-binary-zzz"
	t.Cleanup(func() { backend.Bin = oldBin; backend.DryRun = false })

	code, _, errOut := runCLI("builder", "stop") // real run, no --dry-run
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut, "not installed") {
		t.Fatalf("stderr = %q", errOut)
	}
}

func TestDown_PruneBuilder_DryRun(t *testing.T) {
	code, out, errOut := runCLI("down", "--prune-builder", "--dry-run", "-f", composeFile)
	if code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, errOut)
	}
	mustContain(t, out,
		"container network delete demo_default",
		"container builder stop",
	)
}

func TestVersion_Command(t *testing.T) {
	code, out, errOut := runCLI("version")
	if code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "arboretum dev (commit none, built unknown)") {
		t.Fatalf("version output = %q", out)
	}
}

func TestVersion_Flag(t *testing.T) {
	code, out, errOut := runCLI("--version")
	if code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "arboretum version dev") {
		t.Fatalf("--version output = %q", out)
	}
}

func TestMain_WiringCoversEntrypoint(t *testing.T) {
	var code int
	oldExit, oldArgs := osExit, os.Args
	osExit = func(c int) { code = c }
	os.Args = []string{"arboretum", "--help"}
	t.Cleanup(func() { osExit, os.Args = oldExit, oldArgs })

	main()

	if code != 0 {
		t.Fatalf("entrypoint exit = %d", code)
	}
}

// --- helpers ---------------------------------------------------------------

func mustContain(t *testing.T, hay string, needles ...string) {
	t.Helper()
	for _, n := range needles {
		if !strings.Contains(hay, n) {
			t.Errorf("output missing %q\nfull output:\n%s", n, hay)
		}
	}
}

func idx(hay, needle string) int { return strings.Index(hay, needle) }
