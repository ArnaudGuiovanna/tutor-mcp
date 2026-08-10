// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package models

import "testing"

func TestCanonicalOAuthScope(t *testing.T) {
	tests := []struct {
		raw  string
		want string
		ok   bool
	}{
		{raw: "learner", want: OAuthScopeLearner, ok: true},
		{raw: "learner:read", want: OAuthScopeLearnerRead, ok: true},
		{raw: "learner:write", want: OAuthScopeLearnerWrite, ok: true},
		{raw: "learner:write learner:read", want: OAuthScopeLearnerReadWrite, ok: true},
		{raw: "  learner:read   learner:write ", want: OAuthScopeLearnerReadWrite, ok: true},
		{raw: ""},
		{raw: "admin"},
		{raw: "learner learner:read"},
		{raw: "learner:read learner:read"},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			got, err := CanonicalOAuthScope(tt.raw)
			if tt.ok && (err != nil || got != tt.want) {
				t.Fatalf("CanonicalOAuthScope(%q)=(%q,%v), want (%q,nil)", tt.raw, got, err, tt.want)
			}
			if !tt.ok && err == nil {
				t.Fatalf("CanonicalOAuthScope(%q)=%q, want error", tt.raw, got)
			}
		})
	}
}

func TestOAuthScopeCapabilitiesAndNarrowing(t *testing.T) {
	if !OAuthScopeAllows(OAuthScopeLearner, OAuthScopeLearnerRead) ||
		!OAuthScopeAllows(OAuthScopeLearner, OAuthScopeLearnerWrite) {
		t.Fatal("legacy learner bundle must grant exactly learner read and write")
	}
	if OAuthScopeAllows(OAuthScopeLearnerRead, OAuthScopeLearnerWrite) {
		t.Fatal("read scope granted write capability")
	}
	if OAuthScopeAllows(OAuthScopeLearner, "future:admin") {
		t.Fatal("legacy learner bundle acted as a wildcard")
	}
	if !OAuthScopeCanNarrow(OAuthScopeLearnerReadWrite, OAuthScopeLearnerRead) {
		t.Fatal("read/write grant could not narrow to read")
	}
	if OAuthScopeCanNarrow(OAuthScopeLearnerRead, OAuthScopeLearnerReadWrite) {
		t.Fatal("read grant widened to read/write")
	}
	if OAuthScopeCanNarrow(OAuthScopeLearnerReadWrite, OAuthScopeLearner) {
		t.Fatal("granular grant converted back to legacy bundle")
	}
}
