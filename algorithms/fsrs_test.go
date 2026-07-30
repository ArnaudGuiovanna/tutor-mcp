package algorithms

import (
	"math"
	"testing"
	"time"
)

func approxEqual(a, b, tolerance float64) bool {
	return math.Abs(a-b) < tolerance
}

func TestRetrievability(t *testing.T) {
	tests := []struct {
		elapsed   int
		stability float64
		want      float64
	}{
		{0, 1.0, 1.0},
		{1, 1.0, 0.9},
		{10, 1.0, 0.5468},
		{0, 5.0, 1.0},
		{5, 5.0, 0.9},
	}
	for _, tt := range tests {
		got := Retrievability(tt.elapsed, tt.stability)
		if !approxEqual(got, tt.want, 0.01) {
			t.Errorf("Retrievability(%d, %.1f) = %.4f, want ~%.4f", tt.elapsed, tt.stability, got, tt.want)
		}
	}
}

func TestCurrentElapsedDaysUsesLastReviewNotHistoricalElapsed(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	lastReview := now.Add(-72 * time.Hour)

	if got := CurrentElapsedDays(now, &lastReview); got != 3 {
		t.Fatalf("CurrentElapsedDays() = %d, want 3", got)
	}
	if got := CurrentRetrievability(now, &lastReview, 2); !approxEqual(got, Retrievability(3, 2), 1e-12) {
		t.Fatalf("CurrentRetrievability() = %f, want retention at age 3 days", got)
	}
}

func TestCurrentElapsedDaysClampsMissingAndFutureReview(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	future := now.Add(48 * time.Hour)

	for name, lastReview := range map[string]*time.Time{
		"missing": nil,
		"future":  &future,
	} {
		t.Run(name, func(t *testing.T) {
			if got := CurrentElapsedDays(now, lastReview); got != 0 {
				t.Fatalf("CurrentElapsedDays() = %d, want 0", got)
			}
			if got := CurrentRetrievability(now, lastReview, 2); got != 1 {
				t.Fatalf("CurrentRetrievability() = %f, want 1", got)
			}
		})
	}
}

func TestRetrievabilityClampsNegativeElapsedDays(t *testing.T) {
	got := Retrievability(-30, 1)
	if got != 1 || math.IsNaN(got) || math.IsInf(got, 0) {
		t.Fatalf("Retrievability(-30, 1) = %f, want finite 1", got)
	}
}

func TestInitialStability(t *testing.T) {
	if s := InitialStability(Again); !approxEqual(s, defaultWeights[0], 0.001) {
		t.Errorf("InitialStability(Again) = %f, want %f", s, defaultWeights[0])
	}
	if s := InitialStability(Good); !approxEqual(s, defaultWeights[2], 0.001) {
		t.Errorf("InitialStability(Good) = %f, want %f", s, defaultWeights[2])
	}
}

func TestInitialDifficulty(t *testing.T) {
	d := InitialDifficulty(Good)
	if d < 1 || d > 10 {
		t.Errorf("InitialDifficulty(Good) = %f, want in [1,10]", d)
	}
}

// TestInitialStabilityDifficultyOutOfRange guards against the index-out-of-range
// panic that would occur when a Rating outside [Again..Easy] reaches
// defaultWeights[int(rating)-1]: Rating(0) indexed -1, and a too-large value
// silently read the wrong weight. Both functions must now clamp and return a
// sane bounded result without panicking.
func TestInitialStabilityDifficultyOutOfRange(t *testing.T) {
	for _, r := range []Rating{Rating(0), Rating(-5), Rating(7), Rating(1000)} {
		s := InitialStability(r) // must not panic
		// Result is clamped to one of the canonical weight entries, all > 0.
		if s <= 0 || math.IsNaN(s) || math.IsInf(s, 0) {
			t.Errorf("InitialStability(%d) = %f, want positive finite", r, s)
		}
		d := InitialDifficulty(r) // must not panic
		if d < 1 || d > 10 {
			t.Errorf("InitialDifficulty(%d) = %f, want in [1, 10]", r, d)
		}
	}
	// Clamping low maps to Again, clamping high maps to Easy.
	if s := InitialStability(Rating(0)); !approxEqual(s, InitialStability(Again), 1e-9) {
		t.Errorf("InitialStability(0) = %f, want InitialStability(Again) = %f", s, InitialStability(Again))
	}
	if s := InitialStability(Rating(99)); !approxEqual(s, InitialStability(Easy), 1e-9) {
		t.Errorf("InitialStability(99) = %f, want InitialStability(Easy) = %f", s, InitialStability(Easy))
	}
}

func TestReviewCard(t *testing.T) {
	now := time.Date(2026, 3, 27, 10, 0, 0, 0, time.UTC)
	card := NewFSRSCard()

	card = ReviewCard(card, Good, now)
	if card.State != Review {
		t.Errorf("after Good on new card, state = %s, want Review", card.State)
	}
	if card.Stability <= 0 {
		t.Errorf("stability should be positive, got %f", card.Stability)
	}
	if card.ScheduledDays < 1 {
		t.Errorf("scheduled_days should be >= 1, got %d", card.ScheduledDays)
	}
	if card.Reps != 1 {
		t.Errorf("reps = %d, want 1", card.Reps)
	}

	next := now.AddDate(0, 0, card.ScheduledDays)
	card = ReviewCard(card, Again, next)
	if card.State != Relearning {
		t.Errorf("after Again on review card, state = %s, want Relearning", card.State)
	}
	if card.Lapses != 1 {
		t.Errorf("lapses = %d, want 1", card.Lapses)
	}
}

func TestNextInterval(t *testing.T) {
	interval := NextInterval(1.0, 0.9)
	if interval < 1 {
		t.Errorf("interval for stability=1 should be >= 1, got %d", interval)
	}
	interval5 := NextInterval(5.0, 0.9)
	if interval5 <= interval {
		t.Errorf("higher stability should give longer interval: %d vs %d", interval5, interval)
	}
}

func TestRetrievabilityNonPositiveStability(t *testing.T) {
	tests := []struct {
		name      string
		elapsed   int
		stability float64
	}{
		{"zero stability", 5, 0.0},
		{"negative stability", 5, -1.0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Retrievability(tc.elapsed, tc.stability)
			if got != 0 {
				t.Errorf("Retrievability(%d, %f) = %f, want 0", tc.elapsed, tc.stability, got)
			}
		})
	}
}

func TestClamp(t *testing.T) {
	tests := []struct {
		v, min, max, want float64
	}{
		{5, 1, 10, 5},
		{-1, 0, 10, 0},
		{20, 0, 10, 10},
		{0, 0, 10, 0},
		{10, 0, 10, 10},
	}
	for _, tc := range tests {
		got := clamp(tc.v, tc.min, tc.max)
		if !approxEqual(got, tc.want, 1e-9) {
			t.Errorf("clamp(%f, %f, %f) = %f, want %f", tc.v, tc.min, tc.max, got, tc.want)
		}
	}
}

func TestNextIntervalFloorAtOne(t *testing.T) {
	// Very small stability gives raw interval < 1; should be floored to 1.
	got := NextInterval(0.0001, 0.9)
	if got != 1 {
		t.Errorf("NextInterval(0.0001, 0.9) = %d, want 1", got)
	}
	// High desiredRetention also yields short interval; floor still applies.
	got = NextInterval(0.01, 0.99)
	if got != 1 {
		t.Errorf("NextInterval(0.01, 0.99) = %d, want 1", got)
	}
}

func TestNextRecallStabilityRatings(t *testing.T) {
	// Hard, Good, Easy each apply a different modifier (w[15], 1.0, w[17]).
	// With default weights, Hard's modifier is the smallest, so Hard <= Easy <= Good.
	// (The default w[17] is < 1.0, which makes Easy < Good in this configuration.)
	d, s, r := 5.0, 2.0, 0.9
	hard := nextRecallStability(d, s, r, Hard)
	good := nextRecallStability(d, s, r, Good)
	easy := nextRecallStability(d, s, r, Easy)
	if hard > easy {
		t.Errorf("Hard stability %f should be <= Easy %f", hard, easy)
	}
	if hard > good {
		t.Errorf("Hard stability %f should be <= Good %f", hard, good)
	}
	// All should be positive and finite.
	for name, v := range map[string]float64{"hard": hard, "good": good, "easy": easy} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
			t.Errorf("%s stability not positive/finite: %f", name, v)
		}
	}
}

func TestReviewCardNewWithAgain(t *testing.T) {
	now := time.Date(2026, 3, 27, 10, 0, 0, 0, time.UTC)
	card := NewFSRSCard()
	card = ReviewCard(card, Again, now)
	if card.State != Learning {
		t.Errorf("after Again on new card, state = %s, want Learning", card.State)
	}
	if card.ScheduledDays != 0 {
		t.Errorf("ScheduledDays = %d, want 0", card.ScheduledDays)
	}
	if !approxEqual(card.Stability, InitialStability(Again), 1e-9) {
		t.Errorf("Stability = %f, want %f", card.Stability, InitialStability(Again))
	}
	if card.Reps != 1 {
		t.Errorf("Reps = %d, want 1", card.Reps)
	}
}

func TestReviewCardLearningAgain(t *testing.T) {
	// Card in Learning state receiving Again: ScheduledDays stays 0, state remains Learning.
	now := time.Date(2026, 3, 27, 10, 0, 0, 0, time.UTC)
	card := FSRSCard{State: Learning, Stability: 1.5, Difficulty: 5, LastReview: now.AddDate(0, 0, -1)}
	updated := ReviewCard(card, Again, now)
	if updated.State != Learning {
		t.Errorf("state = %s, want Learning", updated.State)
	}
	if updated.ScheduledDays != 0 {
		t.Errorf("ScheduledDays = %d, want 0", updated.ScheduledDays)
	}
}

func TestReviewCardLearningGood(t *testing.T) {
	// Learning + Good with positive prior stability → Review state, uses nextRecallStability.
	now := time.Date(2026, 3, 27, 10, 0, 0, 0, time.UTC)
	card := FSRSCard{State: Learning, Stability: 1.5, Difficulty: 5, LastReview: now.AddDate(0, 0, -1)}
	updated := ReviewCard(card, Good, now)
	if updated.State != Review {
		t.Errorf("state = %s, want Review", updated.State)
	}
	if updated.ScheduledDays < 1 {
		t.Errorf("ScheduledDays = %d, want >= 1", updated.ScheduledDays)
	}
	if updated.Stability <= 0 {
		t.Errorf("Stability = %f, want > 0", updated.Stability)
	}
	if updated.ElapsedDays != 1 {
		t.Errorf("ElapsedDays = %d, want 1", updated.ElapsedDays)
	}
}

func TestReviewCardLearningGoodZeroStability(t *testing.T) {
	// Learning + Good with zero stability → uses InitialStability(rating).
	now := time.Date(2026, 3, 27, 10, 0, 0, 0, time.UTC)
	card := FSRSCard{State: Learning, Stability: 0, Difficulty: 5, LastReview: now.AddDate(0, 0, -1)}
	updated := ReviewCard(card, Good, now)
	if updated.State != Review {
		t.Errorf("state = %s, want Review", updated.State)
	}
	if !approxEqual(updated.Stability, InitialStability(Good), 1e-9) {
		t.Errorf("Stability = %f, want %f (InitialStability of Good)", updated.Stability, InitialStability(Good))
	}
}

func TestReviewCardRelearningGood(t *testing.T) {
	// Relearning + Good → Review with recomputed stability/difficulty.
	now := time.Date(2026, 3, 27, 10, 0, 0, 0, time.UTC)
	card := FSRSCard{State: Relearning, Stability: 2.0, Difficulty: 6, Lapses: 1, LastReview: now.AddDate(0, 0, -2)}
	updated := ReviewCard(card, Good, now)
	if updated.State != Review {
		t.Errorf("state = %s, want Review", updated.State)
	}
	if updated.Lapses != 1 {
		t.Errorf("Lapses = %d, want 1 (unchanged)", updated.Lapses)
	}
	if updated.ScheduledDays < 1 {
		t.Errorf("ScheduledDays = %d, want >= 1", updated.ScheduledDays)
	}
}

func TestReviewCardReviewGood(t *testing.T) {
	// Review state + Good rating → stays Review, no lapse.
	now := time.Date(2026, 3, 27, 10, 0, 0, 0, time.UTC)
	card := FSRSCard{State: Review, Stability: 5.0, Difficulty: 5, LastReview: now.AddDate(0, 0, -3)}
	updated := ReviewCard(card, Good, now)
	if updated.State != Review {
		t.Errorf("state = %s, want Review", updated.State)
	}
	if updated.Lapses != 0 {
		t.Errorf("Lapses = %d, want 0", updated.Lapses)
	}
	if updated.Stability <= card.Stability {
		t.Errorf("stability should grow on Good in Review, before=%f after=%f", card.Stability, updated.Stability)
	}
}

func TestReviewCardZeroLastReview(t *testing.T) {
	// Card with zero LastReview should yield ElapsedDays == 0.
	now := time.Date(2026, 3, 27, 10, 0, 0, 0, time.UTC)
	card := FSRSCard{State: Review, Stability: 5.0, Difficulty: 5} // LastReview is zero
	updated := ReviewCard(card, Good, now)
	if updated.ElapsedDays != 0 {
		t.Errorf("ElapsedDays = %d, want 0 (zero LastReview)", updated.ElapsedDays)
	}
}

func TestNextForgetStabilityIsPositive(t *testing.T) {
	// Forget stability should be a non-negative finite number.
	got := nextForgetStability(5, 2, 0.5)
	if math.IsNaN(got) || math.IsInf(got, 0) || got < 0 {
		t.Errorf("nextForgetStability = %f, want non-negative finite", got)
	}
}

func TestInitialDifficultyClamped(t *testing.T) {
	// Across all four ratings, difficulty must lie in [1, 10].
	for _, r := range []Rating{Again, Hard, Good, Easy} {
		d := InitialDifficulty(r)
		if d < 1 || d > 10 {
			t.Errorf("InitialDifficulty(%d) = %f, want in [1, 10]", r, d)
		}
	}
}

// TestNextForgetStabilityNoNaNOrInfOnDegenerateInputs verifies
// nextForgetStability never returns NaN or +Inf when difficulty is at or
// below zero. Without a guard, math.Pow(0, -w[12]) is +Inf and math.Pow(neg,
// non-integer) is NaN, both of which corrupt FSRSCard.Stability.
func TestNextForgetStabilityNoNaNOrInfOnDegenerateInputs(t *testing.T) {
	tests := []struct {
		name string
		d    float64
		s    float64
		r    float64
	}{
		{"zero difficulty", 0, 2, 0.5},
		{"negative difficulty", -3, 2, 0.5},
		{"zero stability", 5, 0, 0.5},
		{"negative stability", 5, -1, 0.5},
		{"all-zero", 0, 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := nextForgetStability(tc.d, tc.s, tc.r)
			if math.IsNaN(got) || math.IsInf(got, 0) {
				t.Errorf("nextForgetStability(%f,%f,%f) = %f, want finite", tc.d, tc.s, tc.r, got)
			}
			if got < 0 {
				t.Errorf("nextForgetStability(%f,%f,%f) = %f, want >= 0", tc.d, tc.s, tc.r, got)
			}
		})
	}
}

// TestNextRecallStabilityNoNaNOrInfOnDegenerateInputs verifies
// nextRecallStability never returns NaN or Inf when stability collapses to
// zero. math.Pow(0, -w[9]) is +Inf, which would propagate into a NaN once
// multiplied by the surrounding s=0 factor.
func TestNextRecallStabilityNoNaNOrInfOnDegenerateInputs(t *testing.T) {
	tests := []struct {
		name    string
		d, s, r float64
		rating  Rating
	}{
		{"zero stability + Good", 5, 0, 0.5, Good},
		{"zero stability + Hard", 5, 0, 0.5, Hard},
		{"zero stability + Easy", 5, 0, 0.5, Easy},
		{"negative stability + Good", 5, -1, 0.5, Good},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := nextRecallStability(tc.d, tc.s, tc.r, tc.rating)
			if math.IsNaN(got) || math.IsInf(got, 0) {
				t.Errorf("nextRecallStability(%f,%f,%f,%d) = %f, want finite", tc.d, tc.s, tc.r, tc.rating, got)
			}
		})
	}
}

// TestReviewCardNoNaNOrInfOnDegenerateInputs is an end-to-end guard: a Review
// card with d=0 receiving Again routes through nextForgetStability and would
// surface +Inf in newCard.Stability without the guard.
func TestReviewCardNoNaNOrInfOnDegenerateInputs(t *testing.T) {
	now := time.Date(2026, 3, 27, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		card   FSRSCard
		rating Rating
	}{
		{
			name:   "review with zero difficulty + Again",
			card:   FSRSCard{State: Review, Stability: 5, Difficulty: 0, LastReview: now.AddDate(0, 0, -3)},
			rating: Again,
		},
		{
			name:   "review with negative difficulty + Again",
			card:   FSRSCard{State: Review, Stability: 5, Difficulty: -2, LastReview: now.AddDate(0, 0, -3)},
			rating: Again,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ReviewCard(tc.card, tc.rating, now)
			if math.IsNaN(got.Stability) || math.IsInf(got.Stability, 0) {
				t.Errorf("Stability = %f, want finite", got.Stability)
			}
			if math.IsNaN(got.Difficulty) || math.IsInf(got.Difficulty, 0) {
				t.Errorf("Difficulty = %f, want finite", got.Difficulty)
			}
		})
	}
}

func TestNextDifficultyClamped(t *testing.T) {
	// Even with extreme inputs, nextDifficulty must remain in [1, 10].
	tests := []struct {
		name string
		d    float64
		r    Rating
	}{
		{"max difficulty + Again", 10, Again},
		{"min difficulty + Easy", 1, Easy},
		{"mid + Good", 5, Good},
		{"mid + Hard", 5, Hard},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := nextDifficulty(tc.d, tc.r)
			if got < 1 || got > 10 {
				t.Errorf("nextDifficulty(%f, %d) = %f, want in [1, 10]", tc.d, tc.r, got)
			}
		})
	}
}
