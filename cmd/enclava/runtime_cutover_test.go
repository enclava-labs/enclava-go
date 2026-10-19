package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"enclava-go/internal/httpx"
)

func TestApplicationRuntimeCutover(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list dependencies: %v", err)
	}
	for _, dep := range strings.Split(string(out), "\n") {
		if strings.Contains(dep, "python") || strings.Contains(dep, "node") || strings.Contains(dep, "cgo") {
			t.Errorf("unexpected non-Go runtime dependency %q", dep)
		}
	}

	bin := filepath.Join(t.TempDir(), "enclava")
	build := exec.Command("go", "build", "-trimpath", "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("CGO_ENABLED=0 build: %v\n%s", err, out)
	}
	info, err := os.Stat(bin)
	if err != nil {
		t.Fatalf("stat built binary: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("built binary is empty")
	}

	if _, err := httpx.DashboardTemplates().Parse("{{define \"embedded-smoke\"}}ok{{end}}"); err != nil {
		t.Fatalf("embedded dashboard templates unavailable: %v", err)
	}
}
