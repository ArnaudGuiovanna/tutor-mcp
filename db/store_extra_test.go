// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"testing"
	"time"

	"tutor-mcp/models"
	"tutor-mcp/webhookurl"
)

// ─── IsSafeWebhookURL ───────────────────────────────────────────────────────

func TestIsSafeWebhookURL(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"https://discord.com/api/webhooks/123/abc", true},
		{"https://discordapp.com/api/webhooks/x/y", true},
		{"https://canary.discord.com/api/webhooks/x/y", true},
		{"https://ptb.discordapp.com/api/webhooks/x/y", true},
		{"https://discord.com:443/api/webhooks/123/abc?wait=true", true},
		{"https://discord.com/api/webhooks/123/abc/slack", true},
		{"http://discord.com/api/webhooks/123/abc", false},  // not https
		{"https://example.com/api/webhooks/123/abc", false}, // wrong host
		{"https://discord.com.evil.com/x", false},           // suffix trick
		{"https://untrusted.discord.com/api/webhooks/123/abc", false},
		{"https://discord.com:8443/api/webhooks/123/abc", false},
		{"https://user@discord.com/api/webhooks/123/abc", false},
		{"https://discord.com/login", false},
		{"https://discord.com/api/webhooks/123", false},
		{"https://discord.com/api/webhooks/123/abc/redirect", false},
		{"https://192.168.1.1/x", false},  // IP literal
		{"https://discord..com/x", false}, // double-dot host
		{"https://", false},               // empty host
		{"::not a url::", false},          // unparseable
		{"", false},                       // empty
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.url, func(t *testing.T) {
			if got := webhookurl.IsSafeWebhookURL(tc.url); got != tc.want {
				t.Errorf("IsSafeWebhookURL(%q) = %v want %v", tc.url, got, tc.want)
			}
		})
	}
}

// ─── Learner CRUD ───────────────────────────────────────────────────────────

func TestCreateLearner_GetByIDAndEmail(t *testing.T) {
	store := setupTestDB(t)
	l, err := store.CreateLearner(context.Background(), "alice@example.com", "h", "go-mastery", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if l.ID == "" {
		t.Fatal("expected non-empty id")
	}
	got, err := store.GetLearnerByID(context.Background(), l.ID)
	if err != nil {
		t.Fatalf("by id: %v", err)
	}
	if got.Email != "alice@example.com" || got.Objective != "go-mastery" {
		t.Errorf("by id mismatch: %+v", got)
	}
	// ProfileJSON defaults to "{}"
	if got.ProfileJSON != "{}" {
		t.Errorf("ProfileJSON = %q want '{}'", got.ProfileJSON)
	}

	got2, err := store.GetLearnerByEmail(context.Background(), "alice@example.com")
	if err != nil {
		t.Fatalf("by email: %v", err)
	}
	if got2.ID != l.ID {
		t.Errorf("by email id mismatch: %+v", got2)
	}

	// Missing learner returns error (sql.ErrNoRows wrapped).
	if _, err := store.GetLearnerByID(context.Background(), "nope"); err == nil {
		t.Error("expected error for missing id")
	}
	if _, err := store.GetLearnerByEmail(context.Background(), "nope@nope"); err == nil {
		t.Error("expected error for missing email")
	}
}

func TestCreateLearner_RejectsBadWebhook(t *testing.T) {
	store := setupTestDB(t)
	if _, err := store.CreateLearner(context.Background(), "x@y", "h", "obj", "https://evil.example/wh"); err == nil {
		t.Fatal("expected webhook validation error")
	}
}

func TestUpdateLastActive_AndProfile(t *testing.T) {
	store := setupTestDB(t)
	if err := store.UpdateLastActive(context.Background(), "L1"); err != nil {
		t.Fatalf("UpdateLastActive: %v", err)
	}
	var lastActive *time.Time
	if err := store.root.QueryRow(`SELECT last_active FROM learners WHERE id = 'L1'`).Scan(&lastActive); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if lastActive == nil || lastActive.IsZero() {
		t.Errorf("last_active not set: %v", lastActive)
	}

	if err := store.UpdateLearnerProfile(context.Background(), "L1", `{"foo":"bar"}`, nil); err != nil {
		t.Fatalf("UpdateLearnerProfile: %v", err)
	}
	got, err := store.GetLearnerByID(context.Background(), "L1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ProfileJSON != `{"foo":"bar"}` {
		t.Errorf("ProfileJSON = %q", got.ProfileJSON)
	}
}

func TestGetActiveLearners(t *testing.T) {
	store := setupTestDB(t)
	// L1 (no webhook from setupTestDB) won't appear. Add two with webhooks.
	if _, err := store.root.Exec(
		rb(store, `INSERT INTO learners (id, email, password_hash, objective, webhook_url, created_at) VALUES ('L2','b@b','h','o','https://discord.com/x', ?)`),
		time.Now(),
	); err != nil {
		t.Fatalf("insert L2: %v", err)
	}
	if _, err := store.root.Exec(
		rb(store, `INSERT INTO learners (id, email, password_hash, objective, webhook_url, created_at) VALUES ('L3','c@c','h','o','https://discord.com/y', ?)`),
		time.Now(),
	); err != nil {
		t.Fatalf("insert L3: %v", err)
	}
	got, err := store.GetActiveLearners(context.Background())
	if err != nil {
		t.Fatalf("active: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("expected 2 active learners, got %d", len(got))
	}
	// Each must have a non-empty webhook URL.
	for _, l := range got {
		if l.WebhookURL == "" {
			t.Errorf("expected non-empty webhook for %s", l.ID)
		}
	}
}

func TestWebhookPushLogLifecycle(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now().UTC()
	brief := &models.WebhookBrief{
		Kind:              "olm:d1",
		DomainID:          "d1",
		DomainName:        "Python",
		Concept:           "loops",
		Trigger:           "Useful focus",
		PedagogicalIntent: "revisit the right concept",
		LearningGain:      "Stabilize fragile knowledge.",
		OpenLoop:          "I kept a small loop bug.",
		NextAction:        "Open Claude and revisit loops.",
	}
	if _, err := store.CreateWebhookPushLog(context.Background(), "L1", 42, brief, now); err != nil {
		t.Fatalf("CreateWebhookPushLog: %v", err)
	}
	push, err := store.GetLatestOpenWebhookPush(context.Background(), "L1", "d1", now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("GetLatestOpenWebhookPush: %v", err)
	}
	if push == nil || push.Concept != "loops" || push.QueueID != 42 {
		t.Fatalf("unexpected push: %+v", push)
	}
	if err := store.MarkWebhookPushSessionOpened(context.Background(), "L1", now.Add(time.Minute), now.Add(-time.Hour)); err != nil {
		t.Fatalf("MarkWebhookPushSessionOpened: %v", err)
	}
	push, _ = store.GetLatestOpenWebhookPush(context.Background(), "L1", "d1", now.Add(-time.Hour))
	if push == nil || push.OpenedSessionAt == nil {
		t.Fatalf("expected opened session marker, got %+v", push)
	}
	if err := store.MarkWebhookPushConceptAddressed(context.Background(), "L1", "d1", "loops", now.Add(2*time.Minute), now.Add(-time.Hour)); err != nil {
		t.Fatalf("MarkWebhookPushConceptAddressed: %v", err)
	}
	push, err = store.GetLatestOpenWebhookPush(context.Background(), "L1", "d1", now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("GetLatestOpenWebhookPush after address: %v", err)
	}
	if push != nil {
		t.Fatalf("addressed push should no longer be open: %+v", push)
	}
}

// ─── Refresh tokens ─────────────────────────────────────────────────────────

func TestRefreshTokenLifecycle(t *testing.T) {
	store := setupTestDB(t)
	rt, err := store.CreateRefreshToken(context.Background(), "L1", "client-A", testOAuthResource)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if rt.Token == "" || rt.LearnerID != "L1" {
		t.Errorf("unexpected: %+v", rt)
	}

	got, err := store.GetRefreshToken(context.Background(), rt.Token)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Token != rt.Token || got.LearnerID != "L1" {
		t.Errorf("get mismatch: %+v", got)
	}

	if err := store.DeleteRefreshToken(context.Background(), rt.Token); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.GetRefreshToken(context.Background(), rt.Token); err == nil {
		t.Error("expected error after delete")
	}
}

func TestCleanupExpiredRefreshTokens(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now().UTC()
	// Insert one expired and one valid.
	if _, err := store.root.Exec(
		rb(store, `INSERT INTO refresh_tokens (token, learner_id, expires_at, created_at) VALUES ('expired','L1',?,?)`),
		now.Add(-1*time.Hour), now,
	); err != nil {
		t.Fatalf("insert expired: %v", err)
	}
	if _, err := store.root.Exec(
		rb(store, `INSERT INTO refresh_tokens (token, learner_id, expires_at, created_at) VALUES ('valid','L1',?,?)`),
		now.Add(1*time.Hour), now,
	); err != nil {
		t.Fatalf("insert valid: %v", err)
	}
	n, err := store.CleanupExpiredRefreshTokens(context.Background())
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 deleted, got %d", n)
	}
	var count int
	store.root.QueryRow(`SELECT COUNT(*) FROM refresh_tokens`).Scan(&count)
	if count != 1 {
		t.Errorf("expected 1 remaining, got %d", count)
	}
}

func TestCleanupExpiredCodes(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now().UTC()
	if err := store.CreateAuthCodeWithBinding(context.Background(), "c-old", "L1", "ch", "", "client-A", "", testOAuthResource, now.Add(-1*time.Hour)); err != nil {
		t.Fatalf("create old: %v", err)
	}
	if err := store.CreateAuthCodeWithBinding(context.Background(), "c-new", "L1", "ch", "", "client-A", "", testOAuthResource, now.Add(1*time.Hour)); err != nil {
		t.Fatalf("create new: %v", err)
	}
	n, err := store.CleanupExpiredCodes(context.Background())
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 deleted, got %d", n)
	}
	var count int
	store.root.QueryRow(`SELECT COUNT(*) FROM oauth_codes`).Scan(&count)
	if count != 1 {
		t.Errorf("expected 1 remaining, got %d", count)
	}
}

// ─── Domains ────────────────────────────────────────────────────────────────

func makeKS(concepts ...string) models.KnowledgeSpace {
	return models.KnowledgeSpace{
		Concepts:      concepts,
		Prerequisites: map[string][]string{},
	}
}

func TestDomainCRUDAndArchive(t *testing.T) {
	store := setupTestDB(t)
	graph := makeKS("Goroutines", "Channels")
	d, err := store.CreateDomainWithValueFramings(context.Background(), "L1", "go", "ship feature", graph, `{"axis":"financial"}`)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if d.ID == "" {
		t.Fatal("expected non-empty id")
	}

	got, err := store.GetDomainByID(context.Background(), d.ID)
	if err != nil {
		t.Fatalf("by id: %v", err)
	}
	if got.Name != "go" || got.PersonalGoal != "ship feature" {
		t.Errorf("unexpected: %+v", got)
	}
	if got.ValueFramingsJSON != `{"axis":"financial"}` {
		t.Errorf("vf = %q", got.ValueFramingsJSON)
	}
	if len(got.Graph.Concepts) != 2 {
		t.Errorf("graph concepts = %v", got.Graph.Concepts)
	}

	// GetDomainByLearner: most recent active.
	got2, err := store.GetDomainByLearner(context.Background(), "L1")
	if err != nil {
		t.Fatalf("by learner: %v", err)
	}
	if got2.ID != d.ID {
		t.Errorf("expected %s, got %s", d.ID, got2.ID)
	}

	// Updates.
	if err := store.UpdateDomainValueFramings(context.Background(), d.ID, `{"axis":"intellectual"}`); err != nil {
		t.Fatalf("update vf: %v", err)
	}
	if err := store.UpdateDomainLastValueAxis(context.Background(), d.ID, "intellectual"); err != nil {
		t.Fatalf("update axis: %v", err)
	}
	if err := store.UpdateDomainGraph(context.Background(), d.ID, makeKS("Goroutines", "Channels", "Mutexes")); err != nil {
		t.Fatalf("update graph: %v", err)
	}
	got3, _ := store.GetDomainByID(context.Background(), d.ID)
	if got3.ValueFramingsJSON != `{"axis":"intellectual"}` {
		t.Errorf("vf after update: %q", got3.ValueFramingsJSON)
	}
	if got3.LastValueAxis != "intellectual" {
		t.Errorf("axis after update: %q", got3.LastValueAxis)
	}
	if len(got3.Graph.Concepts) != 3 {
		t.Errorf("graph after update: %v", got3.Graph.Concepts)
	}

	// Archive.
	if err := store.ArchiveDomain(context.Background(), d.ID, "L1"); err != nil {
		t.Fatalf("archive: %v", err)
	}
	got4, _ := store.GetDomainByID(context.Background(), d.ID)
	if !got4.Archived {
		t.Error("expected Archived=true after archive")
	}

	// GetDomainByLearner now should return ErrNoRows-wrapped error.
	if _, err := store.GetDomainByLearner(context.Background(), "L1"); err == nil {
		t.Error("expected error after archive")
	}

	// Unarchive.
	if err := store.UnarchiveDomain(context.Background(), d.ID, "L1"); err != nil {
		t.Fatalf("unarchive: %v", err)
	}
	got5, _ := store.GetDomainByID(context.Background(), d.ID)
	if got5.Archived {
		t.Error("expected Archived=false after unarchive")
	}

	// Archive/Unarchive missing returns "not found".
	if err := store.ArchiveDomain(context.Background(), "nope", "L1"); err == nil {
		t.Error("expected error archiving missing")
	}
	if err := store.UnarchiveDomain(context.Background(), "nope", "L1"); err == nil {
		t.Error("expected error unarchiving missing")
	}

	// CreateDomain (legacy entry) wraps CreateDomainWithValueFramings.
	d2, err := store.CreateDomain(context.Background(), "L1", "second", "g2", makeKS("X"))
	if err != nil {
		t.Fatalf("create legacy: %v", err)
	}
	if d2.ID == d.ID {
		t.Error("expected unique id")
	}

	// GetDomainsByLearner: should return both active domains; with includeArchived=false same.
	all, err := store.GetDomainsByLearner(context.Background(), "L1", false)
	if err != nil {
		t.Fatalf("get all: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("expected 2 domains, got %d", len(all))
	}
	// Archive d, then check filter.
	if err := store.ArchiveDomain(context.Background(), d.ID, "L1"); err != nil {
		t.Fatalf("archive again: %v", err)
	}
	active, _ := store.GetDomainsByLearner(context.Background(), "L1", false)
	if len(active) != 1 || active[0].ID != d2.ID {
		t.Errorf("expected only d2 active, got %+v", active)
	}
	includeAll, _ := store.GetDomainsByLearner(context.Background(), "L1", true)
	if len(includeAll) != 2 {
		t.Errorf("expected 2 with includeArchived, got %d", len(includeAll))
	}

	// ActiveDomainConceptSet: union of active domain concepts.
	set, err := store.ActiveDomainConceptSet(context.Background(), "L1")
	if err != nil {
		t.Fatalf("active concept set: %v", err)
	}
	if !set["X"] {
		t.Error("expected X in active set")
	}
	if set["Goroutines"] {
		t.Error("expected Goroutines NOT in set (its domain is archived)")
	}

	// Delete: hides the tombstone from runtime readers.
	if err := store.DeleteDomain(context.Background(), d2.ID, "L1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := store.DeleteDomain(context.Background(), "nope", "L1"); err == nil {
		t.Error("expected error deleting missing")
	}
}

func TestDeleteDomainCleansAuxiliaryRowsAndKeepsPedagogicalHistory(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now().UTC()

	if _, err := store.root.Exec(
		rb(store, `INSERT INTO learners (id, email, password_hash, objective, created_at) VALUES ('L2','other@test.com','hash','test',?)`),
		now,
	); err != nil {
		t.Fatalf("insert L2: %v", err)
	}

	d, err := store.CreateDomain(context.Background(), "L1", "go", "ship feature", makeKS("Goroutines"))
	if err != nil {
		t.Fatalf("create domain: %v", err)
	}
	other, err := store.CreateDomain(context.Background(), "L1", "rust", "ship feature", makeKS("Borrowing"))
	if err != nil {
		t.Fatalf("create other domain: %v", err)
	}

	if _, err := store.InsertImplementationIntentionForSession(context.Background(), "L1", d.ID, "", "after coffee", "review goroutines", now); err != nil {
		t.Fatalf("insert implementation intention: %v", err)
	}
	if _, err := store.InsertImplementationIntentionForSession(context.Background(), "L1", other.ID, "", "after lunch", "review borrowing", now); err != nil {
		t.Fatalf("insert other implementation intention: %v", err)
	}
	if _, err := store.InsertImplementationIntentionForSession(context.Background(), "L2", d.ID, "", "after work", "foreign row", now); err != nil {
		t.Fatalf("insert foreign implementation intention: %v", err)
	}

	if _, err := store.EnqueueWebhookMessage(context.Background(), "L1", "olm:"+d.ID, "domain olm", now, time.Time{}, 0); err != nil {
		t.Fatalf("enqueue domain olm: %v", err)
	}
	structured, err := models.EncodeWebhookBrief(models.WebhookBrief{
		Kind:       models.WebhookKindDailyRecap,
		DomainID:   d.ID,
		WhyNow:     "domain-specific recap",
		NextAction: "resume the domain",
	})
	if err != nil {
		t.Fatal(err)
	}
	structuredID, err := store.EnqueueWebhookMessage(context.Background(), "L1", models.WebhookKindDailyRecap, structured, now, time.Time{}, 0)
	if err != nil {
		t.Fatalf("enqueue structured domain webhook: %v", err)
	}
	if _, err := store.ClaimNextPendingWebhook(context.Background(), "L1", models.WebhookKindDailyRecap, now, time.Minute); err != nil {
		t.Fatalf("claim structured domain webhook: %v", err)
	}
	if _, err := store.EnqueueWebhookMessage(context.Background(), "L1", "olm:"+other.ID, "other olm", now, time.Time{}, 0); err != nil {
		t.Fatalf("enqueue other olm: %v", err)
	}
	if _, err := store.EnqueueWebhookMessage(context.Background(), "L1", "daily_motivation", "daily", now, time.Time{}, 0); err != nil {
		t.Fatalf("enqueue daily: %v", err)
	}
	if _, err := store.EnqueueWebhookMessage(context.Background(), "L2", "olm:"+d.ID, "foreign olm", now, time.Time{}, 0); err != nil {
		t.Fatalf("enqueue foreign olm: %v", err)
	}

	if err := store.UpsertConceptState(context.Background(), &models.ConceptState{
		LearnerID: "L1",
		Concept:   "Goroutines",
		CardState: "review",
		PMastery:  0.7,
	}); err != nil {
		t.Fatalf("upsert concept state: %v", err)
	}
	if err := store.CreateInteraction(context.Background(), &models.Interaction{
		LearnerID:      "L1",
		DomainID:       d.ID,
		Concept:        "Goroutines",
		ActivityType:   "RECALL_EXERCISE",
		Success:        true,
		ResponseTime:   42,
		Confidence:     0.8,
		Notes:          "kept after domain deletion",
		HintsRequested: 0,
	}); err != nil {
		t.Fatalf("create interaction: %v", err)
	}

	count := func(query string, args ...any) int {
		t.Helper()
		var n int
		if err := store.root.QueryRow(rb(store, query), args...).Scan(&n); err != nil {
			t.Fatalf("count query failed: %v", err)
		}
		return n
	}

	if err := store.DeleteDomain(context.Background(), d.ID, "L2"); err == nil {
		t.Fatal("expected error deleting another learner's domain")
	}
	if count(`SELECT COUNT(*) FROM domains WHERE id = ?`, d.ID) != 1 {
		t.Fatal("foreign delete attempt removed the domain")
	}
	if count(`SELECT COUNT(*) FROM implementation_intentions WHERE learner_id = 'L2' AND domain_id = ?`, d.ID) != 1 {
		t.Fatal("foreign delete attempt removed implementation intentions")
	}
	if count(`SELECT COUNT(*) FROM webhook_message_queue WHERE learner_id = 'L2' AND kind = ?`, "olm:"+d.ID) != 1 {
		t.Fatal("foreign delete attempt removed webhook queue rows")
	}

	if err := store.DeleteDomain(context.Background(), d.ID, "L1"); err != nil {
		t.Fatalf("delete domain: %v", err)
	}

	if count(`SELECT COUNT(*) FROM domains WHERE id = ? AND deleted_at IS NOT NULL`, d.ID) != 1 {
		t.Fatal("domain tombstone was not retained")
	}
	if _, err := store.GetDomainByID(context.Background(), d.ID); err == nil {
		t.Fatal("tombstoned domain remained visible to runtime readers")
	}
	if count(`SELECT COUNT(*) FROM implementation_intentions WHERE learner_id = 'L1' AND domain_id = ?`, d.ID) != 1 {
		t.Fatal("implementation intention audit for deleted domain was not preserved")
	}
	if count(`SELECT COUNT(*) FROM implementation_intentions WHERE learner_id = 'L1' AND domain_id = ? AND status = 'cancelled'`, d.ID) != 1 {
		t.Fatal("pending intention for deleted domain was not auditably cancelled")
	}
	if count(`SELECT COUNT(*) FROM webhook_message_queue WHERE learner_id = 'L1' AND kind = ? AND status = 'expired'`, "olm:"+d.ID) != 1 {
		t.Fatal("webhook OLM row for deleted domain was not terminalized")
	}
	if count(`SELECT COUNT(*) FROM webhook_message_queue WHERE id = ? AND domain_id = ? AND status = 'expired'`, structuredID, d.ID) != 1 {
		t.Fatal("claimed structured webhook for deleted domain was not terminalized")
	}
	if active, err := store.IsWebhookClaimActive(context.Background(), structuredID, "L1"); err != nil || active {
		t.Fatalf("deleted-domain claim remained deliverable: active=%v err=%v", active, err)
	}
	if count(`SELECT COUNT(*) FROM implementation_intentions WHERE learner_id = 'L1' AND domain_id = ?`, other.ID) != 1 {
		t.Fatal("implementation intentions for another domain were removed")
	}
	if count(`SELECT COUNT(*) FROM webhook_message_queue WHERE learner_id = 'L1' AND kind = ?`, "olm:"+other.ID) != 1 {
		t.Fatal("webhook OLM rows for another domain were removed")
	}
	if count(`SELECT COUNT(*) FROM webhook_message_queue WHERE learner_id = 'L1' AND kind = 'daily_motivation'`) != 1 {
		t.Fatal("unrelated webhook rows were removed")
	}
	if count(`SELECT COUNT(*) FROM implementation_intentions WHERE learner_id = 'L2' AND domain_id = ?`, d.ID) != 1 {
		t.Fatal("implementation intentions for another learner were removed")
	}
	if count(`SELECT COUNT(*) FROM webhook_message_queue WHERE learner_id = 'L2' AND kind = ?`, "olm:"+d.ID) != 1 {
		t.Fatal("webhook rows for another learner were removed")
	}
	if count(`SELECT COUNT(*) FROM concept_states WHERE learner_id = 'L1' AND concept = 'Goroutines'`) != 1 {
		t.Fatal("concept state was removed")
	}
	if count(`SELECT COUNT(*) FROM interactions WHERE learner_id = 'L1' AND domain_id = ?`, d.ID) != 1 {
		t.Fatal("interaction history was removed")
	}
}

// ─── Concept States ─────────────────────────────────────────────────────────

func TestConceptStateUpsertAndRead(t *testing.T) {
	store := setupTestDB(t)

	// Truncate to microseconds: Postgres TIMESTAMPTZ has microsecond
	// resolution, so a nanosecond-precision time would not round-trip to an
	// exact match. SQLite preserves the truncated value identically.
	now := time.Now().UTC().Truncate(time.Microsecond)
	cs := &models.ConceptState{
		LearnerID:  "L1",
		Concept:    "C1",
		Stability:  2.0,
		Difficulty: 5.5,
		PMastery:   0.3,
		PLearn:     0.4,
		CardState:  "new",
		Reps:       1,
		LastReview: nil,
		NextReview: nil,
		UpdatedAt:  now,
	}
	if err := store.InsertConceptStateIfNotExists(context.Background(), cs); err != nil {
		t.Fatalf("insert if not exists: %v", err)
	}
	// Second call should be a no-op (no error).
	if err := store.InsertConceptStateIfNotExists(context.Background(), cs); err != nil {
		t.Fatalf("insert if not exists (2nd): %v", err)
	}

	// Upsert with a higher mastery value: update path.
	due := now.Add(24 * time.Hour)
	last := now.Add(-24 * time.Hour)
	cs.PMastery = 0.85
	cs.NextReview = &due
	cs.LastReview = &last
	cs.CardState = "review"
	cs.Reps = 5
	if err := store.UpsertConceptState(context.Background(), cs); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := store.GetConceptState(context.Background(), "L1", "C1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.PMastery != 0.85 {
		t.Errorf("PMastery = %v want 0.85", got.PMastery)
	}
	if got.CardState != "review" {
		t.Errorf("CardState = %q want 'review'", got.CardState)
	}
	if got.NextReview == nil || !got.NextReview.Equal(due) {
		t.Errorf("NextReview = %v want %v", got.NextReview, due)
	}
	if got.LastReview == nil || !got.LastReview.Equal(last) {
		t.Errorf("LastReview = %v want %v", got.LastReview, last)
	}

	all, err := store.GetConceptStatesByLearner(context.Background(), "L1")
	if err != nil {
		t.Fatalf("by learner: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected 1 state, got %d", len(all))
	}

	// Missing concept returns wrapped sql.ErrNoRows.
	if _, err := store.GetConceptState(context.Background(), "L1", "missing"); err == nil {
		t.Error("expected error for missing concept")
	}
}

func TestGetConceptsDueForReview(t *testing.T) {
	store := setupTestDB(t)

	now := time.Now().UTC()
	past := now.Add(-1 * time.Hour)
	future := now.Add(1 * time.Hour)
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.root.Exec(rb(store, q), args...); err != nil {
			t.Fatalf("exec: %v", err)
		}
	}
	// Create a domain with all test concepts.
	mustExec(
		`INSERT INTO domains (id, learner_id, name, graph_json, personal_goal, archived, value_framings_json, last_value_axis, created_at)
		 VALUES ('d-test','L1','test','{"concepts":["C-due","C-future","C-new","C-null"],"prerequisites":{}}','goal',0,'','',?)`,
		now,
	)
	// Due (review state, past next_review) — should appear.
	mustExec(
		`INSERT INTO concept_states (learner_id, concept, card_state, next_review, updated_at)
		 VALUES ('L1','C-due','review',?,?)`,
		past, now,
	)
	// Future review — should NOT appear.
	mustExec(
		`INSERT INTO concept_states (learner_id, concept, card_state, next_review, updated_at)
		 VALUES ('L1','C-future','review',?,?)`,
		future, now,
	)
	// New state — should NOT appear (excluded by card_state != 'new').
	mustExec(
		`INSERT INTO concept_states (learner_id, concept, card_state, next_review, updated_at)
		 VALUES ('L1','C-new','new',?,?)`,
		past, now,
	)
	// NULL next_review — should NOT appear.
	mustExec(
		`INSERT INTO concept_states (learner_id, concept, card_state, next_review, updated_at)
		 VALUES ('L1','C-null','review',NULL,?)`,
		now,
	)

	got, err := store.GetConceptsDueForReview(context.Background(), "L1")
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	if len(got) != 1 || got[0] != "C-due" {
		t.Errorf("expected [C-due], got %v", got)
	}
}

// ─── Interactions ────────────────────────────────────────────────────────────

func TestInteractionsLifecycle(t *testing.T) {
	store := setupTestDB(t)

	// Create three interactions; make sure round-trip preserves all bool/int fields.
	mk := func(concept string, success bool, miscType string) *models.Interaction {
		return &models.Interaction{
			LearnerID:           "L1",
			Concept:             concept,
			ActivityType:        "RECALL_EXERCISE",
			Success:             success,
			ResponseTime:        42,
			Confidence:          0.8,
			ErrorType:           "",
			Notes:               "x",
			HintsRequested:      1,
			SelfInitiated:       true,
			CalibrationID:       "cal-1",
			IsProactiveReview:   true,
			MisconceptionType:   miscType,
			MisconceptionDetail: "d",
		}
	}
	for _, args := range []struct {
		concept string
		success bool
		misc    string
	}{
		{"C1", true, "off-by-one"},
		{"C1", false, "off-by-one"},
		{"C2", true, ""},
	} {
		i := mk(args.concept, args.success, args.misc)
		if err := store.CreateInteraction(context.Background(), i); err != nil {
			t.Fatalf("create %+v: %v", args, err)
		}
		if i.ID == 0 {
			t.Errorf("expected non-zero id after Create")
		}
	}

	// GetRecentInteractions filters by concept.
	got, err := store.GetRecentInteractions(context.Background(), "L1", "C1", 10)
	if err != nil {
		t.Fatalf("recent C1: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("expected 2 for C1, got %d", len(got))
	}
	for _, ii := range got {
		if ii.Concept != "C1" {
			t.Errorf("expected concept=C1, got %q", ii.Concept)
		}
		if !ii.SelfInitiated || !ii.IsProactiveReview {
			t.Errorf("flags lost: %+v", ii)
		}
		if ii.CalibrationID != "cal-1" {
			t.Errorf("calibration id lost: %q", ii.CalibrationID)
		}
	}

	// GetRecentInteractionsByLearner returns all 3.
	all, err := store.GetRecentInteractionsByLearner(context.Background(), "L1", 10)
	if err != nil {
		t.Fatalf("recent learner: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("expected 3 total, got %d", len(all))
	}

	// GetSessionInteractions: cutoff is 2h, all rows are fresh.
	sess, err := store.GetSessionInteractions(context.Background(), "L1")
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	if len(sess) != 3 {
		t.Errorf("expected 3 in session, got %d", len(sess))
	}

	// GetInteractionsSince filters by created_at.
	since, err := store.GetInteractionsSince(context.Background(), "L1", time.Now().UTC().Add(-1*time.Hour))
	if err != nil {
		t.Fatalf("since: %v", err)
	}
	if len(since) != 3 {
		t.Errorf("expected 3 since, got %d", len(since))
	}

	// GetSessionStart for a learner with no interactions returns now (non-zero, no error).
	// The MIN() aggregate path is exercised in TestGetSessionStart_Empty below;
	// when rows exist, the modernc/sqlite driver returns MIN(time) as text and
	// the production caller swallows the error (see tools/get_dashboard_state.go).
	start2, err := store.GetSessionStart(context.Background(), "L-missing")
	if err != nil {
		t.Fatalf("session start missing: %v", err)
	}
	if start2.IsZero() {
		t.Error("expected non-zero session start for missing learner")
	}
}

func TestGetSessionStart_Empty(t *testing.T) {
	store := setupTestDB(t)
	ts, err := store.GetSessionStart(context.Background(), "L1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// With no rows the helper returns time.Now() (non-zero).
	if ts.IsZero() {
		t.Errorf("expected non-zero ts, got zero")
	}
}

func TestCreateInteractionPreservesAuthoritativeTimestamp(t *testing.T) {
	store := setupTestDB(t)
	want := time.Date(2025, time.January, 2, 3, 4, 5, 0, time.UTC)
	interaction := &models.Interaction{
		LearnerID:    "L1",
		Concept:      "C-timestamp",
		ActivityType: string(models.ActivityRecall),
		Success:      true,
		CreatedAt:    want,
	}
	if err := store.CreateInteraction(context.Background(), interaction); err != nil {
		t.Fatalf("create interaction: %v", err)
	}
	got, err := store.GetRecentInteractions(context.Background(), "L1", "C-timestamp", 1)
	if err != nil {
		t.Fatalf("get interaction: %v", err)
	}
	if len(got) != 1 || !got[0].CreatedAt.Equal(want) {
		t.Fatalf("created_at = %v, want %v", got, want)
	}
}

// ─── Availability ───────────────────────────────────────────────────────────

func TestAvailability(t *testing.T) {
	store := setupTestDB(t)
	// Default: GetAvailability returns the canonical defaults when no row exists.
	got, err := store.GetAvailability(context.Background(), "L1")
	if err != nil {
		t.Fatalf("default: %v", err)
	}
	if got.AvgDuration != 30 || got.SessionsWeek != 3 || got.WindowsJSON != "[]" || got.DoNotDisturb {
		t.Errorf("default mismatch: %+v", got)
	}

	// Upsert + read-back.
	a := &models.Availability{
		LearnerID:    "L1",
		WindowsJSON:  `[{"day":"Mon","start":"09:00","end":"10:00"}]`,
		AvgDuration:  45,
		SessionsWeek: 5,
		DoNotDisturb: true,
	}
	if err := store.UpsertAvailability(context.Background(), a); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err = store.GetAvailability(context.Background(), "L1")
	if err != nil {
		t.Fatalf("after upsert: %v", err)
	}
	if got.AvgDuration != 45 || got.SessionsWeek != 5 || !got.DoNotDisturb {
		t.Errorf("after upsert: %+v", got)
	}

	// Update via a second upsert (ON CONFLICT path).
	a.AvgDuration = 60
	a.DoNotDisturb = false
	if err := store.UpsertAvailability(context.Background(), a); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	got, _ = store.GetAvailability(context.Background(), "L1")
	if got.AvgDuration != 60 || got.DoNotDisturb {
		t.Errorf("after second upsert: %+v", got)
	}
}

// ─── Scheduled Alerts ───────────────────────────────────────────────────────

func TestScheduledAlerts(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now().UTC()

	if err := store.CreateScheduledAlert(context.Background(), "L1", "FORGETTING", "C1", now); err != nil {
		t.Fatalf("create alert 1: %v", err)
	}
	if err := store.CreateScheduledAlert(context.Background(), "L1", "PLATEAU", "C2", now); err != nil {
		t.Fatalf("create alert 2: %v", err)
	}

	// Alert rows are daily delivery/reservation stamps.
	sent, err := store.WasAlertSentToday(context.Background(), "L1", "FORGETTING")
	if err != nil {
		t.Fatalf("was sent: %v", err)
	}
	if !sent {
		t.Error("expected WasAlertSentToday=true")
	}
	// Different alert type today: false.
	sent, _ = store.WasAlertSentToday(context.Background(), "L1", "OVERLOAD")
	if sent {
		t.Error("expected false for unused type")
	}
}

// ─── Stats for Scheduler ────────────────────────────────────────────────────

// insertInteractionAtSQLTime inserts an interaction storing created_at in the
// "YYYY-MM-DD HH:MM:SS" textual form recognized by SQLite's DATE() function.
// (modernc.org/sqlite serializes time.Time using RFC3339, which DATE() does
// not parse, so we bypass parameter binding for the timestamp.)
func insertInteractionAtSQLTime(t *testing.T, store *Store, concept string, success int, ts time.Time) {
	t.Helper()
	tsStr := ts.UTC().Format("2006-01-02 15:04:05")
	if _, err := store.root.Exec(
		rb(store, `INSERT INTO interactions (learner_id, concept, activity_type, success, created_at)
		 VALUES ('L1', ?, 'RECALL_EXERCISE', ?, ?)`),
		concept, success, tsStr,
	); err != nil {
		t.Fatalf("exec insert: %v", err)
	}
}

func TestStreakAndTodayStats(t *testing.T) {
	store := setupTestDB(t)

	now := time.Now().UTC()
	today := now.Truncate(24 * time.Hour)
	// 3 interactions today (2 success, 1 fail) → success rate 2/3.
	insertInteractionAtSQLTime(t, store, "C1", 1, today.Add(1*time.Hour))
	insertInteractionAtSQLTime(t, store, "C1", 0, today.Add(2*time.Hour))
	insertInteractionAtSQLTime(t, store, "C1", 1, today.Add(3*time.Hour))
	// Yesterday: 1 interaction.
	insertInteractionAtSQLTime(t, store, "C1", 1, today.AddDate(0, 0, -1).Add(2*time.Hour))
	// Day before yesterday: 1 interaction (so streak is 3 days).
	insertInteractionAtSQLTime(t, store, "C1", 1, today.AddDate(0, 0, -2).Add(2*time.Hour))

	count, err := store.GetTodayInteractionCount(context.Background(), "L1")
	if err != nil {
		t.Fatalf("today count: %v", err)
	}
	if count != 3 {
		t.Errorf("today count = %d want 3", count)
	}

	rate, total, err := store.GetTodaySuccessRate(context.Background(), "L1")
	if err != nil {
		t.Fatalf("today rate: %v", err)
	}
	if total != 3 {
		t.Errorf("total = %d want 3", total)
	}
	if rate < 0.6 || rate > 0.7 {
		t.Errorf("rate = %v want ~0.666", rate)
	}

	// Empty learner: rate=0, total=0.
	rate, total, err = store.GetTodaySuccessRate(context.Background(), "L-missing")
	if err != nil {
		t.Fatalf("rate empty: %v", err)
	}
	if rate != 0 || total != 0 {
		t.Errorf("expected (0,0), got (%v,%d)", rate, total)
	}

	streak, err := store.GetDailyStreak(context.Background(), "L1")
	if err != nil {
		t.Fatalf("streak: %v", err)
	}
	if streak != 3 {
		t.Errorf("streak = %d want 3", streak)
	}

	// Empty learner streak = 0.
	streak, err = store.GetDailyStreak(context.Background(), "L-missing")
	if err != nil {
		t.Fatalf("streak empty: %v", err)
	}
	if streak != 0 {
		t.Errorf("streak empty = %d", streak)
	}
}

func TestGetDailyStreak_GapsBreakStreak(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now().UTC().Truncate(24 * time.Hour)
	// Today: yes. 3 days ago: yes. Skipping day -2 then having day -3 must
	// terminate the streak at 1.
	insertInteractionAtSQLTime(t, store, "C", 1, now.Add(1*time.Hour))
	insertInteractionAtSQLTime(t, store, "C", 1, now.AddDate(0, 0, -3).Add(1*time.Hour))
	streak, err := store.GetDailyStreak(context.Background(), "L1")
	if err != nil {
		t.Fatalf("streak: %v", err)
	}
	if streak != 1 {
		t.Errorf("streak with gap = %d want 1", streak)
	}
}

func TestGetDailyStreak_StaleStartReturnsZero(t *testing.T) {
	store := setupTestDB(t)
	// Last interaction is 5 days ago — too stale to start a streak.
	insertInteractionAtSQLTime(t, store, "C", 1, time.Now().UTC().AddDate(0, 0, -5))
	streak, err := store.GetDailyStreak(context.Background(), "L1")
	if err != nil {
		t.Fatalf("streak: %v", err)
	}
	if streak != 0 {
		t.Errorf("streak stale = %d want 0", streak)
	}
}

// ─── GetConceptsDueForReview ─────────────────────────────────────────────

func TestGetConceptsDueForReview_ExcludesArchivedDomain(t *testing.T) {
	store := setupTestDB(t)

	now := time.Now().UTC()
	past := now.Add(-time.Hour) // due

	// Create a second learner (L1 is created by setupTestDB).
	if _, err := store.root.Exec(
		rb(store, `INSERT INTO learners (id, email, password_hash, objective, created_at) VALUES (?, ?, ?, ?, ?)`),
		"L2", "l2@test.com", "h", "obj", now,
	); err != nil {
		t.Fatal(err)
	}
	// Active domain with concept "active_c".
	if _, err := store.root.Exec(
		rb(store, `INSERT INTO domains (id, learner_id, name, graph_json, personal_goal, archived, value_framings_json, last_value_axis, created_at)
		 VALUES ('d_active','L2','active','{"concepts":["active_c"],"prerequisites":{}}','goal',0,'','',?)`),
		now,
	); err != nil {
		t.Fatal(err)
	}
	// Archived domain with concept "archived_c".
	if _, err := store.root.Exec(
		rb(store, `INSERT INTO domains (id, learner_id, name, graph_json, personal_goal, archived, value_framings_json, last_value_axis, created_at)
		 VALUES ('d_arch','L2','arch','{"concepts":["archived_c"],"prerequisites":{}}','goal',1,'','',?)`),
		now,
	); err != nil {
		t.Fatal(err)
	}
	// Concept states: BOTH due, BOTH non-new.
	for _, c := range []string{"active_c", "archived_c"} {
		if _, err := store.root.Exec(
			rb(store, `INSERT INTO concept_states (learner_id, concept, p_mastery, stability, difficulty, elapsed_days, scheduled_days, reps, lapses, card_state, last_review, next_review, p_learn, p_forget, p_slip, p_guess, theta)
			 VALUES (?, ?, 0.5, 5, 5, 1, 1, 1, 0, 'review', ?, ?, 0.15, 0.1, 0.1, 0.2, 0)`),
			"L2", c, past, past,
		); err != nil {
			t.Fatal(err)
		}
	}

	got, err := store.GetConceptsDueForReview(context.Background(), "L2")
	if err != nil {
		t.Fatalf("GetConceptsDueForReview: %v", err)
	}
	if len(got) != 1 || got[0] != "active_c" {
		t.Fatalf("expected only [active_c], got %v", got)
	}
}

// ─── OAuth client confidential ─────────────────────────────────────────────

func TestCreateOAuthClientWithSecret(t *testing.T) {
	store := setupTestDB(t)
	if err := store.CreateOAuthClientWithSecret(context.Background(), "c-conf", "Confidential", `["https://x"]`, "deadbeef"); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := store.GetOAuthClient(context.Background(), "c-conf")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ClientSecretHash != "deadbeef" {
		t.Errorf("secret hash = %q want 'deadbeef'", got.ClientSecretHash)
	}
}

// ─── GetRecentLearnerEvents ─────────────────────────────────────────────────

func TestGetRecentLearnerEvents_ReturnsEstimateThresholdAndStreakStart(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now().UTC()

	// L1 is pre-created by setupTestDB; seed defensively (idempotent).
	seedLearner(t, store, "L1")
	// concept_state with a high model estimate today -> estimate_threshold event.
	// Insert listing the columns we explicitly want; rest take their DEFAULTs.
	if _, err := store.root.Exec(
		rb(store, `INSERT INTO concept_states (learner_id, concept, p_mastery, stability, difficulty, elapsed_days, reps, lapses, card_state, last_review, next_review, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		"L1", "x", 0.85, 5.0, 0.3, 1, 5, 0, "review", now, now, now,
	); err != nil {
		t.Fatal(err)
	}
	// 3 consecutive interactions (today, -1, -2) -> streak_start at oldest.
	for _, off := range []int{0, -1, -2} {
		if _, err := store.root.Exec(
			rb(store, `INSERT INTO interactions (learner_id, concept, activity_type, success, created_at) VALUES (?, 'x', 'RECALL', 1, ?)`),
			"L1", now.AddDate(0, 0, off),
		); err != nil {
			t.Fatal(err)
		}
	}

	events, err := store.GetRecentLearnerEvents(context.Background(), "L1", now.AddDate(0, 0, -7))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	kinds := map[string]bool{}
	for _, e := range events {
		kinds[e.Kind] = true
	}
	if !kinds["estimate_threshold"] {
		t.Errorf("expected estimate_threshold event, got %v", events)
	}
	if !kinds["streak_start"] {
		t.Errorf("expected streak_start event, got %v", events)
	}
}

// ─── GetActivityStreak ──────────────────────────────────────────────────────

func TestGetActivityStreak(t *testing.T) {
	store := setupTestDB(t)
	now := time.Now().UTC()
	day := func(offset int) time.Time { return now.AddDate(0, 0, offset) }

	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.root.Exec(rb(store, q), args...); err != nil {
			t.Fatalf("exec: %v", err)
		}
	}

	// Learner with no interactions → 0
	mustExec(`INSERT INTO learners (id,email,password_hash,objective,created_at) VALUES ('Lzero','z@t.com','h','o',?)`, now)
	got, err := store.GetActivityStreak(context.Background(), "Lzero")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != 0 {
		t.Errorf("zero-interaction learner streak = %d, want 0", got)
	}

	// Learner with 3 consecutive days (today, -1, -2) → 3
	mustExec(`INSERT INTO learners (id,email,password_hash,objective,created_at) VALUES ('L3','3@t.com','h','o',?)`, now)
	for _, off := range []int{0, -1, -2} {
		mustExec(
			`INSERT INTO interactions (learner_id, concept, activity_type, success, created_at) VALUES (?,'c','RECALL',1,?)`,
			"L3", day(off),
		)
	}
	got, err = store.GetActivityStreak(context.Background(), "L3")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != 3 {
		t.Errorf("3-day learner streak = %d, want 3", got)
	}

	// Learner with a hole (today + day -2 but missing -1) → 1
	mustExec(`INSERT INTO learners (id,email,password_hash,objective,created_at) VALUES ('Lhole','h@t.com','h','o',?)`, now)
	for _, off := range []int{0, -2} {
		mustExec(
			`INSERT INTO interactions (learner_id, concept, activity_type, success, created_at) VALUES (?,'c','RECALL',1,?)`,
			"Lhole", day(off),
		)
	}
	got, err = store.GetActivityStreak(context.Background(), "Lhole")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != 1 {
		t.Errorf("hole-pattern learner streak = %d, want 1", got)
	}
}
