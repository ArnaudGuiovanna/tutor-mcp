// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT
package db

import (
	"encoding/json"
	"errors"
	"testing"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func trainingPrincipal(t *testing.T, s *Store, tenant string, index byte, owner bool) models.Principal {
	t.Helper()
	var n *models.NativeIdentity
	var err error
	if owner {
		n, err = s.BootstrapNativeOwner(t.Context(), nativeTestActor(), models.NativeOwnerProvision{TenantID: tenant, DisplayName: "Direction", PublicKey: nativeTestKey(index), RecoveryPublicKey: nativeTestKey(index + 1)})
		if err != nil {
			t.Fatal(err)
		}
	} else {
		n = nativeTestMember(t, s, tenant, index, []string{models.RoleLearner})
	}
	scopes := []string{models.OAuthScopeLearnerRead, models.OAuthScopeLearnerWrite, models.OAuthScopeFormationContribute}
	if owner {
		scopes = []string{models.OAuthScopeFormationRead, models.OAuthScopeFormationWrite}
	}
	p, err := s.CreateNativeSession(t.Context(), nativeTestSession(n, "training-initial-"+n.ID, scopes))
	if err != nil {
		t.Fatal(err)
	}
	if owner {
		if _, err = s.EnableNativeLearning(t.Context(), p); err != nil {
			t.Fatal(err)
		}
		n, err = s.GetNativeIdentity(t.Context(), tenant, n.PublicKey, "session")
		if err != nil {
			t.Fatal(err)
		}
		p, err = s.CreateNativeSession(t.Context(), nativeTestSession(n, "training-learning-"+n.ID, []string{models.OAuthScopeLearnerRead, models.OAuthScopeLearnerWrite, models.OAuthScopeFormationContribute, models.OAuthScopeFormationRead, models.OAuthScopeFormationWrite}))
		if err != nil {
			t.Fatal(err)
		}
		if err = s.SetInstitutionLearningPolicy(t.Context(), p, true); err != nil {
			t.Fatal(err)
		}
	}
	return p
}
func trainingCall[T any](t *testing.T, s *Store, p models.Principal, action string, in models.TrainingRequest) T {
	t.Helper()
	out, err := s.TrainingAction(t.Context(), p, action, in)
	if err != nil {
		t.Fatalf("%s: %v", action, err)
	}
	// Idempotent replays may be decoded as an untyped JSON value.
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var value T
	if err = json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	return value
}
func trainingDomain(t *testing.T, s *Store, p models.Principal, name string) *models.Domain {
	t.Helper()
	d, err := s.CreateDomain(t.Context(), p.LearnerID, name, "Objectif privé", models.KnowledgeSpace{Concepts: []string{"variables"}, Prerequisites: map[string][]string{}})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.InsertConceptStateIfNotExists(t.Context(), models.NewConceptStateInDomain(p.LearnerID, d.ID, "variables")); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestTrainingConversationLibraryAndRevisionIsolation(t *testing.T) {
	s := setupTestDB(t)
	tenant := nativeTestTenant(t, s, "training-private")
	owner := trainingPrincipal(t, s, tenant, 91, true)
	other := trainingPrincipal(t, s, tenant, 93, false)
	goDomain := trainingDomain(t, s, owner, "Go")
	python := trainingDomain(t, s, owner, "Python")
	trainingDomain(t, s, owner, "Astronomie")
	first := trainingCall[models.TrainingPage](t, s, owner, "list_training_library", models.TrainingRequest{Limit: 2})
	if len(first.Items) != 2 || first.NextAfter == "" {
		t.Fatalf("page=%#v", first)
	}
	second := trainingCall[models.TrainingPage](t, s, owner, "list_training_library", models.TrainingRequest{Limit: 2, After: first.NextAfter})
	if len(second.Items) != 1 {
		t.Fatalf("second=%#v", second)
	}
	search := trainingCall[models.TrainingPage](t, s, owner, "list_training_library", models.TrainingRequest{Query: "python"})
	if len(search.Items) != 1 || search.Items[0].DomainID != python.ID {
		t.Fatalf("search=%#v", search)
	}
	hidden := trainingCall[models.TrainingPage](t, s, other, "list_training_library", models.TrainingRequest{})
	if len(hidden.Items) != 0 {
		t.Fatalf("private programmes leaked: %#v", hidden)
	}
	if _, err := s.TrainingAction(t.Context(), other, "get_training", models.TrainingRequest{ID: "personal:" + goDomain.ID}); err == nil {
		t.Fatal("private direct read leaked")
	}
	version := 0
	for i, id := range []string{goDomain.ID, python.ID, goDomain.ID} {
		c := trainingCall[models.ConversationContext](t, s, owner, "select_conversation_training", models.TrainingRequest{ID: "personal:" + id, ConversationID: "conversation-one", ExpectedSelection: &version, Key: "selection-" + string(rune('a'+i))})
		version = c.SelectionVersion
		if c.DomainID != id {
			t.Fatalf("selected=%#v", c)
		}
	}
	resumed := trainingCall[models.ConversationContext](t, s, owner, "get_conversation_context", models.TrainingRequest{ConversationID: "conversation-one"})
	if resumed.DomainID != goDomain.ID {
		t.Fatalf("resume=%#v", resumed)
	}
	differentAccount := trainingCall[models.ConversationContext](t, s, other, "get_conversation_context", models.TrainingRequest{ConversationID: "conversation-one"})
	if differentAccount.DomainID != "" {
		t.Fatal("context leaked to another account")
	}
	stale := 0
	if _, err := s.TrainingAction(t.Context(), owner, "select_conversation_training", models.TrainingRequest{ID: "personal:" + python.ID, ConversationID: "conversation-one", ExpectedSelection: &stale, Key: "stale"}); !errors.Is(err, storeport.ErrCurriculumVersionConflict) {
		t.Fatalf("stale selection=%v", err)
	}
	doc := trainingCall[models.TrainingDocument](t, s, owner, "get_training", models.TrainingRequest{ID: "personal:" + goDomain.ID})
	doc.Curriculum.Concepts[0].Description = "Une nouvelle définition"
	request := models.TrainingRequest{ID: doc.Training.ID, ExpectedRevision: doc.Training.Revision, Document: &doc, Key: "edit-a"}
	preview := trainingCall[struct {
		Invalidated []string `json:"invalidated_concept_ids"`
	}](t, s, owner, "preview_training_edit", request)
	if len(preview.Invalidated) != 1 {
		t.Fatalf("preview=%#v", preview)
	}
	proposal := trainingCall[models.TrainingProposal](t, s, owner, "propose_training_edit", request)
	if proposal.ExpectedRevision != "1" {
		t.Fatal(proposal)
	}
	unchanged := trainingCall[models.TrainingDocument](t, s, owner, "get_training", models.TrainingRequest{ID: doc.Training.ID})
	if unchanged.Curriculum.Concepts[0].Description != "" || unchanged.Proposal == nil {
		t.Fatal("proposal changed current programme")
	}
	request.Key = "commit-a"
	committed := trainingCall[models.TrainingDocument](t, s, owner, "save_training_edit", request)
	if committed.Training.Revision != "2" || committed.Curriculum.Concepts[0].Description != "Une nouvelle définition" {
		t.Fatalf("commit=%#v", committed)
	}
	replay := trainingCall[models.TrainingDocument](t, s, owner, "save_training_edit", request)
	if replay.Training.Revision != "2" {
		t.Fatal("retry created another revision")
	}
	request.Key = "stale-edit"
	if _, err := s.TrainingAction(t.Context(), owner, "save_training_edit", request); !errors.Is(err, storeport.ErrCurriculumVersionConflict) {
		t.Fatalf("stale edit=%v", err)
	}
	current, err := s.GetLearnerDomainByID(t.Context(), owner.LearnerID, python.ID)
	if err != nil || current.GraphVersion != 1 {
		t.Fatal("another training changed", err)
	}
}

func TestTrainingContributionReviewPrivacyAndImmutableVersions(t *testing.T) {
	s := setupTestDB(t)
	tenant := nativeTestTenant(t, s, "training-contribution")
	owner := trainingPrincipal(t, s, tenant, 101, true)
	author := trainingPrincipal(t, s, tenant, 103, false)
	viewer := trainingPrincipal(t, s, tenant, 105, false)
	d := trainingDomain(t, s, author, "Go privé")
	shared := trainingCall[models.TrainingDocument](t, s, author, "share_training", models.TrainingRequest{ID: "personal:" + d.ID, Name: "Go pour Alice", Audience: "invitation", ExpectedRevision: "1", Key: "share"})
	if shared.Training.Kind != "shared" || shared.Training.CanPublish || shared.Training.DomainID != "" || len(shared.Training.Progress) != 0 {
		t.Fatalf("copy=%#v", shared)
	}
	if author.Authorize(models.PermissionFormationWrite, models.AuthorizationResource{TenantID: tenant}) {
		t.Fatal("contributor acquired administrative rights")
	}
	for _, action := range []string{"list_training_library", "get_training"} {
		request := models.TrainingRequest{}
		if action == "get_training" {
			request.ID = shared.Training.ID
		}
		out, err := s.TrainingAction(t.Context(), viewer, action, request)
		if action == "get_training" && err == nil {
			t.Fatal("draft read leaked")
		}
		if action == "list_training_library" && len(out.(models.TrainingPage).Items) != 0 {
			t.Fatal("draft listed")
		}
	}
	if _, err := s.TrainingAction(t.Context(), author, "publish_training", models.TrainingRequest{ID: shared.Training.ID, ExpectedRevision: shared.Training.Revision, Key: "forbidden-publish"}); err == nil {
		t.Fatal("unreviewed contribution published")
	}
	pending := trainingCall[models.TrainingDocument](t, s, author, "submit_training", models.TrainingRequest{ID: shared.Training.ID, ExpectedRevision: shared.Training.Revision, Key: "submit"})
	if pending.Submission == nil || pending.Submission.Status != "pending" {
		t.Fatal("submission missing")
	}
	review := trainingCall[models.TrainingPage](t, s, owner, "list_training_library", models.TrainingRequest{Filter: "review"})
	if len(review.Items) != 1 {
		t.Fatalf("owner inbox=%#v", review)
	}
	returned := trainingCall[models.TrainingDocument](t, s, owner, "decide_training", models.TrainingRequest{ID: shared.Training.ID, ExpectedRevision: shared.Training.Revision, Decision: "changes_requested", Reason: "Précisez les objectifs", Key: "changes"})
	if returned.Submission.Status != "changes_requested" {
		t.Fatal(returned.Submission)
	}
	shared.Concepts[0].Description = "Définition corrigée"
	edited := trainingCall[models.TrainingDocument](t, s, author, "save_training_edit", models.TrainingRequest{ID: shared.Training.ID, ExpectedRevision: shared.Training.Revision, Document: &shared, Key: "correction"})
	pending = trainingCall[models.TrainingDocument](t, s, author, "submit_training", models.TrainingRequest{ID: edited.Training.ID, ExpectedRevision: edited.Training.Revision, Key: "resubmit"})
	published := trainingCall[models.TrainingDocument](t, s, owner, "decide_training", models.TrainingRequest{ID: pending.Training.ID, ExpectedRevision: pending.Training.Revision, Decision: "approve", Reason: "Programme validé", Key: "approve"})
	if published.Training.Status != "published" || published.Training.Audience != "invitation" || published.Training.CohortID == "" {
		t.Fatalf("publication=%#v", published.Training)
	}
	if _, err := s.TrainingAction(t.Context(), viewer, "get_training", models.TrainingRequest{ID: published.Training.ID}); err == nil {
		t.Fatal("nominative direct read leaked after publication")
	}
	page, err := s.ListLearnerFormations(t.Context(), viewer, false, "", 20)
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("legacy catalogue leaked: %#v %v", page, err)
	}
	if _, err = s.GetLearnerFormation(t.Context(), viewer, published.Training.CohortID); err == nil {
		t.Fatal("guessed cohort leaked")
	}
	trainingCall[models.TrainingDocument](t, s, author, "set_training_access", models.TrainingRequest{ID: published.Training.ID, ExpectedRevision: published.Training.Revision, MemberID: viewer.MembershipID, Decision: "invite", Key: "invite"})
	visible := trainingCall[models.TrainingDocument](t, s, viewer, "get_training", models.TrainingRequest{ID: published.Training.ID})
	if visible.Training.CanEdit {
		t.Fatal("invitation granted editing")
	}
	trainingCall[models.FormationJoinResult](t, s, viewer, "join_training", models.TrainingRequest{ID: published.Training.ID, Key: "join"})
	originalVersion := published.Training.VersionID
	published.Concepts[0].Description = "Version suivante"
	next := trainingCall[models.TrainingDocument](t, s, author, "save_training_edit", models.TrainingRequest{ID: published.Training.ID, ExpectedRevision: published.Training.Revision, Document: &published, Key: "v2"})
	if next.Training.VersionID == originalVersion || next.Training.CanPublish {
		t.Fatal("subsequent version bypassed review")
	}
	still := trainingCall[models.TrainingDocument](t, s, viewer, "get_training", models.TrainingRequest{ID: published.Training.ID})
	if still.Training.VersionID != originalVersion || still.Concepts[0].Description == "Version suivante" {
		t.Fatal("learner silently migrated")
	}
	free := true
	next = trainingCall[models.TrainingDocument](t, s, owner, "set_training_access", models.TrainingRequest{ID: next.Training.ID, ExpectedRevision: next.Training.Revision, FreePublication: &free, Key: "free"})
	next = trainingCall[models.TrainingDocument](t, s, author, "publish_training", models.TrainingRequest{ID: next.Training.ID, ExpectedRevision: next.Training.Revision, Key: "publish-free"})
	if next.Training.Status != "published" {
		t.Fatal(next.Training.Status)
	}
	original := trainingCall[models.TrainingDocument](t, s, author, "get_training", models.TrainingRequest{ID: "personal:" + d.ID})
	if original.Training.Revision != "1" {
		t.Fatal("sharing modified private original")
	}
	different := nativeTestTenant(t, s, "unrelated-training")
	foreign := trainingPrincipal(t, s, different, 107, true)
	if _, err = s.TrainingAction(t.Context(), foreign, "get_training", models.TrainingRequest{ID: next.Training.ID}); err == nil {
		t.Fatal("cross-institution read")
	}
}

func TestTrainingContributionGrantCannotReadPrivateLearningOrPublishWithoutConsent(t *testing.T) {
	s := setupTestDB(t)
	tenant := nativeTestTenant(t, s, "training-grants")
	owner := trainingPrincipal(t, s, tenant, 111, true)
	d := trainingDomain(t, s, owner, "Private scope test")
	personal := models.TrainingRequest{ID: "personal:" + d.ID, ExpectedRevision: "1", Name: "Shared design", Key: "no-consent"}
	legacy := owner
	legacy.Scopes = []string{models.OAuthScopeLearner}
	if _, err := s.TrainingAction(t.Context(), legacy, "share_training", personal); !errors.Is(err, storeport.ErrInvalidPrincipal) {
		t.Fatalf("legacy learner grant acquired contribution: %v", err)
	}
	onlyContribution := owner
	onlyContribution.Scopes = []string{models.OAuthScopeFormationContribute}
	if _, err := s.TrainingAction(t.Context(), onlyContribution, "get_training", models.TrainingRequest{ID: personal.ID}); err == nil {
		t.Fatal("contribution scope read private learning")
	}
	library := trainingCall[models.TrainingPage](t, s, onlyContribution, "list_training_library", models.TrainingRequest{})
	for _, item := range library.Items {
		if item.Kind == "personal" {
			t.Fatal("private domain included in contribution-only grant")
		}
	}
}

func TestTrainingLegacyCatalogueCannotExposePrivateDesignOrBypassReview(t *testing.T) {
	s := setupTestDB(t)
	tenant := nativeTestTenant(t, s, "training-old-api")
	owner := trainingPrincipal(t, s, tenant, 121, true)
	learner := trainingPrincipal(t, s, tenant, 123, false)
	d := trainingDomain(t, s, learner, "Programme strictement privé")
	if _, err := s.GetFormationVersion(t.Context(), owner, "domain_version_"+d.ID); !errors.Is(err, storeport.ErrInvalidPrincipal) {
		t.Fatalf("private compatibility programme accessible to owner: %v", err)
	}
	if _, err := s.GetCurriculumReviewMaterial(t.Context(), owner, d.ID, 1); err == nil {
		t.Fatal("private curriculum accessible through institutional review")
	}
	page, err := s.ListFormations(t.Context(), owner, "", 100)
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("private compatibility catalogue leaked: %#v %v", page, err)
	}
	n := nativeTestMember(t, s, tenant, 125, []string{models.RolePedagogyManager})
	manager, err := s.CreateNativeSession(t.Context(), nativeTestSession(n, "old-authoring", []string{models.OAuthScopeFormationContribute, models.OAuthScopeFormationRead, models.OAuthScopeFormationWrite}))
	if err != nil {
		t.Fatal(err)
	}
	f, v, err := s.CreateFormationDraft(t.Context(), manager, "Contribution historique", "")
	if err != nil {
		t.Fatal(err)
	}
	adminIdentity := nativeTestMember(t, s, tenant, 127, []string{models.RoleAdmin})
	admin, err := s.CreateNativeSession(t.Context(), nativeTestSession(adminIdentity, "unassigned-admin", []string{models.OAuthScopeFormationContribute, models.OAuthScopeFormationRead, models.OAuthScopeFormationWrite}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.GetFormationVersion(t.Context(), admin, v.ID); !errors.Is(err, storeport.ErrInvalidPrincipal) {
		t.Fatalf("unassigned admin read nominative design: %v", err)
	}
	if _, err = s.AddFormationModule(t.Context(), admin, v.ID, models.FormationModuleInput{StableKey: "unauthorized", Title: "Forbidden"}); !errors.Is(err, storeport.ErrInvalidPrincipal) {
		t.Fatalf("unassigned admin edited design: %v", err)
	}
	adminPage, err := s.ListFormations(t.Context(), admin, "", 100)
	if err != nil || len(adminPage.Items) != 0 {
		t.Fatalf("unassigned admin catalogue leaked: %#v %v", adminPage, err)
	}
	if _, err = s.AddFormationModule(t.Context(), manager, v.ID, models.FormationModuleInput{StableKey: "main", Title: "Module"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AddFormationConcept(t.Context(), manager, v.ID, models.FormationConceptInput{ModuleStableKey: "main", StableKey: "kc", Label: "Concept"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PublishFormationVersion(t.Context(), manager, v.ID); !errors.Is(err, storeport.ErrInvalidPrincipal) {
		t.Fatalf("legacy API bypassed review: %v", err)
	}
	doc := trainingCall[models.TrainingDocument](t, s, manager, "get_training", models.TrainingRequest{ID: "formation:" + f.ID})
	if doc.Training.Kind != "shared" || doc.Training.CanPublish {
		t.Fatal("legacy authoring did not create a contribution")
	}
	if _, err = s.TrainingAction(t.Context(), manager, "set_training_access", models.TrainingRequest{ID: doc.Training.ID, ExpectedRevision: doc.Training.Revision, MemberID: learner.MembershipID, Decision: "edit", Key: "invalid-editor"}); err == nil {
		t.Fatal("ordinary learner granted trainer editing")
	}
	trainingCall[models.TrainingDocument](t, s, manager, "submit_training", models.TrainingRequest{ID: doc.Training.ID, ExpectedRevision: doc.Training.Revision, Key: "old-submit"})
	published := trainingCall[models.TrainingDocument](t, s, owner, "decide_training", models.TrainingRequest{ID: doc.Training.ID, ExpectedRevision: doc.Training.Revision, Decision: "approve", Reason: "Programme vérifié", Key: "old-approve"})
	if published.Training.Status != "published" || published.Training.Audience != "invitation" {
		t.Fatal("approval did not preserve nominative publication")
	}
}
