package backend

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStream_DryRunEchoes(t *testing.T) {
	var buf bytes.Buffer
	DryRun, Stdout = true, &buf
	t.Cleanup(func() { DryRun, Stdout = false, os.Stdout })
	if err := Stream(context.Background(), io.Discard, "logs", "-f", "x"); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "container logs -f x\n" {
		t.Fatalf("echo = %q", got)
	}
}

func TestStream_RealWritesOutput(t *testing.T) {
	Bin = makeStub(t, "printf 'a\\nb\\n'\n")
	t.Cleanup(func() { Bin = "container" })
	var buf bytes.Buffer
	if err := Stream(context.Background(), &buf, "logs"); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "a\nb\n" {
		t.Fatalf("stream output = %q", buf.String())
	}
}

func TestStream_SeamCalledAndErrorPropagates(t *testing.T) {
	var gotArgs []string
	t.Cleanup(SetStreamForTest(func(_ context.Context, w io.Writer, args ...string) error {
		gotArgs = args
		_, _ = io.WriteString(w, "hi")
		return errors.New("boom")
	}))
	var buf bytes.Buffer
	if err := Stream(context.Background(), &buf, "logs", "-f", "db"); err == nil {
		t.Fatal("expected error")
	}
	if buf.String() != "hi" || strings.Join(gotArgs, " ") != "logs -f db" {
		t.Fatalf("buf=%q args=%v", buf.String(), gotArgs)
	}
}

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

func TestEnsureInstalled_DryRunSkips(t *testing.T) {
	DryRun = true
	Bin = "orchard-no-such-binary-zzz"
	t.Cleanup(func() { DryRun = false; Bin = "container" })
	if err := EnsureInstalled(); err != nil {
		t.Fatalf("dry-run should skip the check, got %v", err)
	}
}

func TestEnsureInstalled_PresentBinary(t *testing.T) {
	Bin = makeStub(t, "exit 0\n") // a real, executable path
	t.Cleanup(func() { Bin = "container" })
	if err := EnsureInstalled(); err != nil {
		t.Fatalf("present binary should pass, got %v", err)
	}
}

func TestEnsureInstalled_MissingBinaryGuides(t *testing.T) {
	Bin = "orchard-no-such-binary-zzz"
	t.Cleanup(func() { Bin = "container" })
	err := EnsureInstalled()
	var ni *NotInstalledError
	if !errors.As(err, &ni) {
		t.Fatalf("want *NotInstalledError, got %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"not installed", "github.com/apple/container", "system start"} {
		if !strings.Contains(msg, want) {
			t.Errorf("guidance missing %q in:\n%s", want, msg)
		}
	}
}

func TestDNSDomainExists(t *testing.T) {
	swapExec(t, func(_ context.Context, _ bool, _ ...string) ([]byte, error) {
		return []byte(`["alpha","demo"]`), nil
	})
	if ok, err := DNSDomainExists(context.Background(), "demo"); err != nil || !ok {
		t.Fatalf("want found, got %v %v", ok, err)
	}
	if ok, _ := DNSDomainExists(context.Background(), "missing"); ok {
		t.Fatal("want not found")
	}
}

func TestDNSDomainExists_EmptyErrorAndBadJSON(t *testing.T) {
	swapExec(t, func(_ context.Context, _ bool, _ ...string) ([]byte, error) { return nil, nil })
	if ok, err := DNSDomainExists(context.Background(), "x"); ok || err != nil {
		t.Fatalf("empty: %v %v", ok, err)
	}
	swapExec(t, func(_ context.Context, _ bool, _ ...string) ([]byte, error) { return nil, errors.New("down") })
	if _, err := DNSDomainExists(context.Background(), "x"); err == nil {
		t.Fatal("want exec error")
	}
	swapExec(t, func(_ context.Context, _ bool, _ ...string) ([]byte, error) { return []byte("not json"), nil })
	if _, err := DNSDomainExists(context.Background(), "x"); err == nil {
		t.Fatal("want parse error")
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

// Regression: Apple container 1.0 nests labels/id under "configuration" and
// state under "status". Verified against a real `container ls --format json`.
func TestListByProject_NestedConfigurationSchema(t *testing.T) {
	swapExec(t, func(_ context.Context, _ bool, _ ...string) ([]byte, error) {
		return []byte(`[
			{"id":"web","status":{"state":"running"},
			 "configuration":{"id":"web","labels":{"orchard.project":"demo","orchard.service":"web"}}},
			{"id":"other","configuration":{"labels":{"orchard.project":"x"}}}
		]`), nil
	})
	cs, err := ListByProject(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 {
		t.Fatalf("want 1 container for demo, got %+v", cs)
	}
	if cs[0].Name != "web" || cs[0].State != "running" || cs[0].Labels[LabelService] != "web" {
		t.Fatalf("got %+v", cs[0])
	}
}

func TestEnsureNetwork_AlreadyExists_NestedSchema(t *testing.T) {
	var createCalled bool
	swapExec(t, func(_ context.Context, _ bool, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[1] == "list" {
			// No top-level id, so nameOf must fall back to configuration.name.
			return []byte(`[{"configuration":{"name":"demo_default"}}]`), nil
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
		t.Fatal("should not re-create an existing (nested-schema) network")
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
