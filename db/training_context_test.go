// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT
package db

import (
	"context"
	"reflect"
	"testing"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func TestTrainingConversationProjectionDoesNotCreateLegacyBaseline(t *testing.T) {
	s := setupTestDB(t)
	tenant := nativeTestTenant(t, s, "context-read-only")
	principal := trainingPrincipal(t, s, tenant, 121, true)
	// Simulate a legacy personal domain and its selection, without creating
	// an immutable baseline through today's mutation path.
	domain := &models.Domain{ID: "legacy-context", LearnerID: principal.LearnerID, Name: "Read only", GraphVersion: 1}
	if _, err := s.RawDB().Exec(rb(s, "INSERT INTO domains(id,learner_id,tenant_id,name,personal_goal,graph_json,graph_version,created_at) VALUES(?,?,?,?,?,?,1,?)"), domain.ID, principal.LearnerID, tenant, domain.Name, "", `{"concepts":["premise"],"prerequisites":{}}`, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	selected := models.ConversationContext{ConversationID: "context", TrainingID: "personal:" + domain.ID, DomainID: domain.ID, Revision: "1", SelectionVersion: 1, Name: domain.Name}
	selected.Training = &models.TrainingContext{ID: selected.TrainingID, Kind: "personal", Name: domain.Name, DomainID: domain.ID, CurriculumRevision: "1"}
	if _, err := s.RawDB().Exec(rb(s, "INSERT INTO conversation_contexts(tenant_id,membership_id,learner_id,conversation_id,training_id,domain_id,revision,selection_version) VALUES(?,?,?,?,?,?,?,1)"), tenant, principal.MembershipID, principal.LearnerID, selected.ConversationID, selected.TrainingID, selected.DomainID, selected.Revision); err != nil {
		t.Fatal(err)
	}
	var got *models.ConversationContext
	err := s.WithTenantReadTx(t.Context(), principal.TenantScope(), func(ctx context.Context, scoped storeport.Store) error {
		out, err := scoped.(storeport.TrainingStore).TrainingAction(ctx, principal, "get_conversation_context", models.TrainingRequest{ConversationID: "context"})
		if err != nil {
			return err
		}
		got = out.(*models.ConversationContext)
		return nil
	})
	if err != nil || !reflect.DeepEqual(got, &selected) {
		t.Fatalf("context=%#v err=%v", got, err)
	}
	var count int
	if err := s.RawDB().QueryRow(rb(s, "SELECT COUNT(*) FROM curriculum_versions WHERE learner_id=? AND domain_id=?"), principal.LearnerID, domain.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("read created baseline count=%d err=%v", count, err)
	}
}

func TestTrainingConversationProjectionKeepsEnrolledVersionAndRights(t *testing.T) {
	s := setupTestDB(t)
	tenant := nativeTestTenant(t, s, "context-enrolled")
	owner := trainingPrincipal(t, s, tenant, 123, true)
	author := trainingPrincipal(t, s, tenant, 125, false)
	learner := trainingPrincipal(t, s, tenant, 127, false)
	stranger := trainingPrincipal(t, s, tenant, 129, false)
	source := trainingDomain(t, s, author, "Versioned course")
	shared := trainingCall[models.TrainingDocument](t, s, author, "share_training", models.TrainingRequest{ID: "personal:" + source.ID, Name: "Versioned course", Audience: "invitation", ExpectedRevision: "1", Key: "share"})
	trainingCall[models.TrainingDocument](t, s, author, "submit_training", models.TrainingRequest{ID: shared.Training.ID, ExpectedRevision: shared.Training.Revision, Key: "submit"})
	published := trainingCall[models.TrainingDocument](t, s, owner, "decide_training", models.TrainingRequest{ID: shared.Training.ID, ExpectedRevision: shared.Training.Revision, Decision: "approve", Reason: "Approved", Key: "approve"})
	trainingCall[models.TrainingDocument](t, s, author, "set_training_access", models.TrainingRequest{ID: published.Training.ID, ExpectedRevision: published.Training.Revision, MemberID: learner.MembershipID, Decision: "invite", Key: "invite"})
	trainingCall[models.FormationJoinResult](t, s, learner, "join_training", models.TrainingRequest{ID: published.Training.ID, Key: "join"})
	zero := 0
	selected := trainingCall[models.ConversationContext](t, s, learner, "select_conversation_training", models.TrainingRequest{ID: published.Training.ID, ConversationID: "course", ExpectedSelection: &zero, Key: "select"})
	if selected.Training == nil || selected.Training.Description != published.Training.Description || selected.Training.LearningVersionID != published.Training.VersionID || selected.Training.DomainID != selected.DomainID {
		t.Fatalf("missing enrolled programme metadata: %#v", selected)
	}
	published.Concepts[0].Description = "Next draft must not replace enrolled curriculum"
	trainingCall[models.TrainingDocument](t, s, author, "save_training_edit", models.TrainingRequest{ID: published.Training.ID, ExpectedRevision: published.Training.Revision, Document: &published, Key: "draft"})
	err := s.WithTenantReadTx(t.Context(), learner.TenantScope(), func(ctx context.Context, scoped storeport.Store) error {
		tx := scoped.(*Store)
		projection, err := tx.trainingDocumentView(ctx, learner, published.Training.ID, true)
		if err != nil {
			return err
		}
		if projection.Training.VersionID != published.Training.VersionID || len(projection.Modules) != 0 || len(projection.Concepts) != 0 || projection.Curriculum != nil {
			t.Fatalf("projection expanded or changed version: %#v", projection)
		}
		out, err := tx.TrainingAction(ctx, learner, "get_conversation_context", models.TrainingRequest{ConversationID: "course"})
		if err == nil && !reflect.DeepEqual(out, &selected) {
			t.Fatalf("context changed after draft: %#v", out)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	choices, err := s.ResolveResumeTrainings(t.Context(), learner, "", published.Training.Name)
	if err != nil || len(choices.Items) != 1 || choices.Items[0].LearningVersionID != selected.Training.LearningVersionID || choices.Items[0].DomainID != selected.DomainID {
		t.Fatalf("resume resolver changed enrolled version: %#v %v", choices, err)
	}
	choices, err = s.ResolveResumeTrainings(t.Context(), stranger, published.Training.ID, "")
	if err != nil || len(choices.Items) != 0 {
		t.Fatalf("resume implicitly enrolled a stranger: %#v %v", choices, err)
	}
	// Even a forged local selection row cannot grant access to a private course.
	if _, err := s.RawDB().Exec(rb(s, "INSERT INTO conversation_contexts(tenant_id,membership_id,learner_id,conversation_id,training_id,domain_id,revision,selection_version) VALUES(?,?,?,?,?,?,?,1)"), tenant, stranger.MembershipID, stranger.LearnerID, "forged", published.Training.ID, selected.DomainID, selected.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TrainingAction(t.Context(), stranger, "get_conversation_context", models.TrainingRequest{ConversationID: "forged"}); err == nil {
		t.Fatal("projection bypassed visibility")
	}
}
