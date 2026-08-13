// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package db

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"tutor-mcp/models"
	storeport "tutor-mcp/store"
)

func TestLoginChallengeConcurrentSingleActiveAndReplaySafe(t *testing.T) {
	store := setupTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	const contenders = 12
	start := make(chan struct{})
	winners := make(chan string, contenders)
	errs := make(chan error, contenders)
	var wg sync.WaitGroup
	for i := range contenders {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			challenge := loginChallengeFixture(fmt.Sprintf("sha256:challenge-%d", index), now)
			created, err := store.CreateLoginChallenge(ctx, challenge)
			if err != nil {
				errs <- err
				return
			}
			if created {
				winners <- challenge.TokenHash
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(winners)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("create challenge: %v", err)
		}
	}
	var winnerList []string
	for winner := range winners {
		winnerList = append(winnerList, winner)
	}
	if len(winnerList) != 1 {
		t.Fatalf("active challenge winners=%v, want one", winnerList)
	}
	winner := winnerList[0]
	if _, err := store.GetLoginChallenge(ctx, winner, now); err != nil {
		t.Fatalf("get live challenge: %v", err)
	}
	trustedUntil := now.Add(30 * 24 * time.Hour)
	consumed, err := store.ConsumeLoginChallenge(ctx, winner, now, trustedUntil)
	if err != nil || consumed.LearnerID != "L1" {
		t.Fatalf("consume challenge=%+v err=%v", consumed, err)
	}
	if _, err := store.ConsumeLoginChallenge(ctx, winner, now, trustedUntil); !errors.Is(err, storeport.ErrInvalidLoginChallenge) {
		t.Fatalf("replay error=%v, want invalid challenge", err)
	}
	if trusted, err := store.IsTrustedLoginDevice(ctx, "L1", winner, now); err != nil || !trusted {
		t.Fatalf("trusted device=%v err=%v", trusted, err)
	}
	if trusted, err := store.IsTrustedLoginDevice(ctx, "other", winner, now); err != nil || trusted {
		t.Fatalf("cross-account trusted device=%v err=%v", trusted, err)
	}
	if trusted, err := store.IsTrustedLoginDevice(ctx, "L1", winner, trustedUntil); err != nil || trusted {
		t.Fatalf("expired trusted device=%v err=%v", trusted, err)
	}

	created, err := store.CreateLoginChallenge(ctx, loginChallengeFixture("sha256:next", now.Add(time.Minute)))
	if err != nil || !created {
		t.Fatalf("new challenge after consumption created=%v err=%v", created, err)
	}
}

func loginChallengeFixture(tokenHash string, now time.Time) *models.LoginChallenge {
	return &models.LoginChallenge{
		TokenHash: tokenHash, LearnerID: "L1", ClientID: "client-A",
		RedirectURI: "https://client.example/callback", Resource: "https://resource.example/mcp",
		Scope: models.OAuthScopeLearner, CodeChallenge: "challenge", CodeChallengeMethod: "S256",
		CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	}
}
