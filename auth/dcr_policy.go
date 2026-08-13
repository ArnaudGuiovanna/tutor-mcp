// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	storeport "tutor-mcp/store"
)

const (
	DCRModeOpen     = "open"
	DCRModeToken    = "token"
	DCRModeDisabled = "disabled"
)

type dcrAdmissionContextKey struct{}

type dcrAdmission struct {
	TokenID string
}

// ConfigureDynamicClientRegistration installs the boot-time DCR admission
// policy. Only the SHA-256 digest of the Initial Access Token crosses this
// boundary; the raw credential is never retained by OAuthServer.
func (s *OAuthServer) ConfigureDynamicClientRegistration(mode string, initialTokenHash [sha256.Size]byte) error {
	switch mode {
	case DCRModeOpen, DCRModeDisabled:
		if initialTokenHash != ([sha256.Size]byte{}) {
			return fmt.Errorf("DCR initial token hash is only valid in token mode")
		}
	case DCRModeToken:
		if initialTokenHash == ([sha256.Size]byte{}) {
			return fmt.Errorf("DCR token mode requires an initial token hash")
		}
	default:
		return fmt.Errorf("unknown DCR mode %q", mode)
	}
	if mode == DCRModeToken {
		hashHex := hex.EncodeToString(initialTokenHash[:])
		tokenID := "bootstrap-" + hashHex[:16]
		if err := s.store.EnsureDCRInitialAccessToken(
			context.Background(), tokenID, hashHex, "startup configuration",
			defaultDCRInitialTokenRegistrations, time.Now().UTC(),
		); err != nil {
			return fmt.Errorf("configure DCR initial access token: %w", err)
		}
	}
	s.dcrMode = mode
	return nil
}

func (s *OAuthServer) dynamicClientRegistrationEnabled() bool {
	return s.dcrMode == DCRModeOpen || s.dcrMode == DCRModeToken
}

func (s *OAuthServer) initialAccessToken(r *http.Request) (string, error) {
	values := r.Header.Values("Authorization")
	candidate := ""
	validShape := false
	if len(values) == 1 {
		parts := strings.Fields(values[0])
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") && len(parts[1]) <= 512 {
			candidate = parts[1]
			validShape = true
		}
	}
	digest := sha256.Sum256([]byte(candidate))
	token, err := s.store.GetActiveDCRInitialAccessToken(
		r.Context(), hex.EncodeToString(digest[:]), time.Now().UTC(),
	)
	if err != nil {
		return "", err
	}
	if !validShape {
		return "", storeport.ErrDCRInvalidInitialAccessToken
	}
	return token.TokenID, nil
}

// admitDynamicClientRegistration is the first operation on /register. It does
// not read the request body or touch persistence, rate-limit, cleanup, or
// bcrypt state. Missing and invalid Initial Access Tokens deliberately share
// one status, challenge, and body.
func (s *OAuthServer) admitDynamicClientRegistration(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	switch s.dcrMode {
	case DCRModeOpen:
		return r.WithContext(context.WithValue(r.Context(), dcrAdmissionContextKey{}, dcrAdmission{})), true
	case DCRModeToken:
		tokenID, err := s.initialAccessToken(r)
		if err == nil {
			ctx := context.WithValue(r.Context(), dcrAdmissionContextKey{}, dcrAdmission{TokenID: tokenID})
			return r.WithContext(ctx), true
		}
		if !errors.Is(err, storeport.ErrDCRInvalidInitialAccessToken) {
			s.logger.Error("DCR initial access token lookup failed", "err", err)
			w.Header().Set("Retry-After", "5")
			w.Header().Set("Cache-Control", "no-store")
			writeRegistrationErrorStatus(w, http.StatusServiceUnavailable, "temporarily_unavailable", "registration authority is unavailable")
			return r, false
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="oauth-dcr", error="invalid_token"`)
		w.Header().Set("Cache-Control", "no-store")
		writeRegistrationErrorStatus(w, http.StatusUnauthorized, "invalid_token", "initial access token is missing or invalid")
		return r, false
	case DCRModeDisabled:
		http.NotFound(w, r)
		return r, false
	default:
		// NewOAuthServer and the boot-time setter make this unreachable. Fail
		// closed if a zero-value or corrupted server is ever mounted directly.
		http.NotFound(w, r)
		return r, false
	}
}

// DynamicClientRegistrationAdmission must wrap the registration rate limiter.
// Unauthorized token-mode requests then cause no shared rate-limit write. The
// handler retains the same check as defense in depth for direct mounts/tests.
func (s *OAuthServer) DynamicClientRegistrationAdmission(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		admitted, ok := s.admitDynamicClientRegistration(w, r)
		if !ok {
			return
		}
		next.ServeHTTP(w, admitted)
	})
}

func dynamicClientRegistrationAdmission(ctx context.Context) (dcrAdmission, bool) {
	admitted, ok := ctx.Value(dcrAdmissionContextKey{}).(dcrAdmission)
	return admitted, ok
}
