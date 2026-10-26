// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func TestTrainingPersonalArchiveRestoreDeletePreservesEvidenceAndReplay(t *testing.T) {
	s := setupTestDB(t)
	tenant := nativeTestTenant(t, s, "training-lifecycle")
	owner := trainingPrincipal(t, s, tenant, 161, true)
	other := trainingPrincipal(t, s, tenant, 163, false)
	d := trainingDomain(t, s, owner, "Keep learning history")
	doc := trainingCall[models.TrainingDocument](t, s, owner, "get_training", models.TrainingRequest{ID: "personal:" + d.ID})
	if !doc.Training.CanArchive || !doc.Training.CanDelete || doc.Training.CanRestore {
		t.Fatal("wrong personal lifecycle capabilities")
	}
	selection := 0
	trainingCall[models.ConversationContext](t, s, owner, "select_conversation_training", models.TrainingRequest{ID: doc.Training.ID, ConversationID: "lifecycle-chat", ExpectedSelection: &selection, Key: "select"})
	in := models.TrainingRequest{ID: doc.Training.ID, ExpectedRevision: doc.Training.Revision, Key: "archive", Confirm: true}
	if _, err := s.TrainingAction(t.Context(), other, "archive_training", in); err == nil {
		t.Fatal("another member archived private training")
	}
	unconfirmed := in
	unconfirmed.Confirm = false
	if _, err := s.TrainingAction(t.Context(), owner, "archive_training", unconfirmed); err == nil {
		t.Fatal("archive accepted without confirmation")
	}
	trainingCall[map[string]any](t, s, owner, "archive_training", in)
	visible := trainingCall[models.TrainingPage](t, s, owner, "list_training_library", models.TrainingRequest{})
	if len(visible.Items) != 0 {
		t.Fatal("archived personal training still in active library")
	}
	archived := trainingCall[models.TrainingPage](t, s, owner, "list_training_library", models.TrainingRequest{Filter: "archived"})
	if len(archived.Items) != 1 || !archived.Items[0].CanRestore || archived.Items[0].CanLearn {
		t.Fatal("archive was not restorable")
	}
	cleared := trainingCall[models.ConversationContext](t, s, owner, "get_conversation_context", models.TrainingRequest{ConversationID: "lifecycle-chat"})
	if cleared.TrainingID != "" {
		t.Fatal("archived training remained active in a conversation")
	}
	in.Key = "restore"
	trainingCall[map[string]any](t, s, owner, "restore_training", in)
	in.Key = "stale-delete"
	in.ExpectedRevision = "999"
	if _, err := s.TrainingAction(t.Context(), owner, "delete_training", in); !errors.Is(err, storeport.ErrCurriculumVersionConflict) {
		t.Fatalf("stale deletion=%v", err)
	}
	in.Key = "delete"
	in.ExpectedRevision = doc.Training.Revision
	for range 2 {
		result := trainingCall[map[string]any](t, s, owner, "delete_training", in)
		if result["status"] != "deleted" {
			t.Fatal("deletion replay did not return original result")
		}
	}
	if _, err := s.TrainingAction(t.Context(), owner, "get_training", models.TrainingRequest{ID: doc.Training.ID}); err == nil {
		t.Fatal("deleted training readable as active")
	}
	var count int
	if err := s.queryRow(t.Context(), `SELECT COUNT(*) FROM concept_states WHERE domain_id=?`, d.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("learning state deleted: %d %v", count, err)
	}
	if err := s.queryRow(t.Context(), `SELECT COUNT(*) FROM curriculum_versions WHERE domain_id=?`, d.ID).Scan(&count); err != nil || count < 1 {
		t.Fatalf("curriculum evidence deleted: %d %v", count, err)
	}
}

func TestTrainingDraftDeletionAndPublishedWithdrawalKeepEnrolledVersion(t *testing.T) {
	s := setupTestDB(t)
	tenant := nativeTestTenant(t, s, "training-withdraw")
	owner := trainingPrincipal(t, s, tenant, 165, true)
	author := trainingPrincipal(t, s, tenant, 167, false)
	learner := trainingPrincipal(t, s, tenant, 169, false)
	newcomer := trainingPrincipal(t, s, tenant, 171, false)
	d := trainingDomain(t, s, author, "Original private programme")
	draft := trainingCall[models.TrainingDocument](t, s, author, "share_training", models.TrainingRequest{ID: "personal:" + d.ID, ExpectedRevision: "1", Name: "Unpublished", Audience: "open", Key: "copy-draft"})
	if !draft.Training.CanDelete {
		t.Fatal("author cannot delete own unpublished contribution")
	}
	in := models.TrainingRequest{ID: draft.Training.ID, ExpectedRevision: draft.Training.Revision, Key: "delete-draft", Confirm: true}
	for range 2 {
		trainingCall[map[string]any](t, s, author, "delete_training", in)
	}
	published := trainingCall[models.TrainingDocument](t, s, author, "share_training", models.TrainingRequest{ID: "personal:" + d.ID, ExpectedRevision: "1", Name: "Published", Audience: "open", Key: "copy-published"})
	published = trainingCall[models.TrainingDocument](t, s, author, "submit_training", models.TrainingRequest{ID: published.Training.ID, ExpectedRevision: published.Training.Revision, Key: "submit-published"})
	published = trainingCall[models.TrainingDocument](t, s, owner, "decide_training", models.TrainingRequest{ID: published.Training.ID, ExpectedRevision: published.Training.Revision, Decision: "approve", Reason: "approved", Key: "approve-published"})
	joined := trainingCall[models.FormationJoinResult](t, s, learner, "join_training", models.TrainingRequest{ID: published.Training.ID, Key: "join"})
	before := trainingCall[models.TrainingDocument](t, s, learner, "get_training", models.TrainingRequest{ID: published.Training.ID})
	// Preserve demonstrated learning, not merely the initial empty state.
	lastReview := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Microsecond)
	nextReview := lastReview.Add(7 * 24 * time.Hour)
	state := models.NewConceptStateInDomain(learner.LearnerID, before.Training.DomainID, "variables")
	state.PMastery, state.PLearn, state.PForget, state.PSlip, state.PGuess = .73, .23, .04, .09, .17
	state.Stability, state.Difficulty, state.Theta = 6.5, .41, .8
	state.Reps, state.Lapses, state.ElapsedDays, state.ScheduledDays = 7, 2, 1, 7
	state.CardState, state.LastReview, state.NextReview = "review", &lastReview, &nextReview
	if err := s.UpsertConceptState(t.Context(), state); err != nil {
		t.Fatal(err)
	}
	before = trainingCall[models.TrainingDocument](t, s, learner, "get_training", models.TrainingRequest{ID: published.Training.ID})
	if len(before.Training.Progress) != 1 || before.Training.Progress[0].Mastery != .73 {
		t.Fatalf("enriched progression is not visible before withdrawal: %+v", before.Training.Progress)
	}
	legacyBefore, err := s.GetConceptStateInDomain(t.Context(), learner.LearnerID, before.Training.DomainID, "variables")
	if err != nil {
		t.Fatal(err)
	}
	var conceptID string
	if err = s.queryRow(t.Context(), `SELECT id FROM formation_concepts WHERE tenant_id=? AND formation_version_id=? AND stable_key='variables'`, tenant, before.Training.VersionID).Scan(&conceptID); err != nil {
		t.Fatal(err)
	}
	scope := learner.TenantScope()
	scope.EnrollmentID = joined.Enrollment.ID
	canonicalBefore, err := s.GetEnrollmentConceptState(t.Context(), scope, conceptID)
	if err != nil || canonicalBefore.PMastery != .73 || canonicalBefore.Reps != 7 {
		t.Fatalf("canonical enriched progression: state=%+v err=%v", canonicalBefore, err)
	}
	in = models.TrainingRequest{ID: published.Training.ID, ExpectedRevision: published.Training.Revision, Confirm: true, Key: "forbidden-delete"}
	if _, err := s.TrainingAction(t.Context(), owner, "delete_training", in); err == nil {
		t.Fatal("published programme deleted")
	}
	in.Key = "withdraw"
	trainingCall[map[string]any](t, s, owner, "withdraw_training", in)
	after := trainingCall[models.TrainingDocument](t, s, learner, "get_training", models.TrainingRequest{ID: published.Training.ID})
	if after.Training.VersionID != before.Training.VersionID || after.Training.DomainID != before.Training.DomainID || !after.Training.CanLearn || after.Training.Status != "archived" {
		t.Fatalf("learner lost enrolled version %#v", after.Training)
	}
	legacyAfter, err := s.GetConceptStateInDomain(t.Context(), learner.LearnerID, after.Training.DomainID, "variables")
	if err != nil || !reflect.DeepEqual(legacyBefore, legacyAfter) {
		t.Fatalf("withdrawal changed BKT/FSRS state: before=%+v after=%+v err=%v", legacyBefore, legacyAfter, err)
	}
	canonicalAfter, err := s.GetEnrollmentConceptState(t.Context(), scope, conceptID)
	if err != nil || !reflect.DeepEqual(canonicalBefore, canonicalAfter) || !reflect.DeepEqual(before.Training.Progress, after.Training.Progress) {
		t.Fatalf("withdrawal changed canonical or displayed progression: before=%+v after=%+v err=%v", canonicalBefore, canonicalAfter, err)
	}
	if _, err := s.TrainingAction(t.Context(), newcomer, "join_training", models.TrainingRequest{ID: published.Training.ID, Key: "new-admission-after-withdrawal"}); !errors.Is(err, storeport.ErrFormationUnavailable) {
		t.Fatalf("new admission accepted through training API after withdrawal: %v", err)
	}
	if _, _, err := s.JoinFormation(t.Context(), newcomer, "direct-admission-after-withdrawal", joined.Enrollment.CohortID); !errors.Is(err, storeport.ErrFormationUnavailable) {
		t.Fatalf("new admission accepted through enrollment service after withdrawal: %v", err)
	}
	page := trainingCall[models.TrainingPage](t, s, learner, "list_training_library", models.TrainingRequest{})
	if len(page.Items) != 1 || page.Items[0].ID != published.Training.ID {
		t.Fatal("enrolled archived version disappeared from library")
	}
}

func TestTrainingPublishedLifecycleRequiresPublicationAuthority(t *testing.T) {
	s := setupTestDB(t)
	tenant := nativeTestTenant(t, s, "training-lifecycle-authority")
	owner := trainingPrincipal(t, s, tenant, 185, true)
	author := trainingPrincipal(t, s, tenant, 187, false)
	d := trainingDomain(t, s, author, "Submitted contribution")
	doc := trainingCall[models.TrainingDocument](t, s, author, "share_training", models.TrainingRequest{ID: "personal:" + d.ID, ExpectedRevision: "1", Name: "Owner-approved programme", Audience: "open", Key: "copy"})
	doc = trainingCall[models.TrainingDocument](t, s, author, "submit_training", models.TrainingRequest{ID: doc.Training.ID, ExpectedRevision: doc.Training.Revision, Key: "submit"})
	doc = trainingCall[models.TrainingDocument](t, s, owner, "decide_training", models.TrainingRequest{ID: doc.Training.ID, ExpectedRevision: doc.Training.Revision, Decision: "approve", Reason: "approved", Key: "approve"})
	contribution := trainingCall[models.TrainingDocument](t, s, author, "get_training", models.TrainingRequest{ID: doc.Training.ID})
	if !contribution.Training.CanEdit || contribution.Training.CanPublish || contribution.Training.CanArchive || contribution.Training.CanWithdraw {
		t.Fatalf("submitted author received publication lifecycle powers: %+v", contribution.Training)
	}
	in := models.TrainingRequest{ID: doc.Training.ID, ExpectedRevision: doc.Training.Revision, Key: "author-archive", Confirm: true}
	if _, err := s.TrainingAction(t.Context(), author, "archive_training", in); !errors.Is(err, storeport.ErrInvalidPrincipal) {
		t.Fatalf("submitted author archived published programme: %v", err)
	}
	in.Decision = "archive_training"
	if _, err := s.TrainingAction(t.Context(), author, "preview_training_lifecycle", in); !errors.Is(err, storeport.ErrInvalidPrincipal) {
		t.Fatalf("archive preview advertised an unauthorized operation: %v", err)
	}
	in.Decision, in.Key = "", "owner-withdraw"
	trainingCall[map[string]any](t, s, owner, "withdraw_training", in)
	contribution = trainingCall[models.TrainingDocument](t, s, author, "get_training", models.TrainingRequest{ID: doc.Training.ID})
	if contribution.Training.CanRestore {
		t.Fatal("submitted author can restore an offering withdrawn by the owner")
	}
	in.Key = "author-restore"
	if _, err := s.TrainingAction(t.Context(), author, "restore_training", in); !errors.Is(err, storeport.ErrInvalidPrincipal) {
		t.Fatalf("submitted author restored owner-withdrawn offering: %v", err)
	}
	ownerView := trainingCall[models.TrainingDocument](t, s, owner, "get_training", models.TrainingRequest{ID: doc.Training.ID})
	if !ownerView.Training.CanRestore {
		t.Fatal("owner lost permission to restore their published offering")
	}
	in.Key = "owner-restore"
	trainingCall[map[string]any](t, s, owner, "restore_training", in)
}
