package compose

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

const sample = `name: fromfile
services:
  web:
    image: nginx:1.27
`

func writeCompose(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// chdir switches the working directory for the test and restores it after.
func chdir(t *testing.T, dir string) {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
}

func TestLoad_ExplicitFileWithNameOverride(t *testing.T) {
	path := writeCompose(t, t.TempDir(), sample)

	p, err := Load(context.Background(), []string{path}, "override", nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "override" {
		t.Fatalf("name = %q, want override", p.Name)
	}
	if _, ok := p.Services["web"]; !ok {
		t.Fatalf("service web missing: %+v", p.Services)
	}
}

func TestLoad_DefaultDiscovery(t *testing.T) {
	dir := t.TempDir()
	writeCompose(t, dir, sample)
	chdir(t, dir)

	p, err := Load(context.Background(), nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "fromfile" {
		t.Fatalf("name = %q, want fromfile", p.Name)
	}
}

func TestLoad_OptionsError_InvalidProjectName(t *testing.T) {
	// Compose project names must be lower-case [a-z0-9_-]; WithName validates at
	// construction, exercising the NewProjectOptions error branch.
	path := writeCompose(t, t.TempDir(), sample)
	if _, err := Load(context.Background(), []string{path}, "Invalid Name", nil); err == nil {
		t.Fatal("expected options error for invalid project name")
	}
}

func TestLoad_LoadError_MissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.yaml")
	if _, err := Load(context.Background(), []string{missing}, "", nil); err == nil {
		t.Fatal("expected load error for missing file")
	}
}

const profiled = `name: prof
services:
  web:
    image: nginx
  debugger:
    image: busybox
    profiles: [debug]
`

func TestLoad_ProfilesGateServices(t *testing.T) {
	path := writeCompose(t, t.TempDir(), profiled)

	// Without the profile, the profiled service is excluded.
	p, err := Load(context.Background(), []string{path}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.Services["debugger"]; ok {
		t.Fatal("debugger should be inactive without its profile")
	}
	if _, ok := p.Services["web"]; !ok {
		t.Fatal("web (no profile) should always be active")
	}

	// With the profile enabled, it appears.
	p, err = Load(context.Background(), []string{path}, "", []string{"debug"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.Services["debugger"]; !ok {
		t.Fatalf("debugger should be active with --profile debug: %+v", p.Services)
	}
}

func TestLoad_OverrideMerges(t *testing.T) {
	dir := t.TempDir()
	writeCompose(t, dir, sample) // compose.yaml: web -> nginx:1.27
	override := filepath.Join(dir, "compose.override.yaml")
	if err := os.WriteFile(override, []byte("services:\n  web:\n    image: nginx:1.28\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	chdir(t, dir)

	p, err := Load(context.Background(), nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Services["web"].Image; got != "nginx:1.28" {
		t.Fatalf("override not applied, image = %q", got)
	}
}
