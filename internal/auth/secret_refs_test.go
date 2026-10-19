package auth_test

// Binding-boundary regressions for auth.SecretResolver (AUTH-05/INF-01/INF-02).
// Sentinel environment values are temporary test values — never real secret
// files — and assertions observe behavior only: which value comes back or
// that ErrSecretDenied does, never source text or mocked forwarding.

import (
	"errors"
	"strings"
	"testing"

	"enclava-go/internal/auth"
)

const (
	tenantA = "tenant-a-0000-0000-0000-0000000000aa"
	tenantB = "tenant-b-0000-0000-0000-0000000000bb"

	envInferenceA = "ENCLAVA_TEST_CRED_INFER_A"
	envConnectorB = "ENCLAVA_TEST_CRED_CONN_B"
	envOther      = "ENCLAVA_TEST_CRED_OTHER"
)

func testBindings() []auth.SecretBinding {
	return []auth.SecretBinding{
		{
			RefID:    "ep-privatemode",
			TenantID: tenantA,
			Purpose:  auth.SecretPurposeInference,
			EnvName:  envInferenceA,
			Origins:  []string{"https://api.privatemode.example:8443"},
		},
		{
			RefID:    "conn-github",
			TenantID: tenantB,
			Purpose:  auth.SecretPurposeConnector,
			EnvName:  envConnectorB,
			// Source adapters may hold more than one exact approved origin —
			// server-controlled, never an arbitrary redirect allowance.
			Origins: []string{
				"https://api.github.com",
				"https://uploads.github.com",
				"https://github.com",
			},
		},
	}
}

func newTestResolver(t *testing.T) *auth.SecretResolver {
	t.Helper()
	t.Setenv(envInferenceA, "sentinel-alpha-secret")
	t.Setenv(envConnectorB, "sentinel-bravo-token")
	r, err := auth.NewSecretResolver(testBindings())
	if err != nil {
		t.Fatalf("NewSecretResolver returned error: %v", err)
	}
	return r
}

func assertDeniedSanitized(t *testing.T, err error, sentinels ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected denied resolution, got nil error")
	}
	if !errors.Is(err, auth.ErrSecretDenied) {
		t.Fatalf("expected ErrSecretDenied, got %v", err)
	}
	for _, s := range sentinels {
		if strings.Contains(err.Error(), s) {
			t.Fatalf("error leaked secret material: %v", err)
		}
	}
	if strings.Contains(err.Error(), envInferenceA) || strings.Contains(err.Error(), envConnectorB) {
		t.Fatalf("error leaked the environment source name: %v", err)
	}
}

func TestSecretBindings(t *testing.T) {
	resolver := newTestResolver(t)
	originA := "https://api.privatemode.example:8443"
	originB := "https://api.github.com"

	t.Run("valid resolution returns the bound value", func(t *testing.T) {
		got, err := resolver.Resolve(tenantA, auth.SecretPurposeInference, originA, "ep-privatemode")
		if err != nil {
			t.Fatalf("Resolve returned error: %v", err)
		}
		if got != "sentinel-alpha-secret" {
			t.Fatalf("expected sentinel alpha, got %q", got)
		}
	})

	t.Run("origin canonicalization accepts equivalent forms", func(t *testing.T) {
		for _, o := range []string{
			"https://api.privatemode.example:8443",
			"HTTPS://API.PRIVATEMODE.EXAMPLE:8443",
			"https://API.PrivateMode.Example:8443/",
		} {
			got, err := resolver.Resolve(tenantA, auth.SecretPurposeInference, o, "ep-privatemode")
			if err != nil {
				t.Fatalf("Resolve(%q) returned error: %v", o, err)
			}
			if got != "sentinel-alpha-secret" {
				t.Fatalf("Resolve(%q) returned wrong value", o)
			}
		}
	})

	t.Run("multi-origin connector binding resolves each approved origin", func(t *testing.T) {
		for _, o := range []string{"https://api.github.com", "https://uploads.github.com", "https://github.com"} {
			got, err := resolver.Resolve(tenantB, auth.SecretPurposeConnector, o, "conn-github")
			if err != nil {
				t.Fatalf("Resolve(%q) returned error: %v", o, err)
			}
			if got != "sentinel-bravo-token" {
				t.Fatalf("Resolve(%q) returned wrong value", o)
			}
		}
	})

	t.Run("cross tenant access denied", func(t *testing.T) {
		// tenant B guessing tenant A's ref id must not get the alpha sentinel.
		_, err := resolver.Resolve(tenantB, auth.SecretPurposeInference, originA, "ep-privatemode")
		assertDeniedSanitized(t, err, "sentinel-alpha-secret", "sentinel-bravo-token")
		// tenant A guessing tenant B's connector ref for a github origin.
		_, err = resolver.Resolve(tenantA, auth.SecretPurposeConnector, originB, "conn-github")
		assertDeniedSanitized(t, err, "sentinel-alpha-secret", "sentinel-bravo-token")
	})

	t.Run("unknown reference denied", func(t *testing.T) {
		_, err := resolver.Resolve(tenantA, auth.SecretPurposeInference, originA, "ep-other")
		assertDeniedSanitized(t, err, "sentinel-alpha-secret")
	})

	t.Run("wrong purpose denied even for same tenant and origin", func(t *testing.T) {
		_, err := resolver.Resolve(tenantA, auth.SecretPurposeConnector, originA, "ep-privatemode")
		assertDeniedSanitized(t, err, "sentinel-alpha-secret")
		// The connector binding under the inference purpose.
		_, err = resolver.Resolve(tenantB, auth.SecretPurposeInference, originB, "conn-github")
		assertDeniedSanitized(t, err, "sentinel-bravo-token")
	})

	t.Run("changed scheme or port denied", func(t *testing.T) {
		for _, o := range []string{
			"http://api.privatemode.example:8443",  // downgraded scheme
			"https://api.privatemode.example",      // dropped port → default 443
			"https://api.privatemode.example:443",  // different port
			"https://api.privatemode.example:9443", // shifted port
			"https://api.github.com:8443",          // connector value on shifted port
		} {
			_, err := resolver.Resolve(tenantA, auth.SecretPurposeInference, o, "ep-privatemode")
			if err == nil {
				_, err = resolver.Resolve(tenantB, auth.SecretPurposeConnector, o, "conn-github")
			}
			if err == nil {
				t.Fatalf("expected origin %q to be denied for both bindings", o)
			}
			assertDeniedSanitized(t, err, "sentinel-alpha-secret", "sentinel-bravo-token")
		}
	})

	t.Run("redirect destination and credential-bearing origins denied", func(t *testing.T) {
		for _, o := range []string{
			"https://evil.example",                         // unapproved host
			"https://api.privatemode.example.evil.example", // suffix host
			"https://api.github.com.evil.example",          // redirect target lookalike
			"https://user:pw@api.privatemode.example:8443", // embedded credentials
			"https://api.privatemode.example:8443/../path", // path smuggling
			"https://api.privatemode.example:8443?q=x",     // query
			"//api.privatemode.example:8443",               // scheme-relative
			"api.privatemode.example:8443",                 // no scheme
			"ftp://api.privatemode.example:8443",           // non-http scheme
		} {
			_, err := resolver.Resolve(tenantA, auth.SecretPurposeInference, o, "ep-privatemode")
			assertDeniedSanitized(t, err, "sentinel-alpha-secret", "sentinel-bravo-token")
		}
	})

	t.Run("missing value denied without leaking the source name", func(t *testing.T) {
		t.Setenv(envOther, "")
		r, err := auth.NewSecretResolver([]auth.SecretBinding{{
			RefID: "ep-missing", TenantID: tenantA,
			Purpose: auth.SecretPurposeInference,
			EnvName: envOther, Origins: []string{"https://api.privatemode.example:8443"},
		}})
		if err != nil {
			t.Fatalf("NewSecretResolver returned error: %v", err)
		}
		_, err = r.Resolve(tenantA, auth.SecretPurposeInference, originA, "ep-missing")
		assertDeniedSanitized(t, err, "sentinel-alpha-secret")
	})

	t.Run("different sentinel proves wrong binding never serves", func(t *testing.T) {
		// If cross-binding leakage existed this would return the bravo token.
		got, err := resolver.Resolve(tenantB, auth.SecretPurposeConnector, originB, "conn-github")
		if err != nil {
			t.Fatalf("Resolve returned error: %v", err)
		}
		if got == "sentinel-alpha-secret" {
			t.Fatalf("connector binding returned the inference sentinel")
		}
	})

	t.Run("unauthenticated local endpoint resolves to no credential", func(t *testing.T) {
		// An explicitly credential-less endpoint passes an empty ref and must
		// never pick up an unrelated default/SDK environment key.
		got, err := resolver.Resolve(tenantA, auth.SecretPurposeInference, "http://localhost:11434", "")
		if err != nil {
			t.Fatalf("expected empty ref to resolve to no credential, got %v", err)
		}
		if got != "" {
			t.Fatalf("expected no credential for empty ref, got %q", got)
		}
	})

	t.Run("nil resolver denies everything", func(t *testing.T) {
		var nilResolver *auth.SecretResolver
		_, err := nilResolver.Resolve(tenantA, auth.SecretPurposeInference, originA, "ep-privatemode")
		assertDeniedSanitized(t, err, "sentinel-alpha-secret")
	})

	t.Run("metadata never exposes values or sources", func(t *testing.T) {
		for _, m := range resolver.Metadata(tenantA) {
			if m.RefID == "" || m.TenantID != tenantA {
				t.Fatalf("bad metadata row: %+v", m)
			}
		}
		if len(resolver.Metadata(tenantA)) != 1 || len(resolver.Metadata(tenantB)) != 1 {
			t.Fatalf("expected one metadata row per tenant")
		}
		if len(resolver.Metadata("tenant-unknown")) != 0 {
			t.Fatalf("expected no metadata for an unknown tenant")
		}
	})
}

func TestSecretBindingValidation(t *testing.T) {
	valid := testBindings()[0]

	bad := func(mutate func(b *auth.SecretBinding)) auth.SecretBinding {
		b := valid
		mutate(&b)
		return b
	}
	cases := map[string]auth.SecretBinding{
		"empty ref":         bad(func(b *auth.SecretBinding) { b.RefID = "" }),
		"empty tenant":      bad(func(b *auth.SecretBinding) { b.TenantID = " " }),
		"unknown purpose":   bad(func(b *auth.SecretBinding) { b.Purpose = "shell" }),
		"no origins":        bad(func(b *auth.SecretBinding) { b.Origins = nil }),
		"bad env name":      bad(func(b *auth.SecretBinding) { b.EnvName = "9BAD; rm" }),
		"malformed origin":  bad(func(b *auth.SecretBinding) { b.Origins = []string{"https://x.example/path"} }),
		"duplicate origins": bad(func(b *auth.SecretBinding) { b.Origins = []string{"HTTPS://X.EXAMPLE", "https://x.example"} }),
	}
	for name, b := range cases {
		if _, err := auth.NewSecretResolver([]auth.SecretBinding{b}); err == nil {
			t.Fatalf("expected %s binding to be rejected", name)
		}
	}

	dup := []auth.SecretBinding{valid, bad(func(b *auth.SecretBinding) { b.EnvName = envConnectorB })}
	if _, err := auth.NewSecretResolver(dup); err == nil {
		t.Fatalf("expected duplicate binding to be rejected")
	}
}
