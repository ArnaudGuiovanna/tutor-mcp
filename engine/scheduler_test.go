package engine

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"tutor-mcp/memory"
	"tutor-mcp/models"
	"tutor-mcp/webhookurl"
)

func TestIsSafeWebhookURL(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want bool
	}{
		{"https discord.com", "https://discord.com/api/webhooks/123/abc", true},
		{"https ptb.discord.com", "https://ptb.discord.com/api/webhooks/123/abc", true},
		{"https discordapp.com", "https://discordapp.com/api/webhooks/123/abc", true},
		{"http discord.com rejected", "http://discord.com/api/webhooks/123/abc", false},
		{"https evil.com rejected", "https://evil.com/api/webhooks/123/abc", false},
		{"https IMDS rejected", "https://169.254.169.254/latest/meta-data/", false},
		{"empty rejected", "", false},
		{"suffix spoof rejected", "https://evildiscord.com/x", false},
		{"double dot rejected", "https://discord..com/x", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := webhookurl.IsSafeWebhookURL(tc.url); got != tc.want {
				t.Errorf("IsSafeWebhookURL(%q) = %v, want %v", tc.url, got, tc.want)
			}
		})
	}
}

func TestSendOLM_DispatchesFallbackWhenQueueEmpty(t *testing.T) {
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "false")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got := string(body)
		if !strings.Contains(got, "Why now") &&
			!strings.Contains(got, "Next action") &&
			!strings.Contains(got, "Useful focus") {
			t.Errorf("expected an OLM body, got: %s", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	prev := safeWebhookURL
	safeWebhookURL = func(_ string) bool { return true }
	defer func() { safeWebhookURL = prev }()

	store, raw := newOLMTestStore(t)
	if _, err := raw.Exec(
		`INSERT INTO learners (id, email, password_hash, objective, webhook_url, last_active, created_at) VALUES (?,?,?,?,?,?,?)`,
		"L1", "l1@t.com", "h", "obj", srv.URL, time.Now().UTC(), time.Now().UTC(),
	); err != nil {
		t.Fatal(err)
	}
	seedDomain(t, raw, "L1", "math",
		[]string{"a", "b"},
		map[string][]string{"b": {"a"}},
		false,
	)
	seedConceptState(t, store, "L1", "a", 0.90, "review")

	sched := schedulerForTest(store)
	sched.sendOLM()

	sent, _ := store.WasAlertSentToday(context.Background(), "L1", alertKindOLM)
	if !sent {
		t.Errorf("OLM dispatch should mark sent today")
	}
	push, err := store.GetLatestOpenWebhookPush(context.Background(), "L1", "", time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatalf("GetLatestOpenWebhookPush: %v", err)
	}
	if push == nil {
		t.Fatalf("OLM dispatch should record a push log")
	}
}

func TestShouldPushDiscord_RefinesKSTFallbackWithMemory(t *testing.T) {
	fallback := &OLMSnapshot{
		FocusConcept:  "next",
		FocusReason:   "next frontier",
		FocusUrgency:  models.UrgencyInfo,
		HasActionable: true,
	}
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "true")
	if shouldPushDiscord(fallback, nil) {
		t.Fatal("KST fallback without recent narrative signal should not push")
	}
	if !shouldPushDiscord(fallback, &memory.EpisodicContext{
		RecentSessions: []memory.SessionPayload{{Frontmatter: map[string]any{"novelty_flag": true}}},
	}) {
		t.Fatal("KST fallback with recent narrative signal should push")
	}
	alertDriven := &OLMSnapshot{
		FocusConcept:  "forgotten",
		FocusReason:   "retention 25%",
		FocusUrgency:  models.UrgencyCritical,
		HasActionable: true,
	}
	if !shouldPushDiscord(alertDriven, nil) {
		t.Fatal("alert-driven OLM should push even without memory signal")
	}
	if shouldPushDiscord(&OLMSnapshot{HasActionable: false}, &memory.EpisodicContext{}) {
		t.Fatal("non-actionable OLM should not push")
	}
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "false")
	if !shouldPushDiscord(fallback, nil) {
		t.Fatal("memory disabled should preserve legacy actionable behavior")
	}
}

func TestRunConsolidationCycleEnqueuesPendingOnly(t *testing.T) {
	t.Setenv("TUTOR_MCP_MEMORY_ROOT", t.TempDir())
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "true")
	_, store, learnerID := rawTestSetup(t, "https://discord.com/api/webhooks/1/a")
	sched := schedulerForTest(store)

	sched.runConsolidationCycleAt(time.Date(2026, time.May, 3, 13, 30, 0, 0, time.UTC))

	item, err := store.GetConsolidation(context.Background(), learnerID, "monthly", "2026-04")
	if err != nil {
		t.Fatalf("GetConsolidation: %v", err)
	}
	if item.Status != "pending" {
		t.Fatalf("status = %q, want pending", item.Status)
	}
	if body, err := memory.Read(learnerID, memory.ScopeArchive, "2026-04"); err != nil || strings.TrimSpace(body) != "" {
		t.Fatalf("scheduler must not write archive content server-side, body=%q err=%v", body, err)
	}
}

func TestRunConsolidationCycleSkipsWhenMemoryDisabled(t *testing.T) {
	t.Setenv("TUTOR_MCP_MEMORY_ROOT", t.TempDir())
	t.Setenv("TUTOR_MCP_MEMORY_ENABLED", "false")
	_, store, learnerID := rawTestSetup(t, "https://discord.com/api/webhooks/1/a")
	sched := schedulerForTest(store)

	sched.runConsolidationCycleAt(time.Date(2026, time.May, 3, 13, 30, 0, 0, time.UTC))

	pending, err := store.GetPendingConsolidations(context.Background(), learnerID)
	if err != nil {
		t.Fatalf("GetPendingConsolidations: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("memory disabled should not enqueue consolidations: %#v", pending)
	}
}

func TestSendOLM_SkipsWhenNothingActionable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("scheduler should NOT post when nothing is actionable")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	prev := safeWebhookURL
	safeWebhookURL = func(_ string) bool { return true }
	defer func() { safeWebhookURL = prev }()

	store, raw := newOLMTestStore(t)
	if _, err := raw.Exec(
		`INSERT INTO learners (id, email, password_hash, objective, webhook_url, last_active, created_at) VALUES (?,?,?,?,?,?,?)`,
		"L1", "l1@t.com", "h", "obj", srv.URL, time.Now().UTC(), time.Now().UTC(),
	); err != nil {
		t.Fatal(err)
	}
	seedDomain(t, raw, "L1", "math", []string{"a"}, nil, false)
	seedConceptState(t, store, "L1", "a", 0.90, "review")

	sched := schedulerForTest(store)
	sched.sendOLM()

	sent, _ := store.WasAlertSentToday(context.Background(), "L1", alertKindOLM)
	if sent {
		t.Errorf("nothing actionable should NOT mark OLM as sent")
	}
}
