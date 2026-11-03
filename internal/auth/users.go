package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// HashPassword stores a local-account password as a bcrypt hash — the only
// password representation anywhere in identity storage (AUTH-01).
func HashPassword(pw string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

// ValidatePassword is the single server-side password policy (AUTH-I02):
// every path that sets a password — self-service change, admin create/reset
// and bootstrap --password — runs it before hashing. The byte cap bounds
// bcrypt's hashing cost.
func ValidatePassword(pw string) error {
	switch {
	case len(pw) < 8:
		return errors.New("password must be at least 8 characters")
	case strings.TrimSpace(pw) == "":
		return errors.New("password must not be all whitespace")
	case len(pw) > 1024:
		return errors.New("password must be at most 1024 bytes")
	}
	return nil
}

// VerifyPassword reports whether pw matches the stored bcrypt hash.
func VerifyPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// CreateUser creates a local account with a bcrypt-hashed password and its
// initial role memberships atomically — an unknown role id refuses the whole
// write, so a retry never meets a half-created account (ADM-I01). Account
// creation requires an administrator actor — there is no public signup.
func (s *Service) CreateUser(ctx context.Context, admin Principal, tenantID, email, displayName, password, team string, roleIDs []string) (User, error) {
	if !HasScope(admin, ScopePlatformAdmin) {
		return User{}, errors.New("platform.admin scope required to create users")
	}
	if err := ValidatePassword(password); err != nil {
		return User{}, err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return User{}, err
	}
	now := time.Now().UTC()
	user := User{
		ID:            "user_" + randomHex(12),
		TenantID:      tenantID,
		Email:         email,
		DisplayName:   displayName,
		Status:        "active",
		Team:          team,
		PasswordHash:  hash,
		PasswordSetAt: &now,
	}
	return s.repo.CreateUserWithRoles(ctx, tenantID, user, roleIDs)
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return hex.EncodeToString(buf)
}
