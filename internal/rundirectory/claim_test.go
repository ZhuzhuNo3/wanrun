package rundirectory

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestStaleRunClaimIsExclusive(t *testing.T) {
	owner, authorityPath := newTestOwner(t)
	id := makeStaleRun(t, owner)
	authority, err := owner.openAuthority()
	if err != nil {
		t.Fatalf("openAuthority: %v", err)
	}
	defer func() {
		if authority != nil {
			_ = authority.Close()
		}
	}()

	first, err := owner.claimRun(authority.root, id)
	if err != nil {
		t.Fatalf("first claimRun: %v", err)
	}
	if first.kind != claimStale || first.lease == nil {
		t.Fatalf("first claim = {kind:%v lease:%v}, want stale claim with lease", first.kind, first.lease)
	}

	second, err := owner.claimRun(authority.root, id)
	if err != nil {
		t.Fatalf("second claimRun: %v", err)
	}
	if second.kind != claimBusy || second.lease != nil {
		t.Fatalf("second claim = {kind:%v lease:%v}, want busy claim without lease", second.kind, second.lease)
	}
	if err := first.lease.close(); err != nil {
		t.Fatalf("close first claim lease: %v", err)
	}

	third, err := owner.claimRun(authority.root, id)
	if err != nil {
		t.Fatalf("claimRun after release: %v", err)
	}
	if third.kind != claimStale || third.lease == nil {
		t.Fatalf("claim after release = {kind:%v lease:%v}, want stale claim with lease", third.kind, third.lease)
	}
	if err := third.lease.close(); err != nil {
		t.Fatalf("close reacquired claim lease: %v", err)
	}
	if err := authority.Close(); err != nil {
		t.Fatalf("close authority: %v", err)
	}
	authority = nil

	if err := owner.RecoverStale(context.Background(), func(*StaleRun) error { return nil }); err != nil {
		t.Fatalf("RecoverStale cleanup: %v", err)
	}
	assertPathAbsent(t, filepath.Join(authorityPath, id.String()))
}

func TestRecoveryPassRestartsForBusyClaim(t *testing.T) {
	owner, authorityPath := newTestOwner(t)
	id := makeStaleRun(t, owner)
	authority, err := owner.openAuthority()
	if err != nil {
		t.Fatalf("openAuthority: %v", err)
	}
	claim, err := owner.claimRun(authority.root, id)
	if err != nil {
		t.Fatalf("claimRun: %v", err)
	}
	if claim.kind != claimStale || claim.lease == nil {
		t.Fatalf("claim = {kind:%v lease:%v}, want stale claim with lease", claim.kind, claim.lease)
	}

	called := false
	restart, err := owner.recoverPass(context.Background(), func(*StaleRun) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("recoverPass: %v", err)
	}
	if !restart {
		t.Fatal("recoverPass restart = false, want true for busy claim")
	}
	if called {
		t.Fatal("recoverPass called callback for busy claim")
	}
	assertDirectoryExists(t, filepath.Join(authorityPath, id.String()))

	if err := claim.lease.close(); err != nil {
		t.Fatalf("close held claim: %v", err)
	}
	if err := authority.Close(); err != nil {
		t.Fatalf("close authority: %v", err)
	}
	if err := owner.RecoverStale(context.Background(), func(*StaleRun) error { return nil }); err != nil {
		t.Fatalf("RecoverStale cleanup: %v", err)
	}
}

func TestRecoveryRetryStopsOnContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := waitForRetry(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("waitForRetry error = %v, want context.Canceled", err)
	}
}
