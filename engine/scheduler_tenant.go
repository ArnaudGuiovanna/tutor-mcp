// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package engine

import (
	"context"
	"time"

	"tutor-mcp/models"
	"tutor-mcp/observability"
	storeport "tutor-mcp/store"
)

func (s *Scheduler) scheduledTenantJob(name string, fn func(*Scheduler) scheduledJobResult) scheduledJobFunc {
	if s.tenantStore == nil {
		return func() scheduledJobResult { return fn(s) }
	}
	return func() scheduledJobResult { return s.runAcrossTenants(name, fn) }
}

// scheduledMaintenanceTenantJob runs a data-hygiene job in one RLS transaction
// per tenant, including suspended and closed tenants. Their expired OAuth
// codes and refresh tokens are invisible to any other scope under FORCE ROW
// LEVEL SECURITY, so enumerating active tenants only would retain them
// forever. fn must be limited to purging or expiring stale state; it must not
// dispatch, notify or create business state for a tenant that is not active.
func (s *Scheduler) scheduledMaintenanceTenantJob(name string, fn func(*Scheduler) scheduledJobResult) scheduledJobFunc {
	if s.tenantStore == nil {
		return func() scheduledJobResult { return fn(s) }
	}
	list := s.tenantStore.ListWorkerTenantScopes
	if lister, ok := s.tenantStore.(maintenanceTenantLister); ok {
		list = lister.ListWorkerMaintenanceTenantScopes
	}
	return func() scheduledJobResult { return s.runAcrossListedTenants(name, list, fn) }
}

type workerTenantScopeLister func(context.Context, models.WorkerPrincipal, string, int) ([]models.TenantScope, string, error)

// scheduledIndependentTenantJob is reserved for work that crosses a network
// boundary. It enumerates and audits tenants exactly like scheduledTenantJob,
// while each durable store transition opens its own short RLS transaction.
// Consequently an HTTP timeout never pins a database transaction or row lock.
func (s *Scheduler) scheduledIndependentTenantJob(name string, fn func(*Scheduler) scheduledJobResult) scheduledJobFunc {
	if s.tenantStore == nil {
		return func() scheduledJobResult { return fn(s) }
	}
	return func() scheduledJobResult { return s.runAcrossTenantRoots(name, fn) }
}

func (s *Scheduler) runAcrossTenantRoots(name string, fn func(*Scheduler) scheduledJobResult) scheduledJobResult {
	if s.tenantStore == nil || !s.worker.Validate() {
		return scheduledJobFailed("worker_identity_invalid")
	}
	result := scheduledJobSucceeded()
	after := ""
	for {
		listCtx, listCancel := context.WithTimeout(context.Background(), schedulerDBTimeout)
		scopes, next, err := s.tenantStore.ListWorkerTenantScopes(listCtx, s.worker, after, 100)
		listCancel()
		if err != nil {
			result.recordFailure("worker_tenant_enumeration_failed")
			break
		}
		for _, scope := range scopes {
			startedAt := time.Now()
			runCtx, finishSpan := observability.StartWorkerRun(context.Background(), scope.TenantID, name)
			if err := s.tenantStore.RecordWorkerTenantRun(runCtx, s.worker, scope, name, "started", ""); err != nil {
				finishSpan("failed")
				result.recordFailure("worker_tenant_audit_failed")
				continue
			}
			child := *s
			child.tenantStore = nil
			copyScope := scope
			child.currentTenantScope = &copyScope
			tenantResult := fn(&child)
			status, errorCode := "succeeded", ""
			if tenantResult.failed() {
				status, errorCode = "failed", tenantResult.FailureCode
				result.recordFailure(errorCode)
			}
			if err := s.tenantStore.RecordWorkerTenantRun(runCtx, s.worker, scope, name, status, errorCode); err != nil {
				result.recordFailure("worker_tenant_audit_failed")
			}
			observability.RecordWorkerRun(runCtx, scope.TenantID, name, status, time.Since(startedAt))
			finishSpan(status)
		}
		if next == "" {
			break
		}
		after = next
	}
	return result
}

func (s *Scheduler) runAcrossTenants(name string, fn func(*Scheduler) scheduledJobResult) scheduledJobResult {
	if s.tenantStore == nil {
		return scheduledJobFailed("worker_identity_invalid")
	}
	return s.runAcrossListedTenants(name, s.tenantStore.ListWorkerTenantScopes, fn)
}

func (s *Scheduler) runAcrossListedTenants(name string, list workerTenantScopeLister, fn func(*Scheduler) scheduledJobResult) scheduledJobResult {
	if !s.worker.Validate() {
		return scheduledJobFailed("worker_identity_invalid")
	}
	result := scheduledJobSucceeded()
	after := ""
	for {
		listCtx, listCancel := context.WithTimeout(context.Background(), schedulerDBTimeout)
		scopes, next, err := list(listCtx, s.worker, after, 100)
		listCancel()
		if err != nil {
			result.recordFailure("worker_tenant_enumeration_failed")
			break
		}
		for _, scope := range scopes {
			startedAt := time.Now()
			runCtx, finishSpan := observability.StartWorkerRun(context.Background(), scope.TenantID, name)
			if err := s.tenantStore.RecordWorkerTenantRun(runCtx, s.worker, scope, name, "started", ""); err != nil {
				finishSpan("failed")
				result.recordFailure("worker_tenant_audit_failed")
				continue
			}
			tenantResult := scheduledJobSucceeded()
			err := s.tenantStore.WithTenantTx(runCtx, scope, func(txCtx context.Context, scoped storeport.Store) error {
				child := *s
				child.store = scoped
				child.tenantStore = nil
				child.tenantDBPhase = true
				tenantResult = fn(&child)
				// Business methods persist their own durable retry state. A
				// partial job result must commit those transitions.
				return nil
			})
			status, errorCode := "succeeded", ""
			if err != nil {
				status, errorCode = "failed", "worker_tenant_transaction_failed"
				result.recordFailure(errorCode)
			} else if tenantResult.failed() {
				status, errorCode = "failed", tenantResult.FailureCode
				result.recordFailure(errorCode)
			}
			if auditErr := s.tenantStore.RecordWorkerTenantRun(runCtx, s.worker, scope, name, status, errorCode); auditErr != nil {
				result.recordFailure("worker_tenant_audit_failed")
			}
			observability.RecordWorkerRun(runCtx, scope.TenantID, name, status, time.Since(startedAt))
			finishSpan(status)
		}
		if next == "" {
			break
		}
		after = next
	}
	return result
}
