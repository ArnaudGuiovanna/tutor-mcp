// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package store

import (
	"errors"
	"fmt"
)

// ValidationError is a caller-correctable rejection of a request. Its message
// is written for the MCP caller and contains only the caller's own input or a
// fixed explanation, never driver output, SQL, filesystem paths or another
// account's data, so transports may return it verbatim.
type ValidationError struct{ Message string }

func (e *ValidationError) Error() string { return e.Message }

// Invalid builds a ValidationError. Format arguments must come from the
// request being rejected or from constants.
func Invalid(format string, args ...any) error {
	return &ValidationError{Message: fmt.Sprintf(format, args...)}
}

// ValidationMessage returns the public message of the first ValidationError
// in err's chain.
func ValidationMessage(err error) (string, bool) {
	var validation *ValidationError
	if errors.As(err, &validation) && validation.Message != "" {
		return validation.Message, true
	}
	return "", false
}
