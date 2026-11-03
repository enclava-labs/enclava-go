package auth_test

import (
	"strings"
	"testing"

	"enclava-go/internal/auth"
)

// TestValidatePassword covers the shared server-side policy (AUTH-I02): every
// password-setting path rejects under-8, whitespace-only and oversized inputs.
func TestValidatePassword(t *testing.T) {
	rejected := map[string]string{
		"empty":           "",
		"one char":        "a",
		"seven chars":     "abcdefg",
		"eight spaces":    "        ",
		"whitespace only": " \t\n \t",
		"over 1024 bytes": strings.Repeat("x", 1025),
	}
	for name, pw := range rejected {
		if err := auth.ValidatePassword(pw); err == nil {
			t.Fatalf("%s must be rejected", name)
		}
	}
	for name, pw := range map[string]string{
		"eight chars":    "abcdefgh",
		"spaces inside":  " pass word ",
		"exactly 1024":   strings.Repeat("x", 1024),
		"e2e admin pass": "E2e-admin-1234!",
	} {
		if err := auth.ValidatePassword(pw); err != nil {
			t.Fatalf("%s rejected: %v", name, err)
		}
	}
}
