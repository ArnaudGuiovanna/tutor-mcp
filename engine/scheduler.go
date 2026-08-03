// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"tutor-mcp/memory"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
	"tutor-mcp/webhookurl"

	"github.com/robfig/cron/v3"
)

type Scheduler struct {
	store  storeport.Store
	cron   *cron.Cron
	logger *slog.Logger
	client *http.Client

	// stopTimeout bounds how long Stop() waits for in-flight cron jobs to
	// drain before forcing shutdown. Defaults to 25s (5s of margin within
	// the 30s SIGTERM handler in main.go for database.Close() etc.).
	// Overridable via test-only constructors so timeout-path tests don't
	// need to wait the full production budget.
	stopTimeout time.Duration

	// stopCh is closed by Stop() to signal in-flight work — notably the
	// synchronous backoff sleeps in doWithRetry — that the scheduler is
	// shutting down. A nil stopCh (e.g. in struct-literal test fixtures)
	// is a never-fires channel, so the abort path is simply inert. See #2.
	stopCh chan struct{}

	// distributed lets at most one fleet instance claim a scheduled run slot.
	// The lease is not a crash-safe exactly-once protocol: it has no completion
	// state, expiry, or retry. Default false keeps every tick local.
	distributed bool
}

// defaultStopTimeout is the production budget for Stop() to wait for
// in-flight jobs. Sized so that main.go's 30s SIGTERM handler still has
// 5s of margin to close the database and finish other shutdown work.
const defaultStopTimeout = 25 * time.Second

func NewScheduler(store storeport.Store, logger *slog.Logger) *Scheduler {
	// cron.New() ships no recover middleware: an unrecovered panic in any
	// scheduled job is a goroutine panic, which Go terminates the whole
	// process for. The HTTP recoveryMiddleware does not cover scheduler
	// goroutines, so without WithChain(Recover(...)) a single bad-data
	// learner could DoS the entire tutor at scheduled job times. See #35.
	cl := slogCronLogger{l: logger}
	return &Scheduler{
		store:  store,
		cron:   cron.New(cron.WithChain(cron.Recover(cl))),
		logger: logger,
		client: &http.Client{
			Timeout: 10 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return fmt.Errorf("too many webhook redirects")
				}
				if !safeWebhookURL(req.URL.String()) {
					return fmt.Errorf("unsafe webhook redirect")
				}
				return nil
			},
		},
		stopTimeout: defaultStopTimeout,
		stopCh:      make(chan struct{}),
	}
}

// NewDistributedScheduler builds the same scheduler as NewScheduler but in
// distributed mode: at most one fleet instance wins each scheduled run slot,
// gated by a DB lease (Store.ClaimJobRun). This reduces duplicate enqueues but
// does not retry work lost after a claimant crashes.
func NewDistributedScheduler(store storeport.Store, logger *slog.Logger) *Scheduler {
	s := NewScheduler(store, logger)
	s.distributed = true
	return s
}

// schedule registers a cron job. In distributed mode it first claims a lease
// keyed by (name, current time bucket of size period) so at most one fleet
// instance wins the slot; in in-process mode it always runs.
func (s *Scheduler) schedule(name, spec string, period time.Duration, fn func()) error {
	_, err := s.cron.AddFunc(spec, func() {
		if s.distributed {
			key := fmt.Sprintf("%d", time.Now().UTC().Truncate(period).Unix())
			ok, cerr := s.store.ClaimJobRun(context.Background(), name, key)
			if cerr != nil {
				s.logger.Error("claim job run", "job", name, "err", cerr)
				return
			}
			if !ok {
				return
			}
		}
		fn()
	})
	return err
}

// slogCronLogger adapts robfig/cron's Logger interface onto an *slog.Logger.
// cron only emits two kinds of messages: routine Info and Error (the latter
// is what cron.Recover uses to report a recovered panic + stack).
type slogCronLogger struct {
	l *slog.Logger
}

func (s slogCronLogger) Info(msg string, keysAndValues ...interface{}) {
	if s.l == nil {
		return
	}
	s.l.Info("cron: "+msg, keysAndValues...)
}

func (s slogCronLogger) Error(err error, msg string, keysAndValues ...interface{}) {
	if s.l == nil {
		return
	}
	args := append([]interface{}{"err", err}, keysAndValues...)
	s.l.Error("cron: "+msg, args...)
}

func (s *Scheduler) Start() error {
	// OLM: once a day at 13h UTC. Replaces the previous critical-alerts
	// (every 30 min) and review-reminders (3x/day) jobs — see
	// docs/superpowers/specs/2026-05-03-webhook-olm-design.md.
	if err := s.schedule("olm", "0 13 * * *", 24*time.Hour, s.sendOLM); err != nil {
		return fmt.Errorf("add olm job: %w", err)
	}
	if err := s.schedule("consolidation", "30 13 * * *", 24*time.Hour, s.runConsolidationCycle); err != nil {
		return fmt.Errorf("add consolidation job: %w", err)
	}
	if err := s.schedule("requeue_stale", "*/5 * * * *", 5*time.Minute, s.requeueStaleConsolidations); err != nil {
		return fmt.Errorf("add consolidation timeout job: %w", err)
	}
	// Daily motivation: once at 8h UTC.
	if err := s.schedule("motivation", "0 8 * * *", 24*time.Hour, s.sendDailyMotivation); err != nil {
		return fmt.Errorf("add daily motivation job: %w", err)
	}
	// End-of-day recap: once at 21h UTC.
	if err := s.schedule("recap", "0 21 * * *", 24*time.Hour, s.sendDailyRecap); err != nil {
		return fmt.Errorf("add daily recap job: %w", err)
	}
	// Mirror messages: once at 12h UTC. Dispatches any metacognitive mirror
	// nudges queued during sessions (#59). One per learner per day, dedup'd
	// at the alert layer (MIRROR_MESSAGE) so an in-session enqueue + the
	// scheduler tick can't double-fire.
	if err := s.schedule("mirror", "0 12 * * *", 24*time.Hour, s.sendMirrorMessages); err != nil {
		return fmt.Errorf("add mirror messages job: %w", err)
	}
	// Cleanup: hourly.
	if err := s.schedule("cleanup", "0 * * * *", time.Hour, s.cleanupExpiredData); err != nil {
		return fmt.Errorf("add cleanup job: %w", err)
	}
	// Metacognitive alerts: every 30 minutes. Each alert kind is fired at
	// most once per learner per UTC day via WasAlertSentToday so the cron
	// cadence only controls *latency* (worst case 30 min between the state
	// being met and the webhook firing), not frequency-of-spam.
	if err := s.schedule("metacog_alerts", "*/30 * * * *", 30*time.Minute, s.dispatchMetacognitiveAlerts); err != nil {
		return fmt.Errorf("add metacognitive alerts job: %w", err)
	}

	s.cron.Start()
	s.logger.Info("scheduler started", "jobs", "olm(13h), consolidation(13h30), consolidation_timeout(5m), motivation(8h), recap(21h), mirror(12h), cleanup(1h), metacog(30m)")
	return nil
}

// Stop halts the cron scheduler and waits for any in-flight jobs to drain
// before returning, capped by s.stopTimeout (default 25s).
//
// robfig/cron/v3's Stop() returns a context.Context that is cancelled only
// once all running jobs have finished. Discarding that context (the previous
// behaviour) made Stop() effectively non-blocking, so the deferred chain in
// main.go could run database.Close() while a cron tick was mid-iteration —
// producing "sql: database is closed" mid-loop and silent data loss on
// webhook_message_queue + scheduled_alerts. See issue #123.
func (s *Scheduler) Stop() {
	// Signal in-flight webhook backoff sleeps to abort before we start the
	// drain timer, so a job parked in doWithRetry's exponential schedule
	// doesn't burn the Stop() budget waiting out 0+1+5+25s. Guard against a
	// double Stop() (and a nil stopCh from test fixtures).
	if s.stopCh != nil {
		select {
		case <-s.stopCh:
			// already closed
		default:
			close(s.stopCh)
		}
	}

	ctx := s.cron.Stop()
	timeout := s.stopTimeout
	if timeout <= 0 {
		timeout = defaultStopTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
		s.logger.Warn("scheduler: in-flight jobs did not finish within budget — forcing shutdown",
			"timeout", timeout.String())
	}
}

// ─── Daily Motivation (8h) ──────────────────────────────────────────────────
//
// The scheduler no longer composes motivational text. Claude authors messages
// during sessions via queue_webhook_message, the scheduler dispatches from the
// queue. If the queue is empty for a learner, we fall back to a sober one-liner
// (no KPIs, no analytics bulletin tone).

func (s *Scheduler) sendDailyMotivation() {
	s.dispatchQueued("daily_motivation", "DAILY_MOTIVATION", fallbackDailyMotivation)
}

// ─── Daily Recap (21h) ─────────────────────────────────────────────────────

func (s *Scheduler) sendDailyRecap() {
	s.dispatchQueued("daily_recap", "DAILY_RECAP", fallbackDailyRecap)
}

// dispatchQueued pulls the best pending webhook message for each active learner
// (kind + ±30min scheduling window) and posts it. Falls back to a sober template
// when the queue is empty.
func (s *Scheduler) dispatchQueued(kind, alertTag string, fallback func(*models.Learner) discordPayload) {
	ctx := context.Background()
	learners, err := s.store.GetActiveLearners(ctx)
	if err != nil {
		s.logger.Error("scheduler: dispatch get learners", "err", err, "kind", kind)
		return
	}
	now := time.Now().UTC()

	for _, learner := range learners {
		if learner.WebhookURL == "" {
			continue
		}
		avail, err := s.store.GetAvailability(ctx, learner.ID)
		if err != nil {
			s.logger.Error("scheduler: dispatch availability", "err", err, "learner", learner.ID, "kind", kind)
			continue
		}
		allowed, err := avail.AllowsNotificationAt(now)
		if err != nil {
			s.logger.Error("scheduler: invalid notification policy", "err", err, "learner", learner.ID, "kind", kind)
			continue
		}
		if !allowed {
			continue
		}

		item, err := s.store.ClaimNextPendingWebhook(ctx, learner.ID, kind, now, 30*time.Minute)
		if err != nil {
			s.logger.Error("scheduler: claim webhook", "err", err, "learner", learner.ID, "kind", kind)
			continue
		}

		var payload discordPayload
		var source string
		var briefForLog *models.WebhookBrief
		domainID := ""
		if item != nil && item.Content != "" {
			embed, brief := embedFromWebhookContent(kind, item.Content, models.UrgencyInfo)
			payload = discordPayload{Embeds: []discordEmbed{embed}}
			briefForLog = brief
			if brief != nil {
				domainID = brief.DomainID
			}
			if domainID == "" && strings.HasPrefix(kind, "olm:") {
				domainID = strings.TrimPrefix(kind, "olm:")
			}
			source = "queue"
		} else if fallback != nil {
			payload = fallback(learner)
			source = "fallback"
		} else {
			continue
		}

		reservationID, reserved, err := s.store.ReserveNotificationDelivery(
			ctx, learner.ID, alertTag, domainID, models.IsIntrusiveWebhookKind(kind), now,
		)
		if err != nil {
			s.logger.Error("scheduler: reserve notification", "err", err, "learner", learner.ID, "kind", kind)
			if item != nil {
				if releaseErr := s.store.ReleaseWebhookClaim(ctx, item.ID, learner.ID); releaseErr != nil {
					s.logger.Error("scheduler: release webhook claim", "err", releaseErr, "learner", learner.ID, "queue_id", item.ID)
				}
			}
			continue
		}
		if !reserved {
			if item != nil {
				if releaseErr := s.store.ReleaseWebhookClaim(ctx, item.ID, learner.ID); releaseErr != nil {
					s.logger.Error("scheduler: release policy-blocked webhook", "err", releaseErr, "learner", learner.ID, "queue_id", item.ID)
				}
			}
			continue
		}
		accessibility, err := avail.Accessibility()
		if err != nil {
			_ = s.store.ReleaseNotificationDelivery(ctx, reservationID, learner.ID)
			if item != nil {
				_ = s.store.ReleaseWebhookClaim(ctx, item.ID, learner.ID)
			}
			s.logger.Error("scheduler: invalid accessibility policy", "err", err, "learner", learner.ID)
			continue
		}
		payload = applyAccessibilityPreferences(payload, accessibility)
		if item != nil {
			active, activeErr := s.store.IsWebhookClaimActive(ctx, item.ID, learner.ID)
			if activeErr != nil || !active {
				_ = s.store.ReleaseNotificationDelivery(ctx, reservationID, learner.ID)
				if activeErr != nil {
					_ = s.store.ReleaseWebhookClaim(ctx, item.ID, learner.ID)
					s.logger.Error("scheduler: revalidate webhook claim", "err", activeErr, "learner", learner.ID, "queue_id", item.ID)
				}
				continue
			}
		}

		send := s.sendDiscordEmbed
		if item != nil {
			// The queue owns retry timing and the dead-letter budget. Performing
			// multiple HTTP attempts inside one claim would multiply that budget
			// and create bursts after an outage.
			send = s.sendDiscordEmbedDurable
		}
		if err := send(learner.WebhookURL, payload); err != nil {
			s.logger.Error("scheduler: dispatch webhook", "err", err, "learner", learner.ID, "kind", kind)
			if releaseErr := s.store.ReleaseNotificationDelivery(ctx, reservationID, learner.ID); releaseErr != nil {
				s.logger.Error("scheduler: release failed delivery reservation", "err", releaseErr, "learner", learner.ID, "kind", kind)
			}
			if item != nil {
				if markErr := s.store.MarkWebhookFailed(ctx, item.ID, learner.ID); markErr != nil {
					s.logger.Error("scheduler: mark webhook failed", "err", markErr, "learner", learner.ID, "queue_id", item.ID)
				}
			}
			continue
		}
		if item != nil {
			if err := s.store.MarkWebhookSent(ctx, item.ID, learner.ID, now); err != nil {
				s.logger.Error("scheduler: webhook sent but queue update failed", "err", err, "learner", learner.ID, "queue_id", item.ID)
			}
		}
		if briefForLog != nil {
			queueID := int64(0)
			if item != nil {
				queueID = item.ID
			}
			if _, err := s.store.CreateWebhookPushLog(ctx, learner.ID, queueID, briefForLog, now); err != nil {
				s.logger.Warn("scheduler: push log", "err", err, "learner", learner.ID, "kind", kind)
			}
		}
		if err := s.store.CompleteNotificationDelivery(ctx, reservationID, learner.ID); err != nil {
			s.logger.Error("scheduler: webhook sent but reservation completion failed", "err", err, "learner", learner.ID, "kind", kind)
		}
		s.logger.Info("scheduler: dispatched", "learner", learner.ID, "kind", kind, "source", source)
	}
}

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

// ─── Sober fallback templates (no KPI, no analytics) ─────────────────────────

func fallbackDailyMotivation(_ *models.Learner) discordPayload {
	return discordPayload{Embeds: []discordEmbed{{
		Title:       "☀️ Good morning",
		Description: "Even 5 minutes today keeps the trajectory. Come back whenever you want.",
		Color:       0x5865F2,
	}}}
}

func fallbackDailyRecap(_ *models.Learner) discordPayload {
	return discordPayload{Embeds: []discordEmbed{{
		Title:       "🌙 Tonight",
		Description: "If you stop by, we keep going. Otherwise, see you tomorrow.",
		Color:       0x57F287,
	}}}
}

// ─── Cleanup (hourly) ─────────────────────────────────────────────────────

func (s *Scheduler) cleanupExpiredData() {
	ctx := context.Background()
	now := time.Now().UTC()
	codes, err := s.store.CleanupExpiredCodes(ctx)
	if err != nil {
		s.logger.Error("scheduler: cleanup codes", "err", err)
	} else if codes > 0 {
		s.logger.Info("scheduler: cleaned expired codes", "count", codes)
	}

	tokens, err := s.store.CleanupExpiredRefreshTokens(ctx)
	if err != nil {
		s.logger.Error("scheduler: cleanup tokens", "err", err)
	} else if tokens > 0 {
		s.logger.Info("scheduler: cleaned expired refresh tokens", "count", tokens)
	}

	requeued, err := s.store.RequeueStaleWebhookClaims(ctx, now.Add(-15*time.Minute), now)
	if err != nil {
		s.logger.Error("scheduler: requeue stale webhook claims", "err", err)
	} else if requeued > 0 {
		s.logger.Info("scheduler: requeued stale webhook claims", "count", requeued)
	}

	expired, err := s.store.ExpirePastWebhookMessages(ctx, now)
	if err != nil {
		s.logger.Error("scheduler: expire webhook queue", "err", err)
	} else if expired > 0 {
		s.logger.Info("scheduler: expired webhook messages", "count", expired)
	}

	// Housekeeping for the distributed-scheduler lease table so it doesn't
	// grow unbounded. Best-effort: a no-op on single-node deployments (the
	// table is simply empty) and never fails the cleanup tick.
	if purged, err := s.store.PurgeJobRunsBefore(ctx, time.Now().Add(-7*24*time.Hour)); err != nil {
		s.logger.Error("scheduler: purge job runs", "err", err)
	} else if purged > 0 {
		s.logger.Info("scheduler: purged old job runs", "count", purged)
	}
}

// ─── Message Formatting ─────────────────────────────────────────────────────

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

// ─── Discord Webhook ─────────────────────────────────────────────────────────

func (s *Scheduler) sendDiscordEmbed(url string, payload discordPayload) error {
	body, _ := json.Marshal(payload)
	return s.doWithRetry(url, body)
}

// sendDiscordEmbedDurable performs exactly one HTTP request. Its caller has
// claimed a durable queue row, so failures are persisted with a later
// next_attempt_at rather than retried synchronously in the scheduler tick.
func (s *Scheduler) sendDiscordEmbedDurable(url string, payload discordPayload) error {
	body, _ := json.Marshal(payload)
	return s.doOnce(url, body)
}

func (s *Scheduler) doOnce(url string, body []byte) error {
	if !safeWebhookURL(url) {
		s.logger.Error("webhook blocked: unsafe url")
		return fmt.Errorf("unsafe webhook url")
	}
	resp, err := s.client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		// net/url errors can contain the credential-bearing webhook path.
		s.logger.Warn("webhook network error", "error_type", fmt.Sprintf("%T", err))
		return fmt.Errorf("webhook transport error")
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("webhook returned %d", resp.StatusCode)
	}
	return nil
}

// retryDelays defines the exponential backoff schedule for doWithRetry.
// Promoted to a package-level var so tests can shrink delays without
// changing behavior in production.
var retryDelays = []time.Duration{0, 1 * time.Second, 5 * time.Second, 25 * time.Second}

// maxRetryAfter caps how long a single 429 Retry-After header is honored. A
// hostile or misconfigured endpoint can return Retry-After up to 60s; sleeping
// that long synchronously in the cron goroutine would stall the tick and eat
// the Stop() drain budget. We honor the header only up to this small bound. (#2)
const maxRetryAfter = 5 * time.Second

// safeWebhookURL is the SSRF guard used by doWithRetry. It defaults to the
// production Discord-only allowlist and is overridden in tests so that
// httptest.NewServer URLs (http://127.0.0.1:...) can be exercised end-to-end.
var safeWebhookURL = webhookurl.IsSafeWebhookURL

// doWithRetry posts body to url with exponential backoff.
// 4 attempts: immediate, +1s, +5s, +25s.
// Stops on 4xx (except 429). Respects Discord Retry-After header on 429.
func (s *Scheduler) doWithRetry(url string, body []byte) error {
	if !safeWebhookURL(url) {
		// Discord webhook paths contain credentials. Never emit the rejected
		// value: even an invalid URL can still carry a real token in its path.
		s.logger.Error("webhook blocked: unsafe url")
		return fmt.Errorf("unsafe webhook url")
	}
	delays := retryDelays
	var lastErr error
	for attempt, delay := range delays {
		if delay > 0 {
			if !s.sleepOrStop(delay) {
				// Scheduler is shutting down — abandon remaining attempts.
				return lastErr
			}
		}
		resp, err := s.client.Post(url, "application/json", bytes.NewReader(body))
		if err != nil {
			// net/url errors include the full request URL, including Discord's
			// secret webhook token. Preserve only a stable public category.
			lastErr = fmt.Errorf("webhook transport error")
			s.logger.Warn("webhook network error", "attempt", attempt+1, "error_type", fmt.Sprintf("%T", err))
			continue
		}
		resp.Body.Close()
		if resp.StatusCode < 400 {
			return nil
		}
		lastErr = fmt.Errorf("webhook returned %d", resp.StatusCode)
		// 429: respect Retry-After
		if resp.StatusCode == 429 {
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				if secs, err := strconv.Atoi(ra); err == nil && secs > 0 && secs <= 60 {
					wait := time.Duration(secs) * time.Second
					if wait > maxRetryAfter {
						wait = maxRetryAfter
					}
					s.logger.Warn("webhook rate limited, waiting", "retry_after", secs, "honored", wait.String())
					if !s.sleepOrStop(wait) {
						return lastErr
					}
				}
			}
			continue
		}
		// 4xx (not 429): client error, don't retry
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return lastErr
		}
		s.logger.Warn("webhook retry", "attempt", attempt+1, "status", resp.StatusCode)
	}
	return lastErr
}

// sleepOrStop blocks for d, but returns false early (without finishing the
// sleep) if the scheduler's stopCh is closed first. This keeps doWithRetry's
// synchronous backoff interruptible so Stop() isn't held hostage by an
// in-flight webhook retry. A nil stopCh never fires, so the sleep runs in full.
func (s *Scheduler) sleepOrStop(d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-s.stopCh:
		return false
	}
}

// ─── OLM (Open Learner Model) Daily Dispatch ────────────────────────────────

// sendOLM dispatches the daily Open Learner Model webhook at 13h UTC for each
// active learner with at least one actionable domain. Per-domain dispatch
// (cap 3 embeds) lets a learner with multiple domains see all relevant states
// in one Discord message. Falls back to FormatOLMEmbed when no LLM-authored
// message is queued.
func (s *Scheduler) sendOLM() {
	ctx := context.Background()
	learners, err := s.store.GetActiveLearners(ctx)
	if err != nil {
		s.logger.Error("scheduler: olm get learners", "err", err)
		return
	}
	now := time.Now().UTC()

	for _, learner := range learners {
		if learner.WebhookURL == "" {
			continue
		}
		avail, err := s.store.GetAvailability(ctx, learner.ID)
		if err != nil {
			s.logger.Error("scheduler: olm availability", "err", err, "learner", learner.ID)
			continue
		}
		allowed, err := avail.AllowsNotificationAt(now)
		if err != nil {
			s.logger.Error("scheduler: olm invalid notification policy", "err", err, "learner", learner.ID)
			continue
		}
		if !allowed {
			continue
		}

		domains, err := s.store.GetDomainsByLearner(ctx, learner.ID, false /*includeArchived*/)
		if err != nil {
			s.logger.Error("scheduler: olm list domains", "err", err, "learner", learner.ID)
			continue
		}

		var prepared []preparedWebhookEmbed
		for _, d := range domains {
			if len(prepared) >= 3 {
				break
			}
			if d.HighStakes {
				reviewed, err := s.store.HasHumanReviewedEvaluationInDomain(ctx, learner.ID, d.ID)
				if err != nil {
					s.logger.Error("scheduler: olm high-stakes review", "err", err, "learner", learner.ID, "domain", d.ID)
					continue
				}
				if !reviewed {
					continue
				}
			}
			snap, err := BuildOLMSnapshotAt(ctx, s.store, learner.ID, d.ID, now)
			if err != nil {
				s.logger.Warn("scheduler: olm build", "err", err, "learner", learner.ID, "domain", d.ID)
				continue
			}
			if !snap.HasActionable {
				continue
			}
			var memCtx *memory.EpisodicContext
			if memory.Enabled() && snap.FocusReason == "next frontier" {
				if ctx, err := memory.LoadContextForDomain(learner.ID, d.ID, snap.FocusConcept, &memory.OLMView{FocusConcept: snap.FocusConcept}, nil); err == nil {
					memCtx = ctx
				} else {
					s.logger.Warn("scheduler: olm memory context", "err", err, "learner", learner.ID, "domain", d.ID)
				}
			}
			if !shouldPushDiscord(snap, memCtx) {
				continue
			}

			kind := "olm:" + d.ID
			item, err := s.store.ClaimNextPendingWebhook(ctx, learner.ID, kind, now, 30*time.Minute)
			if err != nil {
				s.logger.Error("scheduler: olm claim", "err", err, "learner", learner.ID, "domain", d.ID)
				continue
			}
			if item != nil && item.Content != "" {
				embed, brief := embedFromWebhookContent(kind, item.Content, snap.FocusUrgency)
				prepared = append(prepared, preparedWebhookEmbed{DomainID: d.ID, Embed: embed, Item: item, Brief: brief})
			} else {
				brief := BuildOLMNudgeBrief(snap)
				prepared = append(prepared, preparedWebhookEmbed{DomainID: d.ID, Embed: discordEmbedFromBrief(brief, snap.FocusUrgency), Brief: &brief})
			}
		}

		if len(prepared) == 0 {
			continue
		}
		embeds := make([]discordEmbed, 0, len(prepared))
		for _, p := range prepared {
			embeds = append(embeds, p.Embed)
		}
		reservationID, reserved, err := s.store.ReserveNotificationDelivery(
			ctx, learner.ID, alertKindOLM, prepared[0].DomainID, true, now,
		)
		if err != nil || !reserved {
			if err != nil {
				s.logger.Error("scheduler: olm reserve notification", "err", err, "learner", learner.ID)
			}
			for _, p := range prepared {
				if p.Item != nil {
					_ = s.store.ReleaseWebhookClaim(ctx, p.Item.ID, learner.ID)
				}
			}
			continue
		}
		accessibility, err := avail.Accessibility()
		if err != nil {
			_ = s.store.ReleaseNotificationDelivery(ctx, reservationID, learner.ID)
			for _, p := range prepared {
				if p.Item != nil {
					_ = s.store.ReleaseWebhookClaim(ctx, p.Item.ID, learner.ID)
				}
			}
			s.logger.Error("scheduler: olm invalid accessibility policy", "err", err, "learner", learner.ID)
			continue
		}
		payload := applyAccessibilityPreferences(discordPayload{Embeds: embeds}, accessibility)
		claimsActive := true
		for _, p := range prepared {
			domain, domainErr := s.store.GetDomainByID(ctx, p.DomainID)
			if domainErr != nil || domain.LearnerID != learner.ID || domain.Archived {
				claimsActive = false
				if domainErr != nil {
					s.logger.Warn("scheduler: olm domain became inactive", "error_type", fmt.Sprintf("%T", domainErr), "learner", learner.ID, "domain", p.DomainID)
				}
			}
			if p.Item == nil {
				continue
			}
			active, activeErr := s.store.IsWebhookClaimActive(ctx, p.Item.ID, learner.ID)
			if activeErr != nil || !active {
				claimsActive = false
				if activeErr != nil {
					s.logger.Error("scheduler: olm revalidate claim", "err", activeErr, "learner", learner.ID, "queue_id", p.Item.ID)
				}
			}
		}
		if !claimsActive {
			_ = s.store.ReleaseNotificationDelivery(ctx, reservationID, learner.ID)
			for _, p := range prepared {
				if p.Item != nil {
					_ = s.store.ReleaseWebhookClaim(ctx, p.Item.ID, learner.ID)
				}
			}
			continue
		}

		send := s.sendDiscordEmbed
		for _, p := range prepared {
			if p.Item != nil {
				send = s.sendDiscordEmbedDurable
				break
			}
		}
		if err := send(learner.WebhookURL, payload); err != nil {
			s.logger.Error("scheduler: olm webhook", "err", err, "learner", learner.ID)
			_ = s.store.ReleaseNotificationDelivery(ctx, reservationID, learner.ID)
			for _, p := range prepared {
				if p.Item == nil {
					continue
				}
				if markErr := s.store.MarkWebhookFailed(ctx, p.Item.ID, learner.ID); markErr != nil {
					s.logger.Error("scheduler: olm mark failed", "err", markErr, "learner", learner.ID, "queue_id", p.Item.ID)
				}
			}
			continue
		}
		for _, p := range prepared {
			queueID := int64(0)
			if p.Item != nil {
				queueID = p.Item.ID
				if err := s.store.MarkWebhookSent(ctx, p.Item.ID, learner.ID, now); err != nil {
					s.logger.Error("scheduler: olm sent but queue update failed", "err", err, "learner", learner.ID, "queue_id", p.Item.ID)
				}
			}
			if p.Brief != nil {
				if _, err := s.store.CreateWebhookPushLog(ctx, learner.ID, queueID, p.Brief, now); err != nil {
					s.logger.Warn("scheduler: olm push log", "err", err, "learner", learner.ID)
				}
			}
		}
		if err := s.store.CompleteNotificationDelivery(ctx, reservationID, learner.ID); err != nil {
			s.logger.Error("scheduler: olm sent but reservation completion failed", "err", err, "learner", learner.ID)
		}
		s.logger.Info("scheduler: olm dispatched", "learner", learner.ID, "embeds", len(embeds))
	}
}

func (s *Scheduler) runConsolidationCycle() {
	s.runConsolidationCycleAt(time.Now().UTC())
}

func (s *Scheduler) runConsolidationCycleAt(now time.Time) {
	if !memory.Enabled() {
		return
	}
	ctx := context.Background()
	learnerIDs, err := s.store.GetLearnerIDsForConsolidation(ctx)
	if err != nil {
		s.logger.Error("scheduler: consolidation get learners", "err", err)
		return
	}
	for _, learnerID := range learnerIDs {
		jobs, err := memory.PrepareJobs(learnerID, now)
		if err != nil {
			s.logger.Warn("scheduler: consolidation prepare", "err", err, "learner", learnerID)
			continue
		}
		for _, job := range jobs {
			if err := s.store.UpsertPendingConsolidation(ctx, learnerID, string(job.Period), job.PeriodKey, now); err != nil {
				s.logger.Warn("scheduler: consolidation enqueue failed", "err", err, "learner", learnerID, "period", job.PeriodKey)
			} else {
				s.logger.Info("scheduler: consolidation pending", "learner", learnerID, "period_type", job.Period, "period", job.PeriodKey)
			}
		}
	}
}

func (s *Scheduler) requeueStaleConsolidations() {
	if !memory.Enabled() {
		return
	}
	ctx := context.Background()
	cutoff := time.Now().UTC().Add(-consolidationDeliveryTimeout())
	count, err := s.store.RequeueStaleDeliveredConsolidations(ctx, cutoff)
	if err != nil {
		s.logger.Warn("scheduler: consolidation timeout requeue failed", "err", err)
		return
	}
	if count > 0 {
		s.logger.Info("scheduler: consolidation timeout requeued", "count", count)
	}
}

func consolidationDeliveryTimeout() time.Duration {
	raw := strings.TrimSpace(os.Getenv("TUTOR_MCP_CONSOLIDATION_DELIVERY_TIMEOUT_MINUTES"))
	if raw == "" {
		return 30 * time.Minute
	}
	minutes, err := strconv.Atoi(raw)
	if err != nil || minutes <= 0 {
		return 30 * time.Minute
	}
	return time.Duration(minutes) * time.Minute
}

type preparedWebhookEmbed struct {
	DomainID string
	Embed    discordEmbed
	Item     *models.WebhookQueueItem
	Brief    *models.WebhookBrief
}

func shouldPushDiscord(olm *OLMSnapshot, memCtx *memory.EpisodicContext) bool {
	if olm == nil || !olm.HasActionable {
		return false
	}
	if !memory.Enabled() {
		return true
	}
	if olm.FocusReason != "next frontier" {
		return true
	}
	return memCtx != nil && memCtx.HasRecentNarrativeSignal()
}

func allHighStakesDomainsHumanReviewed(ctx context.Context, persistence storeport.Store, learnerID string) (bool, error) {
	domains, err := persistence.GetDomainsByLearner(ctx, learnerID, false)
	if err != nil {
		return false, err
	}
	for _, domain := range domains {
		if domain == nil || !domain.HighStakes {
			continue
		}
		reviewed, err := persistence.HasHumanReviewedEvaluationInDomain(ctx, learnerID, domain.ID)
		if err != nil {
			return false, err
		}
		if !reviewed {
			return false, nil
		}
	}
	return true, nil
}

// alertKindOLM is the alert tag used by sendOLM for daily-dedup checks
// (WasAlertSentToday + CreateScheduledAlert). Single source of truth so the
// two call sites cannot drift apart.
const alertKindOLM = "OLM"

// ─── Metacognitive Mirror Daily Dispatch ────────────────────────────────────

// sendMirrorMessages dispatches any pending metacognitive mirror nudges
// queued during sessions (engine.EnqueueMirrorWebhook). Mirror content is
// JSON (pattern + message + open question), decoded into a readable embed.
// Dedup at the alert layer (MirrorAlertKind) so an in-session enqueue won't
// race with the scheduler tick on the same day.
func (s *Scheduler) sendMirrorMessages() {
	ctx := context.Background()
	learners, err := s.store.GetActiveLearners(ctx)
	if err != nil {
		s.logger.Error("scheduler: mirror get learners", "err", err)
		return
	}
	now := time.Now().UTC()

	for _, learner := range learners {
		if learner.WebhookURL == "" {
			continue
		}
		avail, err := s.store.GetAvailability(ctx, learner.ID)
		if err != nil {
			s.logger.Error("scheduler: mirror availability", "err", err, "learner", learner.ID)
			continue
		}
		allowed, err := avail.AllowsNotificationAt(now)
		if err != nil {
			s.logger.Error("scheduler: mirror invalid notification policy", "err", err, "learner", learner.ID)
			continue
		}
		if !allowed {
			continue
		}
		highStakesAllowed, err := allHighStakesDomainsHumanReviewed(ctx, s.store, learner.ID)
		if err != nil {
			s.logger.Error("scheduler: mirror high-stakes review", "err", err, "learner", learner.ID)
			continue
		}
		if !highStakesAllowed {
			continue
		}

		// Official in-session enqueue records a daily reservation before the
		// scheduler runs. That reservation prevents duplicate enqueue; it is not
		// proof of delivery, so a pending queue row must still be claimed and sent.
		reserved, reservationErr := s.store.WasAlertSentToday(ctx, learner.ID, MirrorAlertKind)
		if reservationErr != nil {
			s.logger.Warn("scheduler: mirror reservation lookup", "err", reservationErr, "learner", learner.ID)
		}

		item, err := s.store.ClaimNextPendingWebhook(ctx, learner.ID, models.WebhookKindMirror, now, 30*time.Minute)
		if err != nil {
			s.logger.Error("scheduler: mirror claim", "err", err, "learner", learner.ID)
			continue
		}
		if item == nil || item.Content == "" {
			continue
		}

		embed := mirrorEmbedFromContent(item.Content)
		reservationID := int64(0)
		if !reserved {
			reservationID, reserved, err = s.store.ReserveNotificationDelivery(
				ctx, learner.ID, MirrorAlertKind, "", true, now,
			)
			if err != nil || !reserved {
				if err != nil {
					s.logger.Error("scheduler: mirror reserve notification", "err", err, "learner", learner.ID)
				}
				_ = s.store.ReleaseWebhookClaim(ctx, item.ID, learner.ID)
				continue
			}
		}
		accessibility, err := avail.Accessibility()
		if err != nil {
			if reservationID != 0 {
				_ = s.store.ReleaseNotificationDelivery(ctx, reservationID, learner.ID)
			}
			_ = s.store.ReleaseWebhookClaim(ctx, item.ID, learner.ID)
			s.logger.Error("scheduler: mirror invalid accessibility policy", "err", err, "learner", learner.ID)
			continue
		}
		payload := applyAccessibilityPreferences(discordPayload{Embeds: []discordEmbed{embed}}, accessibility)
		active, activeErr := s.store.IsWebhookClaimActive(ctx, item.ID, learner.ID)
		if activeErr != nil || !active {
			if reservationID != 0 {
				_ = s.store.ReleaseNotificationDelivery(ctx, reservationID, learner.ID)
			}
			if activeErr != nil {
				_ = s.store.ReleaseWebhookClaim(ctx, item.ID, learner.ID)
				s.logger.Error("scheduler: mirror revalidate claim", "err", activeErr, "learner", learner.ID, "queue_id", item.ID)
			}
			continue
		}
		if err := s.sendDiscordEmbedDurable(learner.WebhookURL, payload); err != nil {
			s.logger.Error("scheduler: mirror webhook", "err", err, "learner", learner.ID)
			if reservationID != 0 {
				_ = s.store.ReleaseNotificationDelivery(ctx, reservationID, learner.ID)
			}
			if markErr := s.store.MarkWebhookFailed(ctx, item.ID, learner.ID); markErr != nil {
				s.logger.Error("scheduler: mirror mark failed", "err", markErr, "learner", learner.ID, "queue_id", item.ID)
			}
			continue
		}
		if err := s.store.MarkWebhookSent(ctx, item.ID, learner.ID, now); err != nil {
			s.logger.Error("scheduler: mirror sent but queue update failed", "err", err, "learner", learner.ID, "queue_id", item.ID)
		}
		if reservationID != 0 {
			if err := s.store.CompleteNotificationDelivery(ctx, reservationID, learner.ID); err != nil {
				s.logger.Error("scheduler: mirror sent but reservation completion failed", "err", err, "learner", learner.ID)
			}
		}
		s.logger.Info("scheduler: mirror dispatched", "learner", learner.ID)
	}
}

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
