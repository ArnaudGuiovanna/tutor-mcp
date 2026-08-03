// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestSafeErrorResultDoesNotLogUnderlyingSensitiveMessage(t *testing.T) {
	const marker = "secret-email-and-server-path"
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	result, err := safeErrorResult(logger, "operation failed", errors.New(marker))
	if err != nil || result == nil || !result.IsError {
		t.Fatalf("safeErrorResult = result=%v err=%v", result, err)
	}
	if strings.Contains(logs.String(), marker) {
		t.Fatalf("underlying sensitive error leaked: %q", logs.String())
	}
	if !strings.Contains(logs.String(), "error_type=*errors.errorString") {
		t.Fatalf("safe error class missing: %q", logs.String())
	}
}
