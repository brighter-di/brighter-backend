package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/brighter-di/backend/internal/store"
	"github.com/brighter-di/backend/internal/testkit"
)

func TestPublicationQuotaIsAtomic(t *testing.T) {
	db := testkit.Store(t)
	ctx := context.Background()
	var wait sync.WaitGroup
	results := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wait.Go(func() {
			p := store.PublishedStrategy{ID: fmt.Sprintf("p-%d", i), Author: owner, RulesKey: fmt.Sprintf("rules-%d", i), Document: json.RawMessage(`{"strategy":{}}`)}
			results <- db.PublishStrategy(ctx, &p)
		})
	}
	wait.Wait()
	close(results)
	success, limited := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, store.ErrPublicationLimit) {
			limited++
		} else {
			t.Fatal(err)
		}
	}
	count, err := db.PublicationCount(ctx, owner)
	if err != nil || count != 10 || success != 10 || limited != 10 {
		t.Fatalf("count=%d successful=%d limited=%d err=%v", count, success, limited, err)
	}
	p := store.PublishedStrategy{ID: "duplicate", Author: "0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", RulesKey: "rules-1", Document: json.RawMessage(`{}`)}
	rows, err := db.PublishedStrategies(ctx)
	if err != nil || len(rows) != 10 {
		t.Fatal(rows, err)
	}
	p.RulesKey = rows[0].RulesKey
	if !errors.Is(db.PublishStrategy(ctx, &p), store.ErrDuplicateStrategy) {
		t.Fatal("cross-author duplicate accepted")
	}
}
func TestAuthChallengeConsumptionIsAtomic(t *testing.T) {
	db := testkit.Store(t)
	ctx := context.Background()
	challenge := store.AuthChallenge{ID: "nonce", Author: owner, Origin: "https://studio.brighter.fi", Message: "signed message", ExpiresAt: time.Now().Add(time.Minute)}
	if err := db.CreateAuthChallenge(ctx, challenge); err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wait.Go(func() {
			results <- db.CompleteAuthChallenge(ctx, challenge.ID, store.AuthSession{TokenHash: fmt.Sprintf("hash-%d", i), Author: owner, Origin: challenge.Origin, ExpiresAt: time.Now().Add(time.Hour)})
		})
	}
	wait.Wait()
	close(results)
	success, missing := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, store.ErrNotFound) {
			missing++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || missing != 1 {
		t.Fatal(success, missing)
	}
}

func TestPublicationUniquenessIsAtomic(t *testing.T) {
	db := testkit.Store(t)
	ctx := context.Background()
	var wait sync.WaitGroup
	results := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wait.Go(func() {
			p := store.PublishedStrategy{ID: fmt.Sprintf("duplicate-%d", i), Author: fmt.Sprintf("0x%040x", i+1), RulesKey: "identical-rules", Document: json.RawMessage(`{}`)}
			results <- db.PublishStrategy(ctx, &p)
		})
	}
	wait.Wait()
	close(results)
	success, duplicate := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, store.ErrDuplicateStrategy) {
			duplicate++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || duplicate != 11 {
		t.Fatal("concurrent duplicates accepted", success, duplicate)
	}
}
