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
		store:       store,
		cron:        cron.New(cron.WithChain(cron.Recover(cl))),
		logger:      logger,
		client:      &http.Client{Timeout: 10 * time.Second},
		stopTimeout: defaultStopTimeout,
		stopCh:      make(chan struct{}),
	}
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
	if _, err := s.cron.AddFunc("0 13 * * *", s.sendOLM); err != nil {
		return fmt.Errorf("add olm job: %w", err)
	}
	if _, err := s.cron.AddFunc("30 13 * * *", s.runConsolidationCycle); err != nil {
		return fmt.Errorf("add consolidation job: %w", err)
	}
	if _, err := s.cron.AddFunc("*/5 * * * *", s.requeueStaleConsolidations); err != nil {
		return fmt.Errorf("add consolidation timeout job: %w", err)
	}
	// Daily motivation: once at 8h UTC.
	if _, err := s.cron.AddFunc("0 8 * * *", s.sendDailyMotivation); err != nil {
		return fmt.Errorf("add daily motivation job: %w", err)
	}
	// End-of-day recap: once at 21h UTC.
	if _, err := s.cron.AddFunc("0 21 * * *", s.sendDailyRecap); err != nil {
		return fmt.Errorf("add daily recap job: %w", err)
	}
	// Mirror messages: once at 12h UTC. Dispatches any metacognitive mirror
	// nudges queued during sessions (#59). One per learner per day, dedup'd
	// at the alert layer (MIRROR_MESSAGE) so an in-session enqueue + the
	// scheduler tick can't double-fire.
	if _, err := s.cron.AddFunc("0 12 * * *", s.sendMirrorMessages); err != nil {
		return fmt.Errorf("add mirror messages job: %w", err)
	}
	// Cleanup: hourly.
	if _, err := s.cron.AddFunc("0 * * * *", s.cleanupExpiredData); err != nil {
		return fmt.Errorf("add cleanup job: %w", err)
	}
	// Metacognitive alerts: every 30 minutes. Each alert kind is fired at
	// most once per learner per UTC day via WasAlertSentToday so the cron
	// cadence only controls *latency* (worst case 30 min between the state
	// being met and the webhook firing), not frequency-of-spam.
	if _, err := s.cron.AddFunc("*/30 * * * *", s.dispatchMetacognitiveAlerts); err != nil {
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
		avail, _ := s.store.GetAvailability(ctx, learner.ID)
		if avail != nil && avail.DoNotDisturb {
			continue
		}

		// Dedup at the alert layer — don't fire twice in one day.
		sent, _ := s.store.WasAlertSentToday(ctx, learner.ID, alertTag)
		if sent {
			continue
		}

		item, err := s.store.DequeueNextPending(ctx, learner.ID, kind, now, 30*time.Minute)
		if err != nil {
			s.logger.Error("scheduler: dequeue", "err", err, "learner", learner.ID, "kind", kind)
			continue
		}

		var payload discordPayload
		var source string
		var briefForLog *models.WebhookBrief
		if item != nil && item.Content != "" {
			embed, brief := embedFromWebhookContent(kind, item.Content, models.UrgencyInfo)
			payload = discordPayload{Embeds: []discordEmbed{embed}}
			briefForLog = brief
			source = "queue"
		} else if fallback != nil {
			payload = fallback(learner)
			source = "fallback"
		} else {
			continue
		}

		if err := s.sendDiscordEmbed(learner.WebhookURL, payload); err != nil {
			s.logger.Error("scheduler: dispatch webhook", "err", err, "learner", learner.ID, "kind", kind)
			if item != nil {
				_ = s.store.MarkWebhookFailed(ctx, item.ID, learner.ID)
			}
			continue
		}
		if item != nil {
			_ = s.store.MarkWebhookSent(ctx, item.ID, learner.ID, now)
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
		s.store.CreateScheduledAlert(ctx, learner.ID, alertTag, "", time.Now())
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

	expired, err := s.store.ExpirePastWebhookMessages(ctx, time.Now().UTC())
	if err != nil {
		s.logger.Error("scheduler: expire webhook queue", "err", err)
	} else if expired > 0 {
		s.logger.Info("scheduler: expired webhook messages", "count", expired)
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

// Note: formatDailyMotivation / formatDailyRecap / formatInactivityNudge have been
// removed. Daily motivation and recap are now authored by Claude via queue_webhook_message
// and dispatched by dispatchQueued (with sober Go fallbacks when the queue is empty).

// ─── Discord Webhook ─────────────────────────────────────────────────────────

func (s *Scheduler) sendDiscordEmbed(url string, payload discordPayload) error {
	body, _ := json.Marshal(payload)
	return s.doWithRetry(url, body)
}

// sendWebhook sends a plain text message (kept for backwards compatibility).
func (s *Scheduler) sendWebhook(url, message string) error {
	body, _ := json.Marshal(map[string]string{"content": message})
	return s.doWithRetry(url, body)
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
		s.logger.Error("webhook blocked: unsafe url", "url", url)
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
			lastErr = err
			s.logger.Warn("webhook network error", "attempt", attempt+1, "err", err)
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
		avail, _ := s.store.GetAvailability(ctx, learner.ID)
		if avail != nil && avail.DoNotDisturb {
			continue
		}
		if sent, _ := s.store.WasAlertSentToday(ctx, learner.ID, alertKindOLM); sent {
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
			snap, err := BuildOLMSnapshot(ctx, s.store, learner.ID, d.ID)
			if err != nil {
				s.logger.Warn("scheduler: olm build", "err", err, "learner", learner.ID, "domain", d.ID)
				continue
			}
			if !snap.HasActionable {
				continue
			}
			var memCtx *memory.EpisodicContext
			if memory.Enabled() && snap.FocusReason == "next frontier" {
				if ctx, err := memory.LoadContext(learner.ID, snap.FocusConcept, &memory.OLMView{FocusConcept: snap.FocusConcept}, nil); err == nil {
					memCtx = ctx
				} else {
					s.logger.Warn("scheduler: olm memory context", "err", err, "learner", learner.ID, "domain", d.ID)
				}
			}
			if !shouldPushDiscord(snap, memCtx) {
				continue
			}

			kind := "olm:" + d.ID
			item, _ := s.store.DequeueNextPending(ctx, learner.ID, kind, now, 30*time.Minute)
			if item != nil && item.Content != "" {
				embed, brief := embedFromWebhookContent(kind, item.Content, snap.FocusUrgency)
				prepared = append(prepared, preparedWebhookEmbed{Embed: embed, Item: item, Brief: brief})
			} else {
				brief := BuildOLMNudgeBrief(snap)
				prepared = append(prepared, preparedWebhookEmbed{Embed: discordEmbedFromBrief(brief, snap.FocusUrgency), Brief: &brief})
			}
		}

		if len(prepared) == 0 {
			continue
		}
		embeds := make([]discordEmbed, 0, len(prepared))
		for _, p := range prepared {
			embeds = append(embeds, p.Embed)
		}

		if err := s.sendDiscordEmbed(learner.WebhookURL, discordPayload{Embeds: embeds}); err != nil {
			s.logger.Error("scheduler: olm webhook", "err", err, "learner", learner.ID)
			continue
		}
		for _, p := range prepared {
			queueID := int64(0)
			if p.Item != nil {
				queueID = p.Item.ID
				_ = s.store.MarkWebhookSent(ctx, p.Item.ID, learner.ID, now)
			}
			if p.Brief != nil {
				if _, err := s.store.CreateWebhookPushLog(ctx, learner.ID, queueID, p.Brief, now); err != nil {
					s.logger.Warn("scheduler: olm push log", "err", err, "learner", learner.ID)
				}
			}
		}
		_ = s.store.CreateScheduledAlert(ctx, learner.ID, alertKindOLM, "", now)
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
	learners, err := s.store.GetActiveLearners(ctx)
	if err != nil {
		s.logger.Error("scheduler: consolidation get learners", "err", err)
		return
	}
	for _, learner := range learners {
		jobs, err := memory.PrepareJobs(learner.ID, now)
		if err != nil {
			s.logger.Warn("scheduler: consolidation prepare", "err", err, "learner", learner.ID)
			continue
		}
		for _, job := range jobs {
			if err := s.store.UpsertPendingConsolidation(ctx, learner.ID, string(job.Period), job.PeriodKey, now); err != nil {
				s.logger.Warn("scheduler: consolidation enqueue failed", "err", err, "learner", learner.ID, "period", job.PeriodKey)
			} else {
				s.logger.Info("scheduler: consolidation pending", "learner", learner.ID, "period_type", job.Period, "period", job.PeriodKey)
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
	Embed discordEmbed
	Item  *models.WebhookQueueItem
	Brief *models.WebhookBrief
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

// embedFromQueueItem renders a queued LLM-authored OLM message. Structured
// WebhookBrief content gets a pedagogical Discord layout; legacy text is used
// as-is for backwards compatibility.
func embedFromQueueItem(item *models.WebhookQueueItem, urgency models.AlertUrgency) discordEmbed {
	if item == nil {
		return discordEmbed{}
	}
	embed, _ := embedFromWebhookContent(item.Kind, item.Content, urgency)
	return embed
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
		avail, _ := s.store.GetAvailability(ctx, learner.ID)
		if avail != nil && avail.DoNotDisturb {
			continue
		}

		// The in-session emission already records a scheduled_alert with
		// MirrorAlertKind for dedup. WasAlertSentToday catches both the
		// in-session enqueue AND any prior tick today.
		if sent, _ := s.store.WasAlertSentToday(ctx, learner.ID, MirrorAlertKind); sent {
			// We still want to mark queued items as sent so they don't
			// pile up — best-effort dequeue + mark.
			if item, _ := s.store.DequeueNextPending(ctx, learner.ID, models.WebhookKindMirror, now, 30*time.Minute); item != nil {
				_ = s.store.MarkWebhookSent(ctx, item.ID, learner.ID, now)
			}
			continue
		}

		item, err := s.store.DequeueNextPending(ctx, learner.ID, models.WebhookKindMirror, now, 30*time.Minute)
		if err != nil {
			s.logger.Error("scheduler: mirror dequeue", "err", err, "learner", learner.ID)
			continue
		}
		if item == nil || item.Content == "" {
			continue
		}

		embed := mirrorEmbedFromContent(item.Content)
		if err := s.sendDiscordEmbed(learner.WebhookURL, discordPayload{Embeds: []discordEmbed{embed}}); err != nil {
			s.logger.Error("scheduler: mirror webhook", "err", err, "learner", learner.ID)
			_ = s.store.MarkWebhookFailed(ctx, item.ID, learner.ID)
			continue
		}
		_ = s.store.MarkWebhookSent(ctx, item.ID, learner.ID, now)
		_ = s.store.CreateScheduledAlert(ctx, learner.ID, MirrorAlertKind, "", now)
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

// fromExportedEmbed converts engine.DiscordEmbed (used by FormatOLMEmbed for
// testability) to scheduler.discordEmbed. Same shape; plain field copy.
func fromExportedEmbed(e DiscordEmbed) discordEmbed {
	return discordEmbed{Title: e.Title, Description: e.Description, Color: e.Color}
}
