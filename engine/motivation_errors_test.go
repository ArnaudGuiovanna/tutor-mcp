// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package engine

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"tutor-mcp/db"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

var errInjectedMotivationDependency = errors.New("injected motivation dependency failure")

type failingMotivationStore struct {
	storeport.Store
	fail string
}

func (s *failingMotivationStore) GetConceptStateInDomain(ctx context.Context, learnerID, domainID, concept string) (*models.ConceptState, error) {
	if s.fail == motivationComponentConceptState {
		return nil, errInjectedMotivationDependency
	}
	return s.Store.GetConceptStateInDomain(ctx, learnerID, domainID, concept)
}

func (s *failingMotivationStore) LastFailureOnConceptInDomain(ctx context.Context, learnerID, domainID, concept string, window time.Duration) (*models.Interaction, error) {
	if s.fail == motivationComponentRecentFailure {
		return nil, errInjectedMotivationDependency
	}
	return s.Store.LastFailureOnConceptInDomain(ctx, learnerID, domainID, concept, window)
}

func (s *failingMotivationStore) CountSessionsOnConceptInDomain(ctx context.Context, learnerID, domainID, concept string) (int, error) {
	if s.fail == motivationComponentSessionsOnConcept {
		return 0, errInjectedMotivationDependency
	}
	return s.Store.CountSessionsOnConceptInDomain(ctx, learnerID, domainID, concept)
}

func (s *failingMotivationStore) SelfInitiatedRatioInDomain(ctx context.Context, learnerID, domainID, concept string) (float64, error) {
	if s.fail == motivationComponentSelfInitiatedRatio {
		return 0, errInjectedMotivationDependency
	}
	return s.Store.SelfInitiatedRatioInDomain(ctx, learnerID, domainID, concept)
}

func (s *failingMotivationStore) GetRecentAffectStates(ctx context.Context, learnerID string, limit int) ([]*models.AffectState, error) {
	if s.fail == motivationComponentRecentAffect {
		return nil, errInjectedMotivationDependency
	}
	return s.Store.GetRecentAffectStates(ctx, learnerID, limit)
}

func (s *failingMotivationStore) UpdateDomainLastValueAxis(ctx context.Context, domainID, axis string) error {
	if s.fail == motivationComponentValueAxisRotation {
		return errInjectedMotivationDependency
	}
	return s.Store.UpdateDomainLastValueAxis(ctx, domainID, axis)
}

func motivationDependencyFixture(t *testing.T) (*db.Store, string, *models.Domain) {
	t.Helper()
	_, store, learnerID := newMotivationStore(t)
	domain, err := store.CreateDomainWithValueFramings(
		context.Background(), learnerID, "reliability", "ship reliable systems",
		models.KnowledgeSpace{Concepts: []string{"failure handling"}},
		`{"financial":"reduce incident cost","employment":"operate production systems"}`,
	)
	if err != nil {
		t.Fatal(err)
	}
	state := models.NewConceptStateInDomain(learnerID, domain.ID, "failure handling")
	state.PMastery = 0.2
	state.CardState = "learning"
	if err := store.UpsertConceptState(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	return store, learnerID, domain
}

func TestMotivationBuildRequiredDependencyFailures(t *testing.T) {
	components := []string{
		motivationComponentConceptState,
		motivationComponentRecentFailure,
		motivationComponentSessionsOnConcept,
		motivationComponentRecentAffect,
		motivationComponentValueAxisRotation,
	}
	for _, component := range components {
		t.Run(component, func(t *testing.T) {
			store, learnerID, domain := motivationDependencyFixture(t)
			engine := NewMotivationEngine(&failingMotivationStore{Store: store, fail: component})
			brief, err := engine.Build(
				context.Background(), learnerID, domain, "failure handling",
				models.ActivityNewConcept, false, 1,
			)
			if brief != nil {
				t.Fatalf("required failure returned a brief: %+v", brief)
			}
			var dependencyErr *MotivationDependencyError
			if !errors.As(err, &dependencyErr) {
				t.Fatalf("error=%T %v, want MotivationDependencyError", err, err)
			}
			if dependencyErr.Component != component {
				t.Fatalf("component=%q, want %q", dependencyErr.Component, component)
			}
			if !errors.Is(err, errInjectedMotivationDependency) {
				t.Fatalf("error does not preserve injected cause: %v", err)
			}
			if !strings.Contains(err.Error(), component) {
				t.Fatalf("error lacks dependency context %q: %v", component, err)
			}
			if strings.Contains(err.Error(), errInjectedMotivationDependency.Error()) {
				t.Fatalf("required dependency error leaked underlying detail: %v", err)
			}

			if component == motivationComponentValueAxisRotation {
				stored, readErr := store.GetDomainByID(context.Background(), domain.ID)
				if readErr != nil {
					t.Fatal(readErr)
				}
				if stored.LastValueAxis != "" {
					t.Fatalf("failed rotation changed last_value_axis to %q", stored.LastValueAxis)
				}
			}
		})
	}
}

func TestMotivationBuildOptionalSelfInitiatedRatioIsObservable(t *testing.T) {
	store, learnerID, domain := motivationDependencyFixture(t)
	engine := NewMotivationEngine(&failingMotivationStore{
		Store: store,
		fail:  motivationComponentSelfInitiatedRatio,
	})
	brief, err := engine.Build(
		context.Background(), learnerID, domain, "failure handling",
		models.ActivityNewConcept, false, 1,
	)
	if brief == nil || brief.Kind != models.MotivationKindCompetenceValue {
		t.Fatalf("optional failure lost usable brief: %+v", brief)
	}
	assertMotivationDegradation(t, err, []string{motivationComponentSelfInitiatedRatio})
	if !errors.Is(err, errInjectedMotivationDependency) {
		t.Fatalf("degradation does not preserve injected cause: %v", err)
	}
}

func TestMotivationBuildMalformedValueFramingsIsObservable(t *testing.T) {
	store, learnerID, domain := motivationDependencyFixture(t)
	domain.ValueFramingsJSON = `{"financial":`
	brief, err := NewMotivationEngine(store).Build(
		context.Background(), learnerID, domain, "failure handling",
		models.ActivityNewConcept, false, 1,
	)
	if brief == nil || brief.Kind != models.MotivationKindCompetenceValue {
		t.Fatalf("malformed optional framing lost usable brief: %+v", brief)
	}
	if brief.ValueFraming == nil || brief.ValueFraming.Statement != "" {
		t.Fatalf("malformed framing should use generic statement path: %+v", brief.ValueFraming)
	}
	assertMotivationDegradation(t, err, []string{motivationComponentValueFramings})
}

func TestMotivationBuildJoinsOptionalDegradations(t *testing.T) {
	store, learnerID, domain := motivationDependencyFixture(t)
	domain.ValueFramingsJSON = `{bad json}`
	engine := NewMotivationEngine(&failingMotivationStore{
		Store: store,
		fail:  motivationComponentSelfInitiatedRatio,
	})
	brief, err := engine.Build(
		context.Background(), learnerID, domain, "failure handling",
		models.ActivityNewConcept, false, 1,
	)
	if brief == nil {
		t.Fatal("joined optional degradations must preserve the brief")
	}
	assertMotivationDegradation(t, err, []string{
		motivationComponentSelfInitiatedRatio,
		motivationComponentValueFramings,
	})
	if strings.Contains(err.Error(), errInjectedMotivationDependency.Error()) {
		t.Fatalf("degradation error leaked underlying dependency detail: %v", err)
	}
}

func assertMotivationDegradation(t *testing.T, err error, want []string) {
	t.Helper()
	var degraded *MotivationDegradationError
	if !errors.As(err, &degraded) {
		t.Fatalf("error=%T %v, want MotivationDegradationError", err, err)
	}
	if !reflect.DeepEqual(degraded.Components, want) {
		t.Fatalf("degraded components=%v, want %v", degraded.Components, want)
	}
	for _, component := range want {
		if !strings.Contains(err.Error(), component) {
			t.Fatalf("degradation error lacks component %q: %v", component, err)
		}
	}
}
