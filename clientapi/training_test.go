// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT
package clientapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"tutor-mcp/auth"
	"tutor-mcp/db"
	"tutor-mcp/models"
)

func TestTrainingHTTPContractScopesPrivacyAndConflicts(t *testing.T) {
	raw, err := db.OpenDB(t.TempDir() + "/client.db")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if err = db.Migrate(raw); err != nil {
		t.Fatal(err)
	}
	s := db.NewStore(raw)
	if err = s.EnsureInstallation(t.Context(), "local"); err != nil {
		t.Fatal(err)
	}
	learner, err := s.EnsureLocalIdentity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.GetPrincipalForLearner(t.Context(), learner, []string{models.OAuthScopeLearner})
	if err != nil {
		t.Fatal(err)
	}
	domain, err := s.CreateDomain(t.Context(), learner, "Go", "private", models.KnowledgeSpace{Concepts: []string{"variables"}, Prerequisites: map[string][]string{}})
	if err != nil {
		t.Fatal(err)
	}
	api := New(s, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler()
	request := func(actor *models.Principal, action, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest("POST", TrainingPath+action, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if actor != nil {
			ctx, err := auth.WithPrincipal(context.Background(), *actor)
			if err != nil {
				t.Fatal(err)
			}
			r = r.WithContext(ctx)
		}
		w := httptest.NewRecorder()
		api.ServeHTTP(w, r)
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("private reply can be cached")
		}
		return w
	}
	if w := request(nil, "list_training_library", `{}`); w.Code != 401 {
		t.Fatalf("anonymous=%d", w.Code)
	}
	read := p
	read.Scopes = []string{models.OAuthScopeLearnerRead}
	if w := request(&read, "list_training_library", `{"limit":1}`); w.Code != 200 || !strings.Contains(w.Body.String(), domain.ID) {
		t.Fatalf("library=%d %s", w.Code, w.Body.String())
	}
	if w := request(&read, "get_training", `{"id":"personal:foreign"}`); w.Code != 404 {
		t.Fatalf("foreign=%d", w.Code)
	}
	if w := request(&read, "select_conversation_training", `{"id":"personal:`+domain.ID+`","conversation_id":"c","expected_selection":0,"key":"select"}`); w.Code != 403 {
		t.Fatalf("read-only mutation=%d", w.Code)
	}
	body := `{"id":"personal:` + domain.ID + `","conversation_id":"c","expected_selection":0,"key":"select"}`
	if w := request(&p, "select_conversation_training", body); w.Code != 200 {
		t.Fatalf("select=%d %s", w.Code, w.Body.String())
	}
	if w := request(&p, "select_conversation_training", strings.Replace(body, `"select"`, `"conflict"`, 1)); w.Code != 409 {
		t.Fatalf("conflict=%d %s", w.Code, w.Body.String())
	}
	if w := request(&p, "get_conversation_context", `{"conversation_id":"c"}`); w.Code != 200 {
		t.Fatal(w.Code)
	} else {
		var c models.ConversationContext
		if json.Unmarshal(w.Body.Bytes(), &c) != nil || c.DomainID != domain.ID {
			t.Fatal(w.Body.String())
		}
	}
	if w := request(&p, "get_training", `{"id":"personal:`+domain.ID+`","tenant_id":"other"}`); w.Code != 400 {
		t.Fatalf("caller tenant injection=%d", w.Code)
	}
	if w := request(&p, "get_training", `{} {}`); w.Code != 400 {
		t.Fatalf("extra JSON=%d", w.Code)
	}
}
