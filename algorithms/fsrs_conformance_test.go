package algorithms

import (
	"math"
	"testing"
	"time"
)

// Numerical fixtures use the published FSRS-5 default parameters and equations,
// independently of the implementation's parameter array. References:
// https://github.com/open-spaced-repetition/awesome-fsrs/wiki/The-Algorithm#fsrs-5
// https://github.com/open-spaced-repetition/go-fsrs/blob/v3.3.1/parameters.go
func TestFSRS5InitialStateReference(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		rating Rating
		d, s   float64
		days   int
	}{
		{Again, 7.1949, 0.40255, 0},
		{Hard, 6.488305268471453, 1.18385, 1},
		{Good, 5.282434422319005, 3.173, 3},
		{Easy, 3.224501589371368, 15.69105, 16},
	}
	for _, tc := range cases {
		got := ReviewCard(NewFSRSCard(), tc.rating, now)
		if !approxEqual(got.Difficulty, tc.d, 1e-12) || !approxEqual(got.Stability, tc.s, 1e-12) || got.ScheduledDays != tc.days {
			t.Errorf("rating %d: got D=%f S=%f days=%d; want D=%f S=%f days=%d", tc.rating, got.Difficulty, got.Stability, got.ScheduledDays, tc.d, tc.s, tc.days)
		}
	}
}

func TestFSRS5DelayedReviewReference(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	// This can be an existing card from the old implementation. Its memory
	// values and counters are the prior; no reset or historical refit occurs.
	card := FSRSCard{State: Review, Difficulty: 5, Stability: 2, Reps: 17, Lapses: 2, LastReview: now.Add(-48 * time.Hour)}
	cases := []struct {
		rating Rating
		d, s   float64
		days   int
	}{
		{Again, 6.607035107311108, 0.783659085779702, 0},
		{Hard, 5.799433907311109, 3.287542542076151, 3},
		{Good, 4.991832707311108, 7.561738842661560, 8},
		{Easy, 4.184231507311108, 18.62848679178953, 19},
	}
	for _, tc := range cases {
		got := ReviewCard(card, tc.rating, now)
		if !approxEqual(got.Difficulty, tc.d, 1e-12) || !approxEqual(got.Stability, tc.s, 1e-12) || got.ScheduledDays != tc.days {
			t.Errorf("rating %d: got D=%f S=%f days=%d; want D=%f S=%f days=%d", tc.rating, got.Difficulty, got.Stability, got.ScheduledDays, tc.d, tc.s, tc.days)
		}
		wantState, wantLapses := Review, 2
		if tc.rating == Again {
			wantState, wantLapses = Relearning, 3
		}
		if got.Reps != 18 || got.Lapses != wantLapses || got.State != wantState || got.ElapsedDays != 2 || !got.LastReview.Equal(now) {
			t.Errorf("rating %d lost history or wrong transition: %+v", tc.rating, got)
		}
	}
}

func TestFSRS5SameDayMemoryUpdateAcrossStates(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		rating Rating
		s      float64
	}{
		{Again, 1.002057048387017},
		{Hard, 1.679682748761864},
		{Good, 2.815542429475082},
		{Easy, 4.719509787201092},
	}
	for _, state := range []CardState{Learning, Relearning, Review} {
		for _, age := range []time.Duration{time.Minute, 23 * time.Hour} {
			for _, tc := range cases {
				card := FSRSCard{State: state, Difficulty: 5, Stability: 2, LastReview: now.Add(-age)}
				got := ReviewCard(card, tc.rating, now)
				if got.ElapsedDays != 0 || !approxEqual(got.Stability, tc.s, 1e-12) {
					t.Errorf("state=%s age=%s rating=%d: got S=%f days=%d; want S=%f days=0", state, age, tc.rating, got.Stability, got.ElapsedDays, tc.s)
				}
			}
		}
	}
	// Exactly 24 hours uses the long-term forgetting curve, not a second
	// short-term update merely because the card remains in Learning.
	card := FSRSCard{State: Learning, Difficulty: 5, Stability: 2, LastReview: now.Add(-24 * time.Hour)}
	got := ReviewCard(card, Good, now)
	if got.ElapsedDays != 1 || got.Stability <= 2.815542429475082 {
		t.Errorf("expected delayed recall update at 24h, got %+v", got)
	}
}

func TestFSRS5RatingMonotonicity(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	for _, d := range []float64{1, 5, 9.9, 10} {
		for _, s := range []float64{0.1, 2, 100} {
			for _, age := range []time.Duration{time.Hour, 24 * time.Hour, 30 * 24 * time.Hour} {
				card := FSRSCard{State: Review, Difficulty: d, Stability: s, LastReview: now.Add(-age)}
				previous := ReviewCard(card, Again, now)
				for _, rating := range []Rating{Hard, Good, Easy} {
					current := ReviewCard(card, rating, now)
					if current.Difficulty > previous.Difficulty || current.Stability < previous.Stability || current.ScheduledDays < previous.ScheduledDays {
						t.Errorf("better rating worsens memory/schedule: D=%f S=%f age=%s rating=%d previous=%+v current=%+v", d, s, age, rating, previous, current)
					}
					previous = current
				}
			}
		}
	}
}

func TestFSRS5LapseThenImmediateRelearningCannotCreateStability(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	// Very overdue, low-stability material exercises the reference cap on
	// post-lapse stability, rather than allowing failure to increase it.
	card := FSRSCard{State: Review, Difficulty: 1, Stability: 0.1, LastReview: now.Add(-365 * 24 * time.Hour)}
	lapsed := ReviewCard(card, Again, now)
	relearned := ReviewCard(lapsed, Good, now.Add(time.Minute))
	if lapsed.Stability >= card.Stability || relearned.Stability > card.Stability+1e-12 {
		t.Errorf("lapse/relearning manufactured stability: prior=%f lapse=%f relearned=%f", card.Stability, lapsed.Stability, relearned.Stability)
	}
}

func TestFSRS5MalformedStateAndRatingsRemainFinite(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -1, 0} {
		for _, state := range []CardState{New, Learning, Relearning, Review} {
			for _, rating := range []Rating{-100, Again, Hard, Good, Easy, 100} {
				card := FSRSCard{State: state, Difficulty: value, Stability: value, LastReview: now.Add(-48 * time.Hour)}
				got := ReviewCard(card, rating, now)
				if math.IsNaN(got.Difficulty) || math.IsInf(got.Difficulty, 0) || got.Difficulty < 1 || got.Difficulty > 10 || math.IsNaN(got.Stability) || math.IsInf(got.Stability, 0) || got.Stability <= 0 || got.ScheduledDays < 0 || got.ScheduledDays > fsrsMaximumInterval {
					t.Errorf("invalid result for state=%s value=%f rating=%d: %+v", state, value, rating, got)
				}
			}
		}
		if got := Retrievability(1, value); got != 0 {
			t.Errorf("invalid stability %f must not yield a recall claim, got %f", value, got)
		}
	}
	for _, state := range []CardState{New, Learning, Relearning, Review} {
		card := FSRSCard{State: state, Difficulty: 5, Stability: 2, LastReview: now.Add(-time.Hour)}
		if got, want := ReviewCard(card, -1, now), ReviewCard(card, Again, now); got != want {
			t.Errorf("state=%s: invalid low rating must behave as Again", state)
		}
		if got, want := ReviewCard(card, 100, now), ReviewCard(card, Easy, now); got != want {
			t.Errorf("state=%s: invalid high rating must behave as Easy", state)
		}
	}
}

func TestFSRS5IntervalBounds(t *testing.T) {
	for _, retention := range []float64{0, -1, 2, math.NaN(), math.Inf(1)} {
		if got := NextInterval(10, retention); got != 10 {
			t.Errorf("invalid retention %f must use the default target, got interval %d", retention, got)
		}
	}
	if got := NextInterval(math.MaxFloat64, FSRSDesiredRetention); got != fsrsMaximumInterval {
		t.Errorf("expected capped interval, got %d", got)
	}
	if got := NextInterval(math.MaxFloat64, 1); got != 1 {
		t.Errorf("100%% retention must use minimum interval without overflow, got %d", got)
	}
}
