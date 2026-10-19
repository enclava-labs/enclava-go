package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/config"
	"enclava-go/internal/store/postgres"
)

// bootstrapScopes is the seeded admin role: platform.admin plus every feature
// scope the router gates on.
var bootstrapAdminScopes = []string{
	auth.ScopePlatformAdmin,
	"models.list", "chat.completions", "embeddings.create", "responses",
	"openai.proxy", "extract", "extract.manage", "rag", "rag.manage",
	"chatbot", "chatbot.manage", "connectors.manage", "pricing.manage",
}

// runBootstrap implements `enclava bootstrap` — the operator-only first-login
// path (AUTH-01). It creates the first tenant and an admin user with a
// generated password printed exactly once. There is no fixed credential and
// no HTTP signup: database access is the authorization.
func runBootstrap(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	tenantSlug := fs.String("tenant", "default", "slug for the first tenant")
	email := fs.String("email", "admin@local", "admin user email")
	name := fs.String("name", "Administrator", "admin display name")
	issueKey := fs.Bool("issue-key", false, "also issue a bootstrap API key (printed once)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	*tenantSlug = strings.TrimSpace(*tenantSlug)
	if *tenantSlug == "" {
		return errors.New("--tenant must be non-empty")
	}
	if strings.TrimSpace(*email) == "" {
		return errors.New("--email must be non-empty")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	db, err := postgres.Open(ctx, cfg.DatabaseURL) // applies pending migrations
	if err != nil {
		return fmt.Errorf("postgres init: %w", err)
	}
	defer func() { _ = db.Close() }()

	password, err := generatedSecret(24) // 32+ chars base64url
	if err != nil {
		return err
	}
	passwordHash, err := auth.HashPassword(password)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	userID := "user_" + randHex(12)
	roleID := "role_admin"
	now := time.Now().UTC()

	var apiKey string
	err = postgres.WithTx(ctx, db.SQL, func(tx *sql.Tx) error {
		var tenantID string
		err := tx.QueryRowContext(ctx,
			`INSERT INTO tenants (id, slug, display_name)
			 VALUES (gen_random_uuid(), $1, $2) RETURNING id::text`,
			*tenantSlug, *tenantSlug).Scan(&tenantID)
		if err != nil {
			return fmt.Errorf("create tenant %q (refuses an existing slug): %w", *tenantSlug, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO roles (tenant_id, id, name, scopes)
			 VALUES ($1::uuid, $2, 'admin', $3::jsonb)`,
			tenantID, roleID, roleScopesJSON(bootstrapAdminScopes)); err != nil {
			return fmt.Errorf("create admin role: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO users
			 (tenant_id, id, email, display_name, status,
			  password_hash, password_set_at)
			 VALUES ($1::uuid, $2, $3, $4, 'active', $5, $6)`,
			tenantID, userID, strings.TrimSpace(*email), *name, passwordHash, now); err != nil {
			return fmt.Errorf("create user: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO user_roles (tenant_id, user_id, role_id)
			 VALUES ($1::uuid, $2, $3)`,
			tenantID, userID, roleID); err != nil {
			return fmt.Errorf("assign admin role: %w", err)
		}
		if *issueKey {
			key, err := insertAPIKey(ctx, tx, tenantID, userID, cfg)
			if err != nil {
				return err
			}
			apiKey = key
		}
		return nil
	})
	if err != nil {
		return err
	}

	// Secrets are printed once to stdout — never logged, never stored.
	fmt.Printf("tenant %s created\n", *tenantSlug)
	fmt.Printf("admin email: %s\npassword: %s\n", strings.TrimSpace(*email), password)
	if *issueKey {
		fmt.Printf("api key: %s\n", apiKey)
	}
	return nil
}

// insertAPIKey writes a stored key row owned by the admin user, scoped to
// platform.admin, and returns the plaintext secret exactly once.
func insertAPIKey(ctx context.Context, tx *sql.Tx, tenantID, userID string, cfg config.Config) (string, error) {
	plaintext := "enc_" + randHex(24)
	prefix, hash, algo, err := auth.HashForStorage(plaintext, auth.HashAlgoHMACSHA256, cfg.APIKeyPepper)
	if err != nil {
		if cfg.APIKeyPepper == "" {
			prefix, hash, algo, err = auth.HashForStorage(plaintext, auth.HashAlgoSHA256, "")
		}
		if err != nil {
			return "", fmt.Errorf("hash bootstrap key: %w", err)
		}
	}
	keyID := "key_" + randHex(12)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO api_keys
		 (tenant_id, id, owner_user_id, name,
		  key_prefix, key_hash, key_hash_algo, scopes, allowed_models,
		  allowed_extract_templates, allowed_chatbots, allowed_agents,
		  allowed_endpoints, allowed_ips)
		 VALUES ($1::uuid, $2, $3, 'bootstrap',
		  $4, $5, $6, $7::jsonb, $8::jsonb,
		  $8::jsonb, $8::jsonb, $8::jsonb,
		  $8::jsonb, $8::jsonb)`,
		tenantID, keyID, userID,
		prefix, hash, algo, scopesJSON(bootstrapAdminScopes),
		`{"mode":"all"}`); err != nil {
		return "", fmt.Errorf("create bootstrap key: %w", err)
	}
	return plaintext, nil
}

// roleScopesJSON stores []string scopes as a plain JSON array — the roles
// table's shape (scopeSlice decodes it); api_keys.scopes uses the
// mode/items grant document (scopesJSON below).
func roleScopesJSON(scopes []string) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, s := range scopes {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%q", s)
	}
	b.WriteByte(']')
	return b.String()
}

// scopesJSON encodes the mode/items grant document the api_keys.scopes
// column stores.
func scopesJSON(scopes []string) string {
	var b strings.Builder
	b.WriteString(`{"mode":"list","items":[`)
	for i, s := range scopes {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%q", s)
	}
	b.WriteString(`]}`)
	return b.String()
}

func generatedSecret(nbytes int) (string, error) {
	buf := make([]byte, nbytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func randHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return hex.EncodeToString(buf)
}
