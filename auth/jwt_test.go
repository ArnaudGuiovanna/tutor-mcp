// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package auth

import (
	"encoding/base64"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func setTestSecret(t *testing.T) {
	t.Helper()
	raw := []byte("test-secret-32-bytes-for-hs256-ok")
	t.Setenv("JWT_SECRET", base64.StdEncoding.EncodeToString(raw))
	if err := LoadJWTSecret(); err != nil {
		t.Fatalf("load jwt secret: %v", err)
	}
}

func TestVerifyJWT_AcceptsValidIssuerAndAudience(t *testing.T) {
	setTestSecret(t)
	tok, err := GenerateJWT("https://issuer.example", "learner-1")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	sub, err := VerifyJWT(tok, "https://issuer.example")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if sub != "learner-1" {
		t.Fatalf("subject = %q, want learner-1", sub)
	}
}

func TestGenerateJWTUsesShortAccessTokenTTL(t *testing.T) {
	setTestSecret(t)
	before := time.Now()
	tok, err := GenerateJWT("https://issuer.example", "learner-1")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := jwt.ParseWithClaims(tok, &Claims{}, func(token *jwt.Token) (any, error) {
		return jwtSecret, nil
	}, jwt.WithAudience(JWTAudience), jwt.WithIssuer("https://issuer.example"))
	if err != nil {
		t.Fatal(err)
	}
	claims, ok := parsed.Claims.(*Claims)
	if !ok || claims.ExpiresAt == nil || claims.IssuedAt == nil {
		t.Fatalf("missing temporal claims: %#v", parsed.Claims)
	}
	wantExpiry := before.Add(AccessTokenTTL)
	if claims.ExpiresAt.Time.Before(wantExpiry.Add(-time.Second)) || claims.ExpiresAt.Time.After(wantExpiry.Add(2*time.Second)) {
		t.Fatalf("expiry=%v, want approximately %v", claims.ExpiresAt.Time, wantExpiry)
	}
}

func TestVerifyJWT_RejectsWrongIssuer(t *testing.T) {
	setTestSecret(t)
	tok, _ := GenerateJWT("https://issuer.example", "learner-1")
	if _, err := VerifyJWT(tok, "https://other.example"); err == nil {
		t.Fatal("expected error for wrong issuer")
	}
}

func TestVerifyJWT_RejectsMissingAudience(t *testing.T) {
	setTestSecret(t)
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "learner-1",
			Issuer:    "https://issuer.example",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		Scope: "learner",
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(jwtSecret)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := VerifyJWT(tok, "https://issuer.example"); err == nil {
		t.Fatal("expected error for missing audience")
	}
}

func TestVerifyJWT_RejectsAlgNone(t *testing.T) {
	// alg=none token, hand-crafted: header.payload.
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"x","iss":"https://issuer.example","aud":"tutor-mcp/mcp","exp":99999999999}`))
	tok := header + "." + payload + "."

	setTestSecret(t)
	if _, err := VerifyJWT(tok, "https://issuer.example"); err == nil {
		t.Fatal("alg=none must be rejected")
	}
}

func TestLoadJWTSecret_PlainStringErrorMentionsOpenssl(t *testing.T) {
	// A plain (non-base64) value is the exact failure mode users hit when
	// following the README literally — see issue #22. The error message must
	// be actionable and point them at `openssl rand -base64 32`.
	t.Setenv("JWT_SECRET", "hello")
	err := LoadJWTSecret()
	if err == nil {
		t.Fatal("expected error for plain (non-base64) JWT_SECRET")
	}
	if !strings.Contains(err.Error(), "openssl rand -base64 32") {
		t.Fatalf("error message %q must mention `openssl rand -base64 32` to be actionable", err.Error())
	}
}

func TestLoadJWTSecret_MissingReturnsError(t *testing.T) {
	t.Setenv("JWT_SECRET", "")
	if err := LoadJWTSecret(); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("missing JWT_SECRET error = %v, want actionable required error", err)
	}
}

func TestLoadJWTSecret_RejectsShortDecodedSecret(t *testing.T) {
	// A 16-byte decoded secret is too weak for HS256 and must be rejected,
	// even though it is valid base64 — see finding #6.
	saved := jwtSecret
	defer func() { jwtSecret = saved }()

	raw := make([]byte, 16)
	t.Setenv("JWT_SECRET", base64.StdEncoding.EncodeToString(raw))
	err := LoadJWTSecret()
	if err == nil {
		t.Fatal("expected error for 16-byte decoded JWT_SECRET")
	}
	if !strings.Contains(err.Error(), "at least 32 bytes") {
		t.Fatalf("error message %q must mention the 32-byte minimum", err.Error())
	}
}

func TestLoadJWTSecret_AcceptsStrongDecodedSecret(t *testing.T) {
	// A 32-byte decoded secret meets the HS256 strength floor and must load.
	saved := jwtSecret
	defer func() { jwtSecret = saved }()

	raw := make([]byte, 32)
	t.Setenv("JWT_SECRET", base64.StdEncoding.EncodeToString(raw))
	if err := LoadJWTSecret(); err != nil {
		t.Fatalf("expected 32-byte secret to be accepted, got: %v", err)
	}
}

func TestMain(m *testing.M) {
	// Ensure tests don't accidentally inherit a JWT_SECRET from the host env.
	os.Unsetenv("JWT_SECRET")
	os.Exit(m.Run())
}
