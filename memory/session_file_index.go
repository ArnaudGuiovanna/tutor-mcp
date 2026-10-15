package memory

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const fileSessionIndexName = ".session-index.json"
const fileSessionIndexDirty = ".session-index-dirty"

type fileSessionEntry struct {
	DomainDigest string `json:"domain_digest"`
	Size         int64  `json:"size"`
	Modified     int64  `json:"modified"`
	info         os.FileInfo
}

type fileSessionIndex struct {
	Version int                         `json:"version"`
	Entries map[string]fileSessionEntry `json:"entries"`
}

var fileSessionIndexes = struct {
	sync.Mutex
	items map[string]fileSessionIndex
}{items: make(map[string]fileSessionIndex)}

func invalidateFileSessionIndex(base string) {
	fileSessionIndexes.Lock()
	delete(fileSessionIndexes.items, base)
	fileSessionIndexes.Unlock()
}

// The durable dirty marker is written before a session rename. A crash or a
// CommittedWriteError cannot leave an apparently clean index of old content.
func dirtyFileSessionIndex(base string) error {
	if err := atomicWrite(filepath.Join(base, fileSessionIndexDirty), "dirty\n"); err != nil {
		return fmt.Errorf("memory: cannot durably prepare session index invalidation: %v", err)
	}
	invalidateFileSessionIndex(base)
	return nil
}

// RebuildFileSessionIndex is also the operator repair for external tools that
// intentionally preserve file size and modification time. Ordinary external
// edits/additions/removals/renames are detected from the file manifest on reads.
func RebuildFileSessionIndex(ctx context.Context, learnerID string) (int, error) {
	base, err := learnerDir(learnerID)
	if err != nil {
		return 0, err
	}
	return rebuildFileSessionIndexAt(ctx, base)
}

func rebuildFileSessionIndexAt(ctx context.Context, base string) (int, error) {
	info, err := os.Stat(base)
	if err != nil || !info.IsDir() {
		return 0, ErrSessionIndexIncomplete
	}
	lock := memoryWriteLock(base)
	if err := lockSessionIndexContext(ctx, lock); err != nil {
		return 0, err
	}
	defer lock.Unlock()
	// Force rebuild is the remedy for metadata-preserving external edits.
	// Invalidate the old projection before scanning so any failure/cancellation
	// leaves an explicit incomplete state, including across process restarts.
	if err := dirtyFileSessionIndex(base); err != nil {
		return 0, err
	}
	index, err := loadFileSessionIndex(ctx, base, true)
	if err != nil {
		return 0, err
	}
	return len(index.Entries), nil
}

// RebuildFileSessionIndexes repairs existing legacy trees without creating
// profiles. A corrupt learner cannot prevent repair of later learners.
func RebuildFileSessionIndexes(ctx context.Context) (int, error) {
	return RebuildFileSessionIndexesAt(ctx, Root())
}

func RebuildFileSessionIndexesAt(ctx context.Context, root string) (int, error) {
	if root == "" {
		return 0, fmt.Errorf("memory: existing memory root is required")
	}
	return walkFileSessionLearners(ctx, root, func(base string) (int, error) { return rebuildFileSessionIndexAt(ctx, base) })
}

func walkFileSessionLearners(ctx context.Context, root string, fn func(string) (int, error)) (int, error) {
	directory, err := os.Open(filepath.Join(root, "learners"))
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer directory.Close()
	total := 0
	failed := false
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		entries, readErr := directory.ReadDir(128)
		for _, entry := range entries {
			if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
				continue
			}
			count, err := fn(filepath.Join(root, "learners", entry.Name()))
			if ctx.Err() != nil {
				return total, ctx.Err()
			}
			total += count
			if err != nil {
				failed = true
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return total, readErr
		}
	}
	if failed {
		return total, ErrSessionIndexIncomplete
	}
	return total, nil
}

// Directory pages and the quota check precede allocation of a full manifest.
func boundedSessionDirectory(ctx context.Context, path string, limit int) ([]os.DirEntry, error) {
	directory, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	var result []os.DirEntry
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entries, err := directory.ReadDir(128)
		if len(result)+len(entries) > limit {
			return nil, ErrQuotaExceeded
		}
		result = append(result, entries...)
		if errors.Is(err, io.EOF) {
			return result, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// Caller holds the learner lock. Validation lists/stats files, but only changed
// files need their bodies read. Steady-state loading reads three matching
// bodies regardless of the number of sessions belonging to other domains.
func loadFileSessionIndex(ctx context.Context, base string, force bool) (fileSessionIndex, error) {
	limits, _ := configuredLimits()
	index := fileSessionIndex{Version: 1, Entries: make(map[string]fileSessionEntry)}
	known := false
	_, dirtyErr := os.Stat(filepath.Join(base, fileSessionIndexDirty))
	dirty := dirtyErr == nil
	if dirty && !force {
		return index, ErrSessionIndexIncomplete
	}
	if dirtyErr != nil && !os.IsNotExist(dirtyErr) {
		return index, dirtyErr
	}
	if !force && !dirty {
		fileSessionIndexes.Lock()
		cached, ok := fileSessionIndexes.items[base]
		fileSessionIndexes.Unlock()
		if ok {
			index = cached
			known = true
		} else {
			data, err := readNarrativeFile(filepath.Join(base, fileSessionIndexName), int64(limits.MaxFilesPerLearner)*512+1024)
			if err == nil {
				if json.Unmarshal(data, &index) != nil || index.Version != 1 || index.Entries == nil {
					index = fileSessionIndex{Version: 1, Entries: make(map[string]fileSessionEntry)}
				} else {
					known = true
				}
			} else if !os.IsNotExist(err) {
				return index, err
			}
		}
	}
	entries, err := boundedSessionDirectory(ctx, filepath.Join(base, "sessions"), limits.MaxFilesPerLearner)
	if os.IsNotExist(err) {
		entries = nil
		err = nil
	}
	if err != nil {
		return index, err
	}
	next := fileSessionIndex{Version: 1, Entries: make(map[string]fileSessionEntry)}
	changed := force || dirty || !known
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return next, err
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || filepath.Ext(entry.Name()) != ".md" {
			continue
		}
		key := strings.TrimSuffix(entry.Name(), ".md")
		if _, err := parseSessionKey(key); err != nil {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return next, err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if len(next.Entries) >= limits.MaxFilesPerLearner {
			return next, ErrQuotaExceeded
		}
		previous, ok := index.Entries[key]
		if ok {
			digest, err := hex.DecodeString(previous.DomainDigest)
			if err != nil || len(digest) != 32 || hex.EncodeToString(digest) != previous.DomainDigest {
				return next, ErrSessionIndexIncomplete
			}
		}
		current := fileSessionEntry{Size: info.Size(), Modified: info.ModTime().UnixNano(), info: info}
		if ok && previous.Size == current.Size && previous.Modified == current.Modified && (previous.info == nil || os.SameFile(previous.info, info)) {
			current.DomainDigest = previous.DomainDigest
		} else {
			if !force {
				return next, ErrSessionIndexIncomplete
			}
			raw, err := readNarrativeFile(filepath.Join(base, "sessions", entry.Name()), limits.MaxFileBytes)
			if err != nil {
				return next, err
			}
			domain, parseErr := SessionDomainID(string(raw))
			err = parseErr
			current.DomainDigest = SessionDomainDigest(domain)
			if err != nil {
				return next, err
			}
			// Refuse a concurrent external rewrite instead of caching its domain
			// under the stat of another version.
			after, err := os.Stat(filepath.Join(base, "sessions", entry.Name()))
			if err != nil || !os.SameFile(info, after) || info.Size() != after.Size() || !info.ModTime().Equal(after.ModTime()) {
				return next, ErrSessionIndexIncomplete
			}
			changed = true
		}
		next.Entries[key] = current
	}
	if len(next.Entries) != len(index.Entries) {
		if !force {
			return next, ErrSessionIndexIncomplete
		}
		changed = true
	}
	if changed && force {
		// A previous content rename may have reported a directory-sync error.
		// Complete that durability boundary before publishing a clean index.
		sessionDir := filepath.Join(base, "sessions")
		if _, err := os.Stat(sessionDir); err == nil {
			if err := syncNarrativeDirectory(sessionDir); err != nil {
				return next, err
			}
		} else if !os.IsNotExist(err) {
			return next, err
		}
		if err := persistFileSessionIndex(base, next); err != nil {
			return next, err
		}
	} else {
		cacheFileSessionIndex(base, next)
	}

	return next, nil
}

func recentFileSessionPayloads(ctx context.Context, learnerID, domainID string, limit int) ([]SessionPayload, error) {
	base, err := learnerDir(learnerID)
	if err != nil {
		return nil, err
	}
	lock := memoryWriteLock(base)
	if err := lockSessionIndexContext(ctx, lock); err != nil {
		return nil, err
	}
	defer lock.Unlock()
	index, err := loadFileSessionIndex(ctx, base, false)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, limit+1)
	wantedDigest := SessionDomainDigest(domainID)
	for key, entry := range index.Entries {
		if domainID == "" || entry.DomainDigest == wantedDigest {
			keys = append(keys, key)
			sort.Slice(keys, func(i, j int) bool {
				left, _ := parseSessionKey(keys[i])
				right, _ := parseSessionKey(keys[j])
				if left.Equal(right) {
					return keys[i] < keys[j]
				}
				return left.After(right)
			})
			if len(keys) > limit {
				keys = keys[:limit]
			}
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		left, _ := parseSessionKey(keys[i])
		right, _ := parseSessionKey(keys[j])
		if left.Equal(right) {
			return keys[i] < keys[j]
		}
		return left.After(right)
	})
	if len(keys) > limit {
		keys = keys[:limit]
	}
	limits, _ := configuredLimits()
	result := make([]SessionPayload, 0, len(keys))
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		raw, err := readNarrativeFile(filepath.Join(base, "sessions", key+".md"), limits.MaxFileBytes)
		if err != nil {
			return nil, err
		}
		actual, err := SessionDomainID(string(raw))
		if err != nil {
			return nil, err
		}
		if SessionDomainDigest(actual) != index.Entries[key].DomainDigest || (domainID != "" && actual != domainID) {
			if err := dirtyFileSessionIndex(base); err != nil {
				return nil, fmt.Errorf("%w: invalidation failed", ErrSessionIndexIncomplete)
			}
			return nil, ErrSessionIndexIncomplete
		}
		ts, _ := parseSessionKey(key)
		payload, err := ParseSessionPayload(ts, string(raw))
		if err != nil {
			return nil, err
		}
		result = append(result, payload)
	}
	return result, nil
}

func lockSessionIndexContext(ctx context.Context, lock *sync.Mutex) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if lock.TryLock() {
		return nil
	}
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if lock.TryLock() {
				return nil
			}
		}
	}
}

func cacheFileSessionIndex(base string, index fileSessionIndex) {
	fileSessionIndexes.Lock()
	defer fileSessionIndexes.Unlock()
	delete(fileSessionIndexes.items, base)
	count := len(index.Entries)
	for _, entry := range fileSessionIndexes.items {
		count += len(entry.Entries)
	}
	for len(fileSessionIndexes.items) >= 64 || count > 100_000 {
		if len(fileSessionIndexes.items) == 0 {
			return
		}
		for key, entry := range fileSessionIndexes.items {
			count -= len(entry.Entries)
			delete(fileSessionIndexes.items, key)
			break
		}
	}
	fileSessionIndexes.items[base] = index
}

func persistFileSessionIndex(base string, index fileSessionIndex) error {
	data, err := json.Marshal(index)
	if err != nil {
		return err
	}
	if err := atomicWrite(filepath.Join(base, fileSessionIndexName), string(data)); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(base, fileSessionIndexDirty)); err != nil && !os.IsNotExist(err) {
		return err
	}
	cacheFileSessionIndex(base, index)
	return nil
}

// RepairFileSessionIndexes skips clean manifests and only rebuilds stale ones.
// It belongs to startup/maintenance, never the request loader.
func RepairFileSessionIndexes(ctx context.Context) (int, error) {
	return walkFileSessionLearners(ctx, Root(), func(base string) (int, error) {
		lock := memoryWriteLock(base)
		if err := lockSessionIndexContext(ctx, lock); err != nil {
			return 0, err
		}
		_, err := loadFileSessionIndex(ctx, base, false)
		lock.Unlock()
		if errors.Is(err, ErrSessionIndexIncomplete) {
			return rebuildFileSessionIndexAt(ctx, base)
		}
		return 0, err
	})
}
