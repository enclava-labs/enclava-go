package auth

import (
	"context"
	"errors"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

type missingPasswordTimingRepo struct {
	IdentityRepository
}

func (missingPasswordTimingRepo) GetUserByEmail(context.Context, string, string) (User, error) {
	return User{}, errors.New("not found")
}

func TestUnknownEmailUsesDummyBcryptComparison(t *testing.T) {
	original := verifyPassword
	defer func() { verifyPassword = original }()
	calls := 0
	verifyPassword = func(hash, password string) bool {
		calls++
		if hash != dummyPasswordHash || password != "attempt" {
			t.Fatalf("unexpected comparison input: hash %q", hash)
		}
		if _, err := bcrypt.Cost([]byte(hash)); err != nil {
			t.Fatalf("dummy hash is not bcrypt: %v", err)
		}
		return original(hash, password)
	}
	svc := NewService(missingPasswordTimingRepo{}, "")
	if _, err := svc.AuthenticatePassword(context.Background(), "tenant", "missing@example.com", "attempt"); err == nil {
		t.Fatal("unknown account authenticated")
	}
	if calls != 1 {
		t.Fatalf("bcrypt comparisons = %d, want 1", calls)
	}
}
