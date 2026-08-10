// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"fmt"
	"strings"

	"tutor-mcp/models"
)

const oauthScopeKey contextKey = "oauth_scope"

// normalizeAuthorizationScope preserves the historical omitted-scope behavior
// as the explicitly bounded learner bundle. Granular grants are accepted only
// after the fleet-wide rollout flag is enabled; this prevents an old /token
// node from widening a newly-created read-only authorization code to the
// legacy read/write bundle during a mixed-version deployment.
func normalizeAuthorizationScope(raw string, granularEnabled bool) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return models.OAuthScopeLearner, nil
	}
	canonical, err := models.CanonicalOAuthScope(raw)
	if err == nil {
		if !granularEnabled && canonical != models.OAuthScopeLearner {
			return "", fmt.Errorf("granular OAuth scopes are not enabled")
		}
		return canonical, nil
	}

	// Older discovery documents briefly advertised the bounded legacy alias
	// alongside its granular members. Be tolerant of clients that cached that
	// document and request all advertised values: the presence of `learner`
	// already requests exactly read+write, so canonicalizing the redundant set
	// to `learner` cannot expand the requested capabilities. Phase A remains
	// strict so no granular syntax is accepted before every node is upgraded.
	if !granularEnabled {
		return "", err
	}
	seenLegacy := false
	for _, scope := range strings.Fields(raw) {
		switch scope {
		case models.OAuthScopeLearner:
			seenLegacy = true
		case models.OAuthScopeLearnerRead, models.OAuthScopeLearnerWrite:
		default:
			return "", err
		}
	}
	if seenLegacy {
		return models.OAuthScopeLearner, nil
	}
	return "", err
}

// normalizeRefreshScope keeps an omitted refresh scope distinguishable from an
// explicit request: omission preserves the stored grant, while a supplied
// value is canonicalized and later checked as a non-widening subset.
func normalizeRefreshScope(raw string, granularEnabled bool) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	canonical, err := models.CanonicalOAuthScope(raw)
	if err != nil {
		return "", err
	}
	if !granularEnabled && canonical != models.OAuthScopeLearner {
		return "", fmt.Errorf("granular OAuth scopes are not enabled")
	}
	return canonical, nil
}

// WithOAuthScope attaches one validated OAuth grant to an authenticated
// request context. Invalid values are stored as empty so all capability checks
// fail closed.
func WithOAuthScope(ctx context.Context, scope string) context.Context {
	canonical, err := models.CanonicalOAuthScope(scope)
	if err != nil {
		canonical = ""
	}
	return context.WithValue(ctx, oauthScopeKey, canonical)
}

func GetOAuthScope(ctx context.Context) string {
	scope, _ := ctx.Value(oauthScopeKey).(string)
	return scope
}

func HasOAuthScope(ctx context.Context, required string) bool {
	return models.OAuthScopeAllows(GetOAuthScope(ctx), required)
}

// oauthScopeDescription is deliberately capability-based rather than echoing
// the raw OAuth token. Consent pages only call it after validation, but the
// fallback remains explicit if a future caller forgets that boundary.
func oauthScopeDescription(scope string) string {
	canonical, err := models.CanonicalOAuthScope(scope)
	if err != nil {
		return "Requested permissions could not be verified. Do not continue."
	}
	switch canonical {
	case models.OAuthScopeLearnerRead:
		return "Read your learner profile, domains, progress, sessions, and learning history."
	case models.OAuthScopeLearnerWrite:
		return "Modify your learner profile, domains, progress, sessions, and learning history."
	case models.OAuthScopeLearner, models.OAuthScopeLearnerReadWrite:
		return "Read and modify your learner profile, domains, progress, sessions, and learning history."
	default:
		return "Requested permissions could not be verified. Do not continue."
	}
}
