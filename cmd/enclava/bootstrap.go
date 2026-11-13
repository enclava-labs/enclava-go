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

// bootstrapMemberScopes is the §3.2 member scope set; knowledge_manager adds
// the four manage scopes on top.
var bootstrapMemberScopes = []string{
	"models.list", "chat.completions", "embeddings.create", "responses",
	"chatbot", "rag", "extract",
}

// Stored grant documents (D-25): admin is the ONLY all-grant seed; every
// other default role stores explicit empty deny lists — an invited
// non-admin holds zero AI resources until an admin selects items (OA-20/28).
const (
	grantAllDoc  = `{"mode":"all"}`
	grantDenyDoc = `{"mode":"list","items":[]}`
)

// bootstrapRole is one seeded default role: its §3.2 scope list plus the
// explicit grant documents written to the four allowed_* columns
// (allowed_models, allowed_endpoints, allowed_assistants,
// allowed_extract_templates).
type bootstrapRole struct {
	id     string
	name   string
	scopes []string
	grants [4]string
}

var bootstrapRoles = []bootstrapRole{
	{
		id: "role_admin", name: "admin", scopes: bootstrapAdminScopes,
		grants: [4]string{grantAllDoc, grantAllDoc, grantAllDoc, grantAllDoc},
	},
	{
		id:   "role_knowledge_manager",
		name: "knowledge_manager",
		scopes: append(append([]string{}, bootstrapMemberScopes...),
			"rag.manage", "connectors.manage", "chatbot.manage", "extract.manage"),
		grants: [4]string{grantDenyDoc, grantDenyDoc, grantDenyDoc, grantDenyDoc},
	},
	{
		id: "role_member", name: "member", scopes: bootstrapMemberScopes,
		grants: [4]string{grantDenyDoc, grantDenyDoc, grantDenyDoc, grantDenyDoc},
	},
}

// runBootstrap implements `enclava bootstrap` — the operator-only first-login
// path (AUTH-01). It creates the first tenant and an admin user with a
// generated password printed exactly once; --password overrides generation
// for reproducible environments (e2e, walkthroughs) where the credential is
// public anyway. There is no HTTP signup: database access is the authorization.
func runBootstrap(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	tenantSlug := fs.String("tenant", "default", "slug for the first tenant")
	email := fs.String("email", "admin@local", "admin user email")
	name := fs.String("name", "Administrator", "admin display name")
	passwordFlag := fs.String("password", "", "fixed admin password (default: generated, printed once)")
	issueKey := fs.Bool("issue-key", false, "also issue a bootstrap API key (printed once)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) { // `-h`/`--help` is a clean exit (OPS-I01)
			return nil
		}
		return err
	}
	*tenantSlug = strings.TrimSpace(*tenantSlug)
	if *tenantSlug == "" {
		return errors.New("--tenant must be non-empty")
	}
	if strings.TrimSpace(*email) == "" {
		return errors.New("--email must be non-empty")
	}
	if *passwordFlag != "" {
		if err := auth.ValidatePassword(*passwordFlag); err != nil {
			return err
		}
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
	if *passwordFlag != "" {
		password = *passwordFlag
	}
	passwordHash, err := auth.HashPassword(password)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	userID := "user_" + randHex(12)
	now := time.Now().UTC()

	var apiKey string
	err = postgres.WithTx(ctx, db.SQL, func(tx *sql.Tx) error {
		var tenantID string
		err := tx.QueryRowContext(ctx,
			`INSERT INTO tenants (id, slug, display_name)
			 VALUES (gen_random_uuid(), $1, $2)
			 ON CONFLICT (slug) DO NOTHING RETURNING id::text`,
			*tenantSlug, *tenantSlug).Scan(&tenantID)
		if errors.Is(err, sql.ErrNoRows) {
			// A server started before bootstrap has already seeded the bare
			// tenant row; adopt it as long as nobody has been set up in it.
			err = tx.QueryRowContext(ctx,
				`SELECT t.id::text FROM tenants t
				 WHERE t.slug = $1
				   AND NOT EXISTS (SELECT 1 FROM users u WHERE u.tenant_id = t.id)
				   AND NOT EXISTS (SELECT 1 FROM roles r WHERE r.tenant_id = t.id)`,
				*tenantSlug).Scan(&tenantID)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("tenant %q already has users or roles (refuses an existing slug)", *tenantSlug)
			}
		}
		if err != nil {
			return fmt.Errorf("create tenant %q: %w", *tenantSlug, err)
		}
		for _, role := range bootstrapRoles {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO roles (tenant_id, id, name, scopes,
				 allowed_models, allowed_endpoints, allowed_assistants, allowed_extract_templates)
				 VALUES ($1::uuid, $2, $3, $4::jsonb, $5::jsonb, $6::jsonb, $7::jsonb, $8::jsonb)`,
				tenantID, role.id, role.name, roleScopesJSON(role.scopes),
				role.grants[0], role.grants[1], role.grants[2], role.grants[3]); err != nil {
				return fmt.Errorf("create %s role: %w", role.name, err)
			}
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
			tenantID, userID, bootstrapRoles[0].id); err != nil {
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
