// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package memory

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type RetentionResult struct {
	Eligible int64
	Applied  int64
	Held     int64
}

var removeRetentionFile = os.Remove

// RunRetention previews or removes old Markdown narrative files below the
// configured memory root. It never follows symlinks or removes directories.
// The caller must coordinate with the server process before apply mode.
func RunRetention(cutoff time.Time, apply bool) (RetentionResult, error) {
	return RunRetentionWithLegalHolds(cutoff, apply, nil)
}

// RunRetentionWithLegalHolds applies the same policy while preserving every
// narrative owned by a learner under an active durable hold. Shared database
// backends enforce their holds inside the deletion transaction; local files
// use the supplied snapshot from the retention coordinator.
func RunRetentionWithLegalHolds(cutoff time.Time, apply bool, heldLearners map[string]bool) (RetentionResult, error) {
	var result RetentionResult
	if cutoff.IsZero() {
		return result, fmt.Errorf("memory retention cutoff is required")
	}
	if backend := configuredNarrativeStore(); backend != nil {
		eligible, applied, held, err := backend.DeleteNarrativesBefore(context.Background(), cutoff, apply)
		return RetentionResult{Eligible: eligible, Applied: applied, Held: held}, err
	}
	base := filepath.Join(Root(), "learners")
	err := filepath.WalkDir(base, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) && path == base {
				return nil
			}
			return walkErr
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) {
			return nil
		}
		relative, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		parts := strings.Split(relative, string(filepath.Separator))
		if len(parts) > 1 && heldLearners[parts[0]] {
			result.Held++
			return nil
		}
		result.Eligible++
		if apply {
			if err := removeRetentionFile(path); err != nil {
				return err
			}
			result.Applied++
		}
		return nil
	})
	if os.IsNotExist(err) {
		return result, nil
	}
	if err != nil {
		return result, fmt.Errorf("memory retention: %w", err)
	}
	return result, nil
}
