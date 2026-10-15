package recordstore

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestRunMaintenanceRoundOneReplicaSkips asserts two concurrent rounds
// against one database never both run: one takes the advisory lock and
// runs, the other skips.
func TestRunMaintenanceRoundOneReplicaSkips(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	cfg := MaintenanceConfig{
		RunRetention:      365 * 24 * time.Hour,
		LogRetention:      90 * 24 * time.Hour,
		ArtifactRetention: 30 * 24 * time.Hour,
		FinalizeWait:      time.Minute,
	}

	// A held maintenance lock, taken on its own connection before the
	// two rounds start, forces both calls to contend for the same lock
	// instead of racing to be first.
	holder, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire holder connection: %v", err)
	}
	defer holder.Release()
	var held bool
	if err := holder.QueryRow(context.Background(), "SELECT pg_try_advisory_lock($1)", maintenanceLockKey).Scan(&held); err != nil {
		t.Fatalf("hold maintenance lock: %v", err)
	}
	if !held {
		t.Fatal("could not take the maintenance lock to hold it")
	}

	const runners = 2
	ran := make([]bool, runners)
	errs := make([]error, runners)
	var wg sync.WaitGroup
	for i := 0; i < runners; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ran[i], errs[i] = RunMaintenanceRound(context.Background(), pool, cfg)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("runner %d: %v", i, err)
		}
	}
	if ran[0] || ran[1] {
		t.Fatalf("ran = %v, want both false while the lock is held", ran)
	}

	if _, err := holder.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", maintenanceLockKey); err != nil {
		t.Fatalf("release held lock: %v", err)
	}
	holder.Release()

	// With the lock free, exactly one of two concurrent rounds takes it.
	for i := 0; i < runners; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ran[i], errs[i] = RunMaintenanceRound(context.Background(), pool, cfg)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("runner %d: %v", i, err)
		}
	}
	if ran[0] == ran[1] {
		t.Fatalf("ran = %v, want exactly one true", ran)
	}
}

// TestRunMaintenanceRoundExcludesASecondRoundWhileTheFirstHoldsTheLock
// holds the winning round inside the maintenance lock with a barrier,
// then attempts a second round while the first is still blocked there,
// so the test proves mutual exclusion instead of inferring it from
// timing.
func TestRunMaintenanceRoundExcludesASecondRoundWhileTheFirstHoldsTheLock(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	baseCfg := MaintenanceConfig{
		RunRetention:      365 * 24 * time.Hour,
		LogRetention:      90 * 24 * time.Hour,
		ArtifactRetention: 30 * 24 * time.Hour,
		FinalizeWait:      time.Minute,
	}

	enteredBarrier := make(chan struct{})
	releaseBarrier := make(chan struct{})
	winnerCfg := WithTestBarrier(baseCfg, func() {
		close(enteredBarrier)
		<-releaseBarrier
	})

	winnerDone := make(chan error, 1)
	go func() {
		_, err := RunMaintenanceRound(context.Background(), pool, winnerCfg)
		winnerDone <- err
	}()

	select {
	case <-enteredBarrier:
	case <-time.After(5 * time.Second):
		t.Fatal("the winning round never reached the barrier")
	}

	ran, err := RunMaintenanceRound(context.Background(), pool, baseCfg)
	if err != nil {
		t.Fatalf("second round: %v", err)
	}
	if ran {
		t.Fatal("a second round ran while the first held the maintenance lock")
	}

	close(releaseBarrier)
	select {
	case err := <-winnerDone:
		if err != nil {
			t.Fatalf("winning round: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the winning round never returned after the barrier released it")
	}
}
