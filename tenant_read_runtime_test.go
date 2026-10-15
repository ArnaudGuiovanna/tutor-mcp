package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func TestProfileStoreConfiguresIndependentReaders(t *testing.T) {
	for _, profile := range []string{"local", "hobby"} {
		t.Run(profile, func(t *testing.T) {
			publicURL := ""
			if profile == "hobby" {
				publicURL = "https://example.test"
			}
			s, _, err := openProfileStore(t.Context(), profile, filepath.Join(t.TempDir(), "private"), publicURL)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			var learner string
			if profile == "local" {
				learner, err = s.EnsureLocalIdentity(t.Context())
			} else {
				var invite string
				invite, err = s.CreateHobbyLink(t.Context(), "invite", "reader")
				if err == nil {
					learner, err = s.AcceptHobbyInvite(t.Context(), invite, "reader", "$2test")
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			p, err := s.GetPrincipalForLearner(t.Context(), learner, []string{models.OAuthScopeLearner})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			entered := make(chan struct{}, 2)
			release := make(chan struct{})
			done := make(chan error, 2)
			for range 2 {
				go func() {
					done <- s.WithTenantReadTx(ctx, p.TenantScope(), func(ctx context.Context, _ storeport.Store) error {
						if _, err := s.GetLearnerByID(ctx, learner); err != nil {
							return err
						}
						entered <- struct{}{}
						select {
						case <-release:
							return nil
						case <-ctx.Done():
							return ctx.Err()
						}
					})
				}()
			}
			for range 2 {
				select {
				case <-entered:
				case <-ctx.Done():
					close(release)
					t.Fatal("profile failed to configure concurrent readers", ctx.Err())
				}
			}
			err = s.WithTenantTx(ctx, p.TenantScope(), func(ctx context.Context, _ storeport.Store) error {
				return s.UpdateLearnerProfile(ctx, learner, "{}", nil)
			})
			close(release)
			if err != nil {
				t.Fatal("profile writer stalled behind readers", err)
			}
			for range 2 {
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
