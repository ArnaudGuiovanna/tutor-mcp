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
	fsrsDecay  = -0.5
	fsrsFactor = 19.0 / 81.0
	// fsrsEpsilon is the floor applied to Difficulty/Stability before they
	// feed math.Pow(x, -k) inside nextRecallStability/nextForgetStability.
	// Pow(0, -k) returns +Inf and Pow(neg, non-integer) returns NaN; both
	// would corrupt FSRSCard.Stability for the rest of the card's life.
	// Clamping inputs to a small positive number preserves the standard
	// update path for sane inputs and avoids the NaN/Inf without changing
	// observable behaviour.
	fsrsEpsilon = 1e-9
)

var defaultWeights = [19]float64{
	0.4072, 1.1829, 3.1262, 15.4722,
	7.2102, 0.5316, 1.0651, 0.0589,
	1.5330, 0.1544, 1.0166, 1.9210,
	0.0854, 0.2698, 2.2694, 0.2061,
	0.2971, 0.6754, 0.5225,
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
	if stability <= 0 {
		return 0
	}
	return math.Pow(1+fsrsFactor*float64(elapsedDays)/stability, fsrsDecay)
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
	newD := defaultWeights[7]*InitialDifficulty(Good) + (1-defaultWeights[7])*(d+defaultWeights[6]*float64(rating-3))
	return clamp(newD, 1, 10)
}

func nextRecallStability(d, s, r float64, rating Rating) float64 {
	modifier := 1.0
	switch rating {
	case Hard:
		modifier = defaultWeights[15]
	case Easy:
		modifier = defaultWeights[17]
	}
	// math.Pow(s, -w[9]) explodes to +Inf when s == 0 and is NaN for s < 0
	// (non-integer exponent). Clamp to fsrsEpsilon so degenerate cards still
	// update to a finite stability rather than poisoning the FSRS state.
	if s < fsrsEpsilon {
		s = fsrsEpsilon
	}
	return s * (math.Exp(defaultWeights[8])*(11-d)*math.Pow(s, -defaultWeights[9])*(math.Exp(defaultWeights[10]*(1-r))-1)*modifier + 1)
}

func nextForgetStability(d, s, r float64) float64 {
	// Same rationale as nextRecallStability: math.Pow(d, -w[12]) is +Inf for
	// d==0 and NaN for d<0. Stability also feeds math.Pow(s+1, w[13]) which
	// is well-defined for s >= -1 but goes negative for s < -1, so floor s
	// at zero to keep the result non-negative as the spec demands.
	if d < fsrsEpsilon {
		d = fsrsEpsilon
	}
	if s < 0 {
		s = 0
	}
	return defaultWeights[11] * math.Pow(d, -defaultWeights[12]) * (math.Pow(s+1, defaultWeights[13]) - 1) * math.Exp(defaultWeights[14]*(1-r))
}

func NextInterval(stability, desiredRetention float64) int {
	interval := stability / fsrsFactor * (math.Pow(desiredRetention, 1.0/fsrsDecay) - 1)
	days := int(math.Round(interval))
	if days < 1 {
		return 1
	}
	return days
}

func ReviewCard(card FSRSCard, rating Rating, now time.Time) FSRSCard {
	elapsedDays := 0
	if !card.LastReview.IsZero() {
		elapsedDays = int(now.Sub(card.LastReview).Hours() / 24)
	}
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
			newCard.ScheduledDays = NextInterval(newCard.Stability, 0.9)
		}
	case Learning, Relearning:
		if rating == Again {
			newCard.ScheduledDays = 0
		} else {
			if card.Stability > 0 {
				newCard.Stability = nextRecallStability(card.Difficulty, card.Stability, r, rating)
			} else {
				newCard.Stability = InitialStability(rating)
			}
			newCard.Difficulty = nextDifficulty(card.Difficulty, rating)
			newCard.State = Review
			newCard.ScheduledDays = NextInterval(newCard.Stability, 0.9)
		}
	case Review:
		newCard.Difficulty = nextDifficulty(card.Difficulty, rating)
		if rating == Again {
			newCard.Stability = nextForgetStability(card.Difficulty, card.Stability, r)
			newCard.Lapses++
			newCard.State = Relearning
			newCard.ScheduledDays = 0
		} else {
			newCard.Stability = nextRecallStability(card.Difficulty, card.Stability, r, rating)
			newCard.State = Review
			newCard.ScheduledDays = NextInterval(newCard.Stability, 0.9)
		}
	}
	return newCard
}
