// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package auth

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"tutor-mcp/models"

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

func TestGenerateJWTCarriesSingleTenantPrincipal(t *testing.T) {
	setTestSecret(t)
	principal := models.Principal{
		UserID:       "user-global",
		TenantID:     "tenant-a",
		MembershipID: "membership-a",
		LearnerID:    "learner-a",
		Roles:        []string{models.RoleLearner},
		TokenVersion: 7,
	}
	token, err := GenerateJWTForPrincipalAndScope(
		"https://issuer.example", MCPResource("https://issuer.example"),
		"oauth-client-a", principal, models.OAuthScopeLearnerRead,
	)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := VerifyJWTClaims(token, "https://issuer.example")
	if err != nil {
		t.Fatal(err)
	}
	got, err := claims.Principal()
	if err != nil {
		t.Fatal(err)
	}
	if got.UserID != principal.UserID || got.TenantID != principal.TenantID ||
		got.MembershipID != principal.MembershipID || got.TokenVersion != principal.TokenVersion {
		t.Fatalf("principal = %#v, want identity %#v", got, principal)
	}
	if claims.AuthorizedParty != "oauth-client-a" || claims.ID == "" {
		t.Fatalf("azp/jti missing from claims: %#v", claims)
	}
	if len(got.Scopes) != 1 || got.Scopes[0] != models.OAuthScopeLearnerRead {
		t.Fatalf("scopes = %v, want learner:read", got.Scopes)
	}
}

func TestVerifyJWTClaimsRejectsTenantlessAndOldTokens(t *testing.T) {
	setTestSecret(t)
	base := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "user-1",
			Issuer:    "https://issuer.example",
			Audience:  jwt.ClaimStrings{MCPResource("https://issuer.example")},
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ID:        "jti-1",
		},
		TenantID:        "tenant-a",
		MembershipID:    "membership-a",
		LearnerID:       "learner-a",
		Roles:           []string{models.RoleLearner},
		Scope:           models.OAuthScopeLearnerRead,
		AuthorizedParty: "client-a",
		TokenVersion:    1,
	}
	for name, mutate := range map[string]func(*Claims){
		"tenantless":  func(c *Claims) { c.TenantID = "" },
		"membership":  func(c *Claims) { c.MembershipID = "" },
		"old version": func(c *Claims) { c.TokenVersion = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := base
			mutate(&candidate)
			token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, candidate).SignedString(jwtSecret)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := VerifyJWTClaims(token, "https://issuer.example"); err == nil {
				t.Fatal("expected fail-closed claim rejection")
			}
		})
	}
}

func TestGenerateJWTUsesUniqueJTI(t *testing.T) {
	setTestSecret(t)
	one, err := GenerateJWT("https://issuer.example", "learner-1")
	if err != nil {
		t.Fatal(err)
	}
	two, err := GenerateJWT("https://issuer.example", "learner-1")
	if err != nil {
		t.Fatal(err)
	}
	oneClaims, err := VerifyJWTClaims(one, "https://issuer.example")
	if err != nil {
		t.Fatal(err)
	}
	twoClaims, err := VerifyJWTClaims(two, "https://issuer.example")
	if err != nil {
		t.Fatal(err)
	}
	if oneClaims.ID == twoClaims.ID {
		t.Fatal("separately issued access tokens must not share a jti")
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
	}, jwt.WithAudience(MCPResource("https://issuer.example")), jwt.WithIssuer("https://issuer.example"))
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

func TestVerifyJWTClaims_RejectsMissingExpiration(t *testing.T) {
	setTestSecret(t)
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:  "learner-1",
			Issuer:   "https://issuer.example",
			Audience: jwt.ClaimStrings{MCPResource("https://issuer.example")},
			IssuedAt: jwt.NewNumericDate(time.Now()),
		},
		Scope: "learner",
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(jwtSecret)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := VerifyJWTClaims(token, "https://issuer.example"); err == nil {
		t.Fatal("expected error for missing expiration")
	}
}

func TestVerifyJWTClaims_RejectsMissingSubject(t *testing.T) {
	setTestSecret(t)
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "https://issuer.example",
			Audience:  jwt.ClaimStrings{MCPResource("https://issuer.example")},
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		Scope: "learner",
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(jwtSecret)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := VerifyJWTClaims(token, "https://issuer.example"); err == nil {
		t.Fatal("expected error for missing subject")
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

func TestVerifyJWTRejectsLegacyLogicalAudience(t *testing.T) {
	setTestSecret(t)
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "learner-1",
			Issuer:    "https://issuer.example",
			Audience:  jwt.ClaimStrings{"tutor-mcp/mcp"},
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		Scope: "learner",
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(jwtSecret)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := VerifyJWT(token, "https://issuer.example"); err == nil {
		t.Fatal("legacy logical audience must be rejected")
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

func TestEd25519KeyRotationKeepsPreviousTokensValid(t *testing.T) {
	savedSecret := append([]byte(nil), jwtSecret...)
	signingKeys.RLock()
	savedKID := signingKeys.activeKID
	savedPrivate := append(ed25519.PrivateKey(nil), signingKeys.private...)
	savedPublic := make(map[string]ed25519.PublicKey, len(signingKeys.public))
	for kid, key := range signingKeys.public {
		savedPublic[kid] = append(ed25519.PublicKey(nil), key...)
	}
	signingKeys.RUnlock()
	t.Cleanup(func() {
		jwtSecret = savedSecret
		signingKeys.Lock()
		signingKeys.activeKID, signingKeys.private, signingKeys.public = savedKID, savedPrivate, savedPublic
		signingKeys.Unlock()
	})
	seedOne := make([]byte, ed25519.SeedSize)
	seedTwo := make([]byte, ed25519.SeedSize)
	for index := range seedOne {
		seedOne[index] = byte(index + 1)
		seedTwo[index] = byte(index + 33)
	}
	privateOne := ed25519.NewKeyFromSeed(seedOne)
	privateTwo := ed25519.NewKeyFromSeed(seedTwo)
	config := func(active string, includeOne bool) string {
		keys := []ed25519KeyConfig{{
			KID: "key-2", PublicKey: base64.StdEncoding.EncodeToString(privateTwo.Public().(ed25519.PublicKey)),
			Active: active == "key-2",
		}}
		if active == "key-2" {
			keys[0].PrivateKey = base64.StdEncoding.EncodeToString(seedTwo)
		}
		if includeOne {
			one := ed25519KeyConfig{
				KID: "key-1", PublicKey: base64.StdEncoding.EncodeToString(privateOne.Public().(ed25519.PublicKey)),
				Active: active == "key-1",
			}
			if one.Active {
				one.PrivateKey = base64.StdEncoding.EncodeToString(seedOne)
			}
			keys = append(keys, one)
		}
		raw, _ := json.Marshal(keys)
		return string(raw)
	}

	t.Setenv("JWT_ED25519_KEYS", config("key-1", true))
	if err := LoadJWTSecret(); err != nil {
		t.Fatal(err)
	}
	oldToken, err := GenerateJWT("https://issuer.example", "learner-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("JWT_ED25519_KEYS", config("key-2", true))
	if err := LoadJWTSecret(); err != nil {
		t.Fatal(err)
	}
	newToken, err := GenerateJWT("https://issuer.example", "learner-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyJWTClaims(oldToken, "https://issuer.example"); err != nil {
		t.Fatalf("previous key token failed during overlap: %v", err)
	}
	if _, err := VerifyJWTClaims(newToken, "https://issuer.example"); err != nil {
		t.Fatalf("active key token failed: %v", err)
	}

	recorder := httptest.NewRecorder()
	HandleJWKS(recorder, httptest.NewRequest("GET", "/.well-known/jwks.json", nil))
	if body := recorder.Body.String(); strings.Contains(body, base64.StdEncoding.EncodeToString(seedTwo)) ||
		!strings.Contains(body, `"kid":"key-1"`) || !strings.Contains(body, `"kid":"key-2"`) {
		t.Fatalf("JWKS private/public rotation material invalid: %s", body)
	}

	t.Setenv("JWT_ED25519_KEYS", config("key-2", false))
	if err := LoadJWTSecret(); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyJWTClaims(oldToken, "https://issuer.example"); err == nil {
		t.Fatal("retired verification key still accepted")
	}
}

func TestMain(m *testing.M) {
	// Ensure tests don't accidentally inherit a JWT_SECRET from the host env.
	os.Unsetenv("JWT_SECRET")
	os.Unsetenv("JWT_ED25519_KEYS")
	os.Exit(m.Run())
}
