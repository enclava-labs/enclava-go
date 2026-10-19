package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadRejectsNonPostgresDatabaseURL(t *testing.T) {
	tests := []struct {
		name string
		dsn  string
	}{
		{"empty", ""},
		{"filesystem relative path", "./enclava.db"},
		{"filesystem absolute path", "/tmp/enclava.db"},
		{"bare .db filename", "enclava.db"},
		{"sqlite file URI", "file:enclava.db"},
		{"no key=value and no scheme", "enclava"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearConfigEnv(t)
			if tt.dsn != "" {
				t.Setenv("DATABASE_URL", tt.dsn)
			} else {
				t.Setenv("DATABASE_URL", "")
			}
			t.Setenv("PRIVATEMODE_API_KEY", "pm_test_key")
			t.Setenv("PRIVATEMODE_BASE_URL", "https://example.invalid/v1")

			if _, err := Load(); err == nil {
				t.Fatalf("expected Load to reject DATABASE_URL %q", tt.dsn)
			}
		})
	}
}

func TestLoadAcceptsPostgresDatabaseURL(t *testing.T) {
	tests := []struct {
		name string
		dsn  string
	}{
		{"postgres scheme", "postgres://user:pass@localhost:5432/enclava"},
		{"postgresql scheme", "postgresql://user:pass@localhost:5432/enclava?sslmode=disable"},
		{"key=value", "host=localhost port=5432 user=enclava dbname=enclava"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv("DATABASE_URL", tt.dsn)
			t.Setenv("PRIVATEMODE_API_KEY", "pm_test_key")
			t.Setenv("PRIVATEMODE_BASE_URL", "https://example.invalid/v1")

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load returned error: %v", err)
			}
			if cfg.DatabaseURL != tt.dsn {
				t.Fatalf("expected DatabaseURL %q, got %q", tt.dsn, cfg.DatabaseURL)
			}
		})
	}
}

func TestLoadDataDirAndSessionTTL(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("PRIVATEMODE_API_KEY", "pm_test_key")
	t.Setenv("PRIVATEMODE_BASE_URL", "https://example.invalid/v1")
	t.Setenv("ENCLAVA_DATA_DIR", "/var/lib/enclava")
	t.Setenv("SESSION_TTL", "30m")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.DataDir != "/var/lib/enclava" {
		t.Fatalf("expected DataDir /var/lib/enclava, got %q", cfg.DataDir)
	}
	if cfg.SessionTTL != 30*time.Minute {
		t.Fatalf("expected SessionTTL 30m, got %v", cfg.SessionTTL)
	}
}

func TestLoadDataDirAndSessionTTLDefaults(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("PRIVATEMODE_API_KEY", "pm_test_key")
	t.Setenv("PRIVATEMODE_BASE_URL", "https://example.invalid/v1")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.DataDir != "./data" {
		t.Fatalf("expected default DataDir ./data, got %q", cfg.DataDir)
	}
	if cfg.SessionTTL != 12*time.Hour {
		t.Fatalf("expected default SessionTTL 12h, got %v", cfg.SessionTTL)
	}
}

func TestLoadAllowsTinfoilDefaultRouting(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("INFERENCE_PROVIDER", "tinfoil")
	t.Setenv("TINFOIL_API_KEY", "tf_test_key")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.Provider != "tinfoil" {
		t.Fatalf("expected tinfoil provider, got %q", cfg.Provider)
	}
	if cfg.TinfoilEnclave != "" || cfg.TinfoilRepo != "" {
		t.Fatalf("expected empty explicit Tinfoil target, got enclave=%q repo=%q", cfg.TinfoilEnclave, cfg.TinfoilRepo)
	}
}

func TestLoadRejectsPartialTinfoilRouting(t *testing.T) {
	tests := []struct {
		name    string
		enclave string
		repo    string
	}{
		{name: "enclave only", enclave: "inference.tinfoil.sh"},
		{name: "repo only", repo: "tinfoilsh/confidential-model-router"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv("INFERENCE_PROVIDER", "tinfoil")
			t.Setenv("TINFOIL_API_KEY", "tf_test_key")
			t.Setenv("TINFOIL_ENCLAVE", tt.enclave)
			t.Setenv("TINFOIL_REPO", tt.repo)

			_, err := Load()
			if err == nil {
				t.Fatal("Load() error = nil, want routing error")
			}
			if !strings.Contains(err.Error(), "TINFOIL_ENCLAVE and TINFOIL_REPO must be set together") {
				t.Fatalf("Load() error = %q, want Tinfoil routing error", err)
			}
		})
	}
}

func TestLoadCORSAllowedOrigins(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("PRIVATEMODE_API_KEY", "pm_test_key")
	t.Setenv("PRIVATEMODE_BASE_URL", "https://example.invalid/v1")
	t.Setenv("CORS_ALLOWED_ORIGINS", "https://app.example.com, https://admin.example.com")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(cfg.CORSAllowedOrigins) != 2 {
		t.Fatalf("expected two origins, got %#v", cfg.CORSAllowedOrigins)
	}
	if cfg.CORSAllowedOrigins[0] != "https://app.example.com" || cfg.CORSAllowedOrigins[1] != "https://admin.example.com" {
		t.Fatalf("unexpected origins: %#v", cfg.CORSAllowedOrigins)
	}
}

func TestLoadOpenAICompatibleProvider(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("INFERENCE_PROVIDER", "openai_compatible")
	t.Setenv("OPENAI_COMPATIBLE_API_KEY", "oa_test_key")
	t.Setenv("OPENAI_COMPATIBLE_BASE_URL", "https://llm.example.com/v1")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.Provider != "openai_compatible" || cfg.OpenAICompatibleBaseURL != "https://llm.example.com/v1" {
		t.Fatalf("unexpected provider config: %#v", cfg)
	}
}

func TestLoadRedPillProvider(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("INFERENCE_PROVIDER", "redpill")
	t.Setenv("REDPILL_API_KEY", "rp_test_key")
	t.Setenv("REDPILL_BASE_URL", "https://redpill.example.com/v1")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.Provider != "redpill" || cfg.RedPillBaseURL != "https://redpill.example.com/v1" {
		t.Fatalf("unexpected provider config: %#v", cfg)
	}
}
func TestLoadSecretBindings(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("PRIVATEMODE_API_KEY", "pm_test_key")
	t.Setenv("PRIVATEMODE_BASE_URL", "https://example.invalid/v1")
	t.Setenv("SECRET_BINDINGS",
		"endpoint-key|tenant-a|inference|https://api.upstream.example|ENCLAVA_CRED_TENANT_A_EP; "+
			"conn-token|tenant-b|connector|https://api.github.com,https://uploads.github.com|ENCLAVA_CRED_TENANT_B_CONN")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(cfg.SecretBindings) != 2 {
		t.Fatalf("expected 2 secret bindings, got %d", len(cfg.SecretBindings))
	}
	b := cfg.SecretBindings[0]
	if b.RefID != "endpoint-key" || b.TenantID != "tenant-a" ||
		b.Purpose != "inference" || b.EnvName != "ENCLAVA_CRED_TENANT_A_EP" ||
		len(b.Origins) != 1 || b.Origins[0] != "https://api.upstream.example" {
		t.Fatalf("unexpected binding: %+v", b)
	}
	if len(cfg.SecretBindings[1].Origins) != 2 {
		t.Fatalf("expected two approved origins, got %+v", cfg.SecretBindings[1].Origins)
	}
}

func TestLoadRejectsMalformedSecretBindings(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"missing fields", "onlyref|tenant-a"},
		{"empty tenant", "ref||inference|https://api.upstream.example|ENCLAVA_CRED_X"},
		{"unknown purpose", "ref|tenant-a|shell|https://api.upstream.example|ENCLAVA_CRED_X"},
		{"empty origin list", "ref|tenant-a|inference||ENCLAVA_CRED_X"},
		{"origin with path", "ref|tenant-a|inference|https://api.upstream.example/evil|ENCLAVA_CRED_X"},
		{"origin with credentials", "ref|tenant-a|inference|https://user:pw@api.upstream.example|ENCLAVA_CRED_X"},
		{"non-http origin", "ref|tenant-a|inference|file:///etc/passwd|ENCLAVA_CRED_X"},
		{"env-style ref id", "env:OTHER|tenant-a|inference|https://api.upstream.example|ENCLAVA_CRED_X"},
		{"duplicate binding", "ref|tenant-a|inference|https://a.example|ENCLAVA_CRED_X;ref|tenant-a|inference|https://b.example|ENCLAVA_CRED_Y"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv("PRIVATEMODE_API_KEY", "pm_test_key")
			t.Setenv("PRIVATEMODE_BASE_URL", "https://example.invalid/v1")
			t.Setenv("SECRET_BINDINGS", tt.raw)
			if _, err := Load(); err == nil {
				t.Fatalf("expected Load to reject SECRET_BINDINGS %q", tt.raw)
			}
		})
	}
}

func clearConfigEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"APP_HOST",
		"APP_PORT",
		"HTTP_READ_TIMEOUT",
		"HTTP_WRITE_TIMEOUT",
		"HTTP_IDLE_TIMEOUT",
		"HTTP_READ_HEADER_TIMEOUT",
		"MAX_UPLOAD_SIZE_MB",
		"INFERENCE_PROVIDER",
		"PRIVATEMODE_BASE_URL",
		"PRIVATEMODE_API_KEY",
		"PRIVATEMODE_TIMEOUT",
		"OPENAI_COMPATIBLE_BASE_URL",
		"OPENAI_COMPATIBLE_API_KEY",
		"REDPILL_BASE_URL",
		"REDPILL_API_KEY",
		"PROVIDER_PROBE_INTERVAL",
		"PROVIDER_PROBE_TIMEOUT",
		"PROVIDER_MODEL_CACHE_TTL",
		"QDRANT_URL",
		"DEFAULT_EMBEDDING_MODEL",
		"TINFOIL_API_KEY",
		"TINFOIL_ENCLAVE",
		"TINFOIL_REPO",
		"TINFOIL_VERIFY_AT_START",
		"TINFOIL_VERIFY_PER_CALL",
		"TINFOIL_VERIFY_MAX_AGE",
		"API_KEY_HASH_ALGO",
		"API_KEY_PEPPER",
		"API_KEY_PREFIX",
		"CORS_ALLOWED_ORIGINS",
		"ADMIN_EMAIL",
		"ADMIN_PASSWORD",
		"SECRET_BINDINGS",
		"TRUST_PROXY_HEADERS",
		"BOOTSTRAP_API_KEY",
		"BOOTSTRAP_ENABLED",
		"BOOTSTRAP_API_KEY_MIN_LENGTH",
		"ENCLAVA_DATA_DIR",
		"SESSION_TTL",
	} {
		t.Setenv(key, "")
	}
	// Load requires a PostgreSQL DSN; tests that need a different value
	// can override with their own t.Setenv.
	t.Setenv("DATABASE_URL", "postgres://enclava:enclava@localhost:5432/enclava_test")
}
