// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"fmt"
	"testing"

	"tutor-mcp/models"
)

func TestResumeTrainingResolutionIsCompleteBoundedAndIsolated(t *testing.T) {
	s := setupTestDB(t)
	tenant := nativeTestTenant(t, s, "resume-resolution")
	owner := trainingPrincipal(t, s, tenant, 161, true)
	other := trainingPrincipal(t, s, tenant, 163, false)
	for i := 0; i < 25; i++ {
		trainingDomain(t, s, owner, fmt.Sprintf("Other %02d", i))
	}
	target := trainingDomain(t, s, owner, "Harness")
	trainingDomain(t, s, other, "Harness")
	choices, err := s.ResolveResumeTrainings(t.Context(), owner, "", " HARNESS ")
	if err != nil || len(choices.Items) != 1 || choices.Items[0].DomainID != target.ID || choices.HasMore {
		t.Fatalf("exact resolution=%#v %v", choices, err)
	}
	all, err := s.ResolveResumeTrainings(t.Context(), owner, "", "")
	if err != nil || len(all.Items) != 20 || !all.HasMore {
		t.Fatalf("choice bound=%#v %v", all, err)
	}
	trainingDomain(t, s, owner, "harness")
	choices, err = s.ResolveResumeTrainings(t.Context(), owner, "", "Harness")
	if err != nil || len(choices.Items) != 2 || choices.HasMore {
		t.Fatalf("homonyms=%#v %v", choices, err)
	}
	choices, err = s.ResolveResumeTrainings(t.Context(), other, "personal:"+target.ID, "")
	if err != nil || len(choices.Items) != 0 {
		t.Fatalf("foreign target leaked: %#v %v", choices, err)
	}
	for _, name := range []string{"Har", "%", "' OR 1=1 --"} {
		choices, err = s.ResolveResumeTrainings(t.Context(), owner, "", name)
		if err != nil || len(choices.Items) != 0 {
			t.Fatalf("non-exact name %q matched: %#v %v", name, choices, err)
		}
	}
	readOnly := owner
	readOnly.Scopes = []string{models.OAuthScopeFormationRead}
	if _, err := s.ResolveResumeTrainings(t.Context(), readOnly, "", "Harness"); err == nil {
		t.Fatal("resolved learner data without learner read permission")
	}
}
