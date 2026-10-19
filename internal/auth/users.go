package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
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

// VerifyPassword reports whether pw matches the stored bcrypt hash.
func VerifyPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// CreateUser creates a local account with a bcrypt-hashed password. Account
// creation requires an administrator actor — there is no public signup.
func (s *Service) CreateUser(ctx context.Context, admin Principal, tenantID, email, displayName, password string) (User, error) {
	if !HasScope(admin, ScopePlatformAdmin) {
		return User{}, errors.New("platform.admin scope required to create users")
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
		PasswordHash:  hash,
		PasswordSetAt: &now,
	}
	if err := s.repo.CreateUser(ctx, tenantID, user); err != nil {
		return User{}, err
	}
	return user, nil
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return hex.EncodeToString(buf)
}
