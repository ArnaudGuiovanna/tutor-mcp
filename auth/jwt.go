// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"tutor-mcp/models"

	"github.com/golang-jwt/jwt/v5"
)

var jwtSecret []byte

var signingKeys struct {
	sync.RWMutex
	activeKID string
	private   ed25519.PrivateKey
	public    map[string]ed25519.PublicKey
}

type ed25519KeyConfig struct {
	KID        string `json:"kid"`
	PrivateKey string `json:"private_key,omitempty"`
	PublicKey  string `json:"public_key"`
	Active     bool   `json:"active,omitempty"`
}

// AccessTokenTTL bounds exposure of a leaked bearer. Long-lived access is
// provided through rotating refresh-token families, not a day-long stateless
// credential that the server cannot revoke retroactively.
const AccessTokenTTL = 30 * time.Minute

func LoadJWTSecret() error {
	if raw := strings.TrimSpace(os.Getenv("JWT_ED25519_KEYS")); raw != "" {
		return loadEd25519Keys(raw)
	}
	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		return fmt.Errorf("JWT_SECRET env var required")
	}
	decoded, err := base64.StdEncoding.DecodeString(secret)
	if err != nil {
		return fmt.Errorf("JWT_SECRET must be base64-encoded (try: openssl rand -base64 32): %w", err)
	}
	if len(decoded) < 32 {
		return fmt.Errorf("JWT_SECRET must decode to at least 32 bytes (256 bits)")
	}
	jwtSecret = decoded
	signingKeys.Lock()
	signingKeys.activeKID = ""
	signingKeys.private = nil
	signingKeys.public = nil
	signingKeys.Unlock()
	return nil
}

func loadEd25519Keys(raw string) error {
	var configs []ed25519KeyConfig
	if err := json.Unmarshal([]byte(raw), &configs); err != nil {
		return fmt.Errorf("JWT_ED25519_KEYS must be a JSON array: %w", err)
	}
	publicKeys := make(map[string]ed25519.PublicKey, len(configs))
	var activeKID string
	var activePrivate ed25519.PrivateKey
	for _, config := range configs {
		if strings.TrimSpace(config.KID) == "" || config.KID != strings.TrimSpace(config.KID) {
			return fmt.Errorf("JWT_ED25519_KEYS kid is required and must be canonical")
		}
		if _, duplicate := publicKeys[config.KID]; duplicate {
			return fmt.Errorf("JWT_ED25519_KEYS duplicate kid %q", config.KID)
		}
		publicRaw, err := base64.StdEncoding.DecodeString(config.PublicKey)
		if err != nil || len(publicRaw) != ed25519.PublicKeySize {
			return fmt.Errorf("JWT_ED25519_KEYS public key %q must be base64 Ed25519 bytes", config.KID)
		}
		publicKeys[config.KID] = ed25519.PublicKey(append([]byte(nil), publicRaw...))
		if !config.Active {
			continue
		}
		if activeKID != "" {
			return fmt.Errorf("JWT_ED25519_KEYS must contain exactly one active key")
		}
		privateRaw, err := base64.StdEncoding.DecodeString(config.PrivateKey)
		if err != nil || (len(privateRaw) != ed25519.SeedSize && len(privateRaw) != ed25519.PrivateKeySize) {
			return fmt.Errorf("JWT_ED25519_KEYS active private key %q must be a base64 Ed25519 seed or private key", config.KID)
		}
		if len(privateRaw) == ed25519.SeedSize {
			activePrivate = ed25519.NewKeyFromSeed(privateRaw)
		} else {
			activePrivate = ed25519.PrivateKey(append([]byte(nil), privateRaw...))
		}
		if !activePrivate.Public().(ed25519.PublicKey).Equal(publicKeys[config.KID]) {
			return fmt.Errorf("JWT_ED25519_KEYS active public/private key mismatch for %q", config.KID)
		}
		activeKID = config.KID
	}
	if activeKID == "" || len(activePrivate) == 0 {
		return fmt.Errorf("JWT_ED25519_KEYS must contain exactly one active private key")
	}
	signingKeys.Lock()
	signingKeys.activeKID = activeKID
	signingKeys.private = activePrivate
	signingKeys.public = publicKeys
	signingKeys.Unlock()
	jwtSecret = nil
	return nil
}

// MCPResource returns the canonical protected-resource identifier advertised
// by OAuth metadata. Access tokens are deliberately tied to this exact URI so
// a token minted for another deployment or endpoint cannot be replayed here.
func MCPResource(baseURL string) string {
	return strings.TrimRight(baseURL, "/") + "/mcp"
}

type Claims struct {
	jwt.RegisteredClaims
	TenantID        string   `json:"tid"`
	MembershipID    string   `json:"membership_id"`
	LearnerID       string   `json:"learner_id,omitempty"`
	Roles           []string `json:"roles"`
	Scope           string   `json:"scope"`
	AuthorizedParty string   `json:"azp"`
	TokenVersion    int64    `json:"token_version"`
}

func GenerateJWT(issuer, learnerID string) (string, error) {
	return GenerateJWTForResource(issuer, MCPResource(issuer), learnerID)
}

// GenerateJWTForResource signs an access token for an explicitly authorized
// RFC 8707 resource. Only this server's canonical /mcp URI is issuable.
func GenerateJWTForResource(issuer, resource, learnerID string) (string, error) {
	return GenerateJWTForResourceAndScope(issuer, resource, learnerID, models.OAuthScopeLearner)
}

// GenerateJWTForResourceAndScope signs an access token carrying the exact
// canonical grant persisted through the authorization-code lifecycle.
func GenerateJWTForResourceAndScope(issuer, resource, learnerID, scope string) (string, error) {
	principal := models.LegacyPrincipal(learnerID)
	return GenerateJWTForPrincipalAndScope(issuer, resource, "legacy-client", principal, scope)
}

// GenerateJWTForPrincipalAndScope mints an access token for exactly one tenant
// membership and one OAuth client. The tenant is never accepted from an HTTP
// header at the resource-server boundary.
func GenerateJWTForPrincipalAndScope(issuer, resource, authorizedParty string, principal models.Principal, scope string) (string, error) {
	if resource == "" || resource != MCPResource(issuer) {
		return "", fmt.Errorf("resource must equal the canonical MCP resource")
	}
	if strings.TrimSpace(authorizedParty) == "" || authorizedParty != strings.TrimSpace(authorizedParty) {
		return "", fmt.Errorf("authorized party is required and must be canonical")
	}
	canonicalScope, err := models.CanonicalOAuthScope(scope)
	if err != nil {
		return "", fmt.Errorf("invalid OAuth scope: %w", err)
	}
	principal.Scopes = strings.Fields(canonicalScope)
	if err := principal.Validate(); err != nil {
		return "", fmt.Errorf("invalid principal: %w", err)
	}
	jti, err := newJWTID()
	if err != nil {
		return "", fmt.Errorf("generate token identifier: %w", err)
	}
	now := time.Now()
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   principal.UserID,
			Issuer:    issuer,
			Audience:  jwt.ClaimStrings{resource},
			ExpiresAt: jwt.NewNumericDate(now.Add(AccessTokenTTL)),
			IssuedAt:  jwt.NewNumericDate(now),
			ID:        jti,
		},
		TenantID:        principal.TenantID,
		MembershipID:    principal.MembershipID,
		LearnerID:       principal.LearnerID,
		Roles:           append([]string(nil), principal.Roles...),
		Scope:           canonicalScope,
		AuthorizedParty: authorizedParty,
		TokenVersion:    principal.TokenVersion,
	}
	return signAccessToken(claims)
}

func signAccessToken(claims Claims) (string, error) {
	signingKeys.RLock()
	kid := signingKeys.activeKID
	privateKey := append(ed25519.PrivateKey(nil), signingKeys.private...)
	signingKeys.RUnlock()
	if kid != "" && len(privateKey) != 0 {
		token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
		token.Header["kid"] = kid
		return token.SignedString(privateKey)
	}
	if len(jwtSecret) == 0 {
		return "", fmt.Errorf("JWT signing key is not loaded")
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(jwtSecret)
}

// VerifyJWTClaims validates an access token and returns the claims needed by
// HTTP authentication middleware. In particular, callers must use the actual
// expiry carried by the token rather than manufacturing a new lifetime after
// validation.
func VerifyJWTClaims(tokenString, expectedIssuer string) (*Claims, error) {
	return VerifyJWTClaimsForResource(tokenString, expectedIssuer, MCPResource(expectedIssuer))
}

// VerifyJWTClaimsForResource validates issuer and the exact protected-resource
// audience. A legacy logical audience such as "tutor-mcp/mcp" is rejected.
func VerifyJWTClaimsForResource(tokenString, expectedIssuer, expectedResource string) (*Claims, error) {
	if expectedResource == "" || expectedResource != MCPResource(expectedIssuer) {
		return nil, fmt.Errorf("invalid expected resource")
	}
	token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(token *jwt.Token) (interface{}, error) {
		switch token.Method.Alg() {
		case jwt.SigningMethodEdDSA.Alg():
			kid, _ := token.Header["kid"].(string)
			signingKeys.RLock()
			publicKey := append(ed25519.PublicKey(nil), signingKeys.public[kid]...)
			signingKeys.RUnlock()
			if kid == "" || len(publicKey) != ed25519.PublicKeySize {
				return nil, fmt.Errorf("invalid token")
			}
			return publicKey, nil
		case jwt.SigningMethodHS256.Alg():
			if len(jwtSecret) == 0 {
				return nil, fmt.Errorf("invalid token")
			}
			return jwtSecret, nil
		default:
			return nil, fmt.Errorf("invalid token")
		}
	},
		jwt.WithIssuer(expectedIssuer),
		jwt.WithAudience(expectedResource),
		jwt.WithValidMethods([]string{"HS256", "EdDSA"}),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return nil, fmt.Errorf("invalid token: %w", err)
	}
	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid claims")
	}
	if claims.Subject == "" {
		return nil, fmt.Errorf("invalid claims: subject is required")
	}
	if claims.ExpiresAt == nil {
		return nil, fmt.Errorf("invalid claims: expiration is required")
	}
	canonicalScope, err := models.CanonicalOAuthScope(claims.Scope)
	if err != nil || canonicalScope != claims.Scope {
		return nil, fmt.Errorf("invalid claims: unsupported or non-canonical scope")
	}
	if claims.ID == "" {
		return nil, fmt.Errorf("invalid claims: jti is required")
	}
	if strings.TrimSpace(claims.AuthorizedParty) == "" || claims.AuthorizedParty != strings.TrimSpace(claims.AuthorizedParty) {
		return nil, fmt.Errorf("invalid claims: authorized party is required")
	}
	if _, err := claims.Principal(); err != nil {
		return nil, fmt.Errorf("invalid claims: %w", err)
	}
	return claims, nil
}

// Principal converts already verified claims into the typed business identity.
func (c *Claims) Principal() (models.Principal, error) {
	p := models.Principal{
		UserID:       c.Subject,
		TenantID:     c.TenantID,
		MembershipID: c.MembershipID,
		LearnerID:    c.LearnerID,
		Roles:        append([]string(nil), c.Roles...),
		Scopes:       strings.Fields(c.Scope),
		TokenVersion: c.TokenVersion,
	}
	if err := p.Validate(); err != nil {
		return models.Principal{}, err
	}
	return p, nil
}

func newJWTID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

// VerifyJWT is the compatibility wrapper used by callers that only need the
// authenticated learner identifier.
func VerifyJWT(tokenString, expectedIssuer string) (string, error) {
	claims, err := VerifyJWTClaims(tokenString, expectedIssuer)
	if err != nil {
		return "", err
	}
	return claims.Subject, nil
}
