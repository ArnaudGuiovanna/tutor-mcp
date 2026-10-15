package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"tutor-mcp/models"
)

var (
	ErrSessionIndexIncomplete = errors.New("memory: session narrative index is incomplete")
	ErrNarrativeTenantScope   = errors.New("memory: explicit narrative tenant scope is required")
)

// SessionNarrativeIndex selects canonical object identities, never reconstructed
// timestamp-only keys. DomainID in NarrativeKey remains empty for sessions;
// the session's domain is a separate, versioned projection of its plaintext.
type SessionNarrativeIndex interface {
	ListRecentSessionNarratives(context.Context, models.TenantScope, string, string, int) ([]NarrativeMetadata, error)
}

type narrativeTenantScopeKey struct{}

// WithNarrativeTenantScope transports a scope across the storage-independent
// loader. It grants no authority: the persistence port must check it against
// its own transaction and enforce tenant/learner ownership.
func WithNarrativeTenantScope(ctx context.Context, scope models.TenantScope) (context.Context, error) {
	if err := scope.Validate(); err != nil {
		return nil, fmt.Errorf("%w: invalid scope", ErrNarrativeTenantScope)
	}
	if previous, ok := ctx.Value(narrativeTenantScopeKey{}).(models.TenantScope); ok && previous != scope {
		return nil, fmt.Errorf("%w: nested scope mismatch", ErrNarrativeTenantScope)
	}
	return context.WithValue(ctx, narrativeTenantScopeKey{}, scope), nil
}

// NarrativeTenantScope reads only the transported value. Persistence must still
// validate it against its own scope/transaction; this is not authentication.
func NarrativeTenantScope(ctx context.Context) (models.TenantScope, bool) {
	scope, ok := ctx.Value(narrativeTenantScopeKey{}).(models.TenantScope)
	return scope, ok
}

// SessionDomainID uses the same frontmatter interpretation for indexing,
// writes, repairs and the final authenticated read.
func SessionDomainID(raw string) (string, error) {
	payload, err := ParseSessionPayload(time.Time{}, raw)
	if err != nil {
		return "", err
	}
	value, exists := payload.Frontmatter["domain_id"]
	if !exists || value == nil {
		return "", nil
	}
	domainID := strings.TrimSpace(fmt.Sprint(value))
	return domainID, nil
}

func recentSessionPayloads(ctx context.Context, learnerID, domainID string) ([]SessionPayload, error) {
	backend := configuredNarrativeStore()
	if backend == nil {
		return recentFileSessionPayloads(ctx, learnerID, domainID, 3)
	}
	scope, ok := ctx.Value(narrativeTenantScopeKey{}).(models.TenantScope)
	if !ok || scope.Validate() != nil || (scope.LearnerID != "" && scope.LearnerID != learnerID) {
		return nil, ErrNarrativeTenantScope
	}
	index, ok := backend.(SessionNarrativeIndex)
	if !ok {
		return nil, ErrSessionIndexIncomplete
	}
	items, err := index.ListRecentSessionNarratives(ctx, scope, learnerID, domainID, 3)
	if err != nil {
		return nil, err
	}
	payloads := make([]SessionPayload, 0, len(items))
	for _, item := range items {
		object, err := backend.GetNarrative(ctx, item.NarrativeKey)
		if err != nil {
			return nil, err
		}
		actualDomain, err := SessionDomainID(object.Content)
		if err != nil {
			return nil, err
		}
		if object.Version != item.Version || object.Checksum != item.Checksum || (domainID != "" && actualDomain != domainID) {
			return nil, ErrSessionIndexIncomplete
		}
		ts, err := parseSessionKey(item.Key)
		if err != nil {
			return nil, err
		}
		payload, err := ParseSessionPayload(ts, object.Content)
		if err != nil {
			return nil, err
		}
		payloads = append(payloads, payload)
	}
	return payloads, nil
}

// SessionDomainDigest bounds the index projection without imposing a new limit
// on legacy YAML values. Selected plaintext domains are still compared exactly.
func SessionDomainDigest(domainID string) string {
	digest := sha256.Sum256([]byte(domainID))
	return hex.EncodeToString(digest[:])
}

// SessionSortKey projects both accepted timestamp-key encodings into one
// lexically ordered UTC value. It never changes the original object key/AAD.
func SessionSortKey(key string) (string, error) {
	at, err := parseSessionKey(key)
	if err != nil {
		return "", err
	}
	return at.UTC().Format("2006-01-02T15:04:05.000000000Z"), nil
}
