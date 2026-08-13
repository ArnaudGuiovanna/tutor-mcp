// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"tutor-mcp/db"
	"tutor-mcp/models"
)

type options struct {
	action, tenantID, slug, name, region, planID, status string
	flagKey, hostname, verificationToken, entitlements   string
	periodStart, periodEnd, graceUntil                   string
	enabled                                              bool
	reason, requestID                                    string
}

func main() {
	var opts options
	flag.StringVar(&opts.action, "action", "", "provision, status, flag, domain-begin, domain-complete, plan-upsert, or plan-assign")
	flag.StringVar(&opts.tenantID, "tenant", "", "exact tenant ID")
	flag.StringVar(&opts.slug, "slug", "", "tenant slug")
	flag.StringVar(&opts.name, "name", "", "tenant or plan name")
	flag.StringVar(&opts.region, "region", "", "tenant region/cell")
	flag.StringVar(&opts.planID, "plan", "", "plan ID")
	flag.StringVar(&opts.status, "status", "", "tenant, plan, or subscription status")
	flag.StringVar(&opts.flagKey, "flag", "", "feature flag key")
	flag.BoolVar(&opts.enabled, "enabled", false, "feature flag value")
	flag.StringVar(&opts.hostname, "hostname", "", "custom tenant hostname")
	flag.StringVar(&opts.verificationToken, "verification-token", "", "one-time custom-domain verification token")
	flag.StringVar(&opts.entitlements, "entitlements", "", "plan entitlement JSON object")
	flag.StringVar(&opts.periodStart, "period-start", "", "subscription period start RFC3339")
	flag.StringVar(&opts.periodEnd, "period-end", "", "subscription period end RFC3339")
	flag.StringVar(&opts.graceUntil, "grace-until", "", "optional grace deadline RFC3339")
	flag.StringVar(&opts.reason, "reason", "", "operator-approved reason")
	flag.StringVar(&opts.requestID, "request-id", "", "change/ticket identifier")
	flag.Parse()
	if err := opts.validate(); err != nil {
		fatal(err.Error())
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		fatal("DATABASE_URL is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	database, err := db.OpenPostgres(dsn, 2)
	if err != nil {
		fatal(err.Error())
	}
	defer database.Close()
	if err := db.VerifyPostgresSchemaCurrent(ctx, database); err != nil {
		fatal(err.Error())
	}
	store := db.NewStoreWithDialect(database, db.DialectPostgres)
	actor := models.ControlPlanePrincipal{ActorID: "control-plane-operator", Roles: []string{models.RolePlatformAdmin},
		Reason: opts.reason, RequestID: opts.requestID}
	var output any
	switch opts.action {
	case "provision":
		output, err = store.ProvisionTenant(ctx, actor, opts.slug, opts.name, opts.region, opts.planID)
	case "status":
		err = store.SetTenantStatus(ctx, actor, opts.tenantID, opts.status)
		output = map[string]any{"tenant_id": opts.tenantID, "status": opts.status}
	case "flag":
		err = store.SetTenantFeatureFlag(ctx, actor, opts.tenantID, opts.flagKey, opts.enabled)
		output = map[string]any{"tenant_id": opts.tenantID, "flag": opts.flagKey, "enabled": opts.enabled}
	case "domain-begin":
		var token string
		token, err = store.BeginTenantDomainVerification(ctx, actor, opts.tenantID, opts.hostname)
		output = map[string]any{"tenant_id": opts.tenantID, "hostname": opts.hostname, "verification_token": token}
	case "domain-complete":
		err = store.CompleteTenantDomainVerification(ctx, actor, opts.hostname, opts.verificationToken)
		output = map[string]any{"hostname": opts.hostname, "verified": true}
	case "plan-upsert":
		output, err = store.UpsertPlan(ctx, actor, models.Plan{ID: opts.planID, Name: opts.name,
			Status: opts.status, EntitlementsJSON: opts.entitlements})
	case "plan-assign":
		var start, end time.Time
		var grace *time.Time
		start, err = time.Parse(time.RFC3339, opts.periodStart)
		if err == nil {
			end, err = time.Parse(time.RFC3339, opts.periodEnd)
		}
		if err == nil && opts.graceUntil != "" {
			var parsed time.Time
			parsed, err = time.Parse(time.RFC3339, opts.graceUntil)
			grace = &parsed
		}
		if err == nil {
			err = store.AssignTenantPlan(ctx, actor, opts.tenantID, opts.planID, opts.status, start, end, grace)
		}
		output = map[string]any{"tenant_id": opts.tenantID, "plan_id": opts.planID, "status": opts.status}
	}
	if err != nil {
		fatal(err.Error())
	}
	writeJSON(map[string]any{"action": opts.action, "request_id": opts.requestID, "result": output})
}

func (opts options) validate() error {
	if strings.TrimSpace(opts.reason) == "" || strings.TrimSpace(opts.requestID) == "" {
		return fmt.Errorf("-reason and -request-id are required")
	}
	require := func(values ...string) bool {
		for _, value := range values {
			if strings.TrimSpace(value) == "" {
				return false
			}
		}
		return true
	}
	switch opts.action {
	case "provision":
		if !require(opts.slug, opts.name, opts.region, opts.planID) {
			return fmt.Errorf("provision requires -slug, -name, -region and -plan")
		}
	case "status":
		if !require(opts.tenantID, opts.status) {
			return fmt.Errorf("status requires -tenant and -status")
		}
	case "flag":
		if !require(opts.tenantID, opts.flagKey) {
			return fmt.Errorf("flag requires -tenant and -flag")
		}
	case "domain-begin":
		if !require(opts.tenantID, opts.hostname) {
			return fmt.Errorf("domain-begin requires -tenant and -hostname")
		}
	case "domain-complete":
		if !require(opts.hostname, opts.verificationToken) {
			return fmt.Errorf("domain-complete requires -hostname and -verification-token")
		}
	case "plan-upsert":
		if !require(opts.planID, opts.name, opts.status, opts.entitlements) || !json.Valid([]byte(opts.entitlements)) {
			return fmt.Errorf("plan-upsert requires -plan, -name, -status and valid -entitlements JSON")
		}
	case "plan-assign":
		if !require(opts.tenantID, opts.planID, opts.status, opts.periodStart, opts.periodEnd) {
			return fmt.Errorf("plan-assign requires tenant, plan, status and period bounds")
		}
	default:
		return fmt.Errorf("unsupported -action %q", opts.action)
	}
	return nil
}

func writeJSON(value any) {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(true)
	if err := encoder.Encode(value); err != nil {
		fatal(err.Error())
	}
}

func fatal(message string) { _, _ = fmt.Fprintln(os.Stderr, message); os.Exit(1) }
