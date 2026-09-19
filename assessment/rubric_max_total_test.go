// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package assessment

import (
	"reflect"
	"strings"
	"testing"
)

// max_total is a supported field of rubric_score_json, so a caller writing both
// documents naturally carries it up into rubric_json too, and a recorded
// session shows the attempt rejected for it. The rubric already accepts a
// redundant criterion max_score when it agrees with the frozen rubric and
// refuses it when it contradicts; the aggregate now follows the same rule
// instead of being refused outright.
func TestParseRubricAcceptsConsistentMaxTotal(t *testing.T) {
	want, err := ParseRubric(testRubric)
	if err != nil {
		t.Fatal(err)
	}

	consistent := strings.Replace(testRubric, `"passing_score":2.5`, `"max_total":3,"passing_score":2.5`, 1)
	got, err := ParseRubric(consistent)
	if err != nil {
		t.Fatalf("consistent max_total rejected: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("max_total changed the parsed rubric:\n got=%+v\nwant=%+v", got, want)
	}

	contradictory := strings.Replace(testRubric, `"passing_score":2.5`, `"max_total":99,"passing_score":2.5`, 1)
	if _, err := ParseRubric(contradictory); err == nil {
		t.Fatal("a max_total contradicting the criteria was accepted")
	}
}
