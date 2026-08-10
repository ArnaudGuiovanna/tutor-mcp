// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package auth

import (
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"time"

	"tutor-mcp/models"

	"github.com/golang-jwt/jwt/v5"
)

var jwtSecret []byte

// AccessTokenTTL bounds exposure of a leaked bearer. Long-lived access is
// provided through rotating refresh-token families, not a day-long stateless
// credential that the server cannot revoke retroactively.
const AccessTokenTTL = 30 * time.Minute

func LoadJWTSecret() error {
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
	Scope string `json:"scope"`
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
	if resource == "" || resource != MCPResource(issuer) {
		return "", fmt.Errorf("resource must equal the canonical MCP resource")
	}
	canonicalScope, err := models.CanonicalOAuthScope(scope)
	if err != nil {
		return "", fmt.Errorf("invalid OAuth scope: %w", err)
	}
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   learnerID,
			Issuer:    issuer,
			Audience:  jwt.ClaimStrings{resource},
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(AccessTokenTTL)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		Scope: canonicalScope,
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
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("invalid token")
		}
		return jwtSecret, nil
	},
		jwt.WithIssuer(expectedIssuer),
		jwt.WithAudience(expectedResource),
		jwt.WithValidMethods([]string{"HS256"}),
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
	return claims, nil
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
