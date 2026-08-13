// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

var ErrNarrativeBackfillConflict = errors.New("memory: local narrative backfill conflict")

type BackfillReport struct {
	Scanned    int64
	Imported   int64
	Reconciled int64
	Conflicts  int64
}

// BackfillLocalNarratives reconciles the legacy Markdown tree into a shared
// backend. Matching checksums are no-ops, absent objects are imported with a
// deterministic mutation ID, and divergent objects are reported without
// overwriting either copy. Source files are deliberately retained for rollback.
func BackfillLocalNarratives(ctx context.Context, backend NarrativeStore, root string) (BackfillReport, error) {
	var report BackfillReport
	if backend == nil {
		return report, errors.New("memory: narrative backfill backend is required")
	}
	root = strings.TrimSpace(root)
	if root == "" {
		return report, errors.New("memory: narrative backfill root is required")
	}
	base := filepath.Join(root, "learners")
	limits, _ := configuredLimits()
	err := filepath.WalkDir(base, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) && path == base {
				return nil
			}
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		key, recognized, err := narrativeKeyFromLegacyPath(base, path)
		if err != nil {
			return err
		}
		if !recognized {
			return nil
		}
		content, err := readNarrativeFile(path, limits.MaxFileBytes)
		if err != nil {
			return err
		}
		report.Scanned++
		checksum := narrativePlaintextChecksum(string(content))
		existing, err := backend.GetNarrative(ctx, key)
		switch {
		case err == nil:
			if existing.Checksum == checksum {
				report.Reconciled++
				return nil
			}
			report.Conflicts++
			return nil
		case errors.Is(err, ErrNarrativeNotFound):
		case err != nil:
			return err
		}

		identity := narrativeBackfillIdentity(key, checksum)
		_, _, err = backend.CompareAndSwapNarrative(
			ctx, key, 0, string(content), "local-backfill:"+identity[:32], identity, limits,
		)
		if errors.Is(err, ErrNarrativeVersionConflict) {
			// Another node may have imported the same file after our read.
			existing, getErr := backend.GetNarrative(ctx, key)
			if getErr == nil && existing.Checksum == checksum {
				report.Reconciled++
				return nil
			}
			if getErr != nil {
				return getErr
			}
			report.Conflicts++
			return nil
		}
		if err != nil {
			return err
		}
		report.Imported++
		return nil
	})
	if os.IsNotExist(err) {
		err = nil
	}
	if err != nil {
		return report, fmt.Errorf("memory: local narrative backfill: %w", err)
	}
	if report.Conflicts > 0 {
		return report, fmt.Errorf("%w: %d divergent objects", ErrNarrativeBackfillConflict, report.Conflicts)
	}
	return report, nil
}

func narrativeKeyFromLegacyPath(base, path string) (NarrativeKey, bool, error) {
	relative, err := filepath.Rel(base, path)
	if err != nil || relative == "." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return NarrativeKey{}, false, fmt.Errorf("memory: invalid legacy narrative path")
	}
	parts := strings.Split(filepath.ToSlash(relative), "/")
	if len(parts) < 2 {
		return NarrativeKey{}, false, nil
	}
	key := NarrativeKey{LearnerID: unsafeSegment(parts[0])}
	filenameKey := func(name string) string {
		return unsafeSegment(strings.TrimSuffix(name, filepath.Ext(name)))
	}
	switch {
	case len(parts) == 2 && parts[1] == "MEMORY.md":
		key.Scope = ScopeMemory
	case len(parts) == 2 && parts[1] == "MEMORY_pending.md":
		key.Scope = ScopeMemoryPending
	case len(parts) == 3 && parts[1] == "sessions":
		key.Scope = ScopeSession
		ts, err := parseSessionKey(parts[2])
		if err != nil {
			return NarrativeKey{}, false, err
		}
		key.Key = ts.UTC().Format(sessionFilenameLayout)
	case len(parts) == 3 && parts[1] == "archives":
		key.Scope, key.Key = ScopeArchive, filenameKey(parts[2])
	case len(parts) == 3 && parts[1] == "concepts":
		key.Scope, key.Key = ScopeConcept, filenameKey(parts[2])
	case len(parts) == 5 && parts[1] == "domains" && parts[3] == "concepts":
		key.Scope = ScopeConcept
		key.DomainID = unsafeSegment(parts[2])
		key.Key = filenameKey(parts[4])
	default:
		return NarrativeKey{}, false, nil
	}
	if err := key.Validate(); err != nil {
		return NarrativeKey{}, false, err
	}
	return key, true, nil
}

func narrativePlaintextChecksum(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func narrativeBackfillIdentity(key NarrativeKey, checksum string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf(
		"%d:%s%d:%s%d:%s%d:%s%s",
		len(key.LearnerID), key.LearnerID,
		len(key.Scope), key.Scope,
		len(key.DomainID), key.DomainID,
		len(key.Key), key.Key,
		checksum,
	)))
	return hex.EncodeToString(sum[:])
}
