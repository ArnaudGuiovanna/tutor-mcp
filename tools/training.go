// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"errors"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"tutor-mcp/auth"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

var trainingTools = map[string]string{
	"preview_training_lifecycle":   "Preview archival, restoration, withdrawal or deletion effects for this authorized training. decision names the intended lifecycle action. Does not mutate data.",
	"archive_training":             "After an explicit human request and confirm=true, archive this exact revision. Personal learning evidence and existing common learner versions are preserved.",
	"restore_training":             "After human confirmation, restore an archived training without recreating it or replaying its evidence.",
	"delete_training":              "After an explicit human request and confirm=true, remove a personal training or wholly unpublished draft. Published common programmes cannot be deleted. Preserve historical learning evidence.",
	"withdraw_training":            "Authorized publisher only: withdraw a published common training from new admissions. Existing learners keep their enrolled version. Requires confirm=true and expected_revision.",
	"get_training_groups":          "Read the paginated real cohorts assigned to this trainer, limited to this formation. Requires progress:read; personal learning data is excluded.",
	"list_training_library":        "Search the server's unified, paginated library. Each training appears once. Use query to resume an existing training; present ambiguous matches before selecting. Never recreate an existing training.",
	"get_training":                 "Read the authorized programme, personal progress, review decision and proposed edit. id comes from list_training_library or init_domain (personal:domain_id).",
	"get_conversation_context":     "Read this account's confirmed active training and revision for conversation_id. Always use its explicit domain_id for teaching and observations.",
	"select_conversation_training": "Explicitly switch this conversation's active training. Required: id, key (stable idempotency key), expected_selection (the latest selection_version from get_conversation_context). init_domain already selects its new training and returns the confirmed context; do not repeat that selection with the old version. For a later switch read the current context first, then use its latest selection_version and a fresh key. Tell the learner which training is now selected.",
	"preview_training_edit":        "Validate a complete proposed programme against expected_revision. Returns server-computed learning consequences without changing the programme. Manual and AI editing use this same contract.",
	"propose_training_edit":        "Save an examinable proposal in the training's editor. Supply the complete document and expected_revision read from get_training. Does not apply changes or publish. Invite the human to review the editor.",
	"save_training_edit":           "After human review, apply the complete programme using expected_revision. Changes to published common programmes create the next draft; existing learners retain their immutable version.",
	"share_training":               "Copy only the design of a private training to a shared contribution. Conversations and progress stay private. Supply name, audience and expected_revision. With no id, only an institution owner may explicitly create an institutional programme from document. Does not publish or enroll.",
	"submit_training":              "Submit this exact draft revision to the institution owner for review. Every later revision requires a new decision unless the owner enables free publication.",
	"decide_training":              "Owner only: give a reason and approve (atomic approval and publication) or changes_requested on the exact pending revision. Never invent a review decision.",
	"publish_training":             "Publish the reviewed programme only when can_publish is true. Preserve the chosen audience and existing learner versions. Requires explicit human publication request; never enroll anyone.",
	"set_training_access":          "Set a common training's audience (open means all institution members), invite a chosen membership_id, or authorize an editor (decision=edit). Only the owner can set free_publication. No administrative role is granted.",
	"get_training_policy":          "Read whether this institution permits personal learning and whether the current actor can manage that policy.",
	"set_training_policy":          "Owner only: explicitly set personal_learning. Does not change other institutions or publish any training.",
	"find_training_members":        "For an editable common training, find institution members by name or public key; returns only safe display names, keys and membership IDs for invitations.",
	"join_training":                "Explicitly join the chosen training using its real offering. Invitation and approval policies are enforced by the server; does not change the active training automatically.",
}

func contributionTool(name string) bool {
	switch name {
	case "share_training", "submit_training", "decide_training", "publish_training", "set_training_access", "set_training_policy", "find_training_members":
		return true
	}
	return false
}

// These contracts serve both private learning and common programme authoring.
// The store narrows each grant to the actual document and fields requested.
func contributionCompatibleTool(name string) bool {
	switch name {
	case "list_training_library", "get_training", "get_training_policy", "preview_training_edit", "propose_training_edit", "save_training_edit", "preview_training_lifecycle", "archive_training", "restore_training", "delete_training", "withdraw_training":
		return true
	}
	return false
}

func registerTrainingTools(server *mcp.Server, deps *Deps) {
	if _, ok := deps.Store.(storeport.TrainingStore); !ok {
		return
	}
	for name, description := range trainingTools {
		schema, err := jsonschema.For[models.TrainingRequest](&jsonschema.ForOptions{})
		if err != nil {
			panic("cannot infer training input schema: " + err.Error())
		}
		if !models.TrainingReadAction(name) {
			schema.Required = append(schema.Required, "key")
			minimum, maximum := 1, 128
			schema.Properties["key"].MinLength = &minimum
			schema.Properties["key"].MaxLength = &maximum
		}
		if name == "select_conversation_training" {
			schema.Required = append(schema.Required, "id", "expected_selection")
		}
		addTool(server, &mcp.Tool{Name: name, Description: description, InputSchema: schema}, func(ctx context.Context, _ *mcp.CallToolRequest, in models.TrainingRequest) (*mcp.CallToolResult, any, error) {
			p, ok := auth.GetPrincipal(ctx)
			if !ok {
				r, _ := errorResult("authenticated principal required")
				return r, nil, nil
			}
			if bound := storeport.ConversationID(ctx); bound != "" && (name == "get_conversation_context" || name == "select_conversation_training") {
				if in.ConversationID != "" && in.ConversationID != bound {
					r, _ := errorResult("conversation does not match this MCP session")
					return r, nil, nil
				}
				in.ConversationID = bound
			}
			out, err := deps.Store.(storeport.TrainingStore).TrainingAction(ctx, p, name, in)
			if errors.Is(err, storeport.ErrCurriculumVersionConflict) {
				message := "Reload the current programme and review the proposal before trying again."
				if name == "select_conversation_training" {
					message = "Read get_conversation_context again and use its current selection_version as expected_selection, with a fresh key. init_domain already selects a newly created training."
				}
				r, _ := jsonResult(map[string]any{"error": "revision_conflict", "message": message})
				r.IsError = true
				return r, nil, nil
			}
			if err != nil {
				r, _ := safeErrorResult(deps.Logger, "training action rejected", err)
				return r, nil, nil
			}
			r, _ := jsonResult(out)
			return r, nil, nil
		})
	}
}
