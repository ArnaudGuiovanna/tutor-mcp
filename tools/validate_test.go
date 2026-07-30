// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"math"
	"strings"
	"testing"

	"tutor-mcp/models"
)

func TestValidateUnitInterval(t *testing.T) {
	cases := []struct {
		name    string
		v       float64
		wantErr bool
	}{
		{"zero ok", 0, false},
		{"one ok", 1, false},
		{"mid ok", 0.5, false},
		{"below", -0.01, true},
		{"above", 1.01, true},
		{"NaN", math.NaN(), true},
		{"+Inf", math.Inf(1), true},
		{"-Inf", math.Inf(-1), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateUnitInterval("confidence", tc.v)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error for v=%v", tc.v)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error for v=%v: %v", tc.v, err)
			}
			if err != nil && !strings.Contains(err.Error(), "confidence") {
				t.Fatalf("error should mention field name, got %q", err.Error())
			}
		})
	}
}

func TestValidateLikertInt(t *testing.T) {
	if err := validateLikertInt("energy", 0, 1, 4); err != nil {
		t.Fatalf("0 should be allowed (omitted), got %v", err)
	}
	if err := validateLikertInt("energy", 1, 1, 4); err != nil {
		t.Fatalf("1 should be allowed, got %v", err)
	}
	if err := validateLikertInt("energy", 4, 1, 4); err != nil {
		t.Fatalf("4 should be allowed, got %v", err)
	}
	if err := validateLikertInt("energy", 5, 1, 4); err == nil {
		t.Fatal("5 should be rejected")
	}
	if err := validateLikertInt("energy", -1, 1, 4); err == nil {
		t.Fatal("-1 should be rejected")
	}
	if err := validateLikertInt("energy", 99, 1, 4); err == nil || !strings.Contains(err.Error(), "energy") {
		t.Fatalf("expected error mentioning field, got %v", err)
	}
}

func TestValidateLikertFloat(t *testing.T) {
	if err := validateLikertFloat("predicted_mastery", 1, 1, 5); err != nil {
		t.Fatalf("1 should be allowed, got %v", err)
	}
	if err := validateLikertFloat("predicted_mastery", 5, 1, 5); err != nil {
		t.Fatalf("5 should be allowed, got %v", err)
	}
	if err := validateLikertFloat("predicted_mastery", 0.5, 1, 5); err == nil {
		t.Fatal("0.5 should be rejected")
	}
	if err := validateLikertFloat("predicted_mastery", 100, 1, 5); err == nil {
		t.Fatal("100 should be rejected")
	}
	if err := validateLikertFloat("predicted_mastery", math.NaN(), 1, 5); err == nil {
		t.Fatal("NaN should be rejected")
	}
	if err := validateLikertFloat("predicted_mastery", math.Inf(1), 1, 5); err == nil {
		t.Fatal("+Inf should be rejected")
	}
}

func TestValidateNonNegativeDuration(t *testing.T) {
	if err := validateNonNegativeDuration("response_time_seconds", 0, 86400); err != nil {
		t.Fatalf("0 should be allowed, got %v", err)
	}
	if err := validateNonNegativeDuration("response_time_seconds", 30, 86400); err != nil {
		t.Fatalf("30 should be allowed, got %v", err)
	}
	if err := validateNonNegativeDuration("response_time_seconds", -1, 86400); err == nil {
		t.Fatal("-1 should be rejected")
	}
	if err := validateNonNegativeDuration("response_time_seconds", 86401, 86400); err == nil {
		t.Fatal("over-cap should be rejected")
	}
	if err := validateNonNegativeDuration("response_time_seconds", math.NaN(), 86400); err == nil {
		t.Fatal("NaN should be rejected")
	}
	if err := validateNonNegativeDuration("response_time_seconds", math.Inf(1), 86400); err == nil {
		t.Fatal("Inf should be rejected")
	}
}

func TestValidateNonNegativeCount(t *testing.T) {
	if err := validateNonNegativeCount("hints_requested", 0, 50); err != nil {
		t.Fatalf("0 should be allowed, got %v", err)
	}
	if err := validateNonNegativeCount("hints_requested", 50, 50); err != nil {
		t.Fatalf("50 should be allowed, got %v", err)
	}
	if err := validateNonNegativeCount("hints_requested", -1, 50); err == nil {
		t.Fatal("-1 should be rejected")
	}
	if err := validateNonNegativeCount("hints_requested", 51, 50); err == nil {
		t.Fatal("51 should be rejected")
	}
}

func TestRecordInteractionActivityWhitelistRejectsNonEvidenceActivities(t *testing.T) {
	for _, activityType := range []models.ActivityType{
		models.ActivityRest,
		models.ActivitySetupDomain,
		models.ActivityCloseSession,
	} {
		if err := validateEnum("activity_type", string(activityType), allowedActivityTypes); err == nil {
			t.Fatalf("%s must not be accepted as learner evidence", activityType)
		}
	}
}
