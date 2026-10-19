package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLegacyCutoverRemoved(t *testing.T) {
	if _, err := os.Stat(filepath.Join("..", "..", "internal", "store", "sqlite")); !os.IsNotExist(err) {
		t.Fatalf("legacy SQLite store still exists (stat error: %v)", err)
	}
	files, err := filepath.Glob("migrate_import*.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("legacy import tooling remains: %v", files)
	}

	pkgs, err := exec.Command("go", "list", "./...").Output()
	if err != nil {
		t.Fatalf("go list packages: %v", err)
	}
	for _, pkg := range strings.Split(string(pkgs), "\n") {
		if strings.Contains(pkg, "enclava-go/internal/store/sqlite") {
			t.Errorf("legacy SQLite package remains: %s", pkg)
		}
	}

	cmdFiles, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range cmdFiles {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if strings.Contains(string(data), "migrate"+"-import") {
			t.Errorf("legacy import subcommand remains in %s", name)
		}
	}
}
