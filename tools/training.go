// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"tutor-mcp/auth"
	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

// trainingToolSpec describes one MCP training tool. Several tools map onto a
// single store action; edit_training and manage_training_lifecycle select the
// store action from their action/preview inputs (see trainingStoreAction).
type trainingToolSpec struct {
	description string
	// fields lists the advertised inputs, in schema order.
	fields string
	// read is true when every call is a read: no idempotency_key is required
	// and the advertised readOnlyHint is true.
	read bool
	// actionSelected tools choose a read or a write per call from their
	// action/preview inputs (trainingStoreAction): discovery advertises the
	// read scope, the call adds learner:write when it applies a change, and
	// idempotency_key is required by the store only for applying calls.
	actionSelected bool
	// contribution tools are authored under formation:contribute; compatible
	// tools also accept that grant for the documents the store authorizes.
	contribution, contributionCompatible bool
}

var trainingTools = map[string]trainingToolSpec{
	"list_training_library": {
		description: "Search the server's unified, paginated library. Each training appears once. Use query to resume an existing training; present ambiguous matches before selecting. Never recreate an existing training.",
		fields:      "query filter after limit", read: true, contributionCompatible: true,
	},
	"get_training": {
		description: "Read the authorized programme, personal progress, review decision, proposed edit and can_* capabilities. training_id comes from list_training_library or init_domain (personal:<domain_id>).",
		fields:      "training_id", read: true, contributionCompatible: true,
	},
	"get_conversation_context": {
		description: "Read this account's confirmed active training and revision for conversation_id. Always use its explicit domain_id for teaching and observations.",
		fields:      "conversation_id", read: true,
	},
	"select_conversation_training": {
		description: "Explicitly switch this conversation's active training. Pass expected_selection = the latest selection_version from get_conversation_context and a fresh idempotency_key. init_domain already selects its new training and returns the confirmed context; do not repeat that selection with the old version. Tell the learner which training is now selected.",
		fields:      "training_id conversation_id expected_selection idempotency_key",
	},
	"edit_training": {
		description: "Edit a programme with one complete document and the expected_revision read from get_training. action=preview validates the document and returns server-computed learning consequences without changing anything; action=propose stores an examinable proposal in the training editor for human review (does not apply or publish); action=save applies the reviewed document (a published common programme gets a new draft; enrolled learners keep their immutable version). Manual and AI editing share this contract. propose and save require idempotency_key.",
		fields:      "action training_id expected_revision document idempotency_key", actionSelected: true, contributionCompatible: true,
	},
	"share_training": {
		description: "Copy only the design of a private training to a shared contribution. Conversations and progress stay private. Supply name, audience and expected_revision. With no training_id, only an institution owner may explicitly create an institutional programme from document. Does not publish or enroll.",
		fields:      "training_id expected_revision name audience document idempotency_key", contribution: true,
	},
	"submit_training": {
		description: "Submit this exact draft revision to the institution owner for review. Every later revision requires a new decision unless the owner enables free publication.",
		fields:      "training_id expected_revision reason idempotency_key", contribution: true,
	},
	"decide_training": {
		description: "Owner only: give a reason and approve (atomic approval and publication) or changes_requested on the exact pending revision. Never invent a review decision.",
		fields:      "training_id expected_revision decision reason idempotency_key", contribution: true,
	},
	"publish_training": {
		description: "Publish the reviewed programme only when can_publish is true. Preserve the chosen audience and existing learner versions. Requires an explicit human publication request; never enroll anyone.",
		fields:      "training_id expected_revision reason idempotency_key", contribution: true,
	},
	"set_training_access": {
		description: "Set a common training's audience (open means all institution members), invite a chosen member_id, or authorize an editor (decision=edit). Only the owner can set free_publication. No administrative role is granted.",
		fields:      "training_id expected_revision audience member_id decision free_publication idempotency_key", contribution: true,
	},
	"get_training_policy": {
		description: "Read whether this institution permits personal learning and whether the current actor can manage that policy.",
		read:        true, contributionCompatible: true,
	},
	"set_training_policy": {
		description: "Owner only: explicitly set personal_learning. Does not change other institutions or publish any training.",
		fields:      "personal_learning idempotency_key", contribution: true,
	},
	"find_training_members": {
		description: "For an editable common training, find institution members by name or public key; returns only safe display names, keys and membership IDs for invitations.",
		fields:      "training_id query", read: true, contribution: true,
	},
	"join_training": {
		description: "Explicitly join the chosen training using its real offering. Invitation and approval policies are enforced by the server; does not change the active training automatically.",
		fields:      "training_id idempotency_key",
	},
	"get_training_groups": {
		description: "Read the paginated real cohorts assigned to this trainer, limited to this formation. Requires progress:read; personal learning data is excluded.",
		fields:      "training_id after limit", read: true,
	},
	"manage_training_lifecycle": {
		description: "Archive, restore, delete or withdraw a training after an explicit human request. Call first with preview=true to read the effects of the intended action without changing anything; then apply it with confirm=true, the current expected_revision and a fresh idempotency_key. archive keeps learning evidence and enrolled versions; restore reactivates an archived training without replaying evidence; delete removes a personal training or a wholly unpublished draft (published common programmes cannot be deleted); withdraw (authorized publisher only) closes a published common training to new admissions while enrolled learners keep their version and progress.",
		fields:      "action training_id preview expected_revision confirm reason idempotency_key", actionSelected: true, contributionCompatible: true,
	},
}

// Values accepted by the action inputs of the merged training tools.
var (
	trainingEditActions      = []string{"preview", "propose", "save"}
	trainingLifecycleActions = []string{"archive", "restore", "delete", "withdraw"}
)

func trainingTool(name string) bool {
	_, ok := trainingTools[name]
	return ok
}

func contributionTool(name string) bool {
	return trainingTools[name].contribution
}

// These contracts serve both private learning and common programme authoring.
// The store narrows each grant to the actual document and fields requested.
func contributionCompatibleTool(name string) bool {
	return trainingTools[name].contributionCompatible
}

// trainingToolInput is the MCP envelope of every training tool. It is
// translated into the store's models.TrainingRequest by trainingRequest.
type trainingToolInput struct {
	Action            string          `json:"action,omitempty"`
	TrainingID        string          `json:"training_id,omitempty"`
	Preview           bool            `json:"preview,omitempty"`
	Query             string          `json:"query,omitempty"`
	Filter            string          `json:"filter,omitempty"`
	After             string          `json:"after,omitempty"`
	Limit             int             `json:"limit,omitempty"`
	ConversationID    string          `json:"conversation_id,omitempty"`
	ExpectedRevision  string          `json:"expected_revision,omitempty"`
	ExpectedSelection *int            `json:"expected_selection,omitempty"`
	Document          json.RawMessage `json:"document,omitempty"`
	IdempotencyKey    string          `json:"idempotency_key,omitempty"`
	Name              string          `json:"name,omitempty"`
	Audience          string          `json:"audience,omitempty"`
	MemberID          string          `json:"member_id,omitempty"`
	Decision          string          `json:"decision,omitempty"`
	Reason            string          `json:"reason,omitempty"`
	FreePublication   *bool           `json:"free_publication,omitempty"`
	PersonalLearning  *bool           `json:"personal_learning,omitempty"`
	Confirm           bool            `json:"confirm,omitempty"`
}

// trainingStoreAction maps a tool call onto the store action it performs.
// The second result reports whether that action only reads.
func trainingStoreAction(name string, in trainingToolInput) (string, bool, error) {
	switch name {
	case "edit_training":
		if err := validateEnum("action", in.Action, trainingEditActions); err != nil {
			return "", false, err
		}
		action := in.Action + "_training_edit"
		return action, models.TrainingReadAction(action), nil
	case "manage_training_lifecycle":
		if err := validateEnum("action", in.Action, trainingLifecycleActions); err != nil {
			return "", false, err
		}
		if in.Preview {
			return "preview_training_lifecycle", true, nil
		}
		return in.Action + "_training", false, nil
	}
	if !trainingTool(name) {
		return "", false, fmt.Errorf("unknown training tool %s", name)
	}
	return name, trainingTools[name].read, nil
}

// trainingCallReadOnly parses only the inputs that select the store action, so
// authorization and replay middlewares never decode the programme document.
func trainingCallReadOnly(call *mcp.CallToolRequest) (bool, error) {
	var in trainingToolInput
	if len(call.Params.Arguments) > 0 {
		var selector struct {
			Action  string `json:"action"`
			Preview bool   `json:"preview"`
		}
		if err := json.Unmarshal(call.Params.Arguments, &selector); err != nil {
			return false, errors.New("invalid tool arguments")
		}
		in.Action, in.Preview = selector.Action, selector.Preview
	}
	_, read, err := trainingStoreAction(call.Params.Name, in)
	return read, err
}

func trainingRequest(action string, in trainingToolInput) (models.TrainingRequest, error) {
	out := models.TrainingRequest{
		ID: in.TrainingID, Query: in.Query, Filter: in.Filter, After: in.After, Limit: in.Limit,
		ConversationID: in.ConversationID, ExpectedRevision: in.ExpectedRevision, ExpectedSelection: in.ExpectedSelection,
		Key: in.IdempotencyKey, Name: in.Name, Audience: in.Audience, MemberID: in.MemberID, Decision: in.Decision,
		Reason: in.Reason, FreePublication: in.FreePublication, PersonalLearning: in.PersonalLearning, Confirm: in.Confirm,
	}
	if action == "preview_training_lifecycle" {
		out.Decision = in.Action + "_training"
	}
	if len(in.Document) > 0 && string(in.Document) != "null" {
		document, err := decodeTrainingDocument(in.Document)
		if err != nil {
			return out, err
		}
		out.Document = document
	}
	return out, nil
}

// decodeTrainingDocument decodes a programme document; module and concept
// metadata are JSON objects (models.FormationModuleInput.Metadata).
func decodeTrainingDocument(raw json.RawMessage) (*models.TrainingDocument, error) {
	var generic map[string]json.RawMessage
	if err := json.Unmarshal(raw, &generic); err != nil || generic == nil {
		return nil, storeport.Invalid("document must be a JSON object")
	}
	var document models.TrainingDocument
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, storeport.Invalid("document does not match the programme shape (module and concept metadata must be JSON objects)")
	}
	return &document, nil
}

func registerTrainingTools(server *mcp.Server, deps *Deps) {
	if _, ok := deps.Store.(storeport.TrainingStore); !ok {
		return
	}
	for name, spec := range trainingTools {
		schema := trainingInputSchema(name)
		addTool(server, deps, &mcp.Tool{Name: name, Description: spec.description, InputSchema: schema}, func(ctx context.Context, _ *mcp.CallToolRequest, in trainingToolInput) (*mcp.CallToolResult, any, error) {
			p, ok := auth.GetPrincipal(ctx)
			if !ok {
				r, _ := errorResult("authenticated principal required")
				return r, nil, nil
			}
			action, read, err := trainingStoreAction(name, in)
			if err != nil {
				r, _ := errorResult(err.Error())
				return r, nil, nil
			}
			if !read && !validIdempotencyKey.MatchString(in.IdempotencyKey) {
				r, _ := errorResult("idempotency_key is required for this training mutation (1..128 letters, digits, '.', '_', ':', or '-')")
				return r, nil, nil
			}
			if bound := storeport.ConversationID(ctx); bound != "" && (name == "get_conversation_context" || name == "select_conversation_training") {
				if in.ConversationID != "" && in.ConversationID != bound {
					r, _ := errorResult("conversation does not match this MCP session")
					return r, nil, nil
				}
				in.ConversationID = bound
			}
			request, err := trainingRequest(action, in)
			if err != nil {
				r, _ := trainingErrorResult(deps, err)
				return r, nil, nil
			}
			out, err := deps.Store.(storeport.TrainingStore).TrainingAction(ctx, p, action, request)
			if errors.Is(err, storeport.ErrCurriculumVersionConflict) {
				message := "Reload the current programme and review the proposal before trying again."
				if name == "select_conversation_training" {
					message = "Read get_conversation_context again and use its current selection_version as expected_selection, with a fresh idempotency_key. init_domain already selects a newly created training."
				}
				r, _ := jsonResult(map[string]any{"error": "revision_conflict", "message": message})
				r.IsError = true
				return r, nil, nil
			}
			if err != nil {
				r, _ := trainingErrorResult(deps, err)
				return r, nil, nil
			}
			r, _ := jsonResult(out)
			return r, nil, nil
		})
	}
}

// trainingErrorResult keeps the historical "training action rejected" prefix
// and adds the store's caller-correctable reason, or a fixed explanation for
// the sentinel errors a caller can act on. Missing and hidden trainings share
// one message so a response never confirms that an identifier exists.
func trainingErrorResult(deps *Deps, err error) (*mcp.CallToolResult, error) {
	const prefix = "training action rejected"
	switch {
	case errors.Is(err, storeport.ErrInvalidPrincipal):
		return errorResult(prefix + ": this account is not permitted to perform this action on this training (check can_* capabilities from get_training)")
	case errors.Is(err, storeport.ErrFormationUnavailable), errors.Is(err, storeport.ErrNotFound):
		return errorResult(prefix + ": training not found or not available to this account; search with list_training_library")
	case errors.Is(err, storeport.ErrIdempotencyKeyConflict):
		return errorResult(prefix + ": idempotency_key was already used for a different action or arguments; choose a fresh idempotency_key")
	case errors.Is(err, storeport.ErrFormationVersionImmutable):
		return errorResult(prefix + ": this version is no longer a draft; reload the training and edit its current draft")
	}
	return safeErrorResult(deps.Logger, prefix, err)
}
