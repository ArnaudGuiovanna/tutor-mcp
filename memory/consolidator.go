// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package memory

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"time"
)

type ConsolidationPeriod string

const (
	PeriodMonthly   ConsolidationPeriod = "monthly"
	PeriodQuarterly ConsolidationPeriod = "quarterly"
	PeriodAnnual    ConsolidationPeriod = "annual"
)

type ConsolidationJob struct {
	LearnerID string
	Period    ConsolidationPeriod
	PeriodKey string
	StartDate time.Time
	EndDate   time.Time
}

func PrepareJobs(learnerID string, now time.Time) ([]ConsolidationJob, error) {
	return PrepareJobsContext(context.Background(), learnerID, now)
}

func PrepareJobsContext(ctx context.Context, learnerID string, now time.Time) ([]ConsolidationJob, error) {
	if !Enabled() {
		return nil, nil
	}
	now = now.UTC()
	var jobs []ConsolidationJob
	if now.Day() <= 7 {
		start := time.Date(now.Year(), now.Month()-1, 1, 0, 0, 0, 0, time.UTC)
		end := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		key := start.Format("2006-01")
		if !archiveExists(ctx, learnerID, key) {
			jobs = append(jobs, ConsolidationJob{LearnerID: learnerID, Period: PeriodMonthly, PeriodKey: key, StartDate: start, EndDate: end})
		}
	}
	if isQuarterStartWindow(now) {
		start := previousQuarterStart(now)
		end := start.AddDate(0, 3, 0)
		key := fmt.Sprintf("%04d-Q%d", start.Year(), quarter(start.Month()))
		if monthlyArchivesExist(ctx, learnerID, start) && !archiveExists(ctx, learnerID, key) {
			jobs = append(jobs, ConsolidationJob{LearnerID: learnerID, Period: PeriodQuarterly, PeriodKey: key, StartDate: start, EndDate: end})
		}
	}
	if now.Month() == time.January && now.Day() <= 30 {
		start := time.Date(now.Year()-1, time.January, 1, 0, 0, 0, 0, time.UTC)
		end := time.Date(now.Year(), time.January, 1, 0, 0, 0, 0, time.UTC)
		key := strconv.Itoa(start.Year())
		if quarterlyArchivesExist(ctx, learnerID, start.Year()) && !archiveExists(ctx, learnerID, key) {
			jobs = append(jobs, ConsolidationJob{LearnerID: learnerID, Period: PeriodAnnual, PeriodKey: key, StartDate: start, EndDate: end})
		}
	}
	return jobs, nil
}

func SessionsInRange(learnerID string, start, end time.Time) ([]SessionPayload, error) {
	return SessionsInRangeContext(context.Background(), learnerID, start, end)
}

func SessionsInRangeContext(ctx context.Context, learnerID string, start, end time.Time) ([]SessionPayload, error) {
	timestamps, err := ListSessionsContext(ctx, learnerID)
	if err != nil {
		return nil, err
	}
	var out []SessionPayload
	for _, ts := range timestamps {
		if ts.Before(start) || !ts.Before(end) {
			continue
		}
		raw, err := ReadContext(ctx, learnerID, ScopeSession, ts.Format(time.RFC3339))
		if err != nil {
			return nil, err
		}
		payload, err := ParseSessionPayload(ts, raw)
		if err != nil {
			return nil, err
		}
		out = append(out, payload)
	}
	return out, nil
}

func InterleavedReplaySessions(learnerID string, before time.Time, limit int) ([]SessionPayload, error) {
	return InterleavedReplaySessionsContext(context.Background(), learnerID, before, limit)
}

func InterleavedReplaySessionsContext(ctx context.Context, learnerID string, before time.Time, limit int) ([]SessionPayload, error) {
	timestamps, err := ListSessionsContext(ctx, learnerID)
	if err != nil {
		return nil, err
	}
	var older []time.Time
	for _, ts := range timestamps {
		if ts.Before(before) {
			older = append(older, ts)
		}
	}
	if len(older) == 0 || limit <= 0 {
		return nil, nil
	}
	seed := time.Now().UnixNano()
	if raw := strings.TrimSpace(os.Getenv("TUTOR_MCP_CONSOLIDATION_SEED")); raw != "" {
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil {
			seed = parsed
		}
	}
	r := rand.New(rand.NewSource(seed))
	r.Shuffle(len(older), func(i, j int) { older[i], older[j] = older[j], older[i] })
	if len(older) > limit {
		older = older[:limit]
	}
	out := make([]SessionPayload, 0, len(older))
	for _, ts := range older {
		raw, err := ReadContext(ctx, learnerID, ScopeSession, ts.Format(time.RFC3339))
		if err != nil {
			return nil, err
		}
		payload, err := ParseSessionPayload(ts, raw)
		if err != nil {
			return nil, err
		}
		out = append(out, payload)
	}
	return out, nil
}

func ConceptsFromSessions(sessions []SessionPayload) []string {
	set := map[string]bool{}
	for _, session := range sessions {
		raw := session.Frontmatter["concepts_touched"]
		switch v := raw.(type) {
		case []any:
			for _, item := range v {
				if s := strings.TrimSpace(fmt.Sprint(item)); s != "" {
					set[s] = true
				}
			}
		case []string:
			for _, item := range v {
				if item != "" {
					set[item] = true
				}
			}
		}
	}
	out := make([]string, 0, len(set))
	for concept := range set {
		out = append(out, concept)
	}
	for i := 0; i < len(out)-1; i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func archiveExists(ctx context.Context, learnerID, key string) bool {
	body, err := ReadContext(ctx, learnerID, ScopeArchive, key)
	return err == nil && strings.TrimSpace(body) != ""
}

func monthlyArchivesExist(ctx context.Context, learnerID string, start time.Time) bool {
	for i := 0; i < 3; i++ {
		if !archiveExists(ctx, learnerID, start.AddDate(0, i, 0).Format("2006-01")) {
			return false
		}
	}
	return true
}

func quarterlyArchivesExist(ctx context.Context, learnerID string, year int) bool {
	for q := 1; q <= 4; q++ {
		if !archiveExists(ctx, learnerID, fmt.Sprintf("%04d-Q%d", year, q)) {
			return false
		}
	}
	return true
}

func isQuarterStartWindow(now time.Time) bool {
	switch now.Month() {
	case time.January, time.April, time.July, time.October:
		return now.Day() <= 14
	default:
		return false
	}
}

func previousQuarterStart(now time.Time) time.Time {
	month := time.Month(((int(now.Month())-1)/3)*3 + 1)
	return time.Date(now.Year(), month, 1, 0, 0, 0, 0, time.UTC).AddDate(0, -3, 0)
}

func quarter(month time.Month) int {
	return (int(month)-1)/3 + 1
}
