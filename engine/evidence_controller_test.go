// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package engine

import (
	"strings"
	"testing"

	"tutor-mcp/algorithms"
	"tutor-mcp/models"
)

func TestApplyEvidenceController_WeakEvidenceAvoidsMasteryChallenge(t *testing.T) {
	in := baseEvidenceControllerInput()
	in.Activity.Type = models.ActivityMasteryChallenge
	in.Activity.Format = "integrated_performance_task"
	in.TransferProfile.ReadinessLabel = TransferReadinessReady
	in.EvidenceQuality.Quality = EvidenceQualityWeak

	got := ApplyEvidenceController(in)
	if !got.Adjusted {
		t.Fatalf("expected adjustment")
	}
	if got.Activity.Type != models.ActivityPractice {
		t.Fatalf("type: got %q, want PRACTICE", got.Activity.Type)
	}
	if strings.Contains(got.Activity.PromptForLLM, "Target difficulty:") {
		t.Fatalf("prompt should not embed stale numeric difficulty: %q", got.Activity.PromptForLLM)
	}
}

func TestApplyEvidenceController_UnobservedTransferPrefersProbe(t *testing.T) {
	in := baseEvidenceControllerInput()
	in.TransferProfile.ReadinessLabel = TransferReadinessUnobserved

	got := ApplyEvidenceController(in)
	if !got.Adjusted {
		t.Fatalf("expected adjustment")
	}
	if got.Activity.Type != models.ActivityTransferProbe {
		t.Fatalf("type: got %q, want TRANSFER_PROBE", got.Activity.Type)
	}
}

func TestApplyEvidenceController_BlockedTransferPrefersFeynman(t *testing.T) {
	in := baseEvidenceControllerInput()
	in.TransferProfile.ReadinessLabel = TransferReadinessBlocked

	got := ApplyEvidenceController(in)
	if !got.Adjusted {
		t.Fatalf("expected adjustment")
	}
	if got.Activity.Type != models.ActivityFeynmanPrompt {
		t.Fatalf("type: got %q, want FEYNMAN_PROMPT", got.Activity.Type)
	}
}

func TestApplyEvidenceController_StrongEvidenceKeepsActivity(t *testing.T) {
	in := baseEvidenceControllerInput()
	in.TransferProfile.ReadinessLabel = TransferReadinessReady
	in.EvidenceQuality.Quality = EvidenceQualityStrong

	got := ApplyEvidenceController(in)
	if got.Adjusted {
		t.Fatalf("expected no adjustment, got %+v", got)
	}
	if got.Activity.Type != in.Activity.Type {
		t.Fatalf("type changed: got %q, want %q", got.Activity.Type, in.Activity.Type)
	}
}

func TestApplyEvidenceController_DiagnosticCannotBecomeTeachingOrTransfer(t *testing.T) {
	in := baseEvidenceControllerInput()
	in.Activity.Type = models.ActivityDiagnosticAssessment
	in.Activity.Format = "cold_assessment"
	in.Activity.PromptForLLM = BuildActivityPrompt(in.Activity.Type, in.Activity.Concept, in.Activity.Format)
	in.TransferProfile.ReadinessLabel = TransferReadinessUnobserved

	got := ApplyEvidenceController(in)
	if got.Adjusted || got.Activity.Type != models.ActivityDiagnosticAssessment {
		t.Fatalf("cold diagnostic was overridden by evidence controller: %+v", got)
	}
}

func TestBuildActivityPrompt_DiagnosticRequiresResponseBeforeTeaching(t *testing.T) {
	prompt := BuildActivityPrompt(models.ActivityDiagnosticAssessment, "fractions", "cold_assessment")
	for _, required := range []string{"cold diagnostic", "before giving any explanation", "Do not teach", "scoring rubric"} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("diagnostic prompt missing %q: %q", required, prompt)
		}
	}
}

func TestBuildActivityPrompt_MasteryIsDomainNeutral(t *testing.T) {
	for _, concept := range []string{"quadratic equations", "Spanish past tense", "causes of the French Revolution", "concurrency in Go"} {
		prompt := BuildActivityPrompt(models.ActivityMasteryChallenge, concept, "integrated_performance_task")
		lower := strings.ToLower(prompt)
		if strings.Contains(lower, "code") || strings.Contains(lower, "software") || strings.Contains(lower, "build a") {
			t.Fatalf("concept %q received a code-specific prompt: %q", concept, prompt)
		}
		if !strings.Contains(lower, "domain-appropriate") || !strings.Contains(lower, "autonomous") {
			t.Fatalf("concept %q received an incomplete mastery contract: %q", concept, prompt)
		}
	}
}

func baseEvidenceControllerInput() EvidenceControllerInput {
	return EvidenceControllerInput{
		Activity: models.Activity{
			Type:             models.ActivityMasteryChallenge,
			Concept:          "goroutines",
			DifficultyTarget: 0.75,
			Format:           "integrated_performance_task",
			EstimatedMinutes: 45,
			Rationale:        "[phase=MAINTENANCE] selected",
			PromptForLLM:     BuildActivityPrompt(models.ActivityMasteryChallenge, "goroutines", "integrated_performance_task"),
		},
		ConceptState: &models.ConceptState{
			Concept:  "goroutines",
			PMastery: algorithms.MasteryBKT() + 0.05,
		},
		EvidenceQuality: EvidenceQualityAssessment{Quality: EvidenceQualityStrong},
	}
}
