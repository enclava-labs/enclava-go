package config

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"enclava-go/internal/auth"
)

type Config struct {
	Host                    string
	Port                    int
	DatabaseURL             string
	DataDir                 string
	SessionTTL              time.Duration
	ReadTimeout             time.Duration
	WriteTimeout            time.Duration
	IdleTimeout             time.Duration
	ReadHeaderTimeout       time.Duration
	MaxUploadSizeMB         int64
	Provider                string
	UpstreamOpenAIBaseURL   string
	UpstreamOpenAIAPIKey    string
	OpenAICompatibleBaseURL string
	OpenAICompatibleAPIKey  string
	RedPillBaseURL          string
	RedPillAPIKey           string
	UpstreamTimeout         time.Duration
	ProviderProbeInterval   time.Duration
	ProviderProbeTimeout    time.Duration
	ProviderModelCacheTTL   time.Duration
	QdrantURL               string
	DefaultEmbeddingModel   string
	ConnectorSyncInterval   time.Duration
	TinfoilAPIKey           string
	TinfoilEnclave          string
	TinfoilRepo             string
	TinfoilVerifyAtStart    bool
	TinfoilVerifyPerCall    bool
	TinfoilVerifyMaxAge     time.Duration
	APIKeyHashAlgo          string
	APIKeyPepper            string
	APIKeyPrefix            string
	CORSAllowedOrigins      []string
	// Dashboard login runs on server sessions only — there is no
	// ADMIN_EMAIL/ADMIN_PASSWORD path; the `enclava bootstrap` CLI is the
	// first-login story (AUTH-01/AUTH-03).
	TrustProxyHeaders  bool
	BootstrapAPIKey    string
	BootstrapEnabled   bool
	BootstrapMinLength int
	// SecretBindings are the operator-approved tenant-bound credential
	// references (SECRET_BINDINGS). Auth owns resolution; config only parses
	// and validates this trusted runtime configuration — a stored
	// credential_ref never names an environment variable itself.
	SecretBindings []auth.SecretBinding
}

func Load() (Config, error) {
	privatemodeAPIKey := getEnv("PRIVATEMODE_API_KEY", "")
	privatemodeBaseURL := getEnv("PRIVATEMODE_BASE_URL", "")

	cfg := Config{
		Host:        getEnv("APP_HOST", "0.0.0.0"),
		Port:        getEnvInt("APP_PORT", 8080),
		DatabaseURL: getEnv("DATABASE_URL", ""),
		DataDir:     getEnv("ENCLAVA_DATA_DIR", "./data"),
		SessionTTL:  getEnvDuration("SESSION_TTL", 12*time.Hour),
		ReadTimeout: getEnvDuration("HTTP_READ_TIMEOUT", 30*time.Second),
		// Streaming (SSE) can exceed fixed write timeouts; prefer disabling unless you know you don't need streaming.
		WriteTimeout:            getEnvDuration("HTTP_WRITE_TIMEOUT", 0),
		IdleTimeout:             getEnvDuration("HTTP_IDLE_TIMEOUT", 60*time.Second),
		ReadHeaderTimeout:       getEnvDuration("HTTP_READ_HEADER_TIMEOUT", 10*time.Second),
		MaxUploadSizeMB:         int64(getEnvInt("MAX_UPLOAD_SIZE_MB", 15)),
		Provider:                strings.ToLower(strings.TrimSpace(getEnv("INFERENCE_PROVIDER", "privatemode"))),
		UpstreamOpenAIBaseURL:   privatemodeBaseURL,
		UpstreamOpenAIAPIKey:    privatemodeAPIKey,
		OpenAICompatibleBaseURL: strings.TrimSpace(getEnv("OPENAI_COMPATIBLE_BASE_URL", privatemodeBaseURL)),
		OpenAICompatibleAPIKey:  strings.TrimSpace(getEnv("OPENAI_COMPATIBLE_API_KEY", privatemodeAPIKey)),
		RedPillBaseURL:          strings.TrimSpace(getEnv("REDPILL_BASE_URL", "")),
		RedPillAPIKey:           strings.TrimSpace(getEnv("REDPILL_API_KEY", "")),
		UpstreamTimeout:         getEnvDuration("PRIVATEMODE_TIMEOUT", 120*time.Second),
		ProviderProbeInterval:   getEnvDuration("PROVIDER_PROBE_INTERVAL", 5*time.Second),
		ProviderProbeTimeout:    getEnvDuration("PROVIDER_PROBE_TIMEOUT", 10*time.Second),
		ProviderModelCacheTTL:   getEnvDuration("PROVIDER_MODEL_CACHE_TTL", 5*time.Minute),
		QdrantURL:               strings.TrimSpace(getEnv("QDRANT_URL", "http://localhost:6333")),
		DefaultEmbeddingModel:   strings.TrimSpace(getEnv("DEFAULT_EMBEDDING_MODEL", "nomic-embed-text")),
		ConnectorSyncInterval:   getEnvDuration("CONNECTOR_SYNC_INTERVAL", 0),
		TinfoilAPIKey:           getEnv("TINFOIL_API_KEY", ""),
		TinfoilEnclave:          getEnv("TINFOIL_ENCLAVE", ""),
		TinfoilRepo:             getEnv("TINFOIL_REPO", ""),
		TinfoilVerifyAtStart:    getEnvBool("TINFOIL_VERIFY_AT_START", true),
		TinfoilVerifyPerCall:    getEnvBool("TINFOIL_VERIFY_PER_CALL", false),
		TinfoilVerifyMaxAge:     getEnvDuration("TINFOIL_VERIFY_MAX_AGE", 10*time.Minute),
		APIKeyHashAlgo:          getEnv("API_KEY_HASH_ALGO", "sha256"),
		APIKeyPepper:            getEnv("API_KEY_PEPPER", ""),
		APIKeyPrefix:            getEnv("API_KEY_PREFIX", "enc_"),
		CORSAllowedOrigins:      getEnvList("CORS_ALLOWED_ORIGINS"),
		TrustProxyHeaders:       getEnvBool("TRUST_PROXY_HEADERS", false),
		BootstrapAPIKey:         getEnv("BOOTSTRAP_API_KEY", ""),
		BootstrapEnabled:        getEnvBool("BOOTSTRAP_ENABLED", false),
		BootstrapMinLength:      getEnvInt("BOOTSTRAP_API_KEY_MIN_LENGTH", 48),
	}
	if cfg.Provider == "openai" {
		cfg.Provider = "privatemode"
	}
	if cfg.Port <= 0 {
		return Config{}, fmt.Errorf("APP_PORT must be > 0")
	}
	if err := validatePostgresDSN(cfg.DatabaseURL); err != nil {
		return Config{}, err
	}
	if strings.TrimSpace(cfg.DataDir) == "" {
		return Config{}, fmt.Errorf("ENCLAVA_DATA_DIR must be non-empty")
	}
	if cfg.SessionTTL <= 0 {
		return Config{}, fmt.Errorf("SESSION_TTL must be > 0")
	}
	if cfg.MaxUploadSizeMB <= 0 {
		return Config{}, fmt.Errorf("MAX_UPLOAD_SIZE_MB must be > 0")
	}
	if cfg.ProviderProbeInterval <= 0 {
		return Config{}, fmt.Errorf("PROVIDER_PROBE_INTERVAL must be > 0")
	}
	if cfg.ProviderProbeTimeout <= 0 {
		return Config{}, fmt.Errorf("PROVIDER_PROBE_TIMEOUT must be > 0")
	}
	if cfg.ProviderModelCacheTTL < 0 {
		return Config{}, fmt.Errorf("PROVIDER_MODEL_CACHE_TTL must be >= 0")
	}
	if cfg.QdrantURL == "" {
		return Config{}, fmt.Errorf("QDRANT_URL must be non-empty")
	}
	if cfg.DefaultEmbeddingModel == "" {
		return Config{}, fmt.Errorf("DEFAULT_EMBEDDING_MODEL must be non-empty")
	}
	if cfg.ConnectorSyncInterval < 0 {
		return Config{}, fmt.Errorf("CONNECTOR_SYNC_INTERVAL must be >= 0")
	}
	switch cfg.APIKeyHashAlgo {
	case "sha256":
	case "hmac_sha256":
		if cfg.APIKeyPepper == "" {
			return Config{}, fmt.Errorf("API_KEY_PEPPER is required when API_KEY_HASH_ALGO=hmac_sha256")
		}
	default:
		return Config{}, fmt.Errorf("API_KEY_HASH_ALGO must be one of: sha256, hmac_sha256")
	}
	if cfg.APIKeyPrefix == "" {
		return Config{}, fmt.Errorf("API_KEY_PREFIX must be non-empty")
	}

	if cfg.BootstrapAPIKey != "" {
		cfg.BootstrapAPIKey = strings.TrimSpace(cfg.BootstrapAPIKey)
		if cfg.BootstrapAPIKey == "" {
			return Config{}, fmt.Errorf("BOOTSTRAP_API_KEY must not be blank")
		}
		if !cfg.BootstrapEnabled {
			return Config{}, fmt.Errorf("BOOTSTRAP_API_KEY is set but BOOTSTRAP_ENABLED=false; set BOOTSTRAP_ENABLED=true to use bootstrap seeding")
		}
		if cfg.BootstrapMinLength <= 0 {
			return Config{}, fmt.Errorf("BOOTSTRAP_API_KEY_MIN_LENGTH must be > 0")
		}
		if err := validateBootstrapAPIKey(cfg.BootstrapAPIKey, cfg.BootstrapMinLength); err != nil {
			return Config{}, err
		}
	} else if cfg.BootstrapEnabled {
		return Config{}, fmt.Errorf("BOOTSTRAP_ENABLED=true requires BOOTSTRAP_API_KEY")
	}

	if cfg.BootstrapMinLength < 24 && cfg.BootstrapAPIKey != "" {
		return Config{}, fmt.Errorf("BOOTSTRAP_API_KEY_MIN_LENGTH must be at least 24")
	}
	if cfg.Provider != "privatemode" && cfg.Provider != "tinfoil" && cfg.Provider != "openai_compatible" && cfg.Provider != "redpill" {
		return Config{}, fmt.Errorf("INFERENCE_PROVIDER must be one of: privatemode, tinfoil, openai_compatible, redpill")
	}
	if cfg.Provider == "privatemode" && strings.TrimSpace(cfg.UpstreamOpenAIAPIKey) == "" {
		return Config{}, fmt.Errorf("PRIVATEMODE_API_KEY is required when INFERENCE_PROVIDER=privatemode")
	}
	if cfg.Provider == "privatemode" && strings.TrimSpace(cfg.UpstreamOpenAIBaseURL) == "" {
		return Config{}, fmt.Errorf("PRIVATEMODE_BASE_URL is required when INFERENCE_PROVIDER=privatemode")
	}
	if cfg.Provider == "openai_compatible" && strings.TrimSpace(cfg.OpenAICompatibleAPIKey) == "" {
		return Config{}, fmt.Errorf("OPENAI_COMPATIBLE_API_KEY is required when INFERENCE_PROVIDER=openai_compatible")
	}
	if cfg.Provider == "openai_compatible" && strings.TrimSpace(cfg.OpenAICompatibleBaseURL) == "" {
		return Config{}, fmt.Errorf("OPENAI_COMPATIBLE_BASE_URL is required when INFERENCE_PROVIDER=openai_compatible")
	}
	if cfg.Provider == "redpill" && strings.TrimSpace(cfg.RedPillAPIKey) == "" {
		return Config{}, fmt.Errorf("REDPILL_API_KEY is required when INFERENCE_PROVIDER=redpill")
	}
	if cfg.Provider == "redpill" && strings.TrimSpace(cfg.RedPillBaseURL) == "" {
		return Config{}, fmt.Errorf("REDPILL_BASE_URL is required when INFERENCE_PROVIDER=redpill")
	}
	if cfg.Provider == "tinfoil" {
		if strings.TrimSpace(cfg.TinfoilAPIKey) == "" {
			return Config{}, fmt.Errorf("TINFOIL_API_KEY is required when INFERENCE_PROVIDER=tinfoil")
		}
		hasEnclave := strings.TrimSpace(cfg.TinfoilEnclave) != ""
		hasRepo := strings.TrimSpace(cfg.TinfoilRepo) != ""
		if hasEnclave != hasRepo {
			return Config{}, fmt.Errorf("TINFOIL_ENCLAVE and TINFOIL_REPO must be set together when either is set")
		}
	}

	bindings, err := parseSecretBindings(os.Getenv("SECRET_BINDINGS"))
	if err != nil {
		return Config{}, err
	}
	cfg.SecretBindings = bindings

	return cfg, nil
}

// parseSecretBindings decodes the SECRET_BINDINGS list. Format:
// ref_id|tenant_id|purpose|origins|env_name entries separated by ';',
// origins comma-separated. The resolver re-validates everything; the checks
// here reject structurally malformed entries and reserved names at Load so
// bad operator configuration fails before dispatch. The error text never
// includes secret values (there are none in this variable).
func parseSecretBindings(raw string) ([]auth.SecretBinding, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var out []auth.SecretBinding
	seen := make(map[string]struct{})
	for _, entry := range strings.Split(raw, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			return nil, fmt.Errorf("SECRET_BINDINGS contains an empty entry")
		}
		fields := strings.Split(entry, "|")
		if len(fields) != 5 {
			return nil, fmt.Errorf("SECRET_BINDINGS entry must have the form ref_id|tenant_id|purpose|origins|env_name")
		}
		b := auth.SecretBinding{
			RefID:    strings.TrimSpace(fields[0]),
			TenantID: strings.TrimSpace(fields[1]),
			Purpose:  strings.TrimSpace(fields[2]),
			EnvName:  strings.TrimSpace(fields[4]),
		}
		if b.RefID == "" || b.TenantID == "" {
			return nil, fmt.Errorf("SECRET_BINDINGS entry requires a reference id and tenant id")
		}
		switch b.Purpose {
		case auth.SecretPurposeInference, auth.SecretPurposeConnector,
			auth.SecretPurposeMCP, auth.SecretPurposeEmbeddings:
		default:
			return nil, fmt.Errorf("SECRET_BINDINGS entry has unknown purpose %q", b.Purpose)
		}
		if strings.HasPrefix(b.RefID, "env:") {
			return nil, fmt.Errorf("SECRET_BINDINGS reference ids are opaque; %q is reserved", "env:")
		}
		for _, o := range strings.Split(fields[3], ",") {
			o = strings.TrimSpace(o)
			if o == "" {
				return nil, fmt.Errorf("SECRET_BINDINGS entry has an empty origin")
			}
			if _, err := auth.CanonicalizeSecretOrigin(o); err != nil {
				return nil, fmt.Errorf("SECRET_BINDINGS entry has a malformed origin")
			}
			b.Origins = append(b.Origins, o)
		}
		key := b.TenantID + "\x00" + b.Purpose + "\x00" + b.RefID
		if _, dup := seen[key]; dup {
			return nil, fmt.Errorf("SECRET_BINDINGS contains a duplicate binding")
		}
		seen[key] = struct{}{}
		out = append(out, b)
	}
	return out, nil
}

func (c Config) Addr() string {
	return fmt.Sprintf("%s:%d", c.Host, c.Port)
}

func (c Config) MaxUploadBytes() int64 {
	return c.MaxUploadSizeMB * 1024 * 1024
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	i, err := strconv.Atoi(v)
	if err != nil {
		log.Printf("warning: invalid value %q for %s, using default %d", v, key, fallback)
		return fallback
	}
	return i
}

func getEnvDuration(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Printf("warning: invalid duration %q for %s, using default %v", v, key, fallback)
		return fallback
	}
	return d
}

func getEnvList(key string) []string {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		out = append(out, part)
	}
	return out
}

func validateBootstrapAPIKey(value string, minLength int) error {
	normalized := strings.ToLower(strings.TrimSpace(value))
	switch normalized {
	case "",
		"enc_demo_replace_me",
		"demo",
		"change_me",
		"changeme",
		"bootstrap",
		"default",
		"password",
		"test",
		"abc123",
		"abc123456":
		return fmt.Errorf("BOOTSTRAP_API_KEY uses a forbidden placeholder value")
	}

	if len(value) < minLength {
		return fmt.Errorf("BOOTSTRAP_API_KEY must be at least %d characters", minLength)
	}

	if strings.ContainsAny(value, " \t\r\n") {
		return fmt.Errorf("BOOTSTRAP_API_KEY must not contain whitespace")
	}

	return nil
}

func getEnvBool(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return parsed
}

// validatePostgresDSN fails closed on anything that is not a PostgreSQL DSN:
// only postgres://, postgresql:// or key=value connection strings pass;
// postgres.Open re-checks the same rule before opening the pool.
func validatePostgresDSN(dsn string) error {
	v := strings.TrimSpace(dsn)
	if v == "" {
		return fmt.Errorf("DATABASE_URL must be a PostgreSQL DSN (postgres://, postgresql:// or key=value); got empty")
	}
	if strings.HasPrefix(v, "postgres://") || strings.HasPrefix(v, "postgresql://") {
		return nil
	}
	if strings.HasPrefix(v, "file:") || strings.HasSuffix(v, ".db") || strings.ContainsAny(v, "/\\") {
		return fmt.Errorf("DATABASE_URL must be a PostgreSQL DSN (postgres://, postgresql:// or key=value); got %q", dsn)
	}
	if !strings.Contains(v, "=") {
		return fmt.Errorf("DATABASE_URL must be a PostgreSQL DSN (postgres://, postgresql:// or key=value); got %q", dsn)
	}
	return nil
}
