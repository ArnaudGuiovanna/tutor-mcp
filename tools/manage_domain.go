// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Domain lifecycle actions accepted by manage_domain.
const (
	domainActionArchive   = "archive"
	domainActionUnarchive = "unarchive"
	domainActionDelete    = "delete"
)

var domainLifecycleActions = []string{domainActionArchive, domainActionUnarchive, domainActionDelete}

type ManageDomainParams struct {
	IdempotentMutationParams
	Action   string `json:"action" jsonschema:"archive hides the domain from routing and the dashboard while keeping progress; unarchive reactivates an archived domain; delete removes it from runtime views with an auditable tombstone and requires confirm=true"`
	DomainID string `json:"domain_id" jsonschema:"ID of the domain to change"`
	Confirm  bool   `json:"confirm,omitempty" jsonschema:"must be true for action=delete; ignored by archive and unarchive"`
}

// registerManageDomain exposes the three reversible/irreversible lifecycle
// transitions of a personal domain behind one action parameter. They share
// the same input, ownership check and response shape; delete alone requires
// an explicit confirmation.
func registerManageDomain(server *mcp.Server, deps *Deps) {
	addTool(server, deps, &mcp.Tool{
		Name:        "manage_domain",
		Description: "Archive, unarchive or delete one of the learner's domains. Archived domains leave routing and the dashboard but keep all progress; unarchive brings them back unchanged. delete removes the domain from runtime views with an auditable tombstone (curriculum versions and learning evidence are preserved) and requires confirm=true after an explicit learner request.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params ManageDomainParams) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "manage_domain", err)
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		if params.DomainID == "" {
			r, _ := errorResult("domain_id is required")
			return r, nil, nil
		}
		if err := validateEnum("action", params.Action, domainLifecycleActions); err != nil {
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		if params.Action == domainActionDelete && !params.Confirm {
			r, _ := errorResult("confirm must be true to remove a domain from runtime views.")
			return r, nil, nil
		}

		domain, err := deps.Store.GetLearnerDomainByID(ctx, learnerID, params.DomainID)
		if err != nil {
			r, _ := domainLookupErrorResult(deps, err)
			return r, nil, nil
		}

		out := map[string]any{"action": params.Action, "domain_id": domain.ID, "domain_name": domain.Name}
		switch params.Action {
		case domainActionArchive:
			if err := deps.Store.ArchiveDomain(ctx, params.DomainID, learnerID); err != nil {
				r, _ := safeErrorResult(deps.Logger, "failed to archive domain", err)
				return r, nil, nil
			}
			out["archived"] = true
			out["message"] = fmt.Sprintf("Domain '%s' archived. Progress preserved. Use manage_domain(action=unarchive) to restore it.", domain.Name)
		case domainActionUnarchive:
			if err := deps.Store.UnarchiveDomain(ctx, params.DomainID, learnerID); err != nil {
				r, _ := safeErrorResult(deps.Logger, "failed to unarchive domain", err)
				return r, nil, nil
			}
			out["archived"] = false
			out["message"] = fmt.Sprintf("Domain '%s' restored.", domain.Name)
		case domainActionDelete:
			if err := deps.Store.DeleteDomain(ctx, params.DomainID, learnerID); err != nil {
				r, _ := safeErrorResult(deps.Logger, "failed to delete domain", err)
				return r, nil, nil
			}
			out["deleted"] = true
			out["message"] = fmt.Sprintf("Domain %q was removed from runtime views. Curriculum versions and all learning evidence were preserved for audit.", domain.Name)
		}
		deps.Logger.Info("manage_domain: success", "action", params.Action, "domain", params.DomainID, "learner", learnerID)
		r, _ := jsonResult(out)
		return r, nil, nil
	})
}
