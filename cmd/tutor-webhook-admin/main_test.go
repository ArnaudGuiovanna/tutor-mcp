package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"tutor-mcp/db"
	"tutor-mcp/models"
)

func TestParseOptionsRequiresExplicitResolutionIdentity(t *testing.T) {
	for _, args := range [][]string{
		{"--action=resolve", "--learner=L1", "--outcome=delivered"},
		{"--action=resolve", "--learner=L1", "--id=4", "--event-id=wh_event", "--outcome=maybe"},
		{"--action=list", "--learner=L1", "--apply"},
	} {
		if _, err := parseOptions(args, &bytes.Buffer{}, mapEnv(nil)); err == nil {
			t.Fatalf("options unexpectedly accepted: %v", args)
		}
	}
}

func TestRunListsRedactedQuarantineAndRequiresApplyToResolve(t *testing.T) {
	path, id, eventID, sensitiveContent := seedUnknownWebhook(t)
	getenv := mapEnv(map[string]string{"DB_PATH": path})

	var listOut bytes.Buffer
	if err := run([]string{"--action=list", "--learner=L1"}, &listOut, &bytes.Buffer{}, getenv); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(listOut.String(), sensitiveContent) {
		t.Fatalf("list output leaked webhook content: %s", listOut.String())
	}
	var listed listReport
	if err := json.Unmarshal(listOut.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Deliveries) != 1 || listed.Deliveries[0].ID != id || listed.Deliveries[0].EventID != eventID {
		t.Fatalf("list report=%+v", listed)
	}

	resolveArgs := []string{
		"--action=resolve", "--learner=L1", "--id=" + int64String(id),
		"--event-id=" + eventID, "--outcome=not-delivered",
	}
	var previewOut bytes.Buffer
	if err := run(resolveArgs, &previewOut, &bytes.Buffer{}, getenv); err != nil {
		t.Fatal(err)
	}
	assertWebhookStatus(t, path, id, models.WebhookStatusDeliveryUnknown)

	var applyOut bytes.Buffer
	if err := run(append(resolveArgs, "--apply"), &applyOut, &bytes.Buffer{}, getenv); err != nil {
		t.Fatal(err)
	}
	var applied resolutionReport
	if err := json.Unmarshal(applyOut.Bytes(), &applied); err != nil {
		t.Fatal(err)
	}
	if !applied.Applied || applied.Outcome != "not-delivered" {
		t.Fatalf("apply report=%+v", applied)
	}
	assertWebhookStatus(t, path, id, models.WebhookStatusPending)
}

func TestRunRejectsMismatchedEventID(t *testing.T) {
	path, id, _, _ := seedUnknownWebhook(t)
	err := run([]string{
		"--action=resolve", "--learner=L1", "--id=" + int64String(id),
		"--event-id=wh_wrong", "--outcome=delivered", "--apply",
	}, &bytes.Buffer{}, &bytes.Buffer{}, mapEnv(map[string]string{"DB_PATH": path}))
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched event error=%v", err)
	}
	assertWebhookStatus(t, path, id, models.WebhookStatusDeliveryUnknown)
}

func seedUnknownWebhook(t *testing.T) (path string, id int64, eventID, content string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "runtime.db")
	database, err := db.OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(database); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := database.Exec(
		`INSERT INTO learners (id, email, password_hash, objective, created_at)
		 VALUES (?, ?, ?, ?, ?)`, "L1", "operator@test.invalid", "h", "o", now,
	); err != nil {
		t.Fatal(err)
	}
	store := db.NewStore(database)
	availability := models.DefaultAvailability("L1")
	availability.NotificationConsent = true
	availability.NotificationFrequency = models.NotificationFrequencyAsScheduled
	if err := store.UpsertAvailability(context.Background(), availability); err != nil {
		t.Fatal(err)
	}
	content = "sensitive learner-authored payload"
	id, err = store.EnqueueWebhookMessage(context.Background(), "L1", "operator-test", content, now, now.Add(time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	item, err := store.ClaimNextPendingWebhook(context.Background(), "L1", "operator-test", now, time.Minute)
	if err != nil || item == nil {
		t.Fatalf("claim item=%+v err=%v", item, err)
	}
	eventID = item.EventID
	reservationID, prepared, err := store.PrepareWebhookDelivery(
		context.Background(), "L1", "OPERATOR_TEST", "", false, []int64{id}, now,
	)
	if err != nil || !prepared {
		t.Fatalf("prepare reservation=%d prepared=%v err=%v", reservationID, prepared, err)
	}
	if err := store.BeginWebhookDelivery(context.Background(), "L1", []int64{id}, reservationID, now); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkWebhookDeliveryUnknown(
		context.Background(), "L1", []int64{id}, reservationID, "transport_outcome_unknown", now,
	); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return path, id, eventID, content
}

func assertWebhookStatus(t *testing.T, path string, id int64, want string) {
	t.Helper()
	database, err := db.OpenDBReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var got string
	if err := database.QueryRow(`SELECT status FROM webhook_message_queue WHERE id = ?`, id).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("webhook status=%q, want %q", got, want)
	}
}

func int64String(value int64) string {
	return strconv.FormatInt(value, 10)
}

func mapEnv(values map[string]string) func(string) string {
	return func(key string) string {
		if value, ok := values[key]; ok {
			return value
		}
		return ""
	}
}

func TestRunRefusesMissingSQLiteTarget(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.db")
	err := run([]string{"--action=list", "--learner=L1"}, &bytes.Buffer{}, &bytes.Buffer{}, mapEnv(map[string]string{"DB_PATH": missing}))
	if err == nil || !strings.Contains(err.Error(), "refusing to create") {
		t.Fatalf("missing target error=%v", err)
	}
	if _, statErr := os.Stat(missing); !os.IsNotExist(statErr) {
		t.Fatalf("administration command created missing database: %v", statErr)
	}
}
