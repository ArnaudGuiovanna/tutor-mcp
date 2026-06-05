// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package auth

import (
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var jwtSecret []byte

func LoadJWTSecret() error {
	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		log.Fatal("JWT_SECRET env var required")
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

// JWTAudience is the resource identifier embedded in the aud claim and
// required by VerifyJWT. Keeping it stable (independent of BASE_URL) means
// tokens stay valid if the public hostname changes.
const JWTAudience = "tutor-mcp/mcp"

type Claims struct {
	jwt.RegisteredClaims
	Scope string `json:"scope"`
}

func GenerateJWT(issuer, learnerID string) (string, error) {
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   learnerID,
			Issuer:    issuer,
			Audience:  jwt.ClaimStrings{JWTAudience},
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(24 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		Scope: "learner",
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(jwtSecret)
}

func VerifyJWT(tokenString, expectedIssuer string) (string, error) {
	token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("invalid token")
		}
		return jwtSecret, nil
	},
		jwt.WithIssuer(expectedIssuer),
		jwt.WithAudience(JWTAudience),
		jwt.WithValidMethods([]string{"HS256"}),
	)
	if err != nil {
		return "", fmt.Errorf("invalid token: %w", err)
	}
	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return "", fmt.Errorf("invalid claims")
	}
	return claims.Subject, nil
}
