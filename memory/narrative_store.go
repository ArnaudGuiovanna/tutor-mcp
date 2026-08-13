// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	ErrNarrativeNotFound         = errors.New("memory: narrative object not found")
	ErrNarrativeVersionConflict  = errors.New("memory: narrative version conflict")
	ErrNarrativeCorrupt          = errors.New("memory: narrative object is corrupt")
	ErrNarrativeMutationConflict = errors.New("memory: narrative mutation key conflict")
)

// NarrativeKey is the stable, storage-independent identity of one Markdown
// narrative. Key is a timestamp for sessions, a concept slug for concepts, a
// period for archives, and empty for the two singleton memory scopes.
type NarrativeKey struct {
	TenantID     string
	EnrollmentID string
	LearnerID    string
	DomainID     string
	Scope        Scope
	Key          string
}

func (k NarrativeKey) Validate() error {
	if (k.TenantID == "") != (k.EnrollmentID == "") {
		return errors.New("memory: tenant_id and enrollment_id must be supplied together")
	}
	if strings.TrimSpace(k.LearnerID) == "" {
		return errors.New("memory: learner_id is required")
	}
	switch k.Scope {
	case ScopeMemory, ScopeMemoryPending:
		if k.DomainID != "" || k.Key != "" {
			return fmt.Errorf("memory: singleton scope %q cannot have domain or key", k.Scope)
		}
	case ScopeSession:
		if k.DomainID != "" || strings.TrimSpace(k.Key) == "" {
			return errors.New("memory: session key is required and cannot be domain-scoped")
		}
		if _, err := parseSessionKey(k.Key); err != nil {
			return err
		}
	case ScopeConcept:
		if strings.TrimSpace(k.Key) == "" {
			return errors.New("memory: concept key is required")
		}
	case ScopeArchive:
		if k.DomainID != "" || strings.TrimSpace(k.Key) == "" {
			return errors.New("memory: archive key is required and cannot be domain-scoped")
		}
	default:
		return fmt.Errorf("memory: unsupported scope %q", k.Scope)
	}
	return nil
}

// NarrativeObject is plaintext only at the application boundary. Durable
// backends encrypt Content and verify Checksum before constructing this value.
type NarrativeObject struct {
	NarrativeKey
	Content   string
	Version   int64
	Checksum  string
	SizeBytes int64
	UpdatedAt time.Time
}

type NarrativeMetadata struct {
	NarrativeKey
	Version   int64
	Checksum  string
	SizeBytes int64
	UpdatedAt time.Time
}

type NarrativeStats struct {
	ObjectCount int
	TotalBytes  int64
	ScopeCounts map[Scope]int
}

// NarrativeStore is the shared object-storage port. expectedVersion=0 means
// create-only; positive values are an ETag/CAS precondition. mutationID is
// optional, but when supplied an exact replay must return the original result
// and reuse with different key/content must fail.
type NarrativeStore interface {
	GetNarrative(ctx context.Context, key NarrativeKey) (*NarrativeObject, error)
	CompareAndSwapNarrative(
		ctx context.Context,
		key NarrativeKey,
		expectedVersion int64,
		content string,
		mutationID string,
		mutationFingerprint string,
		limits Limits,
	) (object *NarrativeObject, replayed bool, err error)
	ListNarratives(ctx context.Context, learnerID string, scope Scope, domainID string, limit int) ([]NarrativeMetadata, error)
	NarrativeStats(ctx context.Context, learnerID string) (NarrativeStats, error)
	DeleteNarrativesBefore(ctx context.Context, cutoff time.Time, apply bool) (eligible, applied, held int64, err error)
}

var narrativeStoreState struct {
	sync.RWMutex
	store NarrativeStore
}

// ConfigureNarrativeStore selects a shared backend for all package-level
// memory operations. Passing nil restores the local-file backend.
func ConfigureNarrativeStore(store NarrativeStore) {
	narrativeStoreState.Lock()
	narrativeStoreState.store = store
	narrativeStoreState.Unlock()
}

func configuredNarrativeStore() NarrativeStore {
	narrativeStoreState.RLock()
	defer narrativeStoreState.RUnlock()
	return narrativeStoreState.store
}

func UsingSharedNarrativeStore() bool { return configuredNarrativeStore() != nil }

func narrativeKeyForWrite(req WriteRequest) (NarrativeKey, error) {
	key := NarrativeKey{TenantID: req.TenantID, EnrollmentID: req.EnrollmentID,
		LearnerID: req.LearnerID, DomainID: req.DomainID, Scope: req.Scope}
	switch req.Scope {
	case ScopeSession:
		if req.Timestamp.IsZero() {
			return NarrativeKey{}, errors.New("memory: timestamp is required for session scope")
		}
		key.Key = req.Timestamp.UTC().Format(sessionFilenameLayout)
	case ScopeConcept:
		key.Key = req.ConceptSlug
	case ScopeArchive:
		key.Key = req.Period
	}
	if err := key.Validate(); err != nil {
		return NarrativeKey{}, err
	}
	return key, nil
}

func narrativeKeyForRead(learnerID string, scope Scope, key string) (NarrativeKey, error) {
	result := NarrativeKey{LearnerID: learnerID, Scope: scope, Key: key}
	if scope == ScopeSession {
		ts, err := parseSessionKey(key)
		if err != nil {
			return NarrativeKey{}, err
		}
		result.Key = ts.UTC().Format(sessionFilenameLayout)
	}
	if err := result.Validate(); err != nil {
		return NarrativeKey{}, err
	}
	return result, nil
}

func writeSharedNarrative(ctx context.Context, backend NarrativeStore, req WriteRequest, limits Limits) error {
	key, err := narrativeKeyForWrite(req)
	if err != nil {
		return err
	}
	const maxCASAttempts = 128
	for attempt := 0; attempt < maxCASAttempts; attempt++ {
		current := ""
		expectedVersion := int64(0)
		object, getErr := backend.GetNarrative(ctx, key)
		switch {
		case getErr == nil:
			current = object.Content
			expectedVersion = object.Version
		case errors.Is(getErr, ErrNarrativeNotFound):
		case getErr != nil:
			return getErr
		}

		next, err := applyNarrativeOperation(current, req)
		if err != nil {
			return err
		}
		if int64(len(next)) > limits.MaxFileBytes {
			return fmt.Errorf("%w: file exceeds %d bytes", ErrQuotaExceeded, limits.MaxFileBytes)
		}
		_, _, err = backend.CompareAndSwapNarrative(
			ctx, key, expectedVersion, next, req.MutationID, narrativeMutationFingerprint(key, req), limits,
		)
		if errors.Is(err, ErrNarrativeVersionConflict) {
			continue
		}
		return err
	}
	return fmt.Errorf("%w: retry budget exhausted", ErrNarrativeVersionConflict)
}

func narrativeMutationFingerprint(key NarrativeKey, req WriteRequest) string {
	hash := sha256.New()
	for _, field := range []string{
		key.LearnerID, string(key.Scope), key.DomainID, key.Key,
		string(req.Operation), req.SectionKey, req.Content,
	} {
		_, _ = hash.Write([]byte(fmt.Sprintf("%d:", len(field))))
		_, _ = hash.Write([]byte(field))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func applyNarrativeOperation(current string, req WriteRequest) (string, error) {
	switch req.Operation {
	case OpReplaceFile:
		return req.Content, nil
	case OpAppend:
		next := current
		if next != "" && !strings.HasSuffix(next, "\n") {
			next += "\n"
		}
		next += req.Content
		if !strings.HasSuffix(next, "\n") {
			next += "\n"
		}
		return next, nil
	case OpReplaceSection:
		if strings.TrimSpace(req.SectionKey) == "" {
			return "", errors.New("memory: section_key is required for replace_section")
		}
		return replaceMarkdownSection(current, req.SectionKey, req.Content), nil
	default:
		return "", fmt.Errorf("memory: unsupported operation %q", req.Operation)
	}
}

func listSharedNarrativeKeys(ctx context.Context, backend NarrativeStore, learnerID string, scope Scope, domainID string) ([]string, error) {
	limits, _ := configuredLimits()
	items, err := backend.ListNarratives(ctx, learnerID, scope, domainID, limits.MaxFilesPerLearner)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(items))
	for _, item := range items {
		keys = append(keys, item.Key)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(keys)))
	return keys, nil
}

func NarrativeState(ctx context.Context, learnerID string) (NarrativeStats, error) {
	if backend := configuredNarrativeStore(); backend != nil {
		return backend.NarrativeStats(ctx, learnerID)
	}
	base, err := learnerDir(learnerID)
	if err != nil {
		return NarrativeStats{}, err
	}
	limits, _ := configuredLimits()
	stats := NarrativeStats{ScopeCounts: make(map[Scope]int)}
	err = filepath.WalkDir(base, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || filepath.Ext(entry.Name()) != ".md" {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() > limits.MaxFileBytes {
			return nil
		}
		stats.ObjectCount++
		stats.TotalBytes += info.Size()
		relative, _ := filepath.Rel(base, path)
		parts := strings.Split(filepath.ToSlash(relative), "/")
		switch {
		case len(parts) == 1 && parts[0] == "MEMORY.md":
			stats.ScopeCounts[ScopeMemory]++
		case len(parts) == 1 && parts[0] == "MEMORY_pending.md":
			stats.ScopeCounts[ScopeMemoryPending]++
		case len(parts) >= 2 && parts[0] == "sessions":
			stats.ScopeCounts[ScopeSession]++
		case len(parts) >= 2 && parts[0] == "archives":
			stats.ScopeCounts[ScopeArchive]++
		case len(parts) >= 2 && parts[0] == "concepts", len(parts) >= 4 && parts[0] == "domains" && parts[2] == "concepts":
			stats.ScopeCounts[ScopeConcept]++
		}
		return nil
	})
	if os.IsNotExist(err) {
		return NarrativeStats{}, nil
	}
	return stats, err
}

func NarrativeModifiedAt(ctx context.Context, key NarrativeKey) time.Time {
	updatedAt, _ := NarrativeUpdatedAt(ctx, key)
	return updatedAt
}

func NarrativeUpdatedAt(ctx context.Context, key NarrativeKey) (time.Time, error) {
	if backend := configuredNarrativeStore(); backend != nil {
		object, err := backend.GetNarrative(ctx, key)
		if err == nil {
			return object.UpdatedAt.UTC(), nil
		}
		return time.Time{}, err
	}
	path, err := pathFor(WriteRequest{
		LearnerID: key.LearnerID, DomainID: key.DomainID, Scope: key.Scope,
		ConceptSlug: key.Key, Period: key.Key,
	})
	if key.Scope == ScopeSession {
		ts, parseErr := parseSessionKey(key.Key)
		if parseErr != nil {
			return time.Time{}, parseErr
		}
		path, err = pathFor(WriteRequest{LearnerID: key.LearnerID, Scope: key.Scope, Timestamp: ts})
	}
	if err != nil {
		return time.Time{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}, err
	}
	return info.ModTime().UTC(), nil
}
