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

	p, err := Load(context.Background(), []string{path}, "override")
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

	p, err := Load(context.Background(), nil, "")
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
	if _, err := Load(context.Background(), []string{path}, "Invalid Name"); err == nil {
		t.Fatal("expected options error for invalid project name")
	}
}

func TestLoad_LoadError_MissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.yaml")
	if _, err := Load(context.Background(), []string{missing}, ""); err == nil {
		t.Fatal("expected load error for missing file")
	}
}
