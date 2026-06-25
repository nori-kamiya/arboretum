package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
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
//	When the user runs `orchard up --dry-run`
//	Then orchard emits the exact `container` commands, in dependency order,
//	     honoring each service's memory/cpu limit.
func TestUp_DryRun_EmitsExpectedCommands(t *testing.T) {
	code, out, errOut := runCLI("up", "--dry-run", "-f", composeFile)
	if code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, errOut)
	}

	mustContain(t, out,
		"container network create --label orchard.project=demo demo_default",
		"container volume create --label orchard.project=demo dbdata",
		"--name db --network demo_default",
		"--memory 512m --cpus 0.5", // db limits from compose
		"--memory 256m",            // redis limit
		"container build -t demo-api -f Dockerfile",
		"--name api --network demo_default",
		"--memory 512m --cpus 1", // api limits
		"--publish 8080:3000",
	)

	// Dependency order: db and redis must start before api.
	if idx(out, "--name api ") < idx(out, "--name db ") {
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
	if !strings.Contains(out, "container logs api") {
		t.Fatalf("logs output = %q", out)
	}
}

func TestExec_DryRun(t *testing.T) {
	code, out, errOut := runCLI("exec", "--dry-run", "-f", composeFile, "db", "psql", "-U", "postgres")
	if code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "container exec --tty --interactive db psql -U postgres") {
		t.Fatalf("exec output = %q", out)
	}
}

func TestExec_DryRun_WithFlags(t *testing.T) {
	code, out, errOut := runCLI("exec", "--dry-run", "-f", composeFile,
		"-d", "-T", "-e", "K=V", "-w", "/app", "-u", "root", "db", "sh")
	if code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "container exec --detach --env K=V --workdir /app --user root db sh") {
		t.Fatalf("exec output = %q", out)
	}
}

func TestExec_MissingArgs(t *testing.T) {
	code, _, errOut := runCLI("exec", "--dry-run", "-f", composeFile, "db")
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (needs SERVICE + COMMAND)", code)
	}
	if !strings.Contains(errOut, "orchard:") {
		t.Fatalf("stderr = %q", errOut)
	}
}

func TestExec_LoadError(t *testing.T) {
	code, _, errOut := runCLI("exec", "--dry-run", "-f", "no-such-file.yaml", "db", "sh")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut, "orchard:") {
		t.Fatalf("stderr = %q", errOut)
	}
}

func TestEachCommand_LoadError(t *testing.T) {
	for _, sub := range []string{"up", "down", "ps", "logs"} {
		code, _, errOut := runCLI(sub, "--dry-run", "-f", "no-such-file.yaml")
		if code != 1 {
			t.Errorf("%s: exit = %d, want 1", sub, code)
		}
		if !strings.Contains(errOut, "orchard:") {
			t.Errorf("%s: stderr = %q", sub, errOut)
		}
	}
}

func TestMain_WiringCoversEntrypoint(t *testing.T) {
	var code int
	oldExit, oldArgs := osExit, os.Args
	osExit = func(c int) { code = c }
	os.Args = []string{"orchard", "--help"}
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
