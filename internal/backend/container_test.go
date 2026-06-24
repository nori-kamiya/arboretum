package backend

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// swapExec replaces the exec seam for the duration of a test.
func swapExec(t *testing.T, fn func(ctx context.Context, stream bool, args ...string) ([]byte, error)) {
	t.Helper()
	t.Cleanup(SetExecForTest(fn))
}

// makeStub writes an executable shell script and returns its path.
func makeStub(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "stub")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRun_DryRun_EchoesCommand(t *testing.T) {
	// Given dry-run is on
	var buf bytes.Buffer
	DryRun, Stdout = true, &buf
	t.Cleanup(func() { DryRun, Stdout = false, os.Stdout })

	// When running a command
	if err := Run(context.Background(), "network", "create", "x"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Then it is echoed, not executed
	if got := buf.String(); got != "container network create x\n" {
		t.Fatalf("echo = %q", got)
	}
}

func TestOutput_DryRun_ReturnsNothing(t *testing.T) {
	DryRun = true
	t.Cleanup(func() { DryRun = false })
	out, err := Output(context.Background(), "ls")
	if err != nil || out != nil {
		t.Fatalf("got %q, %v", out, err)
	}
}

func TestRun_DelegatesToExecFn(t *testing.T) {
	var gotStream bool
	var gotArgs []string
	swapExec(t, func(_ context.Context, stream bool, args ...string) ([]byte, error) {
		gotStream, gotArgs = stream, args
		return nil, nil
	})
	if err := Run(context.Background(), "stop", "db"); err != nil {
		t.Fatal(err)
	}
	if !gotStream || strings.Join(gotArgs, " ") != "stop db" {
		t.Fatalf("stream=%v args=%v", gotStream, gotArgs)
	}
}

func TestOutput_DelegatesToExecFn(t *testing.T) {
	swapExec(t, func(_ context.Context, _ bool, _ ...string) ([]byte, error) {
		return []byte("payload"), nil
	})
	out, err := Output(context.Background(), "ls")
	if err != nil || string(out) != "payload" {
		t.Fatalf("got %q, %v", out, err)
	}
}

func TestRun_PropagatesExecError(t *testing.T) {
	swapExec(t, func(_ context.Context, _ bool, _ ...string) ([]byte, error) {
		return nil, errors.New("boom")
	})
	if err := Run(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}

func TestRunExec_StreamSuccess(t *testing.T) {
	Bin = makeStub(t, "exit 0\n")
	t.Cleanup(func() { Bin = "container" })
	if err := Run(context.Background(), "anything"); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
}

func TestRunExec_CapturesStdout(t *testing.T) {
	Bin = makeStub(t, "printf 'hello'\n")
	t.Cleanup(func() { Bin = "container" })
	out, err := Output(context.Background(), "ls")
	if err != nil || string(out) != "hello" {
		t.Fatalf("got %q, %v", out, err)
	}
}

func TestRunExec_ExitErrorIncludesStderr(t *testing.T) {
	Bin = makeStub(t, "printf 'kaboom' 1>&2\nexit 3\n")
	t.Cleanup(func() { Bin = "container" })
	_, err := Output(context.Background(), "boom")
	if err == nil || !strings.Contains(err.Error(), "kaboom") {
		t.Fatalf("want stderr in error, got %v", err)
	}
}

func TestRunExec_NonExitError(t *testing.T) {
	Bin = filepath.Join(t.TempDir(), "does-not-exist")
	t.Cleanup(func() { Bin = "container" })
	_, err := Output(context.Background(), "ls")
	if err == nil || strings.Contains(err.Error(), "kaboom") {
		t.Fatalf("want start error, got %v", err)
	}
}

func TestAsExitError_FalseForPlainError(t *testing.T) {
	// The true branch is covered by TestRunExec_ExitErrorIncludesStderr.
	if asExitError(errors.New("x"), nil) {
		t.Fatal("non-ExitError should be false")
	}
}

func TestListByProject_FiltersByLabel_MapForm(t *testing.T) {
	swapExec(t, func(_ context.Context, _ bool, _ ...string) ([]byte, error) {
		return []byte(`[
			{"name":"db","labels":{"orchard.project":"demo","orchard.service":"db"}},
			{"name":"other","labels":{"orchard.project":"x"}}
		]`), nil
	})
	cs, err := ListByProject(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].Name != "db" || cs[0].Labels[LabelService] != "db" {
		t.Fatalf("got %+v", cs)
	}
}

func TestListByProject_ListForm(t *testing.T) {
	swapExec(t, func(_ context.Context, _ bool, _ ...string) ([]byte, error) {
		return []byte(`[{"name":"db","labels":["orchard.project=demo","bad"]}]`), nil
	})
	cs, err := ListByProject(context.Background(), "demo")
	if err != nil || len(cs) != 1 {
		t.Fatalf("got %+v, %v", cs, err)
	}
}

func TestListByProject_EmptyOutput(t *testing.T) {
	swapExec(t, func(_ context.Context, _ bool, _ ...string) ([]byte, error) {
		return nil, nil
	})
	cs, err := ListByProject(context.Background(), "demo")
	if err != nil || cs != nil {
		t.Fatalf("got %+v, %v", cs, err)
	}
}

func TestListByProject_OutputError(t *testing.T) {
	swapExec(t, func(_ context.Context, _ bool, _ ...string) ([]byte, error) {
		return nil, errors.New("down")
	})
	if _, err := ListByProject(context.Background(), "demo"); err == nil {
		t.Fatal("expected error")
	}
}

func TestListByProject_ParseError(t *testing.T) {
	swapExec(t, func(_ context.Context, _ bool, _ ...string) ([]byte, error) {
		return []byte("not json"), nil
	})
	if _, err := ListByProject(context.Background(), "demo"); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestEnsureNetwork_AlreadyExists(t *testing.T) {
	var createCalled bool
	swapExec(t, func(_ context.Context, _ bool, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "network" && args[1] == "list" {
			return []byte(`[{"name":"demo_default"}]`), nil
		}
		if len(args) >= 2 && args[1] == "create" {
			createCalled = true
		}
		return nil, nil
	})
	if err := EnsureNetwork(context.Background(), "demo_default", "demo"); err != nil {
		t.Fatal(err)
	}
	if createCalled {
		t.Fatal("should not create an existing network")
	}
}

func TestEnsureNetwork_CreatesWhenMissing(t *testing.T) {
	var createCalled bool
	swapExec(t, func(_ context.Context, _ bool, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[1] == "list" {
			return []byte(`[]`), nil
		}
		if len(args) >= 2 && args[1] == "create" {
			createCalled = true
		}
		return nil, nil
	})
	if err := EnsureNetwork(context.Background(), "demo_default", "demo"); err != nil {
		t.Fatal(err)
	}
	if !createCalled {
		t.Fatal("expected network create")
	}
}

func TestEnsureNetwork_ListErrorStillCreates(t *testing.T) {
	var createCalled bool
	swapExec(t, func(_ context.Context, _ bool, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[1] == "list" {
			return nil, errors.New("nope")
		}
		createCalled = true
		return nil, nil
	})
	if err := EnsureNetwork(context.Background(), "demo_default", "demo"); err != nil {
		t.Fatal(err)
	}
	if !createCalled {
		t.Fatal("expected create after list error")
	}
}

func TestEnsureVolume(t *testing.T) {
	var got []string
	swapExec(t, func(_ context.Context, _ bool, args ...string) ([]byte, error) {
		got = args
		return nil, nil
	})
	if err := EnsureVolume(context.Background(), "dbdata", "demo"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, " ") != "volume create --label orchard.project=demo dbdata" {
		t.Fatalf("args = %v", got)
	}
}

func TestFirstString_NoMatch(t *testing.T) {
	if firstString(map[string]any{"x": 1}, "name", "Name") != "" {
		t.Fatal("want empty")
	}
}

func TestExtractLabels_Variants(t *testing.T) {
	if l := extractLabels(map[string]any{}); len(l) != 0 {
		t.Fatalf("missing labels -> %v", l)
	}
	if l := extractLabels(map[string]any{"Labels": map[string]any{"a": "b", "n": 1}}); l["a"] != "b" || len(l) != 1 {
		t.Fatalf("map form -> %v", l)
	}
	if l := extractLabels(map[string]any{"labels": []any{"a=b", "nocut", 7}}); l["a"] != "b" || len(l) != 1 {
		t.Fatalf("list form -> %v", l)
	}
}
