package db

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"tutor-mcp/models"
	"tutor-mcp/store"
)

// TestForUpdateNoLostUpdate runs N concurrent read-modify-write cycles on the
// same concept_state inside WithTx. With FOR UPDATE (Postgres) / BEGIN IMMEDIATE
// (SQLite) the final reps count must equal N — no lost updates.
func TestForUpdateNoLostUpdate(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	cs := models.NewConceptState("L1", "C-lock")
	if err := s.UpsertConceptState(ctx, cs); err != nil {
		t.Fatal(err)
	}
	const N = 20
	var wg sync.WaitGroup
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- s.WithTx(ctx, func(tx store.Store) error {
				got, err := tx.GetConceptStateForUpdate(ctx, "L1", "C-lock")
				if err != nil {
					return err
				}
				got.Reps++
				return tx.UpsertConceptState(ctx, got)
			})
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatalf("tx error: %v", e)
		}
	}
	final, err := s.GetConceptState(ctx, "L1", "C-lock")
	if err != nil {
		t.Fatal(err)
	}
	if final.Reps != N {
		t.Fatalf("lost updates: reps=%d, want %d", final.Reps, N)
	}
}

// TestFirstTouchNoLostUpdate is the first-touch variant of the lost-update
// guard: unlike TestForUpdateNoLostUpdate it does NOT pre-create the row. N
// concurrent transactions each materialize-and-lock the (learner, concept)
// state via GetOrCreateConceptStateForUpdate, bump reps, and upsert. On
// Postgres a bare SELECT ... FOR UPDATE locks nothing when the row is absent,
// so without materialize-then-lock two concurrent first interactions both
// bootstrap a fresh state and the second upsert overwrites the first — a lost
// update of the entire algorithmic state. The final reps count must equal N.
func TestFirstTouchNoLostUpdate(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	const N = 50
	var wg sync.WaitGroup
	errs := make(chan error, N)
	start := make(chan struct{})
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release all goroutines together to maximise overlap
			errs <- s.WithTx(ctx, func(tx store.Store) error {
				got, err := tx.GetOrCreateConceptStateForUpdate(ctx, "L1", "C-fresh")
				if err != nil {
					return err
				}
				// Widen the read-modify-write window so every goroutine is past
				// the lock acquisition before any commits. A correct
				// materialize-then-lock implementation serializes here on the
				// row lock; a bare FOR UPDATE on the absent row does not, and
				// the lost update becomes deterministic rather than flaky.
				time.Sleep(15 * time.Millisecond)
				got.Reps++
				return tx.UpsertConceptState(ctx, got)
			})
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatalf("tx error: %v", e)
		}
	}
	final, err := s.GetConceptState(ctx, "L1", "C-fresh")
	if err != nil {
		t.Fatal(err)
	}
	if final.Reps != N {
		t.Fatalf("first-touch lost updates: reps=%d, want %d", final.Reps, N)
	}
}

// TestSkipLockedExactlyOnce enqueues M pending webhook messages and has N
// workers concurrently dequeue+mark-sent inside WithTx. Each message must be
// claimed exactly once (no double dispatch, none lost).
func TestSkipLockedExactlyOnce(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	const M = 30
	for i := 0; i < M; i++ {
		if _, err := s.EnqueueWebhookMessage(ctx, "L1", "olm", fmt.Sprintf("msg-%d", i), now, now.Add(time.Hour), 0); err != nil {
			t.Fatal(err)
		}
	}
	const N = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	claimed := map[int64]int{}
	for w := 0; w < N; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				var gotID int64
				err := s.WithTx(ctx, func(tx store.Store) error {
					item, err := tx.DequeueNextPending(ctx, "L1", "olm", now, time.Hour)
					if err != nil || item == nil {
						return err
					}
					gotID = item.ID
					return tx.MarkWebhookSent(ctx, item.ID, "L1", now)
				})
				if err != nil {
					t.Error(err)
					return
				}
				if gotID == 0 {
					return // queue drained
				}
				mu.Lock()
				claimed[gotID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(claimed) != M {
		t.Fatalf("claimed %d distinct messages, want %d", len(claimed), M)
	}
	for id, c := range claimed {
		if c != 1 {
			t.Fatalf("message %d claimed %d times (want exactly once)", id, c)
		}
	}
}
