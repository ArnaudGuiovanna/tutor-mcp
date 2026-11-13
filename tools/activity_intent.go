// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"tutor-mcp/algorithms"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

const (
	activityIntentAuto   = "auto"
	activityIntentReview = "review"
)

// activityDomainSelectionError represents a caller-correctable domain name,
// as opposed to an unavailable persistence backend. Its message is safe to
// return to the authenticated learner.
type activityDomainSelectionError struct {
	message string
}

func (e *activityDomainSelectionError) Error() string { return e.message }

func normalizeActivityIntent(raw string) (string, error) {
	switch raw {
	case "", activityIntentAuto:
		return activityIntentAuto, nil
	case activityIntentReview:
		return activityIntentReview, nil
	default:
		return "", fmt.Errorf("unsupported intent %q: allowed values are auto, review", raw)
	}
}

// activityDomainStore resolves the domain of an activity request: by ID, by
// name, or by the most critical retention need across the learner's domains.
type activityDomainStore interface {
	storeport.DomainStore
	storeport.ConceptStateStore
}

func resolveActivityDomain(ctx context.Context, store activityDomainStore, learnerID, domainID, domainName string) (*models.Domain, error) {
	if storeport.ConversationID(ctx) != "" {
		return resolveDomain(ctx, store, learnerID, domainID)
	}
	if domainID != "" || strings.TrimSpace(domainName) == "" {
		if domainID == "" {
			if d, err := resolveCriticalRetentionDomain(ctx, store, learnerID); err != nil {
				return nil, err
			} else if d != nil {
				return d, nil
			}
		}
		return resolveDomain(ctx, store, learnerID, domainID)
	}

	domains, err := store.GetDomainsByLearner(ctx, learnerID, false)
	if err != nil {
		return nil, err
	}
	needle := compactToken(domainName)
	if needle == "" {
		return nil, fmt.Errorf("domain_name is empty after normalization")
	}

	for _, d := range domains {
		if compactToken(d.Name) == needle {
			return d, nil
		}
	}

	var matches []*models.Domain
	for _, d := range domains {
		haystack := compactToken(d.Name)
		if strings.Contains(haystack, needle) || strings.Contains(needle, haystack) {
			matches = append(matches, d)
		}
	}

	switch len(matches) {
	case 0:
		return nil, &activityDomainSelectionError{message: fmt.Sprintf(
			"domain_name %q did not match an active domain; available domains: %s",
			domainName, domainNames(domains),
		)}
	case 1:
		return matches[0], nil
	default:
		return nil, &activityDomainSelectionError{message: fmt.Sprintf(
			"domain_name %q is ambiguous; matching domains: %s",
			domainName, domainNames(matches),
		)}
	}
}

// resolveCriticalRetentionDomain selects the active domain containing the most
// urgent reviewed card. States are grouped per domain because concept labels
// are not learner-global identities; one batched read serves every domain.
func resolveCriticalRetentionDomain(ctx context.Context, store activityDomainStore, learnerID string) (*models.Domain, error) {
	domains, err := store.GetDomainsByLearner(ctx, learnerID, false)
	if err != nil {
		return nil, err
	}
	if len(domains) == 0 {
		return nil, nil
	}

	var bestDomain *models.Domain
	bestRetention := 1.0
	now := time.Now().UTC()
	statesByDomain, err := conceptStatesByDomains(ctx, store, learnerID, domains)
	if err != nil {
		return nil, err
	}
	for _, d := range domains {
		for _, cs := range statesByDomain[d.ID] {
			if cs == nil || cs.CardState == "new" {
				continue
			}
			retention := algorithms.CurrentRetrievability(now, cs.LastReview, cs.Stability)
			if retention < algorithms.RetentionAlertCriticalThreshold && retention < bestRetention {
				bestRetention = retention
				bestDomain = d
			}
		}
	}
	return bestDomain, nil
}

func compactToken(raw string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(raw) {
		r = unicode.ToLower(r)
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func domainNames(domains []*models.Domain) string {
	if len(domains) == 0 {
		return "(none)"
	}
	parts := make([]string, 0, len(domains))
	for _, d := range domains {
		parts = append(parts, fmt.Sprintf("%s (%s)", d.Name, d.ID))
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}
