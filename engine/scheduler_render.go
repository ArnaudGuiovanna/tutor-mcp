// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package engine

import (
	"encoding/json"
	"fmt"
	"strings"

	"tutor-mcp/models"
)

// queueKindTitle returns the embed title for a given webhook kind.
func queueKindTitle(kind string) string {
	switch kind {
	case "daily_motivation":
		return "☀️ Good morning"
	case "daily_recap":
		return "🌙 Tonight"
	case "reactivation":
		return "👋 Come back whenever you want"
	case "reminder":
		return "📚 Note"
	}
	return "✉️ Message"
}

func queueKindColor(kind string) int {
	switch kind {
	case "daily_motivation":
		return 0x5865F2
	case "daily_recap":
		return 0x57F287
	case "reactivation":
		return 0xFEE75C
	}
	return 0x99AAB5
}

func embedFromWebhookContent(kind, content string, urgency models.AlertUrgency) (discordEmbed, *models.WebhookBrief) {
	if brief, ok := models.DecodeWebhookBrief(content, kind); ok {
		return discordEmbedFromBrief(*brief, urgency), brief
	}
	return discordEmbed{
		Title:       queueKindTitle(kind),
		Description: trimDiscord(content, 1400),
		Color:       queueKindColor(kind),
	}, nil
}

func discordEmbedFromBrief(brief models.WebhookBrief, urgency models.AlertUrgency) discordEmbed {
	brief.Normalize(brief.Kind)
	title := briefTitle(brief, urgency)
	labels := briefLabels(brief.Language)
	description := brief.OpenLoop
	if description == "" {
		description = brief.WhyNow
	}
	if description == "" {
		description = brief.LearningGain
	}
	if description == "" {
		description = "A useful signal is ready for your next session."
	}

	fields := []discordField{}
	addField := func(name, value string, inline bool) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		fields = append(fields, discordField{
			Name:   name,
			Value:  trimDiscord(value, 900),
			Inline: inline,
		})
	}
	addField(labels.WhyNow, brief.WhyNow, false)
	addField(labels.LearningGain, brief.LearningGain, false)
	addField(labels.Evidence, formatEvidence(brief.Evidence), false)
	addField(labels.GoalLink, brief.GoalLink, false)
	addField(labels.NextAction, actionWithMinutes(brief.NextAction, brief.EstimatedMinutes), false)

	footer := &discordFooter{Text: labels.Footer}
	return discordEmbed{
		Title:       title,
		Description: trimDiscord(description, 900),
		Color:       colorForUrgencyOrKind(urgency, brief.Kind),
		Fields:      fields,
		Footer:      footer,
	}
}

type webhookBriefLabels struct {
	WhyNow       string
	LearningGain string
	Evidence     string
	GoalLink     string
	NextAction   string
	Footer       string
}

func briefLabels(_ string) webhookBriefLabels {
	return webhookBriefLabels{
		WhyNow:       "Why now",
		LearningGain: "Learning gain",
		Evidence:     "Useful signals",
		GoalLink:     "Goal link",
		NextAction:   "Next action",
		Footer:       "Short nudge: solve the challenge in the tutor session, not in Discord.",
	}
}

func briefTitle(brief models.WebhookBrief, urgency models.AlertUrgency) string {
	subject := brief.Concept
	if subject == "" {
		subject = brief.DomainName
	}
	if subject == "" {
		subject = brief.Trigger
	}
	if subject == "" {
		subject = "next session"
	}
	switch urgency {
	case models.UrgencyCritical:
		return "Review soon - " + subject
	case models.UrgencyWarning:
		return "Useful focus - " + subject
	default:
		if brief.Trigger != "" {
			return brief.Trigger + " - " + subject
		}
		return "Learning nudge - " + subject
	}
}

func colorForUrgencyOrKind(urgency models.AlertUrgency, kind string) int {
	switch urgency {
	case models.UrgencyCritical:
		return colorCritical
	case models.UrgencyWarning:
		return colorWarning
	}
	if strings.HasPrefix(kind, "olm") {
		return colorInfo
	}
	return queueKindColor(kind)
}

func formatEvidence(evidence []string) string {
	if len(evidence) == 0 {
		return ""
	}
	if len(evidence) > 3 {
		evidence = evidence[:3]
	}
	lines := make([]string, 0, len(evidence))
	for _, e := range evidence {
		e = strings.TrimSpace(e)
		if e != "" {
			lines = append(lines, "- "+e)
		}
	}
	return strings.Join(lines, "\n")
}

func actionWithMinutes(action string, minutes int) string {
	action = strings.TrimSpace(action)
	if action == "" {
		return ""
	}
	if minutes <= 0 {
		return action
	}
	return fmt.Sprintf("%s (~%d min)", action, minutes)
}

func trimDiscord(s string, max int) string {
	s = strings.TrimSpace(s)
	if max <= 0 || len(s) <= max {
		return s
	}
	if max <= 1 {
		return s[:max]
	}
	return strings.TrimSpace(s[:max-1]) + "…"
}

type discordEmbed struct {
	Title       string         `json:"title"`
	Description string         `json:"description"`
	Color       int            `json:"color"`
	Fields      []discordField `json:"fields,omitempty"`
	Footer      *discordFooter `json:"footer,omitempty"`
}

type discordField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline,omitempty"`
}

type discordFooter struct {
	Text string `json:"text"`
}

type discordPayload struct {
	Embeds []discordEmbed `json:"embeds"`
}

func applyAccessibilityPreferences(payload discordPayload, prefs models.AccessibilityPreferences) discordPayload {
	for i := range payload.Embeds {
		if prefs.AvoidEmojis || prefs.ScreenReaderOptimized {
			payload.Embeds[i].Title = stripEmoji(payload.Embeds[i].Title)
			payload.Embeds[i].Description = stripEmoji(payload.Embeds[i].Description)
			for j := range payload.Embeds[i].Fields {
				payload.Embeds[i].Fields[j].Name = stripEmoji(payload.Embeds[i].Fields[j].Name)
				payload.Embeds[i].Fields[j].Value = stripEmoji(payload.Embeds[i].Fields[j].Value)
			}
			if payload.Embeds[i].Footer != nil {
				payload.Embeds[i].Footer.Text = stripEmoji(payload.Embeds[i].Footer.Text)
			}
		}
		if prefs.ScreenReaderOptimized {
			for j := range payload.Embeds[i].Fields {
				payload.Embeds[i].Fields[j].Inline = false
			}
		}
	}
	return payload
}

func stripEmoji(value string) string {
	value = strings.Map(func(r rune) rune {
		switch {
		case r == '\u200d', r == '\u20e3', r == '\ufe0e', r == '\ufe0f':
			return -1
		case r >= 0x1f000 && r <= 0x1faff:
			return -1
		case r >= 0x2300 && r <= 0x23ff:
			return -1
		case r >= 0x2600 && r <= 0x27bf:
			return -1
		case r >= 0x2b00 && r <= 0x2bff:
			return -1
		case r == 0x00a9, r == 0x00ae, r == 0x203c, r == 0x2049,
			r == 0x2122, r == 0x2139, r == 0x3030, r == 0x303d,
			r == 0x3297, r == 0x3299:
			return -1
		default:
			return r
		}
	}, value)
	return strings.TrimSpace(value)
}

// Note: formatDailyMotivation / formatDailyRecap / formatInactivityNudge have been
// removed. Daily motivation and recap are now authored by Claude via queue_webhook_message
// and dispatched by dispatchQueued (with sober Go fallbacks when the queue is empty).

// mirrorEmbedFromContent renders a queued MirrorWebhookContent payload into
// a Discord embed. Falls back to using the raw content as the description
// when the payload isn't valid JSON (e.g. a manual queue_webhook_message call
// posted plain text under kind=mirror_message).
func mirrorEmbedFromContent(content string) discordEmbed {
	var payload MirrorWebhookContent
	desc := content
	if err := json.Unmarshal([]byte(content), &payload); err == nil && payload.Message != "" {
		desc = payload.Message
		if payload.OpenQuestion != "" {
			desc += "\n\n" + payload.OpenQuestion
		}
	}
	return discordEmbed{
		Title:       "🪞 Mirror of the day",
		Description: desc,
		Color:       0x9B59B6,
	}
}
