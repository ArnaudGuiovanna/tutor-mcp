// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package algorithms

import (
	"math"
	"time"
)

type Rating int

const (
	Again Rating = 1
	Hard  Rating = 2
	Good  Rating = 3
	Easy  Rating = 4
)

type CardState string

const (
	New        CardState = "new"
	Learning   CardState = "learning"
	Review     CardState = "review"
	Relearning CardState = "relearning"
)

const (
	// FSRSVersion identifies the memory equations and default parameter set.
	// This is FSRS-5, as specified at:
	// https://github.com/open-spaced-repetition/awesome-fsrs/wiki/The-Algorithm#fsrs-5
	// The parameters/formulas also match go-fsrs v3.3.1 parameters.go/weights.go.
	// The local scheduling adapter retains whole-day intervals and immediate
	// relearning after Again; it does not implement Anki's minute learning steps.
	FSRSVersion          = "FSRS-5"
	FSRSDesiredRetention = 0.9

	fsrsDecay  = -0.5
	fsrsFactor = 19.0 / 81.0
	// Match the reference scheduler's 100-year interval ceiling, including
	// before conversion to time.Duration in callers.
	fsrsMaximumInterval = 36500
	// fsrsEpsilon is the positive fallback for malformed stability inputs.
	// Fractional/negative powers of zero or negative values must not poison
	// subsequent memory updates. Valid persisted stability is kept as-is.
	fsrsEpsilon = 1e-9
)

var defaultWeights = [19]float64{
	0.40255, 1.18385, 3.173, 15.69105,
	7.1949, 0.5345, 1.4604, 0.0046,
	1.54575, 0.1192, 1.01925, 1.9395,
	0.11, 0.29605, 2.2698, 0.2315,
	2.9898, 0.51655, 0.6621,
}

type FSRSCard struct {
	Stability     float64
	Difficulty    float64
	ElapsedDays   int
	ScheduledDays int
	Reps          int
	Lapses        int
	State         CardState
	LastReview    time.Time
}

func NewFSRSCard() FSRSCard {
	return FSRSCard{State: New}
}

func clamp(v, min, max float64) float64 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

func Retrievability(elapsedDays int, stability float64) float64 {
	if stability <= 0 || math.IsNaN(stability) || math.IsInf(stability, 0) {
		return 0
	}
	// A review timestamp in the future (clock skew, bad import) must not
	// produce a negative card age. Besides being meaningless, sufficiently
	// negative values can make the power base non-positive and return NaN.
	if elapsedDays < 0 {
		elapsedDays = 0
	}
	return math.Pow(1+fsrsFactor*float64(elapsedDays)/stability, fsrsDecay)
}

// CurrentElapsedDays returns the whole number of days elapsed since the last
// review at a decision boundary. ElapsedDays stored on FSRSCard is the interval
// that preceded the previous review; it is historical data and must not be
// reused as the card's current age.
//
// Missing/zero review timestamps and timestamps in the future both mean an age
// of zero. Callers should separately exclude genuinely new cards when their
// routing policy does not apply retention to them.
func CurrentElapsedDays(now time.Time, lastReview *time.Time) int {
	if lastReview == nil || lastReview.IsZero() || !now.After(*lastReview) {
		return 0
	}
	elapsedDays := int(now.Sub(*lastReview).Hours() / 24)
	if elapsedDays < 0 {
		return 0
	}
	return elapsedDays
}

// CurrentRetrievability computes retention from the current age of the card,
// derived from LastReview. This is the decision-time counterpart to
// Retrievability, whose elapsedDays argument is intentionally still useful
// inside the FSRS review update itself.
func CurrentRetrievability(now time.Time, lastReview *time.Time, stability float64) float64 {
	return Retrievability(CurrentElapsedDays(now, lastReview), stability)
}

// clampRating bounds a Rating to the valid [Again..Easy] range. Callers index
// defaultWeights with int(rating)-1, so rating==0 (or any value below Again)
// would read index -1 and panic, and a value above Easy would silently read a
// wrong weight. Clamping keeps both functions total over arbitrary inputs
// while preserving behaviour for the four canonical ratings.
func clampRating(rating Rating) Rating {
	if rating < Again {
		return Again
	}
	if rating > Easy {
		return Easy
	}
	return rating
}

func InitialStability(rating Rating) float64 {
	rating = clampRating(rating)
	return defaultWeights[int(rating)-1]
}

func InitialDifficulty(rating Rating) float64 {
	rating = clampRating(rating)
	d := defaultWeights[4] - math.Exp(defaultWeights[5]*float64(rating-1)) + 1
	return clamp(d, 1, 10)
}

func nextDifficulty(d float64, rating Rating) float64 {
	rating = clampRating(rating)
	d = fsrsDifficulty(d)
	// FSRS-5: higher ratings lower difficulty, with linear damping near D=10
	// and mean reversion toward the initial difficulty of an Easy response.
	delta := -defaultWeights[6] * float64(rating-3)
	damped := d + delta*(10-d)/9
	newD := defaultWeights[7]*InitialDifficulty(Easy) + (1-defaultWeights[7])*damped
	return clamp(newD, 1, 10)
}

func nextRecallStability(d, s, r float64, rating Rating) float64 {
	d = fsrsDifficulty(d)
	s = fsrsPositiveStability(s, fsrsEpsilon)
	r = fsrsRetrievability(r)
	modifier := 1.0
	switch clampRating(rating) {
	case Hard:
		modifier = defaultWeights[15]
	case Easy:
		modifier = defaultWeights[16]
	}
	next := s * (math.Exp(defaultWeights[8])*(11-d)*math.Pow(s, -defaultWeights[9])*(math.Exp(defaultWeights[10]*(1-r))-1)*modifier + 1)
	return fsrsPositiveStability(next, s)
}

func nextForgetStability(d, s, r float64) float64 {
	d = fsrsDifficulty(d)
	s = fsrsPositiveStability(s, fsrsEpsilon)
	r = fsrsRetrievability(r)
	next := defaultWeights[11] * math.Pow(d, -defaultWeights[12]) * (math.Pow(s+1, defaultWeights[13]) - 1) * math.Exp(defaultWeights[14]*(1-r))
	return fsrsPositiveStability(next, fsrsEpsilon)
}

func nextShortTermStability(s float64, rating Rating) float64 {
	s = fsrsPositiveStability(s, fsrsEpsilon)
	rating = clampRating(rating)
	next := s * math.Exp(defaultWeights[17]*(float64(rating-3)+defaultWeights[18]))
	return fsrsPositiveStability(next, s)
}

func fsrsDifficulty(d float64) float64 {
	if math.IsNaN(d) || math.IsInf(d, 0) {
		return InitialDifficulty(Good)
	}
	return clamp(d, 1, 10)
}

func fsrsPositiveStability(s, fallback float64) float64 {
	if s <= 0 || math.IsNaN(s) || math.IsInf(s, 0) {
		return fallback
	}
	return s
}

func fsrsRetrievability(r float64) float64 {
	if math.IsNaN(r) || math.IsInf(r, 0) {
		return 0
	}
	return clamp(r, 0, 1)
}

func NextInterval(stability, desiredRetention float64) int {
	stability = fsrsPositiveStability(stability, fsrsEpsilon)
	if desiredRetention <= 0 || desiredRetention > 1 || math.IsNaN(desiredRetention) || math.IsInf(desiredRetention, 0) {
		desiredRetention = FSRSDesiredRetention
	}
	interval := stability * ((math.Pow(desiredRetention, 1.0/fsrsDecay) - 1) / fsrsFactor)
	if interval >= fsrsMaximumInterval {
		return fsrsMaximumInterval
	}
	return int(math.Max(1, math.Round(interval)))
}

// ReviewCard advances the FSRS-5 memory state on an observed response.
// Existing persisted cards are not reset or retrospectively recalibrated: their
// finite S/D estimates remain the prior for the next update. Historical values
// produced by the former mixed formulas therefore remain legacy estimates until
// enough new evidence is collected; adopting this version cannot validate them.
func ReviewCard(card FSRSCard, rating Rating, now time.Time) FSRSCard {
	rating = clampRating(rating)
	var lastReview *time.Time
	if !card.LastReview.IsZero() {
		lastReview = &card.LastReview
	}
	elapsedDays := CurrentElapsedDays(now, lastReview)
	r := Retrievability(elapsedDays, card.Stability)
	newCard := card
	newCard.ElapsedDays = elapsedDays
	newCard.LastReview = now
	newCard.Reps++

	switch card.State {
	case New:
		newCard.Stability = InitialStability(rating)
		newCard.Difficulty = InitialDifficulty(rating)
		if rating == Again {
			newCard.State = Learning
			newCard.ScheduledDays = 0
		} else {
			newCard.State = Review
			newCard.ScheduledDays = NextInterval(newCard.Stability, FSRSDesiredRetention)
		}
	case Learning, Relearning, Review:
		newCard.Difficulty = nextDifficulty(card.Difficulty, rating)
		switch {
		case card.Stability <= 0 || math.IsNaN(card.Stability) || math.IsInf(card.Stability, 0):
			// Repair malformed imported state without resetting its history.
			newCard.Stability = InitialStability(rating)
		case elapsedDays == 0 && lastReview != nil:
			// FSRS-5 has a separate memory update for reviews within 24 hours.
			// The whole-day storage format remains sufficient for this branch.
			newCard.Stability = nextShortTermStability(card.Stability, rating)
		case rating == Again:
			// The reference short-term scheduler caps lapse stability so a
			// subsequent Good relearning response cannot exceed pre-lapse S.
			maxAfterLapse := card.Stability / math.Exp(defaultWeights[17]*defaultWeights[18])
			newCard.Stability = math.Min(maxAfterLapse, nextForgetStability(card.Difficulty, card.Stability, r))
		default:
			newCard.Stability = nextRecallStability(card.Difficulty, card.Stability, r, rating)
		}
		if rating == Again {
			if card.State == Review {
				newCard.Lapses++
				newCard.State = Relearning
			}
			newCard.ScheduledDays = 0
		} else {
			newCard.State = Review
			newCard.ScheduledDays = NextInterval(newCard.Stability, FSRSDesiredRetention)
		}
	}
	return newCard
}
