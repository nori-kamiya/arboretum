package orch

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/nori-kamiya/orchard/internal/backend"
)

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

func TestTrimFloat(t *testing.T) {
	cases := map[float64]string{1: "1", 0.5: "0.5", 0.25: "0.25"}
	for in, want := range cases {
		if got := trimFloat(in); got != want {
			t.Errorf("trimFloat(%v) = %q, want %q", in, got, want)
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
	if got := cpuLimit(types.ServiceConfig{Deploy: limitFor(0, 1)}); got != "1" {
		t.Errorf("deploy cpus = %q", got)
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
