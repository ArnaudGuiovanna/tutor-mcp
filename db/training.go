// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func trainingOwner(p models.Principal) bool { return slices.Contains(p.Roles, models.RoleOwner) }

// Contribution is a resource capability, independent of administration. No
// synthetic role or principal is used to run the authoring implementation.
func (s *Store) trainingEditable(ctx context.Context, p models.Principal, f models.Formation) (bool, error) {
	grant := strings.Join(p.Scopes, " ")
	if !models.OAuthScopeAllows(grant, models.OAuthScopeFormationContribute) && !models.OAuthScopeAllows(grant, models.OAuthScopeFormationRead) && !models.OAuthScopeAllows(grant, models.OAuthScopeFormationWrite) {
		return false, nil
	}
	if trainingOwner(p) || f.CreatedBy == p.UserID {
		return true, nil
	}
	var count int
	err := s.queryRow(ctx, `SELECT COUNT(*) FROM formation_trainers WHERE tenant_id=? AND formation_id=? AND membership_id=?`, p.TenantID, f.ID, p.MembershipID).Scan(&count)
	return count > 0, err
}

func (s *Store) TrainingAction(ctx context.Context, p models.Principal, action string, in models.TrainingRequest) (any, error) {
	if !models.ValidTrainingAction(action) || len(in.Query) > 200 || len(in.ID) > 255 || len(in.After) > 255 || len(in.ConversationID) > 128 || len(in.Reason) > 4000 {
		return nil, fmt.Errorf("invalid training request")
	}
	commonWrite := slices.Contains([]string{"share_training", "submit_training", "decide_training", "publish_training", "set_training_access", "set_training_policy"}, action) || (strings.HasPrefix(in.ID, "formation:") && slices.Contains([]string{"save_training_edit", "propose_training_edit"}, action))
	if commonWrite && !models.OAuthScopeAllows(strings.Join(p.Scopes, " "), models.OAuthScopeFormationContribute) {
		return nil, storeport.ErrInvalidPrincipal
	}
	read := models.TrainingReadAction(action)
	granted := strings.Join(p.Scopes, " ")
	if action == "get_training_groups" {
		if !models.OAuthScopeAllows(granted, models.OAuthScopeProgressRead) || !p.CanReadInstitutionProgress() {
			return nil, storeport.ErrInvalidPrincipal
		}
	} else if read {
		if !models.OAuthScopeAllows(granted, models.OAuthScopeLearnerRead) && !models.OAuthScopeAllows(granted, models.OAuthScopeFormationRead) && !models.OAuthScopeAllows(granted, models.OAuthScopeFormationContribute) {
			return nil, storeport.ErrInvalidPrincipal
		}
	} else if !models.OAuthScopeAllows(granted, models.OAuthScopeLearnerWrite) && !models.OAuthScopeAllows(granted, models.OAuthScopeFormationWrite) && !models.OAuthScopeAllows(granted, models.OAuthScopeFormationContribute) {
		return nil, storeport.ErrInvalidPrincipal
	}
	var out any
	// Reading a legacy personal document may establish its immutable baseline.
	// Use a normal tenant transaction for this bounded compatibility operation.
	transaction := s.WithTenantTx
	if action == "get_training_groups" {
		transaction = s.WithTenantReadTx
	}
	err := transaction(ctx, p.TenantScope(), func(txCtx context.Context, scoped storeport.Store) error {
		tx := scoped.(*Store)
		if err := tx.ValidatePrincipal(txCtx, p); err != nil {
			return err
		}
		var requestHash string
		if !read {
			if in.Key == "" || len(in.Key) > 128 {
				return fmt.Errorf("key is required (up to 128 bytes)")
			}
			var err error
			requestHash, err = catalogRequestHash(struct {
				Action string
				Input  models.TrainingRequest
			}{action, in})
			if err != nil {
				return err
			}
			if in.ID != "" {
				if _, err = tx.trainingDocument(txCtx, p, in.ID); err != nil {
					return err
				}
			}
			if (action == "decide_training" || action == "set_training_policy" || in.FreePublication != nil) && !trainingOwner(p) {
				return storeport.ErrInvalidPrincipal
			}
			var operation, hash, author, raw string
			err = tx.queryRow(txCtx, `SELECT operation,request_hash,actor_user_id,response_json FROM training_receipts WHERE tenant_id=? AND idempotency_key=?`, p.TenantID, "training:"+in.Key).Scan(&operation, &hash, &author, &raw)
			if err == nil {
				if operation != action || hash != requestHash || author != p.UserID {
					return storeport.ErrIdempotencyKeyConflict
				}
				return json.Unmarshal([]byte(raw), &out)
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		var err error
		switch action {
		case "list_training_library":
			out, err = tx.trainingLibrary(txCtx, p, in)
		case "get_training_groups":
			out, err = tx.trainingGroups(txCtx, p, in)
		case "get_training":
			out, err = tx.trainingDocument(txCtx, p, in.ID)
		case "get_conversation_context":
			out, err = tx.conversationContext(txCtx, p, in.ConversationID)
		case "select_conversation_training":
			out, err = tx.selectConversationTraining(txCtx, p, in)
		case "get_training_policy", "set_training_policy":
			out, err = tx.trainingPolicy(txCtx, p, in, action == "set_training_policy")
		case "find_training_members":
			out, err = tx.trainingMembers(txCtx, p, in)
		case "preview_training_edit", "save_training_edit", "propose_training_edit":
			out, err = tx.trainingEdit(txCtx, p, action, in)
		case "share_training":
			out, err = tx.shareTraining(txCtx, p, in)
		case "submit_training", "decide_training", "publish_training":
			out, err = tx.trainingPublication(txCtx, p, action, in)
		case "set_training_access":
			out, err = tx.trainingAccess(txCtx, p, in)
		case "join_training":
			var doc *models.TrainingDocument
			doc, err = tx.trainingDocument(txCtx, p, in.ID)
			if err == nil {
				if doc.Training.CohortID == "" {
					return storeport.ErrFormationUnavailable
				}
				out, _, err = s.JoinFormation(txCtx, p, in.Key, doc.Training.CohortID)
			}
		}
		if err == nil && !read {
			raw, encodeErr := json.Marshal(out)
			if encodeErr != nil {
				return encodeErr
			}
			_, err = tx.exec(txCtx, `INSERT INTO training_receipts(tenant_id,membership_id,learner_id,idempotency_key,operation,request_hash,response_json,actor_user_id,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, p.TenantID, p.MembershipID, nullableTrainingLearner(p.LearnerID), "training:"+in.Key, action, requestHash, string(raw), p.UserID, time.Now().UTC())
		}
		return err
	})
	return out, err
}

// The SQL projection deduplicates by formation before pagination and searches
// only authorized rows. Personal domains never enter the institutional branch.
func (s *Store) trainingLibrary(ctx context.Context, p models.Principal, in models.TrainingRequest) (models.TrainingPage, error) {
	page := models.TrainingPage{Items: []models.Training{}}
	if in.Limit == 0 {
		in.Limit = 20
	}
	if validatePage(in.Limit) != nil {
		return page, fmt.Errorf("invalid page")
	}
	if !slices.Contains([]string{"", "personal", "institutional", "shared", "mine", "review"}, in.Filter) {
		return page, fmt.Errorf("invalid filter")
	}
	search := "%" + strings.ToLower(in.Query) + "%"
	designGrant := strings.Join(p.Scopes, " ")
	personalLearner := p.LearnerID
	if !models.OAuthScopeAllows(designGrant, models.OAuthScopeLearnerRead) {
		personalLearner = ""
	}
	canDesign := models.OAuthScopeAllows(designGrant, models.OAuthScopeFormationContribute) || models.OAuthScopeAllows(designGrant, models.OAuthScopeFormationRead) || models.OAuthScopeAllows(designGrant, models.OAuthScopeFormationWrite)
	authorID, editorID := p.UserID, p.MembershipID
	if !canDesign {
		authorID = ""
		editorID = ""
	}
	trainerID := ""
	if models.OAuthScopeAllows(designGrant, models.OAuthScopeProgressRead) && p.CanReadInstitutionProgress() {
		trainerID = p.MembershipID
	}
	query := `SELECT id FROM (
 SELECT 'personal:'||d.id AS id, d.name AS name, 'personal' AS kind, 1 AS mine, 0 AS pending
 FROM domains d WHERE d.tenant_id=? AND d.learner_id=? AND d.deleted_at IS NULL AND d.archived=0 AND COALESCE(d.formation_enrollment_id,'')=''
 UNION ALL
 SELECT 'formation:'||f.id AS id, f.name AS name, COALESCE(w.kind,'institutional') AS kind,
 CASE WHEN f.created_by=? OR EXISTS(SELECT 1 FROM enrollments e JOIN formation_versions ev ON ev.tenant_id=e.tenant_id AND ev.id=e.formation_version_id WHERE e.tenant_id=f.tenant_id AND ev.formation_id=f.id AND e.membership_id=?) THEN 1 ELSE 0 END AS mine,
 CASE WHEN EXISTS(SELECT 1 FROM training_submissions ts WHERE ts.tenant_id=f.tenant_id AND ts.formation_id=f.id AND ts.status='pending') THEN 1 ELSE 0 END AS pending
 FROM formations f LEFT JOIN training_workflows w ON w.tenant_id=f.tenant_id AND w.formation_id=f.id
 WHERE f.tenant_id=? AND ` + institutionalFormationSQL + ` AND f.status<>'archived' AND
 (?=1 OR f.created_by=? OR EXISTS(SELECT 1 FROM formation_trainers ft WHERE ft.tenant_id=f.tenant_id AND ft.formation_id=f.id AND ft.membership_id=?) OR
 EXISTS(SELECT 1 FROM cohort_trainers ct JOIN cohorts tc ON tc.tenant_id=ct.tenant_id AND tc.id=ct.cohort_id JOIN formation_versions tv ON tv.tenant_id=tc.tenant_id AND tv.id=tc.formation_version_id WHERE ct.tenant_id=f.tenant_id AND ct.membership_id=? AND tv.formation_id=f.id) OR
 EXISTS(SELECT 1 FROM formation_versions v JOIN cohorts c ON c.tenant_id=v.tenant_id AND c.formation_version_id=v.id
 WHERE v.tenant_id=f.tenant_id AND v.formation_id=f.id AND v.status='published' AND (
 (f.status='active' AND c.status='open' AND f.enrollment_policy<>'invitation' AND (c.ends_at IS NULL OR c.ends_at>?)) OR
 EXISTS(SELECT 1 FROM formation_admissions a WHERE a.tenant_id=c.tenant_id AND a.cohort_id=c.id AND a.membership_id=? AND a.status IN ('invited','approved')) OR
 EXISTS(SELECT 1 FROM enrollments e WHERE e.tenant_id=c.tenant_id AND e.cohort_id=c.id AND e.membership_id=?))))
 ) library WHERE id>? AND lower(name) LIKE ? AND (?='' OR kind=? OR (?='mine' AND mine=1) OR (?='review' AND pending=1 AND ?=1)) ORDER BY id LIMIT ?`
	rows, err := s.query(ctx, query, p.TenantID, personalLearner, p.UserID, p.MembershipID, p.TenantID, boolInt(trainingOwner(p) && canDesign), authorID, editorID, trainerID, time.Now().UTC(), p.MembershipID, p.MembershipID, in.After, search, in.Filter, in.Filter, in.Filter, in.Filter, boolInt(trainingOwner(p)), in.Limit+1)
	if err != nil {
		return page, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return page, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return page, err
	}
	if len(ids) > in.Limit {
		ids = ids[:in.Limit]
		page.NextAfter = ids[len(ids)-1]
	}
	for _, id := range ids {
		doc, err := s.trainingDocument(ctx, p, id)
		if err != nil {
			return page, err
		}
		page.Items = append(page.Items, doc.Training)
	}
	return page, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *Store) trainingDocument(ctx context.Context, p models.Principal, id string) (*models.TrainingDocument, error) {
	doc := &models.TrainingDocument{Modules: []models.FormationModuleInput{}, Concepts: []models.FormationConceptInput{}}
	t := &doc.Training
	t.ID = id
	t.Progress = []models.TrainingProgress{}
	if domainID, ok := strings.CutPrefix(id, "personal:"); ok {
		if p.LearnerID == "" || !models.OAuthScopeAllows(strings.Join(p.Scopes, " "), models.OAuthScopeLearnerRead) {
			return nil, storeport.ErrInvalidPrincipal
		}
		domain, err := s.GetLearnerDomainByID(ctx, p.LearnerID, domainID)
		if err != nil || domain.Archived || domain.FormationEnrollmentID != "" {
			return nil, storeport.ErrFormationUnavailable
		}
		doc.Curriculum, err = s.EnsureCurriculumBaseline(ctx, p.LearnerID, domainID)
		if err != nil {
			return nil, err
		}
		*t = models.Training{ID: id, Name: domain.Name, Description: domain.PersonalGoal, Kind: "personal", DomainID: domainID, Revision: strconv.Itoa(domain.GraphVersion), Status: "active", Audience: "private", AuthorID: p.UserID, CanEdit: true, CanLearn: true, CanShare: p.TenantID != models.LegacyTenantID, Progress: []models.TrainingProgress{}}
	} else if formationID, ok := strings.CutPrefix(id, "formation:"); ok {
		var versionID string
		// Recheck visibility for direct reads as strictly as library searches.
		var f models.Formation
		f.ID = formationID
		f.TenantID = p.TenantID
		err := s.queryRow(ctx, `SELECT name,description,status,created_by,owner_membership_id,enrollment_policy FROM formations WHERE tenant_id=? AND id=? AND substr(id,1,7)<>'legacy_' AND substr(id,1,17)<>'domain_formation_'`, p.TenantID, formationID).Scan(&f.Name, &f.Description, &f.Status, &f.CreatedBy, &f.OwnerMembershipID, &f.EnrollmentPolicy)
		if err != nil {
			return nil, storeport.ErrFormationUnavailable
		}
		designAccess, err := s.trainingEditable(ctx, p, f)
		grant := strings.Join(p.Scopes, " ")
		editable := designAccess && (models.OAuthScopeAllows(grant, models.OAuthScopeFormationContribute) || models.OAuthScopeAllows(grant, models.OAuthScopeFormationWrite))
		if err != nil {
			return nil, err
		}
		var assigned int
		if p.CanReadInstitutionProgress() && models.OAuthScopeAllows(strings.Join(p.Scopes, " "), models.OAuthScopeProgressRead) {
			if err = s.queryRow(ctx, `SELECT COUNT(*) FROM cohort_trainers ct JOIN cohorts c ON c.tenant_id=ct.tenant_id AND c.id=ct.cohort_id JOIN formation_versions v ON v.tenant_id=c.tenant_id AND v.id=c.formation_version_id WHERE ct.tenant_id=? AND ct.membership_id=? AND v.formation_id=?`, p.TenantID, p.MembershipID, formationID).Scan(&assigned); err != nil {
				return nil, err
			}
		}
		learnerDataID := p.LearnerID
		if !models.OAuthScopeAllows(grant, models.OAuthScopeLearnerRead) {
			learnerDataID = ""
		}
		var cohortID, domainID string
		// A learner keeps the enrolled version even after subsequent publication.
		err = s.queryRow(ctx, `SELECT c.id,v.id,COALESCE(d.id,'') FROM cohorts c JOIN formation_versions v ON v.tenant_id=c.tenant_id AND v.id=c.formation_version_id
 LEFT JOIN enrollments e ON e.tenant_id=c.tenant_id AND e.cohort_id=c.id AND e.membership_id=?
 LEFT JOIN domains d ON d.tenant_id=e.tenant_id AND d.formation_enrollment_id=e.id AND d.learner_id=? AND d.deleted_at IS NULL
 WHERE c.tenant_id=? AND v.formation_id=? AND v.status='published' AND (
 (?=1 AND c.status='open' AND (c.ends_at IS NULL OR c.ends_at>CURRENT_TIMESTAMP)) OR EXISTS(SELECT 1 FROM formation_admissions a WHERE a.tenant_id=c.tenant_id AND a.cohort_id=c.id AND a.membership_id=? AND a.status IN ('invited','approved')) OR e.status IN ('active','completed') OR ?=1)
 ORDER BY CASE WHEN ?=1 OR e.status IN ('active','completed') THEN 0 ELSE 1 END,v.version DESC,c.id LIMIT 1`, p.MembershipID, learnerDataID, p.TenantID, formationID, boolInt(f.EnrollmentPolicy != "invitation" && f.Status == "active"), p.MembershipID, boolInt(designAccess || assigned > 0), boolInt(designAccess)).Scan(&cohortID, &versionID, &domainID)
		// The policy predicate is bound separately below (no client SQL enters here).
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if !designAccess && versionID == "" {
			return nil, storeport.ErrFormationUnavailable
		}
		if designAccess {
			if err = s.queryRow(ctx, `SELECT id FROM formation_versions WHERE tenant_id=? AND formation_id=? ORDER BY version DESC LIMIT 1`, p.TenantID, formationID).Scan(&versionID); err != nil {
				return nil, err
			}
		}
		detail, err := s.loadFormationVersion(ctx, p.TenantID, versionID)
		if err != nil {
			return nil, err
		}
		t.Name = f.Name
		t.Description = f.Description
		t.FormationID = formationID
		t.VersionID = versionID
		t.CohortID = cohortID
		t.DomainID = domainID
		t.Kind = "institutional"
		t.AuthorID = f.CreatedBy
		t.Audience = f.EnrollmentPolicy
		t.Status = detail.Version.Status
		t.CanEdit = editable
		t.CanReview = trainingOwner(p) && models.OAuthScopeAllows(grant, models.OAuthScopeFormationContribute)
		var free int
		err = s.queryRow(ctx, `SELECT kind,author_id,free_publication FROM training_workflows WHERE tenant_id=? AND formation_id=?`, p.TenantID, formationID).Scan(&t.Kind, &t.AuthorID, &free)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		t.FreePublication = free == 1
		t.CanPublish = models.OAuthScopeAllows(grant, models.OAuthScopeFormationContribute) && (trainingOwner(p) || (editable && t.FreePublication))
		t.CanLearn = p.LearnerID != "" && (domainID != "" || (cohortID != "" && f.EnrollmentPolicy != "invitation"))
		if !t.CanLearn && p.LearnerID != "" && cohortID != "" {
			var admitted int
			if err = s.queryRow(ctx, `SELECT COUNT(*) FROM formation_admissions WHERE tenant_id=? AND cohort_id=? AND membership_id=? AND status IN ('invited','approved')`, p.TenantID, cohortID, p.MembershipID).Scan(&admitted); err != nil {
				return nil, err
			}
			t.CanLearn = admitted > 0
		}
		for _, m := range detail.Modules {
			doc.Modules = append(doc.Modules, m.FormationModuleInput)
		}
		for _, c := range detail.Concepts {
			doc.Concepts = append(doc.Concepts, c.FormationConceptInput)
		}
		t.Revision, err = catalogRequestHash(struct {
			Version  models.FormationVersion
			Modules  []models.FormationModuleInput
			Concepts []models.FormationConceptInput
		}{detail.Version, doc.Modules, doc.Concepts})
		if err != nil {
			return nil, err
		}
		sub := &models.TrainingSubmission{}
		var submittedJSON string
		err = s.queryRow(ctx, `SELECT id,version_id,revision,status,reason,author_id,document_json FROM training_submissions WHERE tenant_id=? AND formation_id=? ORDER BY created_at DESC,id DESC LIMIT 1`, p.TenantID, formationID).Scan(&sub.ID, &sub.VersionID, &sub.Revision, &sub.Status, &sub.Reason, &sub.AuthorID, &submittedJSON)
		if err == nil && (editable || trainingOwner(p)) {
			if err = json.Unmarshal([]byte(submittedJSON), &sub.Document); err != nil {
				return nil, err
			}
			doc.Submission = sub
			if sub.Status == "pending" {
				t.Status = "pending"
			}
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		calibrationDetail := detail
		if domainID != "" {
			learning, readErr := s.GetLearnerDomainByID(ctx, p.LearnerID, domainID)
			if readErr != nil {
				return nil, readErr
			}
			t.LearningVersionID = learning.FormationVersionID
			if t.LearningVersionID != "" && t.LearningVersionID != detail.Version.ID {
				calibrationDetail, readErr = s.loadFormationVersion(ctx, p.TenantID, t.LearningVersionID)
				if readErr != nil {
					return nil, readErr
				}
			}
		}
		calibration := map[string]any{"minimum_contributors": models.CollectiveWeightMinContributors, "minimum_observations": models.CollectiveWeightMinObservations, "source": "initial_reference", "available_concepts": []string{}}
		keys := []string{}
		for _, c := range calibrationDetail.Concepts {
			w, _, err := s.currentCollectiveWeight(ctx, p.TenantID, c.ID)
			if err != nil {
				return nil, err
			}
			if w != nil {
				keys = append(keys, c.StableKey)
			}
		}
		if len(keys) > 0 {
			calibration["source"] = "collective"
			calibration["available_concepts"] = keys
		}
		doc.Calibration = calibration
	} else {
		return nil, storeport.ErrFormationUnavailable
	}
	if t.DomainID != "" && p.LearnerID != "" {
		states, err := s.GetConceptStatesByDomain(ctx, p.LearnerID, t.DomainID)
		if err != nil {
			return nil, err
		}
		for _, state := range states {
			t.Progress = append(t.Progress, models.TrainingProgress{Concept: state.Concept, Mastery: state.PMastery, LastReview: state.LastReview, NextReview: state.NextReview})
		}
	}
	if t.CanEdit {
		proposal := &models.TrainingProposal{}
		var raw string
		err := s.queryRow(ctx, `SELECT id,expected_revision,document_json FROM training_proposals WHERE tenant_id=? AND membership_id=? AND training_id=?`, p.TenantID, p.MembershipID, id).Scan(&proposal.ID, &proposal.ExpectedRevision, &raw)
		if err == nil {
			if err = json.Unmarshal([]byte(raw), &proposal.Document); err != nil {
				return nil, err
			}
			doc.Proposal = proposal
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	return doc, nil
}

func (s *Store) conversationContext(ctx context.Context, p models.Principal, id string) (*models.ConversationContext, error) {
	if id == "" {
		return nil, fmt.Errorf("conversation_id is required")
	}
	c := &models.ConversationContext{ConversationID: id}
	err := s.queryRow(ctx, `SELECT training_id,domain_id,revision,selection_version FROM conversation_contexts WHERE tenant_id=? AND membership_id=? AND conversation_id=?`, p.TenantID, p.MembershipID, id).Scan(&c.TrainingID, &c.DomainID, &c.Revision, &c.SelectionVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	doc, err := s.trainingDocument(ctx, p, c.TrainingID)
	if err != nil {
		return nil, err
	}
	c.Name = doc.Training.Name
	c.DomainID = doc.Training.DomainID
	c.Revision = doc.Training.Revision
	if c.DomainID != "" {
		domain, err := s.GetLearnerDomainByID(ctx, p.LearnerID, c.DomainID)
		if err != nil {
			return nil, err
		}
		c.Revision = strconv.Itoa(domain.GraphVersion)
	}
	return c, nil
}

func (s *Store) selectConversationTraining(ctx context.Context, p models.Principal, in models.TrainingRequest) (*models.ConversationContext, error) {
	if in.ConversationID == "" || in.ExpectedSelection == nil {
		return nil, fmt.Errorf("conversation_id and expected_selection are required")
	}
	doc, err := s.trainingDocument(ctx, p, in.ID)
	if err != nil {
		return nil, err
	}
	old, err := s.conversationContext(ctx, p, in.ConversationID)
	if err != nil {
		return nil, err
	}
	if old.SelectionVersion != *in.ExpectedSelection {
		return nil, storeport.ErrCurriculumVersionConflict
	}
	next := old.SelectionVersion + 1
	if next == 1 {
		_, err = s.exec(ctx, `INSERT INTO conversation_contexts(tenant_id,membership_id,learner_id,conversation_id,training_id,domain_id,revision,selection_version) VALUES(?,?,?,?,?,?,?,1)`, p.TenantID, p.MembershipID, nullableTrainingLearner(p.LearnerID), in.ConversationID, in.ID, doc.Training.DomainID, doc.Training.Revision)
	} else {
		var res sql.Result
		res, err = s.exec(ctx, `UPDATE conversation_contexts SET training_id=?,domain_id=?,revision=?,selection_version=? WHERE tenant_id=? AND membership_id=? AND conversation_id=? AND selection_version=?`, in.ID, doc.Training.DomainID, doc.Training.Revision, next, p.TenantID, p.MembershipID, in.ConversationID, *in.ExpectedSelection)
		if err == nil {
			if n, _ := res.RowsAffected(); n != 1 {
				return nil, storeport.ErrCurriculumVersionConflict
			}
		}
	}
	if err != nil {
		return nil, err
	}
	return s.conversationContext(ctx, p, in.ConversationID)
}
func nullableTrainingLearner(id string) any {
	if id == "" {
		return nil
	}
	return id
}

func (s *Store) trainingPolicy(ctx context.Context, p models.Principal, in models.TrainingRequest, write bool) (any, error) {
	if write {
		if !trainingOwner(p) {
			return nil, storeport.ErrInvalidPrincipal
		}
		if in.PersonalLearning == nil {
			return nil, fmt.Errorf("personal_learning is required")
		}
		if err := s.SetInstitutionLearningPolicy(ctx, p, *in.PersonalLearning); err != nil {
			return nil, err
		}
	}
	var raw string
	if err := s.queryRow(ctx, `SELECT policy_json FROM tenants WHERE id=?`, p.TenantID).Scan(&raw); err != nil {
		return nil, err
	}
	var policy struct {
		AllowFreeDomains *bool `json:"allow_free_domains"`
	}
	if err := json.Unmarshal([]byte(raw), &policy); err != nil {
		return nil, err
	}
	allowed := p.TenantID == models.LegacyTenantID
	if policy.AllowFreeDomains != nil {
		allowed = *policy.AllowFreeDomains
	}
	return map[string]any{"personal_learning": allowed, "can_manage": trainingOwner(p) && models.OAuthScopeAllows(strings.Join(p.Scopes, " "), models.OAuthScopeFormationContribute)}, nil
}

func validateTrainingEdit(current *models.TrainingDocument, in models.TrainingRequest, p models.Principal) (*models.TrainingDocument, []string, error) {
	if !current.Training.CanEdit {
		return nil, nil, storeport.ErrInvalidPrincipal
	}
	if in.ExpectedRevision == "" || in.ExpectedRevision != current.Training.Revision {
		return nil, nil, storeport.ErrCurriculumVersionConflict
	}
	if in.Document == nil {
		return nil, nil, fmt.Errorf("document is required")
	}
	raw, err := json.Marshal(in.Document)
	if err != nil || len(raw) > 1024*1024 {
		return nil, nil, fmt.Errorf("document exceeds limits")
	}
	candidate := &models.TrainingDocument{}
	if err = json.Unmarshal(raw, candidate); err != nil {
		return nil, nil, err
	}
	candidate.Training = current.Training
	candidate.Proposal = nil
	candidate.Submission = nil
	candidate.Calibration = nil
	invalidated := []string{}
	if current.Curriculum != nil {
		if candidate.Curriculum == nil {
			return nil, nil, fmt.Errorf("curriculum is required")
		}
		next := candidate.Curriculum
		next.DomainID = current.Curriculum.DomainID
		next.Version = current.Curriculum.Version + 1
		next.ParentVersion = current.Curriculum.Version
		next.CreatedBy = p.UserID
		next.CreatedAt = time.Now().UTC()
		next.Reconciliation = nil
		next.Operation = models.CurriculumOperation{Type: models.CurriculumOperationUpdateMetadata, Rationale: "reviewed programme edit"}
		next.Provenance = models.CurriculumProvenance{SourceType: "author", Author: p.UserID, Rationale: "reviewed programme edit"}
		next.Review = models.CurriculumReview{Status: models.CurriculumReviewUnreviewed}
		if len(next.Concepts) > 500 || len(next.Graph.Concepts) == 0 {
			return nil, nil, fmt.Errorf("curriculum requires 1..500 concepts")
		}
		if err = validateCurriculumSnapshot(next); err != nil {
			return nil, nil, err
		}
		byID := map[string]models.CurriculumConcept{}
		for _, c := range next.Concepts {
			byID[c.ID] = c
			if len(c.Label) > 500 || len(c.Description) > 16000 {
				return nil, nil, fmt.Errorf("concept exceeds limits")
			}
		}
		for _, c := range current.Curriculum.Concepts {
			replacement, ok := byID[c.ID]
			if !ok || replacement.Key != c.Key {
				return nil, nil, fmt.Errorf("retain stable identities; retire concepts instead of removing them")
			}
			if c.Status != replacement.Status || !models.SameCurriculumDefinition(c, replacement) {
				invalidated = append(invalidated, c.ID)
			}
		}
	} else {
		candidate.Curriculum = nil
		if len(candidate.Modules) < 1 || len(candidate.Modules) > 100 || len(candidate.Concepts) < 1 || len(candidate.Concepts) > 500 {
			return nil, nil, fmt.Errorf("programme requires modules and concepts")
		}
		modules := map[string]bool{}
		keys := map[string]bool{}
		for _, m := range candidate.Modules {
			if m.StableKey == "" || len(m.StableKey) > 200 || len(m.Title) > 500 || strings.TrimSpace(m.Title) == "" || modules[m.StableKey] || m.Position < 0 {
				return nil, nil, fmt.Errorf("invalid module")
			}
			modules[m.StableKey] = true
		}
		detail := &models.FormationVersionDetail{}
		for i := range candidate.Concepts {
			c := &candidate.Concepts[i]
			if keys[c.StableKey] || !modules[c.ModuleStableKey] || c.Position < 0 || strings.TrimSpace(c.Label) == "" {
				return nil, nil, fmt.Errorf("invalid concept or module")
			}
			keys[c.StableKey] = true
			if err = normalizeFormationConcept(c); err != nil {
				return nil, nil, err
			}
			detail.Concepts = append(detail.Concepts, models.FormationConcept{ID: c.StableKey, FormationConceptInput: *c})
		}
		if _, err = formationCurriculum(detail, "validation", p.UserID, time.Now().UTC()); err != nil {
			return nil, nil, err
		}
	}
	return candidate, invalidated, nil
}

func (s *Store) lockTraining(ctx context.Context, p models.Principal, id string) error {
	if s.dialect != DialectPostgres {
		return nil
	}
	if domain, ok := strings.CutPrefix(id, "personal:"); ok {
		var locked string
		return s.queryRow(ctx, `SELECT id FROM domains WHERE tenant_id=? AND id=? AND learner_id=? FOR UPDATE`, p.TenantID, domain, p.LearnerID).Scan(&locked)
	}
	formation, ok := strings.CutPrefix(id, "formation:")
	if !ok {
		return storeport.ErrFormationUnavailable
	}
	var locked string
	return s.queryRow(ctx, `SELECT id FROM formations WHERE tenant_id=? AND id=? FOR UPDATE`, p.TenantID, formation).Scan(&locked)
}

func (s *Store) trainingEdit(ctx context.Context, p models.Principal, action string, in models.TrainingRequest) (any, error) {
	if err := s.lockTraining(ctx, p, in.ID); err != nil {
		return nil, err
	}
	current, err := s.trainingDocument(ctx, p, in.ID)
	if err != nil {
		return nil, err
	}
	candidate, invalidated, err := validateTrainingEdit(current, in, p)
	if err != nil {
		return nil, err
	}
	if action == "preview_training_edit" {
		return map[string]any{"document": candidate, "invalidated_concept_ids": invalidated, "existing_learners_keep_version": current.Curriculum == nil}, nil
	}
	if action == "propose_training_edit" {
		id, err := generateID()
		if err != nil {
			return nil, err
		}
		raw, err := json.Marshal(candidate)
		if err != nil {
			return nil, err
		}
		_, err = s.exec(ctx, `INSERT INTO training_proposals(tenant_id,membership_id,learner_id,training_id,id,expected_revision,document_json,created_at) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(tenant_id,membership_id,training_id) DO UPDATE SET id=excluded.id,expected_revision=excluded.expected_revision,document_json=excluded.document_json,created_at=excluded.created_at`, p.TenantID, p.MembershipID, nullableTrainingLearner(p.LearnerID), in.ID, id, in.ExpectedRevision, string(raw), time.Now().UTC())
		return &models.TrainingProposal{ID: id, ExpectedRevision: in.ExpectedRevision, Document: &models.TrainingProgramme{Training: candidate.Training, Curriculum: candidate.Curriculum, Modules: candidate.Modules, Concepts: candidate.Concepts}}, err
	}
	if candidate.Curriculum != nil {
		if err = s.CompareAndSwapCurriculum(ctx, p.LearnerID, current.Training.DomainID, current.Curriculum.Version, candidate.Curriculum); err != nil {
			return nil, err
		}
		for _, key := range candidate.Curriculum.Graph.Concepts {
			if err = s.InsertConceptStateIfNotExists(ctx, models.NewConceptStateInDomain(p.LearnerID, current.Training.DomainID, key)); err != nil {
				return nil, err
			}
		}
	} else {
		versionID := current.Training.VersionID
		var status string
		if err = s.queryRow(ctx, `SELECT status FROM formation_versions WHERE tenant_id=? AND id=?`, p.TenantID, versionID).Scan(&status); err != nil {
			return nil, err
		}
		if status == "published" {
			versionID, err = s.newTrainingVersion(ctx, p, current.Training.FormationID)
			if err != nil {
				return nil, err
			}
		}
		if err = s.replaceTrainingContent(ctx, p, versionID, candidate.Modules, candidate.Concepts); err != nil {
			return nil, err
		}
		if _, err = s.exec(ctx, `UPDATE training_submissions SET status='superseded' WHERE tenant_id=? AND formation_id=? AND status='pending'`, p.TenantID, current.Training.FormationID); err != nil {
			return nil, err
		}
	}
	if _, err = s.exec(ctx, `DELETE FROM training_proposals WHERE tenant_id=? AND membership_id=? AND training_id=?`, p.TenantID, p.MembershipID, in.ID); err != nil {
		return nil, err
	}
	if err = s.AppendAuditEvent(ctx, p, models.AuditEvent{Action: "training.edit", TargetType: "training", TargetID: in.ID}); err != nil {
		return nil, err
	}
	return s.trainingDocument(ctx, p, in.ID)
}

func (s *Store) newTrainingVersion(ctx context.Context, p models.Principal, formationID string) (string, error) {
	id, err := generateID()
	if err != nil {
		return "", err
	}
	_, err = s.exec(ctx, `INSERT INTO formation_versions(id,tenant_id,formation_id,version,status,metadata_json,created_by,created_at) SELECT ?,?,?,COALESCE(MAX(version),0)+1,'draft','{}',?,? FROM formation_versions WHERE tenant_id=? AND formation_id=?`, id, p.TenantID, formationID, p.UserID, time.Now().UTC(), p.TenantID, formationID)
	return id, err
}

// The caller has checked the concrete resource capability under its lock. This
// helper shares normalization and graph validation with institutional authoring.
func (s *Store) replaceTrainingContent(ctx context.Context, p models.Principal, versionID string, modules []models.FormationModuleInput, concepts []models.FormationConceptInput) error {
	var status string
	if err := s.queryRow(ctx, `SELECT status FROM formation_versions WHERE tenant_id=? AND id=?`, p.TenantID, versionID).Scan(&status); err != nil {
		return err
	}
	if status != "draft" {
		return storeport.ErrFormationVersionImmutable
	}
	for _, table := range []string{"concept_prerequisites", "formation_concepts", "formation_modules"} {
		if _, err := s.exec(ctx, `DELETE FROM `+table+` WHERE tenant_id=? AND formation_version_id=?`, p.TenantID, versionID); err != nil {
			return err
		}
	}
	moduleIDs := map[string]string{}
	conceptIDs := map[string]string{}
	for _, m := range modules {
		id, err := generateID()
		if err != nil {
			return err
		}
		moduleIDs[m.StableKey] = id
		metadata := m.MetadataJSON
		if metadata == "" {
			metadata = "{}"
		}
		if !json.Valid([]byte(metadata)) {
			return fmt.Errorf("invalid module metadata")
		}
		if _, err = s.exec(ctx, `INSERT INTO formation_modules(id,tenant_id,formation_version_id,stable_key,title,position,metadata_json) VALUES(?,?,?,?,?,?,?)`, id, p.TenantID, versionID, m.StableKey, m.Title, m.Position, metadata); err != nil {
			return err
		}
	}
	for _, c := range concepts {
		id, err := generateID()
		if err != nil {
			return err
		}
		conceptIDs[c.StableKey] = id
		if err = normalizeFormationConcept(&c); err != nil {
			return err
		}
		if _, err = s.exec(ctx, `INSERT INTO formation_concepts(id,tenant_id,formation_version_id,module_id,stable_key,label,position,metadata_json) VALUES(?,?,?,?,?,?,?,?)`, id, p.TenantID, versionID, moduleIDs[c.ModuleStableKey], c.StableKey, c.Label, c.Position, c.MetadataJSON); err != nil {
			return err
		}
	}
	for _, c := range concepts {
		for _, pre := range c.Prerequisites {
			if conceptIDs[pre] == "" {
				return fmt.Errorf("unknown prerequisite")
			}
			if _, err := s.exec(ctx, `INSERT INTO concept_prerequisites(tenant_id,formation_version_id,concept_id,prerequisite_id) VALUES(?,?,?,?)`, p.TenantID, versionID, conceptIDs[c.StableKey], conceptIDs[pre]); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) shareTraining(ctx context.Context, p models.Principal, in models.TrainingRequest) (any, error) {
	if p.TenantID == models.LegacyTenantID {
		return nil, storeport.ErrInvalidPrincipal
	}
	name := strings.TrimSpace(in.Name)
	if name == "" || len(name) > 500 {
		return nil, fmt.Errorf("name is required")
	}
	var source *models.TrainingDocument
	var err error
	kind := "shared"
	sourceID := ""
	if in.ID != "" {
		source, err = s.trainingDocument(ctx, p, in.ID)
		if err != nil {
			return nil, err
		}
		if source.Curriculum == nil || !source.Training.CanShare {
			return nil, storeport.ErrInvalidPrincipal
		}
		sourceID = source.Training.DomainID
		if in.ExpectedRevision != source.Training.Revision {
			return nil, storeport.ErrCurriculumVersionConflict
		}
		source.Modules = []models.FormationModuleInput{{StableKey: "main", Title: name}}
		for i, c := range source.Curriculum.Concepts {
			if c.Status != models.CurriculumConceptActive {
				continue
			}
			source.Concepts = append(source.Concepts, models.FormationConceptInput{ModuleStableKey: "main", StableKey: c.Key, Label: c.Label, Description: c.Description, Level: c.Level, Outcomes: c.Outcomes, Criteria: c.Criteria, Prerequisites: source.Curriculum.Graph.Prerequisites[c.Key], Position: i})
		}
	} else {
		if !trainingOwner(p) || in.Document == nil {
			return nil, storeport.ErrInvalidPrincipal
		}
		kind = "institutional"
		source = in.Document
	}
	ownerID := p.MembershipID
	if !trainingOwner(p) {
		if err = s.queryRow(ctx, `SELECT id FROM tenant_memberships WHERE tenant_id=? AND status='active' AND CAST(roles_json AS TEXT) LIKE '%"owner"%' ORDER BY id LIMIT 1`, p.TenantID).Scan(&ownerID); err != nil {
			return nil, err
		}
	}
	id, err := generateID()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	audience := in.Audience
	if audience == "" {
		audience = "invitation"
	}
	if !slices.Contains([]string{"open", "invitation", "approval"}, audience) {
		return nil, fmt.Errorf("invalid audience")
	}
	if _, err = s.exec(ctx, `INSERT INTO formations(id,tenant_id,name,description,status,created_by,created_at,updated_at,owner_membership_id,enrollment_policy) VALUES(?,?,?,?,'draft',?,?,?,?,?)`, id, p.TenantID, name, source.Training.Description, p.UserID, now, now, ownerID, audience); err != nil {
		return nil, err
	}
	if _, err = s.exec(ctx, `INSERT INTO training_workflows(tenant_id,formation_id,author_id,source_domain_id,kind) VALUES(?,?,?,?,?)`, p.TenantID, id, p.UserID, sourceID, kind); err != nil {
		return nil, err
	}
	versionID, err := s.newTrainingVersion(ctx, p, id)
	if err != nil {
		return nil, err
	}
	current := &models.TrainingDocument{Training: models.Training{CanEdit: true, Revision: "initial"}}
	candidate, _, err := validateTrainingEdit(current, models.TrainingRequest{ExpectedRevision: "initial", Document: source}, p)
	if err != nil {
		return nil, err
	}
	if err = s.replaceTrainingContent(ctx, p, versionID, candidate.Modules, candidate.Concepts); err != nil {
		return nil, err
	}
	if err = s.AppendAuditEvent(ctx, p, models.AuditEvent{Action: "training.copy_design", TargetType: "formation", TargetID: id, Reason: "copy of design only; private observations and conversations excluded"}); err != nil {
		return nil, err
	}
	return s.trainingDocument(ctx, p, "formation:"+id)
}

func (s *Store) trainingPublication(ctx context.Context, p models.Principal, action string, in models.TrainingRequest) (any, error) {
	if err := s.lockTraining(ctx, p, in.ID); err != nil {
		return nil, err
	}
	doc, err := s.trainingDocument(ctx, p, in.ID)
	if err != nil {
		return nil, err
	}
	t := doc.Training
	if !t.CanEdit || t.FormationID == "" {
		return nil, storeport.ErrInvalidPrincipal
	}
	if in.ExpectedRevision != t.Revision {
		return nil, storeport.ErrCurriculumVersionConflict
	}
	var status string
	if err = s.queryRow(ctx, `SELECT status FROM formation_versions WHERE tenant_id=? AND id=?`, p.TenantID, t.VersionID).Scan(&status); err != nil {
		return nil, err
	}
	if status != "draft" {
		return nil, storeport.ErrFormationVersionImmutable
	}
	current := &models.TrainingDocument{Training: t}
	if _, _, err = validateTrainingEdit(current, models.TrainingRequest{ExpectedRevision: t.Revision, Document: doc}, p); err != nil {
		return nil, err
	}
	if action == "submit_training" {
		id, err := generateID()
		if err != nil {
			return nil, err
		}
		if _, err = s.exec(ctx, `UPDATE training_submissions SET status='superseded' WHERE tenant_id=? AND formation_id=? AND status='pending'`, p.TenantID, t.FormationID); err != nil {
			return nil, err
		}
		design := models.Training{ID: t.ID, Name: t.Name, Description: t.Description, Kind: t.Kind, FormationID: t.FormationID, VersionID: t.VersionID, Revision: t.Revision, Audience: t.Audience, AuthorID: t.AuthorID}
		raw, err := json.Marshal(models.TrainingProgramme{Training: design, Modules: doc.Modules, Concepts: doc.Concepts})
		if err != nil {
			return nil, err
		}
		if _, err = s.exec(ctx, `INSERT INTO training_submissions(tenant_id,id,formation_id,version_id,revision,author_id,status,document_json,created_at) VALUES(?,?,?,?,?,?,'pending',?,?)`, p.TenantID, id, t.FormationID, t.VersionID, t.Revision, p.UserID, string(raw), time.Now().UTC()); err != nil {
			return nil, err
		}
		return s.trainingDocument(ctx, p, in.ID)
	}
	if action == "decide_training" {
		if !trainingOwner(p) || doc.Submission == nil || doc.Submission.Status != "pending" || doc.Submission.Revision != t.Revision || doc.Submission.VersionID != t.VersionID {
			return nil, storeport.ErrInvalidPrincipal
		}
		if strings.TrimSpace(in.Reason) == "" {
			return nil, fmt.Errorf("a reason is required")
		}
		if in.Decision == "changes_requested" {
			if _, err = s.exec(ctx, `UPDATE training_submissions SET status='changes_requested',reason=? WHERE tenant_id=? AND id=? AND status='pending'`, in.Reason, p.TenantID, doc.Submission.ID); err != nil {
				return nil, err
			}
			return s.trainingDocument(ctx, p, in.ID)
		}
		if in.Decision != "approve" {
			return nil, fmt.Errorf("invalid decision")
		}
	} else if !t.CanPublish {
		return nil, storeport.ErrInvalidPrincipal
	}
	// Approval and publication have the same transaction boundary. Existing
	// enrollments and their immutable formation_concept IDs are never migrated.
	now := time.Now().UTC()
	if _, err = s.exec(ctx, `UPDATE formation_versions SET status='published',published_at=? WHERE tenant_id=? AND id=? AND status='draft'`, now, p.TenantID, t.VersionID); err != nil {
		return nil, err
	}
	if _, err = s.exec(ctx, `UPDATE formations SET status='active',updated_at=? WHERE tenant_id=? AND id=?`, now, p.TenantID, t.FormationID); err != nil {
		return nil, err
	}
	cohortID := stableLearningID("catalog_", p.TenantID, t.VersionID)
	if _, err = s.exec(ctx, `INSERT INTO cohorts(id,tenant_id,formation_version_id,name,capacity,reserved_seats,status,version,created_by,created_at,updated_at) VALUES(?,?,?,?,1000000,0,'open',1,?,?,?)`, cohortID, p.TenantID, t.VersionID, t.Name, p.UserID, now, now); err != nil {
		return nil, err
	}
	// Carry only explicit access grants into the next offering. Never enroll.
	if _, err = s.exec(ctx, `INSERT INTO formation_admissions(tenant_id,cohort_id,membership_id,status,version,updated_at)
 SELECT a.tenant_id,?,a.membership_id,a.status,1,? FROM formation_admissions a JOIN cohorts c ON c.tenant_id=a.tenant_id AND c.id=a.cohort_id
 WHERE a.tenant_id=? AND c.formation_version_id IN (SELECT id FROM formation_versions WHERE tenant_id=? AND formation_id=?) AND a.status IN ('invited','approved')
 ON CONFLICT(tenant_id,cohort_id,membership_id) DO NOTHING`, cohortID, now, p.TenantID, p.TenantID, t.FormationID); err != nil {
		return nil, err
	}
	if _, err = s.exec(ctx, `UPDATE cohorts SET status='closed',version=version+1,updated_at=? WHERE tenant_id=? AND substr(id,1,8)='catalog_' AND id<>? AND formation_version_id IN (SELECT id FROM formation_versions WHERE tenant_id=? AND formation_id=?)`, now, p.TenantID, cohortID, p.TenantID, t.FormationID); err != nil {
		return nil, err
	}
	if _, err = s.exec(ctx, `UPDATE training_submissions SET status='published',reason=? WHERE tenant_id=? AND formation_id=? AND version_id=? AND status='pending'`, in.Reason, p.TenantID, t.FormationID, t.VersionID); err != nil {
		return nil, err
	}
	if err = s.AppendAuditEvent(ctx, p, models.AuditEvent{Action: "training.publish", TargetType: "formation_version", TargetID: t.VersionID, Reason: in.Reason}); err != nil {
		return nil, err
	}
	return s.trainingDocument(ctx, p, in.ID)
}

func (s *Store) trainingAccess(ctx context.Context, p models.Principal, in models.TrainingRequest) (any, error) {
	if err := s.lockTraining(ctx, p, in.ID); err != nil {
		return nil, err
	}
	doc, err := s.trainingDocument(ctx, p, in.ID)
	if err != nil {
		return nil, err
	}
	t := doc.Training
	if !t.CanEdit || t.FormationID == "" {
		return nil, storeport.ErrInvalidPrincipal
	}
	if in.ExpectedRevision != t.Revision {
		return nil, storeport.ErrCurriculumVersionConflict
	}
	if in.FreePublication != nil {
		if !trainingOwner(p) {
			return nil, storeport.ErrInvalidPrincipal
		}
		if _, err = s.exec(ctx, `INSERT INTO training_workflows(tenant_id,formation_id,author_id,kind,free_publication) VALUES(?,?,?,'institutional',?) ON CONFLICT(tenant_id,formation_id) DO UPDATE SET free_publication=excluded.free_publication`, p.TenantID, t.FormationID, t.AuthorID, boolInt(*in.FreePublication)); err != nil {
			return nil, err
		}
	}
	if in.Audience != "" {
		if !slices.Contains([]string{"open", "invitation", "approval"}, in.Audience) {
			return nil, fmt.Errorf("invalid audience")
		}
		if _, err = s.exec(ctx, `UPDATE formations SET enrollment_policy=?,updated_at=? WHERE tenant_id=? AND id=?`, in.Audience, time.Now().UTC(), p.TenantID, t.FormationID); err != nil {
			return nil, err
		}
	}
	if in.MemberID != "" {
		var member, rolesJSON string
		if err = s.queryRow(ctx, `SELECT id,roles_json FROM tenant_memberships WHERE tenant_id=? AND id=? AND status='active'`, p.TenantID, in.MemberID).Scan(&member, &rolesJSON); err != nil {
			return nil, storeport.ErrInvalidPrincipal
		}
		if in.Decision == "edit" || in.Decision == "remove_editor" {
			if !trainingOwner(p) && t.AuthorID != p.UserID {
				return nil, storeport.ErrInvalidPrincipal
			}
			if in.Decision == "edit" {
				var roles []string
				if json.Unmarshal([]byte(rolesJSON), &roles) != nil || (!slices.Contains(roles, models.RoleTrainer) && !slices.Contains(roles, models.RolePedagogyManager)) {
					return nil, fmt.Errorf("programme editors must be trainers or pedagogy managers")
				}
				if _, err = s.exec(ctx, `INSERT INTO formation_trainers(tenant_id,formation_id,membership_id,assigned_by,assigned_at) VALUES(?,?,?,?,?) ON CONFLICT DO NOTHING`, p.TenantID, t.FormationID, member, p.UserID, time.Now().UTC()); err != nil {
					return nil, err
				}
			} else {
				if _, err = s.exec(ctx, `DELETE FROM formation_trainers WHERE tenant_id=? AND formation_id=? AND membership_id=?`, p.TenantID, t.FormationID, member); err != nil {
					return nil, err
				}
			}
		} else {
			if t.CohortID == "" {
				return nil, fmt.Errorf("publish the version before inviting members")
			}
			decision := "invited"
			if in.Decision == "revoke" {
				decision = "revoked"
			}
			if err = s.setFormationAdmission(ctx, p.TenantID, t.CohortID, member, decision); err != nil {
				return nil, err
			}
		}
	}
	if err = s.AppendAuditEvent(ctx, p, models.AuditEvent{Action: "training.access", TargetType: "formation", TargetID: t.FormationID}); err != nil {
		return nil, err
	}
	return s.trainingDocument(ctx, p, in.ID)
}

func (s *Store) trainingMembers(ctx context.Context, p models.Principal, in models.TrainingRequest) (any, error) {
	doc, err := s.trainingDocument(ctx, p, in.ID)
	if err != nil {
		return nil, err
	}
	if !doc.Training.CanEdit || doc.Training.FormationID == "" {
		return nil, storeport.ErrInvalidPrincipal
	}
	out := []map[string]string{}
	if len(strings.TrimSpace(in.Query)) < 2 {
		return out, nil
	}
	publicKey := in.Query
	if normalized, err := NormalizeNativePublicKey(in.Query); err == nil {
		publicKey = normalized
	}
	rows, err := s.query(ctx, `SELECT m.id,COALESCE(n.display_name,''),COALESCE(n.public_key,'') FROM tenant_memberships m JOIN users u ON u.id=m.user_id LEFT JOIN native_identities n ON n.user_id=u.id AND n.status='active'
 WHERE m.tenant_id=? AND m.status='active' AND u.status='active' AND (lower(COALESCE(n.display_name,'')) LIKE ? OR n.public_key=?) ORDER BY n.display_name,m.id LIMIT 20`, p.TenantID, "%"+strings.ToLower(in.Query)+"%", publicKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, name, key string
		if err = rows.Scan(&id, &name, &key); err != nil {
			return nil, err
		}
		out = append(out, map[string]string{"membership_id": id, "name": name, "public_key": key})
	}
	return out, rows.Err()
}

func (s *Store) trainingGroups(ctx context.Context, p models.Principal, in models.TrainingRequest) (models.TrainerCohortPage, error) {
	page := models.TrainerCohortPage{Items: []models.TrainerCohort{}}
	if in.Limit == 0 {
		in.Limit = 20
	}
	if validatePage(in.Limit) != nil {
		return page, fmt.Errorf("invalid page")
	}
	if !strings.HasPrefix(in.ID, "formation:") {
		return page, storeport.ErrFormationUnavailable
	}
	from, args, err := s.progressAccess(ctx, p)
	if err != nil {
		return page, err
	}
	args = append(args, strings.TrimPrefix(in.ID, "formation:"), in.After, in.Limit+1)
	rows, err := s.query(ctx, `SELECT `+trainerCohortColumns+from+` AND f.id=? AND c.id>? ORDER BY c.id LIMIT ?`, args...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		cohort, err := scanTrainerCohort(rows)
		if err != nil {
			return page, err
		}
		page.Items = append(page.Items, cohort)
	}
	if len(page.Items) > in.Limit {
		page.Items = page.Items[:in.Limit]
		page.NextAfter = page.Items[in.Limit-1].CohortID
	}
	return page, rows.Err()
}
