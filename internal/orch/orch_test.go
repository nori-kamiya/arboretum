package orch

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/nori-kamiya/arboretum/internal/backend"
)

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

// swapSleep replaces the health-poll sleep with a no-op; returns a restore func.
func swapSleep() func() {
	prev := sleepFn
	sleepFn = func(time.Duration) {}
	return func() { sleepFn = prev }
}

func TestLogs_FollowMultiplexesWithPrefixes(t *testing.T) {
	LogColor = false
	var buf bytes.Buffer
	backend.DryRun, backend.Stdout = false, &buf
	t.Cleanup(func() { LogColor = true; backend.DryRun, backend.Stdout = false, os.Stdout })
	t.Cleanup(backend.SetStreamForTest(func(_ context.Context, w io.Writer, args ...string) error {
		name := args[len(args)-1] // last arg is the container name
		_, _ = io.WriteString(w, "hello from "+name+"\n")
		return nil
	}))
	p := &types.Project{Name: "demo", Services: types.Services{"api": {Name: "api"}, "db": {Name: "db"}}}
	if err := Logs(context.Background(), p, true); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	// width = 3 ("api"), so "db" is padded to "db ".
	if !strings.Contains(got, "api | hello from api") || !strings.Contains(got, "db  | hello from db") {
		t.Fatalf("multiplexed output = %q", got)
	}
}

func TestLogs_FollowErrorPropagates(t *testing.T) {
	backend.DryRun = false
	t.Cleanup(func() { backend.DryRun = false })
	t.Cleanup(backend.SetStreamForTest(func(_ context.Context, _ io.Writer, _ ...string) error {
		return errors.New("stream failed")
	}))
	p := &types.Project{Name: "demo", Services: types.Services{"api": {Name: "api"}}}
	if err := Logs(context.Background(), p, true); err == nil {
		t.Fatal("expected stream error")
	}
}

func TestLogs_FollowSwallowsErrorOnCancel(t *testing.T) {
	backend.DryRun = false
	t.Cleanup(func() { backend.DryRun = false })
	t.Cleanup(backend.SetStreamForTest(func(_ context.Context, _ io.Writer, _ ...string) error {
		return errors.New("killed by cancel")
	}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := &types.Project{Name: "demo", Services: types.Services{"api": {Name: "api"}}}
	if err := Logs(ctx, p, true); err != nil {
		t.Fatalf("cancellation should swallow the error, got %v", err)
	}
}

func TestLogPrefix_Colored(t *testing.T) {
	got := logPrefix("api", 0, 5) // LogColor defaults to true
	if !strings.Contains(got, "\x1b[36m") || !strings.Contains(got, "api") || !strings.Contains(got, "| ") {
		t.Fatalf("colored prefix = %q", got)
	}
}

func TestPrefixWriter_PropagatesWriteError(t *testing.T) {
	pw := &prefixWriter{w: errWriter{}, prefix: "x | "}
	if _, err := pw.Write([]byte("hi\n")); err == nil {
		t.Fatal("expected write error")
	}
}

func ptr(s string) *string { return &s }

// captureDryRun runs fn with backend dry-run on and returns emitted commands.
func captureDryRun(t *testing.T, fn func() error) string {
	t.Helper()
	var buf bytes.Buffer
	backend.DryRun, backend.Stdout = true, &buf
	t.Cleanup(func() { backend.DryRun, backend.Stdout = false, os.Stdout })
	if err := fn(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return buf.String()
}

func limitFor(mem types.UnitBytes, cpu types.NanoCPUs) *types.DeployConfig {
	return &types.DeployConfig{Resources: types.Resources{Limits: &types.Resource{MemoryBytes: mem, NanoCPUs: cpu}}}
}

// --- pure helpers ----------------------------------------------------------

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		1 << 30:   "1g",
		512 << 20: "512m",
		500 << 10: "500k",
		1500:      "1500",
	}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestImageRef(t *testing.T) {
	p := &types.Project{Name: "demo"}
	if got := imageRef(p, types.ServiceConfig{Image: "redis:7"}); got != "redis:7" {
		t.Errorf("explicit image = %q", got)
	}
	if got := imageRef(p, types.ServiceConfig{Name: "api"}); got != "demo-api" {
		t.Errorf("built tag = %q", got)
	}
}

func TestContainerName(t *testing.T) {
	if got := containerName(&types.Project{Name: "demo"}, "db"); got != "db.demo" {
		t.Fatalf("container name = %q, want db.demo", got)
	}
}

func TestMemLimit(t *testing.T) {
	if got := memLimit(types.ServiceConfig{Deploy: limitFor(512<<20, 0)}); got != "512m" {
		t.Errorf("deploy limit = %q", got)
	}
	if got := memLimit(types.ServiceConfig{MemLimit: 256 << 20}); got != "256m" {
		t.Errorf("legacy mem_limit = %q", got)
	}
	if got := memLimit(types.ServiceConfig{}); got != "" {
		t.Errorf("no limit = %q", got)
	}
}

func TestCPULimit(t *testing.T) {
	// Apple container takes whole CPUs; fractional compose limits round up.
	cases := map[types.NanoCPUs]string{1: "1", 2: "2", 0.5: "1", 0.1: "1", 1.5: "2"}
	for in, want := range cases {
		if got := cpuLimit(types.ServiceConfig{Deploy: limitFor(0, in)}); got != want {
			t.Errorf("cpuLimit(%v) = %q, want %q", in, got, want)
		}
	}
	if got := cpuLimit(types.ServiceConfig{}); got != "" {
		t.Errorf("no cpus = %q", got)
	}
}

func TestLimits_NilDeploy(t *testing.T) {
	if limits(types.ServiceConfig{}) != nil {
		t.Fatal("nil deploy should yield nil limits")
	}
}

// --- runArgs ---------------------------------------------------------------

func TestRunArgs_FullService(t *testing.T) {
	p := &types.Project{Name: "demo"}
	svc := types.ServiceConfig{
		Name:  "api",
		Image: "myapi:latest",
		Environment: types.MappingWithEquals{
			"A": ptr("1"),
			"B": nil, // pass-through (no value)
		},
		Ports: []types.ServicePortConfig{
			{Target: 3000, Published: "8080"},
			{Target: 9000, Published: ""}, // skipped
		},
		Volumes: []types.ServiceVolumeConfig{
			{Source: "data", Target: "/data"},
			{Source: "", Target: "/x"}, // skipped
		},
		Command: types.ShellCommand{"node", "server.js"},
		Deploy:  limitFor(512<<20, 1),
	}
	got := strings.Join(runArgs(p, svc), " ")
	want := "run --detach --name api.demo --network demo_default --dns-domain demo " +
		"--label arboretum.project=demo --label arboretum.service=api " +
		"--memory 512m --cpus 1 --env A=1 --env B --publish 8080:3000 " +
		"--volume data:/data --label arboretum.config-hash=" + configHash(p, svc) +
		" myapi:latest node server.js"
	if got != want {
		t.Fatalf("runArgs mismatch:\n got: %s\nwant: %s", got, want)
	}
}

func TestRunArgs_WorkdirUserEntrypointLabels(t *testing.T) {
	p := &types.Project{Name: "demo"}
	svc := types.ServiceConfig{
		Name:       "api",
		Image:      "myapi:latest",
		WorkingDir: "/srv",
		User:       "node",
		Entrypoint: types.ShellCommand{"./entry.sh", "--flag", "x"},
		Labels:     types.Labels{"com.example.tier": "web", "com.example.team": "core"},
		Command:    types.ShellCommand{"start"},
	}
	got := strings.Join(runArgs(p, svc), " ")
	want := "run --detach --name api.demo --network demo_default --dns-domain demo " +
		"--label arboretum.project=demo --label arboretum.service=api " +
		"--workdir /srv --user node --entrypoint ./entry.sh " +
		"--label com.example.team=core --label com.example.tier=web " +
		"--label arboretum.config-hash=" + configHash(p, svc) +
		" myapi:latest --flag x start"
	if got != want {
		t.Fatalf("runArgs mismatch:\n got: %s\nwant: %s", got, want)
	}
}

// --- Builder ---------------------------------------------------------------

func TestBuilder_DryRunEmitsAction(t *testing.T) {
	out := captureDryRun(t, func() error { return Builder(context.Background(), "stop") })
	if strings.TrimSpace(out) != "container builder stop" {
		t.Fatalf("builder cmd = %q", out)
	}
}

func TestBuilder_ErrorPropagates(t *testing.T) {
	t.Cleanup(backend.SetExecForTest(func(_ context.Context, _ bool, _ ...string) ([]byte, error) {
		return nil, errors.New("builder down")
	}))
	backend.DryRun = false
	t.Cleanup(func() { backend.DryRun = false })
	if err := Builder(context.Background(), "stop"); err == nil {
		t.Fatal("expected builder error")
	}
}

// --- service DNS hint ------------------------------------------------------

func TestHintServiceDNS(t *testing.T) {
	var buf bytes.Buffer
	backend.Stdout = &buf
	backend.DryRun = false
	t.Cleanup(func() { backend.Stdout = os.Stdout; backend.DryRun = false })

	dnsList := func(domains string) func() {
		return backend.SetExecForTest(func(_ context.Context, _ bool, _ ...string) ([]byte, error) {
			return []byte(domains), nil
		})
	}
	two := &types.Project{Name: "demo", Services: types.Services{"a": {Name: "a"}, "b": {Name: "b"}}}
	one := &types.Project{Name: "demo", Services: types.Services{"a": {Name: "a"}}}

	// Missing domain + multi-service → hint.
	restore := dnsList(`["other"]`)
	hintServiceDNS(context.Background(), two)
	restore()
	if !strings.Contains(buf.String(), "sudo container system dns create demo") {
		t.Fatalf("expected hint, got %q", buf.String())
	}

	// Domain exists → no hint.
	buf.Reset()
	restore = dnsList(`["demo"]`)
	hintServiceDNS(context.Background(), two)
	restore()
	if buf.Len() != 0 {
		t.Fatalf("no hint expected when domain exists, got %q", buf.String())
	}

	// Single service → no hint (and no lookup).
	buf.Reset()
	hintServiceDNS(context.Background(), one)
	if buf.Len() != 0 {
		t.Fatalf("no hint for single-service, got %q", buf.String())
	}

	// dns list errors → silent.
	buf.Reset()
	restore = backend.SetExecForTest(func(_ context.Context, _ bool, _ ...string) ([]byte, error) {
		return nil, errors.New("boom")
	})
	hintServiceDNS(context.Background(), two)
	restore()
	if buf.Len() != 0 {
		t.Fatalf("errors should be silent, got %q", buf.String())
	}

	// dry-run → silent.
	buf.Reset()
	backend.DryRun = true
	hintServiceDNS(context.Background(), two)
	if buf.Len() != 0 {
		t.Fatalf("dry-run should be silent, got %q", buf.String())
	}
}

// --- restart policy --------------------------------------------------------

func TestRestartPolicy(t *testing.T) {
	if got := restartPolicy(types.ServiceConfig{Restart: "always"}); got != "always" {
		t.Errorf("restart field = %q", got)
	}
	if got := restartPolicy(types.ServiceConfig{Restart: "no"}); got != "" {
		t.Errorf("'no' should be empty, got %q", got)
	}
	dep := &types.DeployConfig{RestartPolicy: &types.RestartPolicy{Condition: "on-failure"}}
	if got := restartPolicy(types.ServiceConfig{Deploy: dep}); got != "deploy.restart_policy: on-failure" {
		t.Errorf("deploy policy = %q", got)
	}
	none := &types.DeployConfig{RestartPolicy: &types.RestartPolicy{Condition: "none"}}
	if got := restartPolicy(types.ServiceConfig{Deploy: none}); got != "" {
		t.Errorf("'none' should be empty, got %q", got)
	}
	if got := restartPolicy(types.ServiceConfig{}); got != "" {
		t.Errorf("unset should be empty, got %q", got)
	}
}

func TestUp_WarnsOnUnsupportedRestart(t *testing.T) {
	var buf bytes.Buffer
	backend.DryRun, backend.Stdout = true, &buf
	t.Cleanup(func() { backend.DryRun, backend.Stdout = false, os.Stdout })
	p := &types.Project{Name: "demo", Services: types.Services{
		"web": {Name: "web", Image: "nginx", Restart: "always"},
	}}
	if err := Up(context.Background(), p, true, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `service "web" requests restart "always"`) {
		t.Fatalf("missing restart warning:\n%s", buf.String())
	}
}

// --- healthcheck -----------------------------------------------------------

func hcService(name string, test types.HealthCheckTest, retries uint64) types.ServiceConfig {
	return types.ServiceConfig{
		Name:        name,
		Image:       "img",
		HealthCheck: &types.HealthCheckConfig{Test: test, Retries: &retries},
	}
}

func TestHealthTest_Forms(t *testing.T) {
	cmd, shell, ok := healthTest(types.ServiceConfig{HealthCheck: &types.HealthCheckConfig{Test: types.HealthCheckTest{"CMD", "pg_isready", "-q"}}})
	if !ok || shell || strings.Join(cmd, " ") != "pg_isready -q" {
		t.Fatalf("CMD: %v %v %v", cmd, shell, ok)
	}
	cmd, shell, ok = healthTest(types.ServiceConfig{HealthCheck: &types.HealthCheckConfig{Test: types.HealthCheckTest{"CMD-SHELL", "curl -f localhost"}}})
	if !ok || !shell || cmd[0] != "curl -f localhost" {
		t.Fatalf("CMD-SHELL: %v %v %v", cmd, shell, ok)
	}
	cmd, shell, ok = healthTest(types.ServiceConfig{HealthCheck: &types.HealthCheckConfig{Test: types.HealthCheckTest{"echo", "hi"}}})
	if !ok || !shell || cmd[0] != "echo hi" {
		t.Fatalf("legacy: %v %v %v", cmd, shell, ok)
	}
	for _, bad := range []*types.HealthCheckConfig{
		nil,
		{Disable: true},
		{Test: types.HealthCheckTest{}},
		{Test: types.HealthCheckTest{"NONE"}},
	} {
		if _, _, ok := healthTest(types.ServiceConfig{HealthCheck: bad}); ok {
			t.Fatalf("expected unusable for %+v", bad)
		}
	}
}

func TestDurationOr(t *testing.T) {
	if durationOr(nil, time.Second) != time.Second {
		t.Fatal("nil should give default")
	}
	d := types.Duration(2 * time.Second)
	if durationOr(&d, time.Minute) != 2*time.Second {
		t.Fatal("should use provided")
	}
}

func TestWaitHealthy_BecomesHealthyAfterRetries(t *testing.T) {
	t.Cleanup(swapSleep())
	calls := 0
	t.Cleanup(backend.SetExecForTest(func(_ context.Context, _ bool, _ ...string) ([]byte, error) {
		calls++
		if calls < 3 {
			return nil, errors.New("not ready")
		}
		return nil, nil
	}))
	backend.DryRun = false
	t.Cleanup(func() { backend.DryRun = false })
	sp := types.Duration(time.Second)
	p := &types.Project{Name: "demo", Services: types.Services{
		"db": {Name: "db", HealthCheck: &types.HealthCheckConfig{Test: types.HealthCheckTest{"CMD", "ok"}, StartPeriod: &sp}},
	}}
	if err := waitHealthy(context.Background(), p, "db"); err != nil {
		t.Fatalf("expected healthy, got %v", err)
	}
	if calls != 3 {
		t.Fatalf("want 3 probes, got %d", calls)
	}
}

func TestWaitHealthy_NeverHealthy(t *testing.T) {
	t.Cleanup(swapSleep())
	t.Cleanup(backend.SetExecForTest(func(_ context.Context, _ bool, _ ...string) ([]byte, error) {
		return nil, errors.New("down")
	}))
	backend.DryRun = false
	t.Cleanup(func() { backend.DryRun = false })
	p := &types.Project{Name: "demo", Services: types.Services{"db": hcService("db", types.HealthCheckTest{"CMD", "ok"}, 2)}}
	if err := waitHealthy(context.Background(), p, "db"); err == nil {
		t.Fatal("expected unhealthy error")
	}
}

func TestWaitHealthy_ShellForm(t *testing.T) {
	t.Cleanup(swapSleep())
	var sawShell bool
	t.Cleanup(backend.SetExecForTest(func(_ context.Context, _ bool, args ...string) ([]byte, error) {
		if strings.Join(args, " ") == "exec db.demo sh -c curl -f localhost" {
			sawShell = true
		}
		return nil, nil
	}))
	backend.DryRun = false
	t.Cleanup(func() { backend.DryRun = false })
	p := &types.Project{Name: "demo", Services: types.Services{
		"db": {Name: "db", HealthCheck: &types.HealthCheckConfig{Test: types.HealthCheckTest{"CMD-SHELL", "curl -f localhost"}}},
	}}
	if err := waitHealthy(context.Background(), p, "db"); err != nil {
		t.Fatal(err)
	}
	if !sawShell {
		t.Fatal("expected CMD-SHELL probe via sh -c")
	}
}

func TestUp_HealthyDependencyNeverReady(t *testing.T) {
	t.Cleanup(swapSleep())
	t.Cleanup(backend.SetExecForTest(func(_ context.Context, _ bool, args ...string) ([]byte, error) {
		cmd := strings.Join(args, " ")
		if strings.HasPrefix(cmd, "network list") || strings.HasPrefix(cmd, "ls --all") {
			return []byte("[]"), nil
		}
		if strings.HasPrefix(cmd, "exec db") {
			return nil, errors.New("not ready") // healthcheck never passes
		}
		return nil, nil
	}))
	backend.DryRun = false
	t.Cleanup(func() { backend.DryRun = false })
	retries := uint64(1)
	p := &types.Project{Name: "demo", Services: types.Services{
		"db":  {Name: "db", Image: "postgres", HealthCheck: &types.HealthCheckConfig{Test: types.HealthCheckTest{"CMD", "pg_isready"}, Retries: &retries}},
		"web": {Name: "web", Image: "nginx", DependsOn: types.DependsOnConfig{"db": {Condition: types.ServiceConditionHealthy}}},
	}}
	if err := Up(context.Background(), p, true, false); err == nil {
		t.Fatal("expected Up to fail when dependency never becomes healthy")
	}
}

func TestUp_StartedConditionSkipsHealthWait(t *testing.T) {
	t.Cleanup(backend.SetExecForTest(func(_ context.Context, _ bool, args ...string) ([]byte, error) {
		cmd := strings.Join(args, " ")
		if strings.HasPrefix(cmd, "exec ") {
			t.Errorf("service_started must not trigger a healthcheck probe: %q", cmd)
		}
		if strings.HasPrefix(cmd, "network list") || strings.HasPrefix(cmd, "ls --all") {
			return []byte("[]"), nil
		}
		return nil, nil
	}))
	backend.DryRun = false
	t.Cleanup(func() { backend.DryRun = false })
	p := &types.Project{Name: "demo", Services: types.Services{
		"db":  {Name: "db", Image: "postgres"},
		"web": {Name: "web", Image: "nginx", DependsOn: types.DependsOnConfig{"db": {Condition: types.ServiceConditionStarted}}},
	}}
	if err := Up(context.Background(), p, true, false); err != nil {
		t.Fatal(err)
	}
}

func TestWaitHealthy_NoHealthcheck(t *testing.T) {
	p := &types.Project{Name: "demo", Services: types.Services{"db": {Name: "db"}}}
	if err := waitHealthy(context.Background(), p, "db"); err == nil {
		t.Fatal("expected error for missing healthcheck")
	}
}

func TestWaitHealthy_ContextCancelled(t *testing.T) {
	t.Cleanup(swapSleep())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := &types.Project{Name: "demo", Services: types.Services{"db": hcService("db", types.HealthCheckTest{"CMD", "ok"}, 1)}}
	if err := waitHealthy(ctx, p, "db"); err == nil {
		t.Fatal("expected context error")
	}
}

func TestUp_WaitsForHealthyDependency(t *testing.T) {
	t.Cleanup(swapSleep())
	var sawExec, sawWebRun bool
	t.Cleanup(backend.SetExecForTest(func(_ context.Context, _ bool, args ...string) ([]byte, error) {
		cmd := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(cmd, "network list"), strings.HasPrefix(cmd, "ls --all"):
			return []byte("[]"), nil
		case strings.HasPrefix(cmd, "exec db"):
			sawExec = true // the healthcheck probe
		case strings.Contains(cmd, "--name web"):
			if !sawExec {
				t.Error("web started before db healthcheck passed")
			}
			sawWebRun = true
		}
		return nil, nil
	}))
	backend.DryRun = false
	t.Cleanup(func() { backend.DryRun = false })
	p := &types.Project{Name: "demo", Services: types.Services{
		"db":  {Name: "db", Image: "postgres", HealthCheck: &types.HealthCheckConfig{Test: types.HealthCheckTest{"CMD", "pg_isready"}}},
		"web": {Name: "web", Image: "nginx", DependsOn: types.DependsOnConfig{"db": {Condition: types.ServiceConditionHealthy}}},
	}}
	if err := Up(context.Background(), p, true, false); err != nil {
		t.Fatal(err)
	}
	if !sawExec || !sawWebRun {
		t.Fatalf("exec=%v webRun=%v", sawExec, sawWebRun)
	}
}

// --- Exec ------------------------------------------------------------------

func TestExec_DefaultAllocatesTTY(t *testing.T) {
	p := &types.Project{Name: "demo", Services: types.Services{"db": {Name: "db"}}}
	out := captureDryRun(t, func() error {
		return Exec(context.Background(), p, "db", ExecOptions{}, "psql", "-U", "postgres")
	})
	if strings.TrimSpace(out) != "container exec --tty --interactive db.demo psql -U postgres" {
		t.Fatalf("exec cmd = %q", out)
	}
}

func TestExec_AllOptions(t *testing.T) {
	p := &types.Project{Name: "demo", Services: types.Services{"db": {Name: "db"}}}
	out := captureDryRun(t, func() error {
		return Exec(context.Background(), p, "db", ExecOptions{
			Detach:  true,
			NoTTY:   true,
			Env:     []string{"A=1", "B=2"},
			Workdir: "/tmp",
			User:    "root",
		}, "sh", "-c", "echo hi")
	})
	want := "container exec --detach --env A=1 --env B=2 --workdir /tmp --user root db.demo sh -c echo hi"
	if strings.TrimSpace(out) != want {
		t.Fatalf("exec cmd = %q", out)
	}
}

func TestExec_UnknownService(t *testing.T) {
	p := &types.Project{Name: "demo", Services: types.Services{"db": {Name: "db"}}}
	if err := Exec(context.Background(), p, "ghost", ExecOptions{}, "sh"); err == nil {
		t.Fatal("expected error for unknown service")
	}
}

func TestExec_NoCommand(t *testing.T) {
	p := &types.Project{Name: "demo", Services: types.Services{"db": {Name: "db"}}}
	if err := Exec(context.Background(), p, "db", ExecOptions{}); err == nil {
		t.Fatal("expected error when no command given")
	}
}

func TestExec_RunErrorPropagates(t *testing.T) {
	t.Cleanup(backend.SetExecForTest(func(_ context.Context, _ bool, _ ...string) ([]byte, error) {
		return nil, errors.New("no such container")
	}))
	backend.DryRun = false
	t.Cleanup(func() { backend.DryRun = false })
	p := &types.Project{Name: "demo", Services: types.Services{"db": {Name: "db"}}}
	if err := Exec(context.Background(), p, "db", ExecOptions{}, "sh"); err == nil {
		t.Fatal("expected exec error")
	}
}

// --- topoSort --------------------------------------------------------------

func dep(names ...string) types.DependsOnConfig {
	m := types.DependsOnConfig{}
	for _, n := range names {
		m[n] = types.ServiceDependency{}
	}
	return m
}

func TestTopoSort_DependencyOrder(t *testing.T) {
	p := &types.Project{Name: "p", Services: types.Services{
		"a": {Name: "a", DependsOn: dep("b")},
		"b": {Name: "b", DependsOn: dep("c")},
		"c": {Name: "c"},
	}}
	order, err := topoSort(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "c,b,a" {
		t.Fatalf("order = %v", order)
	}
}

func TestTopoSort_IndependentIsAlphabetical(t *testing.T) {
	p := &types.Project{Name: "p", Services: types.Services{
		"z": {Name: "z"}, "a": {Name: "a"}, "m": {Name: "m"},
	}}
	order, _ := topoSort(p)
	if strings.Join(order, ",") != "a,m,z" {
		t.Fatalf("order = %v", order)
	}
}

func TestTopoSort_UnknownDependencyIgnored(t *testing.T) {
	p := &types.Project{Name: "p", Services: types.Services{
		"a": {Name: "a", DependsOn: dep("ghost")},
	}}
	order, err := topoSort(p)
	if err != nil || strings.Join(order, ",") != "a" {
		t.Fatalf("order = %v err = %v", order, err)
	}
}

func TestTopoSort_CycleError(t *testing.T) {
	p := &types.Project{Name: "p", Services: types.Services{
		"a": {Name: "a", DependsOn: dep("b")},
		"b": {Name: "b", DependsOn: dep("a")},
	}}
	if _, err := topoSort(p); err == nil {
		t.Fatal("expected cycle error")
	}
}

// --- build -----------------------------------------------------------------

func TestBuild_WithDockerfileAndContext(t *testing.T) {
	p := &types.Project{Name: "demo"}
	svc := types.ServiceConfig{Name: "api", Build: &types.BuildConfig{Context: "./app", Dockerfile: "Dockerfile.dev"}}
	out := captureDryRun(t, func() error { return build(context.Background(), p, svc) })
	if strings.TrimSpace(out) != "container build -t demo-api -f Dockerfile.dev ./app" {
		t.Fatalf("build cmd = %q", out)
	}
}

func TestBuild_DefaultContextNoDockerfile(t *testing.T) {
	p := &types.Project{Name: "demo"}
	svc := types.ServiceConfig{Name: "api", Build: &types.BuildConfig{}}
	out := captureDryRun(t, func() error { return build(context.Background(), p, svc) })
	if strings.TrimSpace(out) != "container build -t demo-api ." {
		t.Fatalf("build cmd = %q", out)
	}
}

// --- Up --------------------------------------------------------------------

func demoProject() *types.Project {
	return &types.Project{Name: "demo",
		Services: types.Services{
			"api": {Name: "api", Build: &types.BuildConfig{Context: ".", Dockerfile: "Dockerfile"}, DependsOn: dep("db")},
			"db":  {Name: "db", Image: "postgres:16", Deploy: limitFor(512<<20, 0)},
		},
		Volumes: types.Volumes{"dbdata": {}},
	}
}

func TestUp_DetachedEmitsFullPipeline(t *testing.T) {
	out := captureDryRun(t, func() error { return Up(context.Background(), demoProject(), true, false) })
	lines := strings.Split(strings.TrimSpace(out), "\n")
	wantPrefixes := []string{
		"container network create",
		"container volume create --label arboretum.project=demo dbdata",
		"container run --detach --name db", // db before api (dependency)
		"container build -t demo-api",      // api builds
		"container run --detach --name api",
	}
	for i, want := range wantPrefixes {
		if i >= len(lines) || !strings.HasPrefix(lines[i], want) {
			t.Fatalf("line %d = %q, want prefix %q\nfull:\n%s", i, safeIdx(lines, i), want, out)
		}
	}
	// detached => no logs lines
	if strings.Contains(out, "container logs") {
		t.Fatal("detached up should not tail logs")
	}
}

func TestUp_AttachedTailsLogs(t *testing.T) {
	out := captureDryRun(t, func() error { return Up(context.Background(), demoProject(), false, false) })
	if !strings.Contains(out, "container logs -f api") || !strings.Contains(out, "container logs -f db") {
		t.Fatalf("attached up should tail logs, got:\n%s", out)
	}
}

func safeIdx(s []string, i int) string {
	if i < len(s) {
		return s[i]
	}
	return "<none>"
}

// failOn installs an exec seam that errors when the joined args contain substr.
// "network list" returns an empty array so EnsureNetwork proceeds to create.
func failOn(t *testing.T, substr string) {
	t.Cleanup(backend.SetExecForTest(func(_ context.Context, _ bool, args ...string) ([]byte, error) {
		cmd := strings.Join(args, " ")
		if strings.HasPrefix(cmd, "network list") {
			return []byte("[]"), nil
		}
		if substr != "" && strings.Contains(cmd, substr) {
			return nil, errors.New("boom: " + cmd)
		}
		return nil, nil
	}))
	backend.DryRun = false
	t.Cleanup(func() { backend.DryRun = false })
}

func TestUp_NetworkErrorPropagates(t *testing.T) {
	failOn(t, "network create")
	if err := Up(context.Background(), demoProject(), true, false); err == nil {
		t.Fatal("expected network error")
	}
}

func TestUp_VolumeErrorPropagates(t *testing.T) {
	failOn(t, "volume create")
	if err := Up(context.Background(), demoProject(), true, false); err == nil {
		t.Fatal("expected volume error")
	}
}

func TestUp_BuildErrorPropagates(t *testing.T) {
	failOn(t, "build")
	if err := Up(context.Background(), demoProject(), true, false); err == nil {
		t.Fatal("expected build error")
	}
}

func TestUp_RunErrorPropagates(t *testing.T) {
	failOn(t, "run --detach --name db")
	if err := Up(context.Background(), demoProject(), true, false); err == nil {
		t.Fatal("expected run error")
	}
}

// up is idempotent: a running container is left alone, a stopped one is
// (re)started, and an absent one is created.
func TestUp_SkipsRunningStartsStopped(t *testing.T) {
	p := demoProject()
	hDb := configHash(p, p.Services["db"])
	hAPI := configHash(p, p.Services["api"])
	var started []string
	t.Cleanup(backend.SetExecForTest(func(_ context.Context, _ bool, args ...string) ([]byte, error) {
		cmd := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(cmd, "ls --all"):
			return []byte(`[
				{"id":"db.demo","status":{"state":"running"},"configuration":{"labels":{"arboretum.project":"demo","arboretum.service":"db","arboretum.config-hash":"` + hDb + `"}}},
				{"id":"api.demo","status":{"state":"stopped"},"configuration":{"labels":{"arboretum.project":"demo","arboretum.service":"api","arboretum.config-hash":"` + hAPI + `"}}}
			]`), nil
		case strings.HasPrefix(cmd, "network list"):
			return []byte("[]"), nil
		case strings.HasPrefix(cmd, "start "):
			started = append(started, args[1])
		case strings.HasPrefix(cmd, "build ") || strings.HasPrefix(cmd, "run "):
			t.Errorf("must not recreate an unchanged container: %q", cmd)
		}
		return nil, nil
	}))
	backend.DryRun = false
	t.Cleanup(func() { backend.DryRun = false })

	if err := Up(context.Background(), p, true, false); err != nil {
		t.Fatal(err)
	}
	if len(started) != 1 || started[0] != "api.demo" {
		t.Fatalf("want only the stopped service (api) started, got %v", started)
	}
}

func TestUp_StartStoppedErrorPropagates(t *testing.T) {
	p := demoProject()
	hDb := configHash(p, p.Services["db"])
	t.Cleanup(backend.SetExecForTest(func(_ context.Context, _ bool, args ...string) ([]byte, error) {
		cmd := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(cmd, "ls --all"):
			return []byte(`[{"id":"db.demo","status":{"state":"exited"},"configuration":{"labels":{"arboretum.project":"demo","arboretum.service":"db","arboretum.config-hash":"` + hDb + `"}}}]`), nil
		case strings.HasPrefix(cmd, "network list"):
			return []byte("[]"), nil
		case strings.HasPrefix(cmd, "start "):
			return nil, errors.New("cannot start")
		}
		return nil, nil
	}))
	backend.DryRun = false
	t.Cleanup(func() { backend.DryRun = false })
	if err := Up(context.Background(), p, true, false); err == nil {
		t.Fatal("expected start error")
	}
}

func TestUp_RecreatesOnConfigChangeAndForce(t *testing.T) {
	for _, tc := range []struct {
		name  string
		hash  string // stored config-hash on the existing container
		force bool
	}{
		{"config changed", "stale-hash", false},
		{"force recreate", "", true}, // hash filled with the real one below
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := demoProject()
			storedHash := tc.hash
			if storedHash == "" {
				storedHash = configHash(p, p.Services["db"])
			}
			var stopped, removed, ran bool
			t.Cleanup(backend.SetExecForTest(func(_ context.Context, _ bool, args ...string) ([]byte, error) {
				cmd := strings.Join(args, " ")
				switch {
				case strings.HasPrefix(cmd, "ls --all"):
					return []byte(`[{"id":"db.demo","status":{"state":"running"},"configuration":{"labels":{"arboretum.project":"demo","arboretum.service":"db","arboretum.config-hash":"` + storedHash + `"}}}]`), nil
				case strings.HasPrefix(cmd, "network list"):
					return []byte("[]"), nil
				case cmd == "stop db.demo":
					stopped = true
				case cmd == "rm db.demo":
					removed = true
				case strings.Contains(cmd, "--name db.demo"):
					ran = true
				}
				return nil, nil
			}))
			backend.DryRun = false
			t.Cleanup(func() { backend.DryRun = false })

			if err := Up(context.Background(), p, true, tc.force); err != nil {
				t.Fatal(err)
			}
			if !stopped || !removed || !ran {
				t.Fatalf("expected recreate (stop=%v rm=%v run=%v)", stopped, removed, ran)
			}
		})
	}
}

func TestUp_ListExistingErrorPropagates(t *testing.T) {
	t.Cleanup(backend.SetExecForTest(func(_ context.Context, _ bool, args ...string) ([]byte, error) {
		cmd := strings.Join(args, " ")
		if strings.HasPrefix(cmd, "network list") {
			return []byte("[]"), nil
		}
		if strings.HasPrefix(cmd, "ls --all") {
			return nil, errors.New("ls boom")
		}
		return nil, nil
	}))
	backend.DryRun = false
	t.Cleanup(func() { backend.DryRun = false })
	if err := Up(context.Background(), demoProject(), true, false); err == nil {
		t.Fatal("expected list error")
	}
}

func TestUp_TopoErrorPropagates(t *testing.T) {
	failOn(t, "") // no command failure; cycle should surface first
	p := &types.Project{Name: "demo", Services: types.Services{
		"a": {Name: "a", DependsOn: dep("b")},
		"b": {Name: "b", DependsOn: dep("a")},
	}}
	if err := Up(context.Background(), p, true, false); err == nil {
		t.Fatal("expected topo cycle error")
	}
}

// --- Down ------------------------------------------------------------------

func TestDown_StopsRemovesAndDeletesNetwork(t *testing.T) {
	var calls []string
	t.Cleanup(backend.SetExecForTest(func(_ context.Context, _ bool, args ...string) ([]byte, error) {
		cmd := strings.Join(args, " ")
		calls = append(calls, cmd)
		if strings.HasPrefix(cmd, "ls ") {
			return []byte(`[{"name":"db","labels":{"arboretum.project":"demo","arboretum.service":"db"}}]`), nil
		}
		return nil, nil
	}))
	backend.DryRun = false
	t.Cleanup(func() { backend.DryRun = false })

	p := &types.Project{Name: "demo", Services: types.Services{"db": {Name: "db"}}}
	if err := Down(context.Background(), p, DownOptions{}); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(calls, "|")
	for _, want := range []string{"stop db", "rm db", "network delete demo_default"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in calls: %s", want, joined)
		}
	}
}

func TestDown_ListErrorPropagates(t *testing.T) {
	t.Cleanup(backend.SetExecForTest(func(_ context.Context, _ bool, _ ...string) ([]byte, error) {
		return nil, errors.New("runtime down")
	}))
	backend.DryRun = false
	t.Cleanup(func() { backend.DryRun = false })
	if err := Down(context.Background(), &types.Project{Name: "demo"}, DownOptions{}); err == nil {
		t.Fatal("expected list error")
	}
}

// --- Ps --------------------------------------------------------------------

func TestPs_ListsContainers(t *testing.T) {
	t.Cleanup(backend.SetExecForTest(func(_ context.Context, _ bool, _ ...string) ([]byte, error) {
		return []byte(`[{"name":"db","labels":{"arboretum.project":"demo","arboretum.service":"db"}}]`), nil
	}))
	backend.DryRun = false
	t.Cleanup(func() { backend.DryRun = false })

	var buf bytes.Buffer
	if err := Ps(context.Background(), &types.Project{Name: "demo"}, &buf, PsOptions{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "db") {
		t.Fatalf("ps output = %q", buf.String())
	}
}

func TestPs_Empty(t *testing.T) {
	t.Cleanup(backend.SetExecForTest(func(_ context.Context, _ bool, _ ...string) ([]byte, error) {
		return []byte(`[]`), nil
	}))
	backend.DryRun = false
	t.Cleanup(func() { backend.DryRun = false })

	var buf bytes.Buffer
	if err := Ps(context.Background(), &types.Project{Name: "demo"}, &buf, PsOptions{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "no containers") {
		t.Fatalf("ps output = %q", buf.String())
	}
}

func TestPs_ErrorPropagates(t *testing.T) {
	t.Cleanup(backend.SetExecForTest(func(_ context.Context, _ bool, _ ...string) ([]byte, error) {
		return nil, errors.New("nope")
	}))
	backend.DryRun = false
	t.Cleanup(func() { backend.DryRun = false })
	if err := Ps(context.Background(), &types.Project{Name: "demo"}, &bytes.Buffer{}, PsOptions{}); err == nil {
		t.Fatal("expected error")
	}
}

func psStub(jsonOut string) func() {
	return backend.SetExecForTest(func(_ context.Context, _ bool, _ ...string) ([]byte, error) {
		return []byte(jsonOut), nil
	})
}

const oneRunningWeb = `[{"id":"web.demo","status":{"state":"running"},"configuration":{"labels":{"arboretum.project":"demo","arboretum.service":"web"}}}]`

func TestPs_TableWithPorts(t *testing.T) {
	t.Cleanup(psStub(oneRunningWeb))
	backend.DryRun = false
	t.Cleanup(func() { backend.DryRun = false })
	p := &types.Project{Name: "demo", Services: types.Services{
		"web": {Name: "web", Ports: []types.ServicePortConfig{{Published: "8080", Target: 3000}}},
	}}
	var buf bytes.Buffer
	if err := Ps(context.Background(), p, &buf, PsOptions{}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"SERVICE", "NAME", "STATE", "PORTS", "web.demo", "running", "8080->3000"} {
		if !strings.Contains(out, want) {
			t.Fatalf("table missing %q:\n%s", want, out)
		}
	}
}

func TestPs_QuietAndJSON(t *testing.T) {
	t.Cleanup(psStub(oneRunningWeb))
	backend.DryRun = false
	t.Cleanup(func() { backend.DryRun = false })
	p := &types.Project{Name: "demo"}

	var buf bytes.Buffer
	if err := Ps(context.Background(), p, &buf, PsOptions{Quiet: true}); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(buf.String()) != "web.demo" {
		t.Fatalf("quiet = %q", buf.String())
	}

	buf.Reset()
	if err := Ps(context.Background(), p, &buf, PsOptions{Format: "json"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"name": "web.demo"`) || !strings.Contains(buf.String(), `"service": "web"`) {
		t.Fatalf("json = %q", buf.String())
	}
}

func TestPortsFor(t *testing.T) {
	p := &types.Project{Services: types.Services{
		"web": {Ports: []types.ServicePortConfig{{Published: "8080", Target: 3000}, {Target: 9000}}},
	}}
	if got := portsFor(p, "web"); got != "8080->3000" { // unpublished port skipped
		t.Fatalf("ports = %q", got)
	}
	if got := portsFor(p, "missing"); got != "" {
		t.Fatalf("unknown service = %q", got)
	}
}

// --- start/stop/restart ----------------------------------------------------

const twoContainers = `[
	{"id":"b.demo","configuration":{"labels":{"arboretum.project":"demo","arboretum.service":"b"}}},
	{"id":"a.demo","configuration":{"labels":{"arboretum.project":"demo","arboretum.service":"a"}}}
]`

func TestStopStartRestart(t *testing.T) {
	var actions []string
	t.Cleanup(backend.SetExecForTest(func(_ context.Context, _ bool, args ...string) ([]byte, error) {
		cmd := strings.Join(args, " ")
		if strings.HasPrefix(cmd, "ls --all") {
			return []byte(twoContainers), nil
		}
		actions = append(actions, cmd)
		return nil, nil
	}))
	backend.DryRun = false
	t.Cleanup(func() { backend.DryRun = false })
	p := &types.Project{Name: "demo"}

	if err := Stop(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if strings.Join(actions, "|") != "stop a.demo|stop b.demo" { // sorted
		t.Fatalf("stop actions = %v", actions)
	}
	actions = nil
	if err := Restart(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if strings.Join(actions, "|") != "stop a.demo|stop b.demo|start a.demo|start b.demo" {
		t.Fatalf("restart actions = %v", actions)
	}
}

func TestForEachContainer_Errors(t *testing.T) {
	// ListByProject error.
	restore := backend.SetExecForTest(func(_ context.Context, _ bool, _ ...string) ([]byte, error) {
		return nil, errors.New("ls fail")
	})
	backend.DryRun = false
	t.Cleanup(func() { backend.DryRun = false })
	if err := Stop(context.Background(), &types.Project{Name: "demo"}); err == nil {
		t.Fatal("expected list error")
	}
	if err := Restart(context.Background(), &types.Project{Name: "demo"}); err == nil {
		t.Fatal("expected restart (stop) error")
	}
	restore()

	// Per-container action error.
	t.Cleanup(backend.SetExecForTest(func(_ context.Context, _ bool, args ...string) ([]byte, error) {
		if strings.HasPrefix(strings.Join(args, " "), "ls --all") {
			return []byte(twoContainers), nil
		}
		return nil, errors.New("action fail")
	}))
	if err := Start(context.Background(), &types.Project{Name: "demo"}); err == nil {
		t.Fatal("expected action error")
	}
}

// --- down options / build / pull / run / completed -------------------------

func TestDown_VolumesAndOrphans(t *testing.T) {
	// db is defined; ghost is an orphan (service not in the project).
	lsJSON := `[
		{"id":"db.demo","configuration":{"labels":{"arboretum.project":"demo","arboretum.service":"db"}}},
		{"id":"ghost.demo","configuration":{"labels":{"arboretum.project":"demo","arboretum.service":"ghost"}}}
	]`
	run := func(opts DownOptions) (string, string) {
		var calls []string
		var buf bytes.Buffer
		backend.Stdout = &buf
		restore := backend.SetExecForTest(func(_ context.Context, _ bool, args ...string) ([]byte, error) {
			cmd := strings.Join(args, " ")
			if strings.HasPrefix(cmd, "ls --all") {
				return []byte(lsJSON), nil
			}
			calls = append(calls, cmd)
			return nil, nil
		})
		backend.DryRun = false
		p := &types.Project{Name: "demo", Services: types.Services{"db": {Name: "db"}}, Volumes: types.Volumes{"data": {}}}
		err := Down(context.Background(), p, opts)
		restore()
		backend.Stdout = os.Stdout
		backend.DryRun = false
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(calls, "|"), buf.String()
	}

	// Default: db removed, ghost kept (warned), no volume delete.
	calls, warn := run(DownOptions{})
	if !strings.Contains(calls, "stop db.demo") || strings.Contains(calls, "ghost") {
		t.Fatalf("default down calls = %q", calls)
	}
	if !strings.Contains(warn, "orphan container ghost.demo") {
		t.Fatalf("expected orphan warning, got %q", warn)
	}
	// --remove-orphans + --volumes.
	calls, _ = run(DownOptions{RemoveOrphans: true, Volumes: true})
	for _, want := range []string{"stop ghost.demo", "rm ghost.demo", "volume delete data"} {
		if !strings.Contains(calls, want) {
			t.Fatalf("calls %q missing %q", calls, want)
		}
	}
}

func TestBuildAll(t *testing.T) {
	var built []string
	t.Cleanup(backend.SetExecForTest(func(_ context.Context, _ bool, args ...string) ([]byte, error) {
		if args[0] == "build" {
			built = append(built, "built")
		}
		return nil, nil
	}))
	backend.DryRun = false
	t.Cleanup(func() { backend.DryRun = false })
	p := &types.Project{Name: "demo", Services: types.Services{
		"api": {Name: "api", Build: &types.BuildConfig{Context: "."}},
		"db":  {Name: "db", Image: "postgres"}, // no build → skipped
	}}
	if err := BuildAll(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if len(built) != 1 {
		t.Fatalf("want 1 build, got %d", len(built))
	}
}

func TestBuildAll_Error(t *testing.T) {
	t.Cleanup(backend.SetExecForTest(func(_ context.Context, _ bool, _ ...string) ([]byte, error) {
		return nil, errors.New("build fail")
	}))
	backend.DryRun = false
	t.Cleanup(func() { backend.DryRun = false })
	p := &types.Project{Name: "demo", Services: types.Services{"api": {Name: "api", Build: &types.BuildConfig{Context: "."}}}}
	if err := BuildAll(context.Background(), p); err == nil {
		t.Fatal("expected build error")
	}
}

func TestPull(t *testing.T) {
	var pulled []string
	t.Cleanup(backend.SetExecForTest(func(_ context.Context, _ bool, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "image" && args[1] == "pull" {
			pulled = append(pulled, args[2])
		}
		return nil, nil
	}))
	backend.DryRun = false
	t.Cleanup(func() { backend.DryRun = false })
	p := &types.Project{Name: "demo", Services: types.Services{
		"db":  {Name: "db", Image: "postgres:16"},
		"api": {Name: "api", Build: &types.BuildConfig{Context: "."}}, // no image → skipped
	}}
	if err := Pull(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if len(pulled) != 1 || pulled[0] != "postgres:16" {
		t.Fatalf("pulled = %v", pulled)
	}
}

func TestPull_Error(t *testing.T) {
	t.Cleanup(backend.SetExecForTest(func(_ context.Context, _ bool, _ ...string) ([]byte, error) {
		return nil, errors.New("pull fail")
	}))
	backend.DryRun = false
	t.Cleanup(func() { backend.DryRun = false })
	p := &types.Project{Name: "demo", Services: types.Services{"db": {Name: "db", Image: "x"}}}
	if err := Pull(context.Background(), p); err == nil {
		t.Fatal("expected pull error")
	}
}

func TestRunOneOff(t *testing.T) {
	v := "1"
	p := &types.Project{Name: "demo", Services: types.Services{
		"web": {Name: "web", Image: "nginx", Environment: types.MappingWithEquals{"A": &v, "NOVAL": nil}},
	}}
	out := captureDryRun(t, func() error {
		return RunOneOff(context.Background(), p, "web", RunOptions{Env: []string{"B=2"}}, "sh", "-c", "echo hi")
	})
	want := "container run --rm --tty --interactive --network demo_default --dns-domain demo --env A=1 --env NOVAL --env B=2 nginx sh -c echo hi"
	if strings.TrimSpace(out) != want {
		t.Fatalf("run = %q", out)
	}

	// Detached + default command (no override).
	out = captureDryRun(t, func() error {
		return RunOneOff(context.Background(), p, "web", RunOptions{Detach: true})
	})
	if !strings.Contains(out, "--detach") || strings.Contains(out, "--tty") {
		t.Fatalf("detached run = %q", out)
	}

	if err := RunOneOff(context.Background(), p, "ghost", RunOptions{}); err == nil {
		t.Fatal("expected unknown-service error")
	}
}

func TestWaitCompleted(t *testing.T) {
	t.Cleanup(swapSleep())
	calls := 0
	t.Cleanup(backend.SetExecForTest(func(_ context.Context, _ bool, _ ...string) ([]byte, error) {
		calls++
		if calls < 2 {
			return []byte(`[{"status":{"state":"running"}}]`), nil
		}
		return []byte(`[{"status":{"state":"stopped"}}]`), nil
	}))
	backend.DryRun = false
	t.Cleanup(func() { backend.DryRun = false })
	p := &types.Project{Name: "demo", Services: types.Services{"job": {Name: "job"}}}
	if err := waitCompleted(context.Background(), p, "job"); err != nil {
		t.Fatal(err)
	}
}

func TestWaitCompleted_ErrorAndCancel(t *testing.T) {
	t.Cleanup(swapSleep())
	t.Cleanup(backend.SetExecForTest(func(_ context.Context, _ bool, _ ...string) ([]byte, error) {
		return nil, errors.New("inspect fail")
	}))
	backend.DryRun = false
	t.Cleanup(func() { backend.DryRun = false })
	p := &types.Project{Name: "demo", Services: types.Services{"job": {Name: "job"}}}
	if err := waitCompleted(context.Background(), p, "job"); err == nil {
		t.Fatal("expected inspect error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitCompleted(ctx, p, "job"); err == nil {
		t.Fatal("expected context error")
	}
}

func TestUp_WaitsForCompletedDependency(t *testing.T) {
	t.Cleanup(swapSleep())
	var sawInspect, sawWebRun bool
	t.Cleanup(backend.SetExecForTest(func(_ context.Context, _ bool, args ...string) ([]byte, error) {
		cmd := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(cmd, "network list"), strings.HasPrefix(cmd, "ls --all"):
			return []byte("[]"), nil
		case strings.HasPrefix(cmd, "inspect migrate.demo"):
			sawInspect = true
			return []byte(`[{"status":{"state":"stopped"}}]`), nil
		case strings.Contains(cmd, "--name web.demo"):
			if !sawInspect {
				t.Error("web started before migrate completed")
			}
			sawWebRun = true
		}
		return nil, nil
	}))
	backend.DryRun = false
	t.Cleanup(func() { backend.DryRun = false })
	p := &types.Project{Name: "demo", Services: types.Services{
		"migrate": {Name: "migrate", Image: "migrate"},
		"web":     {Name: "web", Image: "nginx", DependsOn: types.DependsOnConfig{"migrate": {Condition: types.ServiceConditionCompletedSuccessfully}}},
	}}
	if err := Up(context.Background(), p, true, false); err != nil {
		t.Fatal(err)
	}
	if !sawInspect || !sawWebRun {
		t.Fatalf("inspect=%v webRun=%v", sawInspect, sawWebRun)
	}
}

func TestUp_CompletedDependencyError(t *testing.T) {
	t.Cleanup(swapSleep())
	t.Cleanup(backend.SetExecForTest(func(_ context.Context, _ bool, args ...string) ([]byte, error) {
		cmd := strings.Join(args, " ")
		if strings.HasPrefix(cmd, "network list") || strings.HasPrefix(cmd, "ls --all") {
			return []byte("[]"), nil
		}
		if strings.HasPrefix(cmd, "inspect") {
			return nil, errors.New("inspect fail")
		}
		return nil, nil
	}))
	backend.DryRun = false
	t.Cleanup(func() { backend.DryRun = false })
	p := &types.Project{Name: "demo", Services: types.Services{
		"migrate": {Name: "migrate", Image: "migrate"},
		"web":     {Name: "web", Image: "nginx", DependsOn: types.DependsOnConfig{"migrate": {Condition: types.ServiceConditionCompletedSuccessfully}}},
	}}
	if err := Up(context.Background(), p, true, false); err == nil {
		t.Fatal("expected completed-dependency error")
	}
}

// --- config ----------------------------------------------------------------

func TestConfig(t *testing.T) {
	p := &types.Project{Name: "demo", Services: types.Services{"web": {Name: "web", Image: "nginx"}}}

	var buf bytes.Buffer
	if err := Config(p, &buf, ConfigOptions{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "web") || !strings.Contains(buf.String(), "nginx") {
		t.Fatalf("yaml = %q", buf.String())
	}

	buf.Reset()
	if err := Config(p, &buf, ConfigOptions{ServicesOnly: true}); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(buf.String()) != "web" {
		t.Fatalf("services = %q", buf.String())
	}

	buf.Reset()
	if err := Config(p, &buf, ConfigOptions{Format: "json"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"web"`) {
		t.Fatalf("json = %q", buf.String())
	}
}

func TestConfig_MarshalError(t *testing.T) {
	prev := marshalProject
	marshalProject = func(*types.Project, bool) ([]byte, error) { return nil, errors.New("marshal fail") }
	t.Cleanup(func() { marshalProject = prev })
	if err := Config(&types.Project{Name: "demo"}, &bytes.Buffer{}, ConfigOptions{}); err == nil {
		t.Fatal("expected marshal error")
	}
}

func TestConfig_WriteError(t *testing.T) {
	p := &types.Project{Name: "demo", Services: types.Services{"web": {Name: "web", Image: "nginx"}}}
	if err := Config(p, errWriter{}, ConfigOptions{}); err == nil {
		t.Fatal("expected write error")
	}
}

// --- Logs ------------------------------------------------------------------

func TestLogs_FollowAndNoFollow(t *testing.T) {
	p := &types.Project{Name: "demo", Services: types.Services{"db": {Name: "db"}}}

	out := captureDryRun(t, func() error { return Logs(context.Background(), p, true) })
	if strings.TrimSpace(out) != "container logs -f db.demo" {
		t.Fatalf("follow logs = %q", out)
	}

	out = captureDryRun(t, func() error { return Logs(context.Background(), p, false) })
	if strings.TrimSpace(out) != "container logs db.demo" {
		t.Fatalf("no-follow logs = %q", out)
	}
}

func TestLogs_ErrorPropagates(t *testing.T) {
	t.Cleanup(backend.SetExecForTest(func(_ context.Context, _ bool, _ ...string) ([]byte, error) {
		return nil, errors.New("no such container")
	}))
	backend.DryRun = false
	t.Cleanup(func() { backend.DryRun = false })
	p := &types.Project{Name: "demo", Services: types.Services{"db": {Name: "db"}}}
	if err := Logs(context.Background(), p, false); err == nil {
		t.Fatal("expected logs error")
	}
}
