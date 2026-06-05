// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type ArchiveDomainParams struct {
	DomainID string `json:"domain_id" jsonschema:"id of the domain to archive"`
}

func registerArchiveDomain(server *mcp.Server, deps *Deps) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "archive_domain",
		Description: "Archive a domain - it disappears from the dashboard and routing but progress is preserved. Use unarchive_domain to reactivate it.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params ArchiveDomainParams) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "archive_domain", err)
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		if params.DomainID == "" {
			r, _ := errorResult("domain_id is required")
			return r, nil, nil
		}

		// Verify domain exists and belongs to learner
		domain, err := deps.Store.GetDomainByID(params.DomainID)
		if err != nil {
			r, _ := errorResult(fmt.Sprintf("domain not found: %s", params.DomainID))
			return r, nil, nil
		}
		if domain.LearnerID != learnerID {
			r, _ := errorResult("domain not found")
			return r, nil, nil
		}

		if err := deps.Store.ArchiveDomain(params.DomainID, learnerID); err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to archive domain", err)
			return r, nil, nil
		}

		deps.Logger.Info("archive_domain: success", "domain", params.DomainID, "name", domain.Name, "learner", learnerID)
		r, _ := jsonResult(map[string]interface{}{
			"archived":    true,
			"domain_id":   domain.ID,
			"domain_name": domain.Name,
			"message":     fmt.Sprintf("Domain '%s' archived. Progress preserved. Use unarchive_domain to restore it.", domain.Name),
		})
		return r, nil, nil
	})
}

type UnarchiveDomainParams struct {
	DomainID string `json:"domain_id" jsonschema:"id of the domain to reactivate"`
}

func registerUnarchiveDomain(server *mcp.Server, deps *Deps) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "unarchive_domain",
		Description: "Reactivate an archived domain - it reappears in the dashboard and routing with all its progress preserved.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params UnarchiveDomainParams) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "unarchive_domain", err)
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		if params.DomainID == "" {
			r, _ := errorResult("domain_id is required")
			return r, nil, nil
		}

		domain, err := deps.Store.GetDomainByID(params.DomainID)
		if err != nil {
			r, _ := errorResult(fmt.Sprintf("domain not found: %s", params.DomainID))
			return r, nil, nil
		}
		if domain.LearnerID != learnerID {
			r, _ := errorResult("domain not found")
			return r, nil, nil
		}

		if err := deps.Store.UnarchiveDomain(params.DomainID, learnerID); err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to unarchive domain", err)
			return r, nil, nil
		}

		deps.Logger.Info("unarchive_domain: success", "domain", params.DomainID, "name", domain.Name, "learner", learnerID)
		r, _ := jsonResult(map[string]interface{}{
			"archived":    false,
			"domain_id":   domain.ID,
			"domain_name": domain.Name,
			"message":     fmt.Sprintf("Domain '%s' restored.", domain.Name),
		})
		return r, nil, nil
	})
}

type DeleteDomainParams struct {
	DomainID string `json:"domain_id" jsonschema:"id of the domain to permanently delete"`
	Confirm  bool   `json:"confirm" jsonschema:"must be true to confirm deletion"`
}

func registerDeleteDomain(server *mcp.Server, deps *Deps) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "delete_domain",
		Description: "Permanently delete a domain. The concept_states and interactions are preserved. Requires confirm=true.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, params DeleteDomainParams) (*mcp.CallToolResult, any, error) {
		learnerID, err := getLearnerID(ctx)
		if err != nil {
			logAuthFailure(deps, "delete_domain", err)
			r, _ := errorResult(err.Error())
			return r, nil, nil
		}
		if params.DomainID == "" {
			r, _ := errorResult("domain_id is required")
			return r, nil, nil
		}
		if !params.Confirm {
			r, _ := errorResult("confirm must be true to delete a domain. This action is irreversible.")
			return r, nil, nil
		}

		domain, err := deps.Store.GetDomainByID(params.DomainID)
		if err != nil {
			r, _ := errorResult(fmt.Sprintf("domain not found: %s", params.DomainID))
			return r, nil, nil
		}
		if domain.LearnerID != learnerID {
			r, _ := errorResult("domain not found")
			return r, nil, nil
		}

		if err := deps.Store.DeleteDomain(params.DomainID, learnerID); err != nil {
			r, _ := safeErrorResult(deps.Logger, "failed to delete domain", err)
			return r, nil, nil
		}

		deps.Logger.Info("delete_domain: success", "domain", params.DomainID, "name", domain.Name, "learner", learnerID)
		r, _ := jsonResult(map[string]interface{}{
			"deleted":     true,
			"domain_id":   domain.ID,
			"domain_name": domain.Name,
			"message":     fmt.Sprintf("Domain %q was permanently deleted. concept_states and interaction history were preserved.", domain.Name),
		})
		return r, nil, nil
	})
}
