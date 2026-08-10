// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

type failingOpenLearningSessionStore struct {
	storeport.Store
	err error
}

func (s *failingOpenLearningSessionStore) OpenLearningSession(context.Context, string, string, string, time.Time) (*models.LearningSession, error) {
	return nil, s.err
}

func TestResolveOpenLearningSessionDistinguishesRequestAndBackendFailures(t *testing.T) {
	store, deps := setupToolsTest(t)
	now := time.Now().UTC()

	_, err := resolveOpenLearningSession(context.Background(), deps, "L_owner", "", "missing", now)
	var requestErr *learningSessionRequestError
	if !errors.As(err, &requestErr) || requestErr.Error() != "learning session not found" {
		t.Fatalf("missing session error = %T %v", err, err)
	}

	secretPath := "/private/tenant/L_owner/sessions.db"
	deps.Store = &failingLearningSessionLookupStore{
		Store:   store,
		byIDErr: &os.PathError{Op: "read", Path: secretPath, Err: os.ErrPermission},
	}
	_, err = resolveOpenLearningSession(context.Background(), deps, "L_owner", "", "session", now)
	if err == nil || errors.As(err, &requestErr) {
		t.Fatalf("backend lookup failure was classified as caller error: %T %v", err, err)
	}
	result := learningSessionResolutionErrorResult(deps, err)
	if !result.IsError || resultText(result) != "failed to resolve learning session" {
		t.Fatalf("backend lookup result = error=%v text=%q", result.IsError, resultText(result))
	}
	if strings.Contains(resultText(result), secretPath) {
		t.Fatalf("backend lookup leaked a path: %q", resultText(result))
	}

	deps.Store = &failingOpenLearningSessionStore{
		Store: store,
		err:   &os.PathError{Op: "write", Path: secretPath, Err: os.ErrPermission},
	}
	_, err = resolveOpenLearningSession(context.Background(), deps, "L_owner", "", "", now)
	result = learningSessionResolutionErrorResult(deps, err)
	if !result.IsError || resultText(result) != "failed to resolve learning session" || strings.Contains(resultText(result), secretPath) {
		t.Fatalf("backend open result = error=%v text=%q", result.IsError, resultText(result))
	}
}
