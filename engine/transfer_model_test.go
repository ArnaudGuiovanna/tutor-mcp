// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna>
// SPDX-License-Identifier: MIT

package engine

import (
	"reflect"
	"testing"
	"time"

	"tutor-mcp/models"
)

func TestTransferDimensionsStableNames(t *testing.T) {
	want := []TransferDimension{
		TransferDimensionNear,
		TransferDimensionFar,
		TransferDimensionDebugging,
		TransferDimensionTeaching,
		TransferDimensionCreative,
	}

	if got := append([]TransferDimension(nil), canonicalTransferDimensions...); !reflect.DeepEqual(got, want) {
		t.Fatalf("TransferDimensions() = %v, want %v", got, want)
	}

	for _, dimension := range want {
		if string(dimension) == "" {
			t.Fatalf("empty transfer dimension name: %q", dimension)
		}
	}
}

func TestBuildTransferProfile_NoTransfer(t *testing.T) {
	profile := BuildTransferProfile("calc", []*models.TransferRecord{
		{ConceptID: "other", ContextType: "near", Score: 1},
	})

	if profile.ReadinessLabel != TransferReadinessUnobserved {
		t.Fatalf("ReadinessLabel = %q, want %q", profile.ReadinessLabel, TransferReadinessUnobserved)
	}
	if profile.EvidenceTrust != TransferEvidenceUnverified {
		t.Fatalf("EvidenceTrust = %q, want %q", profile.EvidenceTrust, TransferEvidenceUnverified)
	}
	if profile.Attempts != 0 || profile.GlobalScore != 0 || profile.ObservedScore != 0 || profile.Coverage != 0 {
		t.Fatalf("unexpected empty profile metrics: %+v", profile)
	}
	assertTransferDimensions(t, "missing", profile.MissingDimensions, append([]TransferDimension(nil), canonicalTransferDimensions...))
	assertTransferDimensions(t, "weakest", profile.WeakestDimensions, nil)
	if len(profile.DimensionSummaries) != len(append([]TransferDimension(nil), canonicalTransferDimensions...)) {
		t.Fatalf("DimensionSummaries len = %d, want %d", len(profile.DimensionSummaries), len(append([]TransferDimension(nil), canonicalTransferDimensions...)))
	}
}

func TestBuildTrustedTransferProfileMarksPrevalidatedEvidence(t *testing.T) {
	profile := BuildTrustedTransferProfileAt("calc", []*models.TransferRecord{
		{ConceptID: "calc", ContextType: "near", Score: 0.9},
	}, time.Time{})

	if profile.EvidenceTrust != TransferEvidenceTrusted {
		t.Fatalf("EvidenceTrust = %q, want %q", profile.EvidenceTrust, TransferEvidenceTrusted)
	}
}

func TestBuildTransferProfile_NearOnly(t *testing.T) {
	profile := BuildTransferProfile("calc", []*models.TransferRecord{
		{ConceptID: "calc", ContextType: "near", Score: 0.7},
		{ConceptID: "calc", ContextType: "near", Score: 0.9},
	})

	if profile.ReadinessLabel != TransferReadinessNarrow {
		t.Fatalf("ReadinessLabel = %q, want %q", profile.ReadinessLabel, TransferReadinessNarrow)
	}
	if profile.Attempts != 2 {
		t.Fatalf("Attempts = %d, want 2", profile.Attempts)
	}
	if profile.GlobalScore != 0.16 || profile.ObservedScore != 0.8 || profile.Coverage != 0.2 || profile.PassingCoverage != 0.2 {
		t.Fatalf("unexpected near-only metrics: %+v", profile)
	}
	assertTransferDimensions(t, "covered", profile.CoveredDimensions, []TransferDimension{TransferDimensionNear})
	assertTransferDimensions(t, "missing", profile.MissingDimensions, []TransferDimension{
		TransferDimensionFar,
		TransferDimensionDebugging,
		TransferDimensionTeaching,
		TransferDimensionCreative,
	})
	assertTransferDimensions(t, "weakest", profile.WeakestDimensions, []TransferDimension{TransferDimensionNear})

	near := profile.DimensionSummaries[0]
	if near.Dimension != TransferDimensionNear || near.Attempts != 2 || near.AverageScore != 0.8 || near.BestScore != 0.9 || !near.Passing {
		t.Fatalf("unexpected near summary: %+v", near)
	}
}

func TestBuildTransferProfile_FarCreativeCoverage(t *testing.T) {
	profile := BuildTransferProfile("calc", []*models.TransferRecord{
		{ConceptID: "calc", ContextType: "interview", Score: 0.7},
		{ConceptID: "calc", ContextType: "far", Score: 0.9},
		{ConceptID: "calc", ContextType: " Real_World ", Score: 0.8},
		{ConceptID: "calc", ContextType: "creative", Score: 0.85},
	})

	if profile.ReadinessLabel != TransferReadinessDeveloping {
		t.Fatalf("ReadinessLabel = %q, want %q", profile.ReadinessLabel, TransferReadinessDeveloping)
	}
	if profile.Attempts != 4 {
		t.Fatalf("Attempts = %d, want 4", profile.Attempts)
	}
	if profile.GlobalScore != 0.33 || profile.ObservedScore != 0.825 || profile.Coverage != 0.4 || profile.PassingCoverage != 0.4 {
		t.Fatalf("unexpected far/creative metrics: %+v", profile)
	}
	assertTransferDimensions(t, "covered", profile.CoveredDimensions, []TransferDimension{
		TransferDimensionFar,
		TransferDimensionCreative,
	})
	assertTransferDimensions(t, "missing", profile.MissingDimensions, []TransferDimension{
		TransferDimensionNear,
		TransferDimensionDebugging,
		TransferDimensionTeaching,
	})
	assertTransferDimensions(t, "weakest", profile.WeakestDimensions, []TransferDimension{TransferDimensionFar})

	far := profile.DimensionSummaries[1]
	if far.Dimension != TransferDimensionFar || far.Attempts != 3 || far.AverageScore != 0.8 || far.BestScore != 0.9 {
		t.Fatalf("unexpected far summary: %+v", far)
	}
}

func TestBuildTransferProfile_Failures(t *testing.T) {
	profile := BuildTransferProfile("calc", []*models.TransferRecord{
		nil,
		{ConceptID: "calc", ContextType: "near", Score: 0.4},
		{ConceptID: "calc", ContextType: "far", Score: 0.3},
		{ConceptID: "calc", ContextType: "creative", Score: 0.9},
		{ConceptID: "calc", ContextType: "banana", Score: 0.1},
		{ConceptID: "other", ContextType: "near", Score: 0.1},
	})

	if profile.ReadinessLabel != TransferReadinessBlocked {
		t.Fatalf("ReadinessLabel = %q, want %q", profile.ReadinessLabel, TransferReadinessBlocked)
	}
	if profile.Attempts != 3 || profile.FailureCount != 2 || profile.UnknownCount != 1 {
		t.Fatalf("unexpected counts: %+v", profile)
	}
	if profile.FailureRate != 0.6667 || profile.ObservedScore != 0.5333 || profile.GlobalScore != 0.32 {
		t.Fatalf("unexpected failure metrics: %+v", profile)
	}
	assertTransferDimensions(t, "weakest", profile.WeakestDimensions, []TransferDimension{TransferDimensionFar})
}

func TestBuildTransferProfile_Deterministic(t *testing.T) {
	first := []*models.TransferRecord{
		{ConceptID: "calc", ContextType: "creative", Score: 0.9},
		{ConceptID: "calc", ContextType: "near", Score: 0.8},
		{ConceptID: "calc", ContextType: "debugging", Score: 0.6},
		{ConceptID: "calc", ContextType: "far", Score: 0.7},
		{ConceptID: "calc", ContextType: "teaching", Score: 0.65},
	}
	second := []*models.TransferRecord{
		first[3],
		first[1],
		first[4],
		first[0],
		first[2],
	}

	got := BuildTransferProfile("calc", first)
	want := BuildTransferProfile("calc", second)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BuildTransferProfile order changed result:\ngot:  %+v\nwant: %+v", got, want)
	}
	if got.ReadinessLabel != TransferReadinessReady {
		t.Fatalf("ReadinessLabel = %q, want %q", got.ReadinessLabel, TransferReadinessReady)
	}
}

func assertTransferDimensions(t *testing.T, name string, got, want []TransferDimension) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s dimensions = %v, want %v", name, got, want)
	}
}

// P2-12: one rule for "blocked". A repaired dimension is judged on its
// latest observation by both builders, and an expired trusted failure no
// longer blocks through the aggregate label.
func TestTransferProfileBlockedRuleIsConsistent(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	repaired := []*models.TransferRecord{
		{ConceptID: "c", ContextType: "near", Score: 0.1, CreatedAt: now.Add(-48 * time.Hour)},
		{ConceptID: "c", ContextType: "near", Score: 0.9, CreatedAt: now.Add(-24 * time.Hour)},
	}
	if p := BuildTransferProfile("c", repaired); p.ReadinessLabel == TransferReadinessBlocked {
		t.Fatalf("a repaired dimension must not stay blocked: %+v", p)
	}
	if p := BuildTrustedTransferProfileAt("c", repaired, now); p.ReadinessLabel == TransferReadinessBlocked || p.BlockingFailures != 0 {
		t.Fatalf("trusted profile disagrees: %+v", p)
	}
	expired := []*models.TransferRecord{
		{ConceptID: "c", ContextType: "near", Score: 0.1, CreatedAt: now.Add(-60 * 24 * time.Hour)},
		{ConceptID: "c", ContextType: "far", Score: 0.9, CreatedAt: now.Add(-24 * time.Hour)},
	}
	p := BuildTrustedTransferProfileAt("c", expired, now)
	if p.BlockingFailures != 0 || p.ReadinessLabel == TransferReadinessBlocked {
		t.Fatalf("an expired failure must not block: %+v", p)
	}
}
