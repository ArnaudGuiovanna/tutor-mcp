// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"syscall"
	"testing"

	storeport "tutor-mcp/store"

	"github.com/jackc/pgx/v5/pgconn"
)

func safeErrorLogs(t *testing.T, publicMsg string, err error) (string, string) {
	t.Helper()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	result, callErr := safeErrorResult(logger, publicMsg, err)
	if callErr != nil || result == nil || !result.IsError {
		t.Fatalf("safeErrorResult = result=%v err=%v", result, callErr)
	}
	return resultText(result), logs.String()
}

// Application error messages can quote concept labels, learner identifiers or
// curriculum content: only the error types reach the log, and the caller still
// sees only the public message.
func TestSafeErrorResultKeepsApplicationErrorTextOutOfLog(t *testing.T) {
	const marker = "learner-written concept label"
	err := fmt.Errorf("initialize concept %q: %w", marker, &customRootError{})
	text, logs := safeErrorLogs(t, "operation failed", err)
	if text != "operation failed" {
		t.Fatalf("public message changed: %q", text)
	}
	if strings.Contains(logs, marker) {
		t.Fatalf("application error text leaked into log: %q", logs)
	}
	if !strings.Contains(logs, "error_type=*fmt.wrapError") || !strings.Contains(logs, "root_type=*tools.customRootError") {
		t.Fatalf("error types missing from log: %q", logs)
	}
}

type customRootError struct{}

func (*customRootError) Error() string { return "root detail with learner content" }

// Known sentinels carry a fixed message, logged even when the wrapping text
// is not.
func TestSafeErrorResultLogsKnownSentinel(t *testing.T) {
	const marker = "learner-42"
	err := fmt.Errorf("load %s: %w", marker, storeport.ErrNotFound)
	_, logs := safeErrorLogs(t, "operation failed", err)
	if strings.Contains(logs, marker) || !strings.Contains(logs, `sentinel="store item not found"`) {
		t.Fatalf("sentinel logging wrong: %q", logs)
	}
}

// Filesystem errors name the memory file, which encodes the learner, domain
// and concept: only the operation and errno are logged.
func TestSafeErrorResultLogsOnlyOperationForFilesystemErrors(t *testing.T) {
	const path = "/var/lib/tutor/memory/learner-42/domain-7/concepts/fractions.md"
	for name, err := range map[string]error{
		"path":    fmt.Errorf("write memory: %w", &fs.PathError{Op: "open", Path: path, Err: syscall.ENOSPC}),
		"link":    &os.LinkError{Op: "rename", Old: path + ".tmp", New: path, Err: syscall.EXDEV},
		"syscall": fmt.Errorf("sync %s: %w", path, os.NewSyscallError("fsync", syscall.EIO)),
	} {
		text, logs := safeErrorLogs(t, "memory write failed", err)
		if text != "memory write failed" || strings.Contains(logs, "learner-42") || strings.Contains(logs, "fractions") {
			t.Fatalf("%s: path leaked: text=%q logs=%q", name, text, logs)
		}
		if !strings.Contains(logs, "os_op=") || !strings.Contains(logs, "errno=") {
			t.Fatalf("%s: operation or errno missing: %q", name, logs)
		}
	}
}

// Driver errors may quote learner values, SQL or connection details: only the
// SQLSTATE reaches the log, whether the typed error survived or not.
func TestSafeErrorResultLogsOnlySQLStateForDriverErrors(t *testing.T) {
	const secret = "learner-secret@example.org"
	typed := fmt.Errorf("insert learner: %w", &pgconn.PgError{Code: "23505", Message: "duplicate key value violates unique constraint", Detail: "Key (email)=(" + secret + ") already exists."})
	flattened := fmt.Errorf("insert learner: %v", typed)
	sqlite := errors.New("constraint failed: UNIQUE constraint failed: learners.email (" + secret + ")")
	for name, err := range map[string]error{"typed": typed, "flattened": flattened, "sqlite": sqlite} {
		text, logs := safeErrorLogs(t, "operation failed", err)
		if text != "operation failed" || strings.Contains(logs, secret) || strings.Contains(logs, "learners.email") {
			t.Fatalf("%s: driver text leaked: text=%q logs=%q", name, text, logs)
		}
		if name != "sqlite" && !strings.Contains(logs, "sqlstate=23505") {
			t.Fatalf("%s: SQLSTATE missing from log: %q", name, logs)
		}
	}
}

// Validation errors are written for the caller and are returned verbatim.
func TestSafeErrorResultReturnsValidationReason(t *testing.T) {
	err := fmt.Errorf("training action: %w", storeport.Invalid("invalid filter"))
	text, logs := safeErrorLogs(t, "training action rejected", err)
	if text != "training action rejected: invalid filter" {
		t.Fatalf("validation reason not returned: %q", text)
	}
	if strings.Contains(logs, "level=ERROR") {
		t.Fatalf("caller rejection logged as a server error: %q", logs)
	}
}

// A validation reason may quote the caller's own curriculum content: it is
// returned to the caller, while the log records only its class.
func TestSafeErrorResultKeepsValidationReasonOutOfLog(t *testing.T) {
	const content = "Mes notes privées sur les dérivées"
	err := fmt.Errorf("update curriculum: %w", &storeport.ValidationError{Message: "concept label too long: " + content})
	text, logs := safeErrorLogs(t, "curriculum update rejected", err)
	if !strings.Contains(text, content) {
		t.Fatalf("validation reason not returned to the caller: %q", text)
	}
	if strings.Contains(logs, content) || !strings.Contains(logs, "error_class=validation") {
		t.Fatalf("validation reason logged: %q", logs)
	}
	if attrs := fmt.Sprint(diagnosticErrorAttrs(err)); strings.Contains(attrs, content) {
		t.Fatalf("diagnostic attributes carry the validation reason: %s", attrs)
	}
}
