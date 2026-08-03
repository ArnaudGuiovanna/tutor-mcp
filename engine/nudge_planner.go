// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package engine

import (
	"fmt"
	"sort"
	"strings"

	"tutor-mcp/models"
)

// NudgeCandidate is the scheduler-facing decision made by the nudge planner.
// It separates pedagogical priority from transport details so the scheduler
// can enforce dedup and delivery windows without knowing why a nudge matters.
type NudgeCandidate struct {
	Kind     string
	AlertTag string
	Priority int
	Urgency  models.AlertUrgency
	Brief    models.WebhookBrief
}

// BuildOLMNudgeBrief turns an OLM snapshot into an open-loop Discord brief.
// The message is intentionally incomplete: it tells the learner what is worth
// reopening, but keeps the actual challenge for the tutor session.
func BuildOLMNudgeBrief(snap *OLMSnapshot) models.WebhookBrief {
	if snap == nil {
		return models.WebhookBrief{}
	}
	concept := snap.FocusConcept
	trigger := "Learning state"
	whyNow := "One part of your learning model is worth a short review today."
	if concept != "" {
		switch snap.FocusUrgency {
		case models.UrgencyCritical:
			trigger = "Priority review"
			whyNow = fmt.Sprintf("%s is in a fragile zone (%s). Reviewing it now avoids a heavier review later.", concept, snap.FocusReason)
		case models.UrgencyWarning:
			trigger = "Current focus"
			whyNow = fmt.Sprintf("%s currently looks like the best learning lever (%s).", concept, snap.FocusReason)
		default:
			trigger = "Next frontier"
			whyNow = fmt.Sprintf("%s is the next useful small step.", concept)
		}
	}

	evidence := []string{}
	if buckets := compactBuckets(snap); buckets != "" {
		evidence = append(evidence, "Distribution: "+buckets)
	}
	if snap.FocusReason != "" {
		evidence = append(evidence, "Signal: "+snap.FocusReason)
	}
	if line := MetacogLine(snap); line != "" {
		evidence = append(evidence, "Meta: "+line)
	}

	openLoop := "I kept a small challenge to check this point without giving the solution here."
	nextAction := "Open Claude and start with today's focus."
	if concept != "" {
		openLoop = fmt.Sprintf("I kept a small challenge on %s: short enough to try, not trivial enough to solve in Discord.", concept)
		nextAction = fmt.Sprintf("Open Claude and ask for the challenge on %s.", concept)
	}

	return models.WebhookBrief{
		Version:           models.WebhookBriefVersion,
		Kind:              "olm",
		DomainID:          snap.DomainID,
		DomainName:        snap.DomainName,
		Concept:           concept,
		Trigger:           trigger,
		PedagogicalIntent: "Route the next session toward the highest learning gain.",
		LearningGain:      "Avoid vague review: you know what to revisit, why, and which action to start with.",
		WhyNow:            whyNow,
		Evidence:          evidence,
		GoalLink:          goalLinkSentence(snap.PersonalGoal, snap.EvidenceProgress),
		OpenLoop:          openLoop,
		NextAction:        nextAction,
		EstimatedMinutes:  8,
		Language:          "en",
		Tone:              "clear, concrete, encouraging",
	}
}

// BuildMetacognitiveNudgeCandidates converts raw metacognitive alerts into
// learner-facing candidates, sorted by expected learning value. The scheduler
// will enqueue at most one candidate per learner per tick to avoid noisy
// parallel nudges.
func BuildMetacognitiveNudgeCandidates(learner *models.Learner, domains []*models.Domain, alerts []models.Alert) []NudgeCandidate {
	var out []NudgeCandidate
	for _, alert := range alerts {
		kind := metacogKindToWebhookKind(alert.Type)
		if kind == "" {
			continue
		}
		domain := domainForAlert(domains, alert)
		brief := metacognitiveBrief(learner, domain, alert, kind)
		out = append(out, NudgeCandidate{
			Kind:     kind,
			AlertTag: kind,
			Priority: metacognitiveNudgePriority(alert.Type),
			Urgency:  alert.Urgency,
			Brief:    brief,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Priority > out[j].Priority
	})
	return out
}

func metacognitiveNudgePriority(t models.AlertType) int {
	switch t {
	case models.AlertTransferBlocked:
		return 95
	case models.AlertCalibrationDiverging:
		return 90
	case models.AlertDependencyIncreasing:
		return 80
	case models.AlertAffectNegative:
		return 70
	default:
		return 0
	}
}

func metacognitiveBrief(_ *models.Learner, domain *models.Domain, alert models.Alert, kind string) models.WebhookBrief {
	brief := models.WebhookBrief{
		Version:           models.WebhookBriefVersion,
		Kind:              kind,
		DomainID:          domainID(domain),
		DomainName:        domainName(domain),
		Concept:           alert.Concept,
		EstimatedMinutes:  8,
		Language:          "en",
		Tone:              "simple, concrete, non-blaming",
		PedagogicalIntent: "Turn a metacognitive signal into a short learning action.",
	}

	switch alert.Type {
	case models.AlertTransferBlocked:
		concept := alert.Concept
		if concept == "" {
			concept = "a previously practiced concept"
		}
		brief.Trigger = "Transfer blocked"
		brief.LearningGain = "Turn recognized knowledge into reusable competence in a new context."
		brief.WhyNow = fmt.Sprintf("%s looks familiar, but transfer is still fragile: this is the right moment to change context.", concept)
		brief.Evidence = []string{"High model estimate", "At least two weak transfer contexts"}
		brief.OpenLoop = fmt.Sprintf("I kept a Feynman question on %s: it forces explanation, not just recognition.", concept)
		brief.NextAction = fmt.Sprintf("Open Claude and start by explaining %s in your own words.", concept)
	case models.AlertCalibrationDiverging:
		brief.Trigger = "Calibration to revisit"
		brief.LearningGain = "Estimate your level more accurately so exercises are neither too easy nor too hard."
		brief.WhyNow = "Your self-rating is drifting away from recent results: a cold check will give you a better compass."
		brief.Evidence = []string{alert.RecommendedAction}
		brief.OpenLoop = "I kept a mini-test without hints: it will quickly show whether your impression matches what you can do."
		brief.NextAction = "Open Claude and start with a short check before any new exercise."
	case models.AlertDependencyIncreasing:
		brief.Trigger = "Autonomy declining"
		brief.LearningGain = "Regain control gradually, without removing help when it is useful."
		brief.WhyNow = "Recent signals show more reliance on tutoring. The gain comes from a short attempt with less help."
		brief.Evidence = []string{"Trend across several sessions", alert.RecommendedAction}
		brief.OpenLoop = "I kept an exercise where the first move is yours; help returns only after your attempt."
		brief.NextAction = "Open Claude and ask for a first attempt without hints, then compare with feedback."
	case models.AlertAffectNegative:
		brief.Trigger = "Load to adjust"
		brief.LearningGain = "Keep progressing without turning difficulty into unnecessary fatigue."
		brief.WhyNow = "Recent sessions were costly. A shorter, better-calibrated return will produce more learning."
		brief.Evidence = []string{"Low satisfaction or difficulty across two recent sessions"}
		brief.OpenLoop = "I kept a lighter version of the next exercise: same concept, less extraneous load."
		brief.NextAction = "Open Claude and start with a short activity, then decide whether to increase difficulty."
	default:
		brief.Trigger = "Learning signal"
		brief.LearningGain = "Turn the signal into a clear next action."
		brief.WhyNow = alert.RecommendedAction
		brief.OpenLoop = "I kept a short question to check the useful point."
		brief.NextAction = "Open Claude and start with that point."
	}
	brief.GoalLink = goalLinkFromDomain(domain)
	return brief
}

func domainForAlert(domains []*models.Domain, alert models.Alert) *models.Domain {
	if alert.Concept == "" {
		if len(domains) > 0 {
			return domains[0]
		}
		return nil
	}
	for _, d := range domains {
		if d == nil {
			continue
		}
		for _, c := range d.Graph.Concepts {
			if c == alert.Concept {
				return d
			}
		}
	}
	if len(domains) > 0 {
		return domains[0]
	}
	return nil
}

func goalLinkSentence(goal string, progress float64) string {
	goal = strings.TrimSpace(goal)
	if goal == "" {
		return ""
	}
	return fmt.Sprintf("Goal: %s. Current state: %s.", goal, progressPhrase(progress))
}

func goalLinkFromDomain(domain *models.Domain) string {
	if domain == nil || strings.TrimSpace(domain.PersonalGoal) == "" {
		return ""
	}
	return "Goal link: " + strings.TrimSpace(domain.PersonalGoal) + "."
}

func domainID(domain *models.Domain) string {
	if domain == nil {
		return ""
	}
	return domain.ID
}

func domainName(domain *models.Domain) string {
	if domain == nil {
		return ""
	}
	return domain.Name
}
