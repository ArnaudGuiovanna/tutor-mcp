// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// GitHub: https://github.com/ArnaudGuiovanna
// SPDX-License-Identifier: MIT

package engine

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"tutor-mcp/models"
)

// processWebhookTargets keyset-pages a narrow scheduler read model into a
// bounded worker pool. Only one page plus the small channel buffer is live at
// once. Per-target deadlines propagate to DB and HTTP calls, while Stop()
// cancels production and lets workers drain or abort without accepting more
// plaintext webhook targets.
func (s *Scheduler) processWebhookTargets(
	jobName, listFailureCode, partialFailureCode string,
	work func(context.Context, models.WebhookDispatchTarget) bool,
) scheduledJobResult {
	pageSize, workerCount, workDeadline := s.learnerPoolConfig()
	ctx, cancel := context.WithCancel(context.Background())
	stopWatchDone := make(chan struct{})
	go func() {
		defer close(stopWatchDone)
		select {
		case <-s.stopCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	defer func() {
		cancel()
		<-stopWatchDone
	}()

	jobs := make(chan models.WebhookDispatchTarget, workerCount)
	var workers sync.WaitGroup
	var processed atomic.Int64
	var failed atomic.Bool
	for range workerCount {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case target, ok := <-jobs:
					if !ok {
						return
					}
					targetCtx, targetCancel := context.WithTimeout(ctx, workDeadline)
					workFailed := func() (result bool) {
						defer func() {
							if recover() != nil {
								result = true
								if s.logger != nil {
									s.logger.Error("scheduler learner work panic recovered", "job", jobName)
								}
							}
						}()
						return work(targetCtx, target)
					}()
					targetCancel()
					if workFailed {
						failed.Store(true)
					}
					processed.Add(1)
				}
			}
		}()
	}

	result := scheduledJobSucceeded()
	afterID := ""
	pages := 0
produce:
	for {
		pageCtx, pageCancel := context.WithTimeout(ctx, schedulerDBTimeout)
		var page []models.WebhookDispatchTarget
		err := s.withTenantStore(pageCtx, func(txCtx context.Context, child *Scheduler) error {
			var err error
			page, err = child.store.ListWebhookDispatchTargetsPage(txCtx, afterID, pageSize)
			return err
		})
		pageCancel()
		if err != nil {
			if ctx.Err() == nil {
				result.recordFailure(listFailureCode)
				if s.logger != nil {
					s.logger.Error("scheduler target page failed", "job", jobName, "error_type", fmt.Sprintf("%T", err))
				}
			} else {
				result.recordFailure(partialFailureCode)
			}
			break
		}
		pages++
		for _, target := range page {
			select {
			case jobs <- target:
			case <-ctx.Done():
				result.recordFailure(partialFailureCode)
				break produce
			}
		}
		if s.logger != nil {
			s.logger.Info("scheduler target page queued", "job", jobName, "page", pages, "page_size", len(page), "has_more", len(page) == pageSize)
		}
		if len(page) < pageSize {
			break
		}
		afterID = page[len(page)-1].LearnerID
	}
	close(jobs)
	workers.Wait()
	if failed.Load() {
		result.recordFailure(partialFailureCode)
	}
	if s.logger != nil {
		s.logger.Info("scheduler target run finished", "job", jobName, "pages", pages, "processed", processed.Load(), "failed", result.failed())
	}
	return result
}

func (s *Scheduler) processConsolidationLearners(
	work func(context.Context, string) bool,
) scheduledJobResult {
	pageSize, workerCount, workDeadline := s.learnerPoolConfig()
	ctx, cancel := context.WithCancel(context.Background())
	stopWatchDone := make(chan struct{})
	go func() {
		defer close(stopWatchDone)
		select {
		case <-s.stopCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	defer func() {
		cancel()
		<-stopWatchDone
	}()

	jobs := make(chan string, workerCount)
	var workers sync.WaitGroup
	var processed atomic.Int64
	var failed atomic.Bool
	for range workerCount {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case learnerID, ok := <-jobs:
					if !ok {
						return
					}
					targetCtx, targetCancel := context.WithTimeout(ctx, workDeadline)
					workFailed := func() (result bool) {
						defer func() {
							if recover() != nil {
								result = true
								if s.logger != nil {
									s.logger.Error("scheduler consolidation work panic recovered")
								}
							}
						}()
						return work(targetCtx, learnerID)
					}()
					if workFailed {
						failed.Store(true)
					}
					targetCancel()
					processed.Add(1)
				}
			}
		}()
	}

	result := scheduledJobSucceeded()
	afterID := ""
	pages := 0
produce:
	for {
		pageCtx, pageCancel := context.WithTimeout(ctx, schedulerDBTimeout)
		page, err := s.store.ListLearnerIDsForConsolidationPage(pageCtx, afterID, pageSize)
		pageCancel()
		if err != nil {
			if ctx.Err() == nil {
				result.recordFailure("consolidation_list_learners_failed")
				if s.logger != nil {
					s.logger.Error("scheduler consolidation page failed", "error_type", fmt.Sprintf("%T", err))
				}
			} else {
				result.recordFailure("consolidation_partial_failure")
			}
			break
		}
		pages++
		for _, learnerID := range page {
			select {
			case jobs <- learnerID:
			case <-ctx.Done():
				result.recordFailure("consolidation_partial_failure")
				break produce
			}
		}
		if s.logger != nil {
			s.logger.Info("scheduler consolidation page queued", "page", pages, "page_size", len(page), "has_more", len(page) == pageSize)
		}
		if len(page) < pageSize {
			break
		}
		afterID = page[len(page)-1]
	}
	close(jobs)
	workers.Wait()
	if failed.Load() {
		result.recordFailure("consolidation_partial_failure")
	}
	if s.logger != nil {
		s.logger.Info("scheduler consolidation run finished", "pages", pages, "processed", processed.Load(), "failed", result.failed())
	}
	return result
}
