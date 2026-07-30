// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package memory

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Scope string

const (
	ScopeMemory        Scope = "memory"
	ScopeMemoryPending Scope = "memory_pending"
	ScopeSession       Scope = "session"
	ScopeConcept       Scope = "concept"
	ScopeArchive       Scope = "archive"
)

type Operation string

const (
	OpAppend         Operation = "append"
	OpReplaceSection Operation = "replace_section"
	OpReplaceFile    Operation = "replace_file"
)

type WriteRequest struct {
	LearnerID   string
	Scope       Scope
	ConceptSlug string
	Period      string
	Timestamp   time.Time
	Operation   Operation
	Content     string
	SectionKey  string
}

const sessionFilenameLayout = "2006-01-02T15-04-05Z"

// writeLocks serialize read-modify-write operations on narrative-memory files
// inside one server process. A fixed set of shards keeps the lock table
// bounded while allowing unrelated learners/files to be written concurrently.
// Multi-node deployments must still disable this node-local backend.
var writeLocks [64]sync.Mutex

func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TUTOR_MCP_MEMORY_ENABLED"))) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

func Root() string {
	if root := strings.TrimSpace(os.Getenv("TUTOR_MCP_MEMORY_ROOT")); root != "" {
		return root
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".tutor-mcp"
	}
	return filepath.Join(home, ".tutor-mcp")
}

func EnsureLearnerDirs(learnerID string) error {
	base, err := learnerDir(learnerID)
	if err != nil {
		return err
	}
	for _, dir := range []string{
		base,
		filepath.Join(base, "sessions"),
		filepath.Join(base, "archives"),
		filepath.Join(base, "concepts"),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("memory: create %s: %w", dir, err)
		}
	}
	return nil
}

func Write(req WriteRequest) error {
	if !Enabled() {
		return errors.New("memory: not_enabled")
	}
	if req.LearnerID == "" {
		return errors.New("memory: learner_id is required")
	}
	if req.Operation == "" {
		req.Operation = defaultOperation(req.Scope)
	}
	if err := EnsureLearnerDirs(req.LearnerID); err != nil {
		return err
	}
	path, err := pathFor(req)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("memory: create parent: %w", err)
	}

	lock := memoryWriteLock(path)
	lock.Lock()
	defer lock.Unlock()

	switch req.Operation {
	case OpReplaceFile:
		return atomicWrite(path, req.Content)
	case OpAppend:
		current, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("memory: read for append: %w", err)
		}
		next := string(current)
		if next != "" && !strings.HasSuffix(next, "\n") {
			next += "\n"
		}
		next += req.Content
		if !strings.HasSuffix(next, "\n") {
			next += "\n"
		}
		return atomicWrite(path, next)
	case OpReplaceSection:
		if strings.TrimSpace(req.SectionKey) == "" {
			return errors.New("memory: section_key is required for replace_section")
		}
		currentBytes, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("memory: read for replace_section: %w", err)
		}
		next := replaceMarkdownSection(string(currentBytes), req.SectionKey, req.Content)
		return atomicWrite(path, next)
	default:
		return fmt.Errorf("memory: unsupported operation %q", req.Operation)
	}
}

func Read(learnerID string, scope Scope, key string) (string, error) {
	if !Enabled() {
		return "", nil
	}
	req := WriteRequest{LearnerID: learnerID, Scope: scope}
	switch scope {
	case ScopeConcept:
		req.ConceptSlug = key
	case ScopeArchive:
		req.Period = key
	case ScopeSession:
		ts, err := parseSessionKey(key)
		if err != nil {
			return "", err
		}
		req.Timestamp = ts
	}
	path, err := pathFor(req)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("memory: read %s: %w", scope, err)
	}
	return string(data), nil
}

func ListSessions(learnerID string) ([]time.Time, error) {
	base, err := learnerDir(learnerID)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(base, "sessions"))
	if os.IsNotExist(err) {
		return []time.Time{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("memory: list sessions: %w", err)
	}
	out := make([]time.Time, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" {
			continue
		}
		stem := strings.TrimSuffix(entry.Name(), ".md")
		ts, err := time.ParseInLocation(sessionFilenameLayout, stem, time.UTC)
		if err != nil {
			continue
		}
		out = append(out, ts.UTC())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].After(out[j]) })
	return out, nil
}

func ListArchives(learnerID string) ([]string, error) {
	return listMarkdownKeys(learnerID, "archives", false)
}

func ListConcepts(learnerID string) ([]string, error) {
	return listMarkdownKeys(learnerID, "concepts", true)
}

func PathForRead(learnerID string, scope Scope, key string) (string, error) {
	req := WriteRequest{LearnerID: learnerID, Scope: scope}
	switch scope {
	case ScopeConcept:
		req.ConceptSlug = key
	case ScopeArchive:
		req.Period = key
	case ScopeSession:
		ts, err := parseSessionKey(key)
		if err != nil {
			return "", err
		}
		req.Timestamp = ts
	}
	return pathFor(req)
}

func defaultOperation(scope Scope) Operation {
	switch scope {
	case ScopeMemoryPending:
		return OpAppend
	case ScopeSession, ScopeArchive:
		return OpReplaceFile
	default:
		return OpReplaceSection
	}
}

func pathFor(req WriteRequest) (string, error) {
	base, err := learnerDir(req.LearnerID)
	if err != nil {
		return "", err
	}
	switch req.Scope {
	case ScopeMemory:
		return filepath.Join(base, "MEMORY.md"), nil
	case ScopeMemoryPending:
		return filepath.Join(base, "MEMORY_pending.md"), nil
	case ScopeSession:
		if req.Timestamp.IsZero() {
			return "", errors.New("memory: timestamp is required for session scope")
		}
		return filepath.Join(base, "sessions", sessionFilename(req.Timestamp)), nil
	case ScopeConcept:
		if req.ConceptSlug == "" {
			return "", errors.New("memory: concept_slug is required for concept scope")
		}
		return filepath.Join(base, "concepts", safeSegment(req.ConceptSlug)+".md"), nil
	case ScopeArchive:
		if req.Period == "" {
			return "", errors.New("memory: period is required for archive scope")
		}
		return filepath.Join(base, "archives", safeSegment(req.Period)+".md"), nil
	default:
		return "", fmt.Errorf("memory: unsupported scope %q", req.Scope)
	}
}

func learnerDir(learnerID string) (string, error) {
	if learnerID == "" {
		return "", errors.New("memory: learner_id is required")
	}
	return filepath.Join(Root(), "learners", safeSegment(learnerID)), nil
}

func safeSegment(s string) string {
	escaped := url.PathEscape(s)
	// PathEscape intentionally leaves "." and ".." untouched. They are valid
	// labels but must never acquire filesystem traversal semantics.
	switch escaped {
	case ".":
		return "%2E"
	case "..":
		return "%2E%2E"
	default:
		return escaped
	}
}

func unsafeSegment(s string) string {
	if decoded, err := url.PathUnescape(s); err == nil {
		return decoded
	}
	return s
}

func sessionFilename(ts time.Time) string {
	return ts.UTC().Format(sessionFilenameLayout) + ".md"
}

func parseSessionKey(key string) (time.Time, error) {
	key = strings.TrimSuffix(strings.TrimSpace(key), ".md")
	if key == "" {
		return time.Time{}, errors.New("memory: timestamp is required")
	}
	if ts, err := time.Parse(time.RFC3339, key); err == nil {
		return ts.UTC(), nil
	}
	ts, err := time.ParseInLocation(sessionFilenameLayout, key, time.UTC)
	if err != nil {
		return time.Time{}, fmt.Errorf("memory: invalid session timestamp %q", key)
	}
	return ts.UTC(), nil
}

func listMarkdownKeys(learnerID, dir string, unescape bool) ([]string, error) {
	base, err := learnerDir(learnerID)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(base, dir))
	if os.IsNotExist(err) {
		return []string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("memory: list %s: %w", dir, err)
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" {
			continue
		}
		key := strings.TrimSuffix(entry.Name(), ".md")
		if unescape {
			key = unsafeSegment(key)
		}
		out = append(out, key)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out, nil
}

func atomicWrite(path, content string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return fmt.Errorf("memory: create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("memory: write temp: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("memory: chmod temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("memory: sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("memory: close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("memory: rename temp: %w", err)
	}
	parent, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("memory: open parent for sync: %w", err)
	}
	if err := parent.Sync(); err != nil {
		_ = parent.Close()
		return fmt.Errorf("memory: sync parent: %w", err)
	}
	if err := parent.Close(); err != nil {
		return fmt.Errorf("memory: close parent: %w", err)
	}
	return nil
}

func memoryWriteLock(path string) *sync.Mutex {
	// FNV-1a, inlined to avoid allocating a hash.Hash for every write.
	var hash uint64 = 14695981039346656037
	for i := 0; i < len(path); i++ {
		hash ^= uint64(path[i])
		hash *= 1099511628211
	}
	return &writeLocks[hash%uint64(len(writeLocks))]
}

func replaceMarkdownSection(current, sectionKey, content string) string {
	heading := "## " + strings.TrimSpace(sectionKey)
	replacement := heading + "\n" + strings.TrimRight(content, "\n") + "\n"
	if strings.TrimSpace(current) == "" {
		return replacement
	}

	lines := strings.SplitAfter(current, "\n")
	start := -1
	end := len(lines)
	for i, line := range lines {
		if strings.TrimRight(line, "\r\n") == heading {
			start = i
			for j := i + 1; j < len(lines); j++ {
				trimmed := strings.TrimSpace(lines[j])
				if strings.HasPrefix(trimmed, "## ") && !strings.HasPrefix(trimmed, "### ") {
					end = j
					break
				}
			}
			break
		}
	}
	if start == -1 {
		if !strings.HasSuffix(current, "\n") {
			current += "\n"
		}
		return current + "\n" + replacement
	}
	out := strings.Join(lines[:start], "") + replacement + strings.Join(lines[end:], "")
	return out
}
