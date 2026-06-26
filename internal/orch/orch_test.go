package orch

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/nori-kamiya/orchard/internal/backend"
)

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

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
	if containerName(nil, "db") != "db" {
		t.Fatal("container name should be the service name")
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
	want := "run --detach --name api --network demo_default " +
		"--label orchard.project=demo --label orchard.service=api " +
		"--memory 512m --cpus 1 --env A=1 --env B --publish 8080:3000 " +
		"--volume data:/data myapi:latest node server.js"
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
	want := "run --detach --name api --network demo_default " +
		"--label orchard.project=demo --label orchard.service=api " +
		"--workdir /srv --user node --entrypoint ./entry.sh " +
		"--label com.example.team=core --label com.example.tier=web " +
		"myapi:latest --flag x start"
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

// --- Exec ------------------------------------------------------------------

func TestExec_DefaultAllocatesTTY(t *testing.T) {
	p := &types.Project{Name: "demo", Services: types.Services{"db": {Name: "db"}}}
	out := captureDryRun(t, func() error {
		return Exec(context.Background(), p, "db", ExecOptions{}, "psql", "-U", "postgres")
	})
	if strings.TrimSpace(out) != "container exec --tty --interactive db psql -U postgres" {
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
	want := "container exec --detach --env A=1 --env B=2 --workdir /tmp --user root db sh -c echo hi"
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
	out := captureDryRun(t, func() error { return Up(context.Background(), demoProject(), true) })
	lines := strings.Split(strings.TrimSpace(out), "\n")
	wantPrefixes := []string{
		"container network create",
		"container volume create --label orchard.project=demo dbdata",
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
	out := captureDryRun(t, func() error { return Up(context.Background(), demoProject(), false) })
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
	if err := Up(context.Background(), demoProject(), true); err == nil {
		t.Fatal("expected network error")
	}
}

func TestUp_VolumeErrorPropagates(t *testing.T) {
	failOn(t, "volume create")
	if err := Up(context.Background(), demoProject(), true); err == nil {
		t.Fatal("expected volume error")
	}
}

func TestUp_BuildErrorPropagates(t *testing.T) {
	failOn(t, "build")
	if err := Up(context.Background(), demoProject(), true); err == nil {
		t.Fatal("expected build error")
	}
}

func TestUp_RunErrorPropagates(t *testing.T) {
	failOn(t, "run --detach --name db")
	if err := Up(context.Background(), demoProject(), true); err == nil {
		t.Fatal("expected run error")
	}
}

// up is idempotent: a running container is left alone, a stopped one is
// (re)started, and an absent one is created.
func TestUp_SkipsRunningStartsStopped(t *testing.T) {
	var started []string
	t.Cleanup(backend.SetExecForTest(func(_ context.Context, _ bool, args ...string) ([]byte, error) {
		cmd := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(cmd, "ls --all"):
			return []byte(`[
				{"id":"db","status":{"state":"running"},"configuration":{"labels":{"orchard.project":"demo","orchard.service":"db"}}},
				{"id":"api","status":{"state":"stopped"},"configuration":{"labels":{"orchard.project":"demo","orchard.service":"api"}}}
			]`), nil
		case strings.HasPrefix(cmd, "network list"):
			return []byte("[]"), nil
		case strings.HasPrefix(cmd, "start "):
			started = append(started, args[1])
		case strings.HasPrefix(cmd, "build ") || strings.HasPrefix(cmd, "run "):
			t.Errorf("must not build/recreate an existing container: %q", cmd)
		}
		return nil, nil
	}))
	backend.DryRun = false
	t.Cleanup(func() { backend.DryRun = false })

	if err := Up(context.Background(), demoProject(), true); err != nil {
		t.Fatal(err)
	}
	if len(started) != 1 || started[0] != "api" {
		t.Fatalf("want only the stopped service (api) started, got %v", started)
	}
}

func TestUp_StartStoppedErrorPropagates(t *testing.T) {
	t.Cleanup(backend.SetExecForTest(func(_ context.Context, _ bool, args ...string) ([]byte, error) {
		cmd := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(cmd, "ls --all"):
			return []byte(`[{"id":"db","status":{"state":"exited"},"configuration":{"labels":{"orchard.project":"demo","orchard.service":"db"}}}]`), nil
		case strings.HasPrefix(cmd, "network list"):
			return []byte("[]"), nil
		case strings.HasPrefix(cmd, "start "):
			return nil, errors.New("cannot start")
		}
		return nil, nil
	}))
	backend.DryRun = false
	t.Cleanup(func() { backend.DryRun = false })
	if err := Up(context.Background(), demoProject(), true); err == nil {
		t.Fatal("expected start error")
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
	if err := Up(context.Background(), demoProject(), true); err == nil {
		t.Fatal("expected list error")
	}
}

func TestUp_TopoErrorPropagates(t *testing.T) {
	failOn(t, "") // no command failure; cycle should surface first
	p := &types.Project{Name: "demo", Services: types.Services{
		"a": {Name: "a", DependsOn: dep("b")},
		"b": {Name: "b", DependsOn: dep("a")},
	}}
	if err := Up(context.Background(), p, true); err == nil {
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
			return []byte(`[{"name":"db","labels":{"orchard.project":"demo","orchard.service":"db"}}]`), nil
		}
		return nil, nil
	}))
	backend.DryRun = false
	t.Cleanup(func() { backend.DryRun = false })

	if err := Down(context.Background(), &types.Project{Name: "demo"}); err != nil {
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
	if err := Down(context.Background(), &types.Project{Name: "demo"}); err == nil {
		t.Fatal("expected list error")
	}
}

// --- Ps --------------------------------------------------------------------

func TestPs_ListsContainers(t *testing.T) {
	t.Cleanup(backend.SetExecForTest(func(_ context.Context, _ bool, _ ...string) ([]byte, error) {
		return []byte(`[{"name":"db","labels":{"orchard.project":"demo","orchard.service":"db"}}]`), nil
	}))
	backend.DryRun = false
	t.Cleanup(func() { backend.DryRun = false })

	var buf bytes.Buffer
	if err := Ps(context.Background(), &types.Project{Name: "demo"}, &buf); err != nil {
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
	if err := Ps(context.Background(), &types.Project{Name: "demo"}, &buf); err != nil {
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
	if err := Ps(context.Background(), &types.Project{Name: "demo"}, &bytes.Buffer{}); err == nil {
		t.Fatal("expected error")
	}
}

// --- Logs ------------------------------------------------------------------

func TestLogs_FollowAndNoFollow(t *testing.T) {
	p := &types.Project{Name: "demo", Services: types.Services{"db": {Name: "db"}}}

	out := captureDryRun(t, func() error { return Logs(context.Background(), p, true) })
	if strings.TrimSpace(out) != "container logs -f db" {
		t.Fatalf("follow logs = %q", out)
	}

	out = captureDryRun(t, func() error { return Logs(context.Background(), p, false) })
	if strings.TrimSpace(out) != "container logs db" {
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
