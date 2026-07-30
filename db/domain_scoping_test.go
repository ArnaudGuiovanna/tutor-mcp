package db

import (
	"context"
	"testing"
	"time"

	"tutor-mcp/models"
)

func TestConceptStateIsScopedByDomain(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	graph := models.KnowledgeSpace{
		Concepts:      []string{"functions"},
		Prerequisites: map[string][]string{},
	}
	domainA, err := s.CreateDomain(ctx, "L1", "Programming", "", graph)
	if err != nil {
		t.Fatalf("create domain A: %v", err)
	}
	domainB, err := s.CreateDomain(ctx, "L1", "Mathematics", "", graph)
	if err != nil {
		t.Fatalf("create domain B: %v", err)
	}

	stateA := models.NewConceptStateInDomain("L1", domainA.ID, "functions")
	stateA.PMastery = 0.91
	stateB := models.NewConceptStateInDomain("L1", domainB.ID, "functions")
	stateB.PMastery = 0.22
	if err := s.UpsertConceptState(ctx, stateA); err != nil {
		t.Fatalf("upsert state A: %v", err)
	}
	if err := s.UpsertConceptState(ctx, stateB); err != nil {
		t.Fatalf("upsert state B: %v", err)
	}

	gotA, err := s.GetConceptStateInDomain(ctx, "L1", domainA.ID, "functions")
	if err != nil {
		t.Fatalf("get state A: %v", err)
	}
	gotB, err := s.GetConceptStateInDomain(ctx, "L1", domainB.ID, "functions")
	if err != nil {
		t.Fatalf("get state B: %v", err)
	}
	if gotA.PMastery != 0.91 || gotB.PMastery != 0.22 {
		t.Fatalf("cross-domain state collision: A=%v B=%v", gotA.PMastery, gotB.PMastery)
	}
	if gotA.DomainID != domainA.ID || gotB.DomainID != domainB.ID {
		t.Fatalf("domain identity lost: A=%q B=%q", gotA.DomainID, gotB.DomainID)
	}
}

func TestInteractionEvidenceIsScopedByDomain(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	graph := models.KnowledgeSpace{
		Concepts:      []string{"probability"},
		Prerequisites: map[string][]string{},
	}
	domainA, err := s.CreateDomain(ctx, "L1", "Statistics", "", graph)
	if err != nil {
		t.Fatalf("create domain A: %v", err)
	}
	domainB, err := s.CreateDomain(ctx, "L1", "Game design", "", graph)
	if err != nil {
		t.Fatalf("create domain B: %v", err)
	}

	for _, interaction := range []*models.Interaction{
		{
			LearnerID:         "L1",
			DomainID:          domainA.ID,
			Concept:           "probability",
			ActivityType:      string(models.ActivityMasteryChallenge),
			Success:           false,
			MisconceptionType: "independence",
		},
		{
			LearnerID:    "L1",
			DomainID:     domainB.ID,
			Concept:      "probability",
			ActivityType: string(models.ActivityPractice),
			Success:      true,
		},
	} {
		if err := s.CreateInteraction(ctx, interaction); err != nil {
			t.Fatalf("create interaction: %v", err)
		}
	}

	miscA, err := s.GetActiveMisconceptionsBatchInDomain(
		ctx, "L1", domainA.ID, []string{"probability"},
	)
	if err != nil {
		t.Fatalf("misconceptions A: %v", err)
	}
	miscB, err := s.GetActiveMisconceptionsBatchInDomain(
		ctx, "L1", domainB.ID, []string{"probability"},
	)
	if err != nil {
		t.Fatalf("misconceptions B: %v", err)
	}
	if !miscA["probability"] || miscB["probability"] {
		t.Fatalf("cross-domain misconception leak: A=%v B=%v", miscA, miscB)
	}

	historyA, err := s.GetActionHistoryForConceptInDomain(
		ctx, "L1", domainA.ID, "probability", 50,
	)
	if err != nil {
		t.Fatalf("history A: %v", err)
	}
	historyB, err := s.GetActionHistoryForConceptInDomain(
		ctx, "L1", domainB.ID, "probability", 50,
	)
	if err != nil {
		t.Fatalf("history B: %v", err)
	}
	if historyA.MasteryChallengeCount != 1 || historyB.MasteryChallengeCount != 0 {
		t.Fatalf("cross-domain action-history leak: A=%+v B=%+v", historyA, historyB)
	}
}

func TestDomainIdentityBackfillOnlyInfersUniqueConceptMembership(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	unique, err := s.CreateDomain(ctx, "L1", "Unique", "", models.KnowledgeSpace{
		Concepts:      []string{"unique-concept"},
		Prerequisites: map[string][]string{},
	})
	if err != nil {
		t.Fatalf("create unique domain: %v", err)
	}
	for _, name := range []string{"Shared A", "Shared B"} {
		if _, err := s.CreateDomain(ctx, "L1", name, "", models.KnowledgeSpace{
			Concepts:      []string{"shared-concept"},
			Prerequisites: map[string][]string{},
		}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}

	now := time.Now().UTC()
	for _, concept := range []string{"unique-concept", "shared-concept"} {
		if _, err := s.root.Exec(
			rb(s, `INSERT INTO interactions
			    (learner_id, domain_id, concept, activity_type, success, created_at)
			 VALUES ('L1', '', ?, 'PRACTICE', 1, ?)`),
			concept, now,
		); err != nil {
			t.Fatalf("seed interaction %s: %v", concept, err)
		}
		if _, err := s.root.Exec(
			rb(s, `INSERT INTO calibration_records
			    (prediction_id, learner_id, domain_id, concept_id, predicted, created_at)
			 VALUES (?, 'L1', '', ?, 0.5, ?)`),
			"cal-"+concept, concept, now,
		); err != nil {
			t.Fatalf("seed calibration %s: %v", concept, err)
		}
		if _, err := s.root.Exec(
			rb(s, `INSERT INTO transfer_records
			    (learner_id, domain_id, concept_id, context_type, score, created_at)
			 VALUES ('L1', '', ?, 'near', 0.5, ?)`),
			concept, now,
		); err != nil {
			t.Fatalf("seed transfer %s: %v", concept, err)
		}
	}

	version := "0013_backfill_unique_domain_identity"
	if s.dialect == DialectPostgres {
		version = "postgres_0003_backfill_unique_domain_identity"
	}
	if _, err := s.root.Exec(rb(s, `DELETE FROM schema_migrations WHERE version = ?`), version); err != nil {
		t.Fatalf("reset backfill migration: %v", err)
	}
	if s.dialect == DialectPostgres {
		if err := MigratePostgres(ctx, s.root); err != nil {
			t.Fatalf("rerun postgres backfill: %v", err)
		}
	} else if err := Migrate(s.root); err != nil {
		t.Fatalf("rerun sqlite backfill: %v", err)
	}

	for _, table := range []string{"interactions", "calibration_records", "transfer_records"} {
		conceptColumn := "concept"
		if table != "interactions" {
			conceptColumn = "concept_id"
		}
		var uniqueDomain, sharedDomain string
		query := "SELECT domain_id FROM " + table + " WHERE " + conceptColumn + " = ?"
		if err := s.root.QueryRow(rb(s, query), "unique-concept").Scan(&uniqueDomain); err != nil {
			t.Fatalf("read %s unique row: %v", table, err)
		}
		if err := s.root.QueryRow(rb(s, query), "shared-concept").Scan(&sharedDomain); err != nil {
			t.Fatalf("read %s shared row: %v", table, err)
		}
		if uniqueDomain != unique.ID {
			t.Errorf("%s unique domain = %q, want %q", table, uniqueDomain, unique.ID)
		}
		if sharedDomain != "" {
			t.Errorf("%s ambiguous domain = %q, want blank", table, sharedDomain)
		}
	}
}

func TestCalibrationAndTransferEvidenceIsScopedByDomain(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	graph := models.KnowledgeSpace{
		Concepts:      []string{"functions"},
		Prerequisites: map[string][]string{},
	}
	domainA, err := s.CreateDomain(ctx, "L1", "Programming", "", graph)
	if err != nil {
		t.Fatalf("create domain A: %v", err)
	}
	domainB, err := s.CreateDomain(ctx, "L1", "Mathematics", "", graph)
	if err != nil {
		t.Fatalf("create domain B: %v", err)
	}

	for _, record := range []*models.CalibrationRecord{
		{PredictionID: "cal-a", LearnerID: "L1", DomainID: domainA.ID, ConceptID: "functions", Predicted: 0.9},
		{PredictionID: "cal-b", LearnerID: "L1", DomainID: domainB.ID, ConceptID: "functions", Predicted: 0.1},
	} {
		if err := s.CreateCalibrationPrediction(ctx, record); err != nil {
			t.Fatalf("create calibration: %v", err)
		}
		actual := 0.5
		if err := s.CompleteCalibrationRecord(ctx, record.PredictionID, "L1", actual, record.Predicted-actual); err != nil {
			t.Fatalf("complete calibration: %v", err)
		}
	}
	biasA, err := s.GetCalibrationBiasInDomain(ctx, "L1", domainA.ID, 20)
	if err != nil {
		t.Fatalf("get bias A: %v", err)
	}
	biasB, err := s.GetCalibrationBiasInDomain(ctx, "L1", domainB.ID, 20)
	if err != nil {
		t.Fatalf("get bias B: %v", err)
	}
	if biasA <= 0 || biasB >= 0 {
		t.Fatalf("cross-domain calibration leak: A=%v B=%v", biasA, biasB)
	}

	for _, record := range []*models.TransferRecord{
		{LearnerID: "L1", DomainID: domainA.ID, ConceptID: "functions", ContextType: "near", Score: 0.9},
		{LearnerID: "L1", DomainID: domainB.ID, ConceptID: "functions", ContextType: "near", Score: 0.1},
	} {
		if err := s.CreateTransferRecord(ctx, record); err != nil {
			t.Fatalf("create transfer: %v", err)
		}
	}
	transfersA, err := s.GetTransferScoresInDomain(ctx, "L1", domainA.ID, "functions")
	if err != nil {
		t.Fatalf("get transfers A: %v", err)
	}
	transfersB, err := s.GetTransferScoresInDomain(ctx, "L1", domainB.ID, "functions")
	if err != nil {
		t.Fatalf("get transfers B: %v", err)
	}
	if len(transfersA) != 1 || len(transfersB) != 1 || transfersA[0].Score != 0.9 || transfersB[0].Score != 0.1 {
		t.Fatalf("cross-domain transfer leak: A=%+v B=%+v", transfersA, transfersB)
	}
}
