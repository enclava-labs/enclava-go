package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"enclava-go/internal/testdb"
)

func TestBootstrap(t *testing.T) {
	db, dsn := testdb.NewWithDSN(t)
	slug := fmt.Sprintf("bootstrap-%d", time.Now().UnixNano())
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"DATABASE_URL=" + dsn,
		"ENCLAVA_DATA_DIR=" + t.TempDir(),
		"INFERENCE_PROVIDER=openai_compatible",
		"OPENAI_COMPATIBLE_BASE_URL=http://127.0.0.1:18443",
		"OPENAI_COMPATIBLE_API_KEY=stub-key",
	}
	bin := filepath.Join(t.TempDir(), "enclava")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build enclava binary: %v\n%s", err, out)
	}
	args := []string{"bootstrap", "--tenant", slug, "--email", "a@b.c", "--name", "A", "--issue-key"}
	first := exec.Command(bin, args...)
	first.Env = env
	output, err := first.CombinedOutput()
	if err != nil {
		t.Fatalf("first bootstrap: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "tenant "+slug+" created") {
		t.Fatalf("bootstrap did not report created tenant: %s", output)
	}

	keyMatch := regexp.MustCompile(`(?m)^api key: (\S+)\s*$`).FindStringSubmatch(string(output))
	if keyMatch == nil || !strings.HasPrefix(keyMatch[1], "enc_") {
		t.Fatalf("bootstrap API key missing or malformed: %s", output)
	}

	var tenantCount int
	if err := db.SQL.QueryRowContext(context.Background(), "SELECT count(*) FROM tenants WHERE slug = $1", slug).Scan(&tenantCount); err != nil {
		t.Fatalf("query bootstrap tenant: %v", err)
	}
	if tenantCount != 1 {
		t.Fatalf("tenant count = %d, want 1", tenantCount)
	}

	second := exec.Command(bin, args...)
	second.Env = env
	if output, err := second.CombinedOutput(); err == nil {
		t.Fatalf("bootstrap unexpectedly accepted existing slug: %s", output)
	}

	var keyCount int
	if err := db.SQL.QueryRowContext(context.Background(), `SELECT count(*) FROM api_keys k JOIN tenants t ON t.id = k.tenant_id WHERE t.slug = $1`, slug).Scan(&keyCount); err != nil {
		t.Fatalf("query bootstrap API key: %v", err)
	}
	if keyCount != 1 {
		t.Fatalf("API key count = %d, want 1", keyCount)
	}

}
