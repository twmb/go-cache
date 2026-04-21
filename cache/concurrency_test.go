package cache

import (
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestSwap_CancelsInFlightGet verifies that Swap overrides a currently-loading
// entry: the goroutine waiting in Get sees the Swap value, and the underlying
// miss function's return value is discarded.
//
// Covers cache.go Swap defer block for !was.finalized() (the cancel path).
func TestSwap_CancelsInFlightGet(t *testing.T) {
	c := New[string, int]()
	missStart := make(chan struct{})
	missRelease := make(chan struct{})
	missDone := make(chan struct{})

	var getV int
	var getDone sync.WaitGroup
	getDone.Add(1)
	go func() {
		defer getDone.Done()
		v, _, _ := c.Get("k", func() (int, error) {
			close(missStart)
			<-missRelease
			close(missDone)
			return 99, nil
		})
		getV = v
	}()

	<-missStart // miss function has begun

	// Swap while the miss is still running.
	_, _, oldS := c.Swap("k", 7)
	if oldS != Miss {
		t.Fatalf("Swap old state: got %v want Miss", oldS)
	}

	// Let the miss function finish. Its value MUST be discarded.
	close(missRelease)
	<-missDone     // ensure the miss goroutine actually ran to completion
	getDone.Wait() // ensure the Get goroutine returned

	if getV != 7 {
		t.Fatalf("Get value = %d, want 7 (Swap should cancel miss)", getV)
	}

	// Verify the cache holds Swap's value, not the miss's.
	v, _, s := c.TryGet("k")
	if v != 7 || !s.IsHit() {
		t.Fatalf("TryGet after Swap: v=%d s=%v, want 7 Hit", v, s)
	}
}

// TestSwap_OverExpiredFinalizedWithStale exercises the defer branch that
// reads the prior entry's stale when the prior entry has expired but a valid
// stale is still in its window.
//
// Covers cache.go Swap defer lines for expired-with-stale (lines 451-457).
func TestSwap_OverExpiredFinalizedWithStale(t *testing.T) {
	c := New[string, int](
		MaxAge(5*time.Millisecond),
		MaxStaleAge(time.Hour),
	)
	c.Set("k", 1)
	time.Sleep(15 * time.Millisecond)
	// Entry is expired but stale is still valid.
	old, _, oldS := c.Swap("k", 2)
	if oldS != Stale {
		t.Fatalf("Swap state: got %v want Stale", oldS)
	}
	if old != 1 {
		t.Fatalf("Swap old: got %d want 1", old)
	}
}

// TestSwap_OverErroredWithStale covers the defer branch where the prior
// entry is finalized but holds an error, and a valid stale exists.
func TestSwap_OverErroredWithStale(t *testing.T) {
	c := New[string, int](MaxStaleAge(time.Hour))
	c.Set("k", 1)
	// Replace the value with an error result. We drive this via Get so the
	// cached state becomes (err, stale=1).
	c.Expire("k")
	_, _, _ = c.Get("k", func() (int, error) {
		return 0, errors.New("boom")
	})
	// Now the entry has err set and stale=1.
	old, oldErr, oldS := c.Swap("k", 2)
	if oldS != Stale {
		t.Fatalf("Swap state: got %v want Stale", oldS)
	}
	if old != 1 || oldErr != nil {
		t.Fatalf("Swap old: v=%d err=%v, want v=1 err=nil", old, oldErr)
	}
}

// TestDelete_DuringInFlightLoad verifies that deleting while a load is in
// flight: (a) the in-flight Get still returns the miss fn's result, and (b)
// a subsequent Get re-triggers the miss fn.
func TestDelete_DuringInFlightLoad(t *testing.T) {
	c := New[string, int]()
	missStart := make(chan struct{})
	missRelease := make(chan struct{})

	var getV int
	var getDone sync.WaitGroup
	getDone.Add(1)
	go func() {
		defer getDone.Done()
		v, _, _ := c.Get("k", func() (int, error) {
			close(missStart)
			<-missRelease
			return 5, nil
		})
		getV = v
	}()

	<-missStart
	c.Delete("k")
	close(missRelease)
	getDone.Wait()

	if getV != 5 {
		t.Fatalf("in-flight Get got %d, want 5", getV)
	}

	// Next Get must re-run the miss.
	var called bool
	v, _, _ := c.Get("k", func() (int, error) {
		called = true
		return 6, nil
	})
	if !called {
		t.Fatal("miss function should have run after Delete")
	}
	if v != 6 {
		t.Fatalf("post-Delete Get got %d, want 6", v)
	}
}

// TestExpire_NoOpDuringInFlightLoad verifies the documented behavior: Expire
// on an in-flight load is a no-op; the load completes with its normal TTL.
func TestExpire_NoOpDuringInFlightLoad(t *testing.T) {
	c := New[string, int](MaxAge(time.Hour))
	missStart := make(chan struct{})
	missRelease := make(chan struct{})

	var getDone sync.WaitGroup
	getDone.Add(1)
	go func() {
		defer getDone.Done()
		c.Get("k", func() (int, error) {
			close(missStart)
			<-missRelease
			return 11, nil
		})
	}()

	<-missStart
	c.Expire("k") // must be a no-op while loading
	close(missRelease)
	getDone.Wait()

	v, _, s := c.TryGet("k")
	if v != 11 || !s.IsHit() {
		t.Fatalf("after in-flight Expire: v=%d s=%v, want 11 Hit (Expire should not have affected the load)", v, s)
	}
}

// TestCollapsing_NoCaching runs many concurrent Gets against a cache with
// MaxAge(0) (no caching). The documented use case is collapsing simultaneous
// queries for the same key.
//
// The guarantee is weaker than it looks: with del0, the loading finalizes
// expired, so the *next* goroutine that acquires c.mu after the leader's
// setve sees an expired entry and triggers a fresh miss. Concurrent arrivals
// still collapse in batches, but "batch" here means "whatever queued on a
// given loading's WaitGroup before it was Done'd," which is scheduler-
// dependent.
//
// We assert: (1) miss runs at least once, (2) collapsing happens (fewer miss
// calls than workers), (3) every returned value corresponds to some miss
// invocation.
func TestCollapsing_NoCaching(t *testing.T) {
	c := New[string, int](MaxAge(0))
	const workers = 100

	var missStart sync.WaitGroup
	missStart.Add(1)
	missRelease := make(chan struct{})
	var missCalls int32

	var wg sync.WaitGroup
	wg.Add(workers)
	vals := make([]int, workers)
	for i := range workers {
		go func(i int) {
			defer wg.Done()
			v, _, _ := c.Get("k", func() (int, error) {
				n := atomic.AddInt32(&missCalls, 1)
				if n == 1 {
					missStart.Done()
					<-missRelease
				}
				return int(n), nil
			})
			vals[i] = v
		}(i)
	}

	missStart.Wait()
	time.Sleep(5 * time.Millisecond) // let as many callers as possible queue
	close(missRelease)
	wg.Wait()

	calls := atomic.LoadInt32(&missCalls)
	if calls < 1 {
		t.Fatalf("miss never called")
	}
	if calls >= workers {
		t.Fatalf("no collapsing observed: miss called %d times across %d Gets", calls, workers)
	}
	for i, v := range vals {
		if v < 1 || int32(v) > calls {
			t.Fatalf("goroutine %d got value %d, outside valid miss-call range [1,%d]", i, v, calls)
		}
	}

	// Nothing is cached. A fresh Get runs the miss again.
	var called bool
	c.Get("k", func() (int, error) {
		called = true
		return 99, nil
	})
	if !called {
		t.Fatal("after collapsed Get, cache should not have retained a value")
	}
}

// TestCleanUnderConcurrency stresses Clean racing with concurrent
// Set/Get/Delete to catch races in the clean path. Correctness here is that
// the test finishes without -race complaints or panics; we also assert that
// no value ever mysteriously appears that we never Set.
func TestCleanUnderConcurrency(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stress test in -short")
	}
	c := New[int, int](
		MaxAge(time.Millisecond),
		MaxStaleAge(time.Millisecond),
	)

	var stop atomic.Bool
	var wg sync.WaitGroup

	// Setters.
	for w := range 4 {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for !stop.Load() {
				for i := range 64 {
					c.Set(i, i*1000+w)
				}
			}
		}(w)
	}

	// Getters.
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				for i := range 64 {
					v, _, s := c.TryGet(i)
					if s.IsHit() {
						// Value is i*1000+worker; must satisfy the invariant.
						if v%1000 > 3 || v/1000 != i {
							t.Errorf("impossible value v=%d for key=%d", v, i)
							return
						}
					}
				}
			}
		}()
	}

	// Deleters.
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				for i := range 64 {
					c.Delete(i)
				}
			}
		}()
	}

	// Cleaner.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			c.Clean()
			time.Sleep(200 * time.Microsecond)
		}
	}()

	time.Sleep(500 * time.Millisecond)
	stop.Store(true)
	wg.Wait()
}

// TestSwap_OverPromotingDelete forces two rare branches simultaneously:
//
//  1. Swap's unlocked fast path observes an entry whose pointer is the
//     promotingDelete sentinel (cache.go:474-475) and falls through to the
//     locked path.
//  2. promote's inner CAS(nil, pd) fails because a concurrent Swap wrote a
//     fresh loading into e.p between promote's Load and CAS (cache.go:636).
//
// Both windows are ~nanoseconds wide; we stress with many deleters + many
// swappers + an aggressive Range driver for long enough that both branches
// land. On a fast machine, either can land within a few hundred ms, but we
// leave a generous budget for slow CI runners.
func TestSwap_OverPromotingDelete(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stress test in -short")
	}
	c := New[int, int]()
	const readKeys = 32
	// Prime stable keys into the read map.
	for i := range readKeys {
		c.Set(i, i)
	}
	c.Range(func(int, int, error) bool { return true })

	var wg sync.WaitGroup
	var stop atomic.Bool

	// Deleters churn the primed keys, repeatedly creating the nil-e.p window
	// that promote's CAS(nil, pd) targets.
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				for i := range readKeys {
					c.Delete(i)
				}
			}
		}()
	}
	// Swappers on the same primed keys exercise the unlocked fast-path CAS
	// that races with promote.
	for w := range 8 {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for !stop.Load() {
				for i := range readKeys {
					c.Swap(i, i*1000+w)
				}
			}
		}(w)
	}
	// Churners add+delete NEW keys continuously so that dirty stays populated,
	// which keeps r.incomplete=true and forces Range to call promote on every
	// iteration. Without this, the initial promote fires once and subsequent
	// Ranges skip the work.
	for w := range 4 {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			base := readKeys + w*10000
			for !stop.Load() {
				for i := range 64 {
					c.Set(base+i, i)
					c.Delete(base + i)
				}
			}
		}(w)
	}
	// Range callers hammer promote.
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				c.Range(func(int, int, error) bool { return true })
			}
		}()
	}

	time.Sleep(2 * time.Second)
	stop.Store(true)
	wg.Wait()
}

// TestTryCAS_RetryOnRace exercises the tryCAS CAS-loop retry path by running
// many concurrent CompareAndSwap calls for the same key. When one thread's CAS
// succeeds, others retry (loading the new value) and may observe a value that
// no longer matches `old`, exiting the loop.
func TestTryCAS_RetryOnRace(t *testing.T) {
	c := New[string, int]()
	c.Set("k", 0)

	const workers = 32
	const iters = 1000

	var wg sync.WaitGroup
	var successes int32
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for range iters {
				for {
					v, _, s := c.TryGet("k")
					if !s.IsHit() {
						break
					}
					if c.CompareAndSwap("k", v, v+1) {
						atomic.AddInt32(&successes, 1)
						break
					}
					runtime.Gosched()
				}
			}
		}()
	}
	wg.Wait()

	v, _, _ := c.TryGet("k")
	if int32(v) != successes {
		t.Fatalf("value %d != successful CAS count %d", v, successes)
	}
	if successes != workers*iters {
		t.Fatalf("successes %d != expected %d (every worker should eventually succeed its increments)", successes, workers*iters)
	}
}

// TestCompareAndDelete_RaceOnDelete stresses the CompareAndDelete path with
// concurrent deletes/sets to exercise both the retry loop inside tryCAS and
// the value-mismatch path after a concurrent mutation.
func TestCompareAndDelete_RaceOnDelete(t *testing.T) {
	c := New[int, int]()
	const workers = 16
	var wg sync.WaitGroup
	wg.Add(workers * 2)

	var setDone atomic.Int64
	for w := range workers {
		go func(w int) {
			defer wg.Done()
			for i := range 1000 {
				c.Set(i%8, w)
				setDone.Add(1)
			}
		}(w)
	}
	for range workers {
		go func() {
			defer wg.Done()
			for i := range 1000 {
				v, _, s := c.TryGet(i % 8)
				if s.IsHit() {
					c.CompareAndDelete(i%8, v)
				}
			}
		}()
	}
	wg.Wait()
	// No assertion on final state; the race-detector + lack of panic is the
	// signal.
}

// TestSetve_RacesWithSwap runs many iterations where a slow miss function
// races against a Swap on the same key. The purpose is to exercise the
// double-check-under-mutex branch of loading.setve (miss grabs l.mu after
// Swap has already finalized l under the same mutex in its defer block).
//
// The branch is race-dependent; we run many iterations to land it. The test
// is correctness-only (value observed by all Gets is consistent).
func TestSetve_RacesWithSwap(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping race-iteration test in -short")
	}
	for iter := range 200 {
		c := New[string, int]()
		missRelease := make(chan struct{})
		var wg sync.WaitGroup
		var gotV int32
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, _, _ := c.Get("k", func() (int, error) {
				<-missRelease
				return 1, nil
			})
			atomic.StoreInt32(&gotV, int32(v))
		}()
		// Allow the miss goroutine to enter the function, then release it
		// close in time with the Swap.
		runtime.Gosched()
		close(missRelease)
		old, _, _ := c.Swap("k", 2)
		_ = old
		wg.Wait()
		v := atomic.LoadInt32(&gotV)
		if v != 1 && v != 2 {
			t.Fatalf("iter %d: got %d, want 1 or 2", iter, v)
		}
	}
}

// TestPromote_ConcurrentDeleteOfNilEntry forces promote to observe an entry
// whose pointer is nil but becomes non-nil between the Load and the
// CompareAndSwap(nil, pd) inside promote's retry loop. Covers the reload
// statement inside promote.
//
// This exercises cache.go:promote retry loop. Deterministic coverage of the
// exact reload line requires a goroutine interleaving that we cannot force
// from the outside; we stress via concurrent Delete/Set/Range to try to hit
// it.
func TestPromote_ConcurrentDeleteOfNilEntry(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stress test in -short")
	}
	c := New[int, int]()
	for i := range 128 {
		c.Set(i, i)
	}

	var wg sync.WaitGroup
	var stop atomic.Bool
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			for i := range 128 {
				c.Delete(i)
				c.Set(i, i)
			}
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			c.Range(func(int, int, error) bool { return true })
		}
	}()
	time.Sleep(100 * time.Millisecond)
	stop.Store(true)
	wg.Wait()
}

// TestClean_NoopWhenStalesArePermanent covers the MaxStaleAge<0 early return
// in Clean (the "stales kept forever" opt-in).
func TestClean_NoopWhenStalesArePermanent(t *testing.T) {
	c := New[string, int](MaxAge(time.Nanosecond), MaxStaleAge(-1))
	c.Set("k", 1)
	time.Sleep(time.Millisecond) // main entry is now expired, stale is forever
	c.Clean()                    // must be a no-op
	// The stale is still queryable.
	_, _, s := c.TryGet("k")
	if s != Stale {
		t.Fatalf("after Clean with MaxStaleAge=-1: state=%v want Stale", s)
	}
}

// TestGet_UnlockedMissThenLockedHit targets Get's locked-path Hit (cache.go
// lines 233-236): unlocked e.get returned Miss, but by the time we acquire
// c.mu and re-check, a concurrent Swap has published a fresh value for the
// same key in the read map.
//
// Requires the entry to live in the read map (not dirty). We force that by
// priming + Range (promote), then Expire to make the unlocked fast-path Miss,
// then race a Swap against a Get. Hitting the exact timing window requires
// multiple iterations.
func TestGet_UnlockedMissThenLockedHit(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping race-iteration test in -short")
	}
	for iter := range 5000 {
		c := New[int, int](MaxAge(time.Hour))
		c.Set(1, 1)
		c.Range(func(int, int, error) bool { return true })
		c.Expire(1)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			c.Swap(1, iter+100)
		}()
		go func() {
			defer wg.Done()
			v, _, s := c.Get(1, func() (int, error) { return -1, nil })
			if s.IsHit() && v != iter+100 {
				t.Errorf("iter %d: Hit but v=%d, want %d", iter, v, iter+100)
			}
		}()
		wg.Wait()
	}
}

// TestCompareAndSwap_LockPromotedEntry targets the CompareAndSwap locked
// branch where the entry was only observable via dirty on fast path but has
// since been promoted into the read map (cache.go lines 532-534). Race window.
func TestCompareAndSwap_LockPromotedEntry(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping race-iteration test in -short")
	}
	for iter := range 5000 {
		c := New[int, int]()
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			c.Set(iter, iter)
			c.Range(func(int, int, error) bool { return true })
		}()
		go func() {
			defer wg.Done()
			c.CompareAndSwap(iter, iter, iter+1)
		}()
		wg.Wait()
	}
}

// TestPromote_CASRetryOnConcurrentWrite targets promote's reload-after-failed-
// CAS line (cache.go line 636). The condition: promote holds c.mu, sees
// e.p=nil, tries CAS(nil, c.pd). The CAS fails because a concurrent goroutine
// (Get under c.mu could not run; Swap via unlocked fast path could not, since
// it requires e in read map — wait, it IS in read map here — so this window
// opens when Swap's unlocked CAS loop is active for an entry that was deleted
// during promote's iteration).
//
// We exercise by having many concurrent Swap/Delete/Range cycles so promote
// encounters entries mid-flux. Race-dependent; stress only.
func TestPromote_CASRetryOnConcurrentWrite(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stress test in -short")
	}
	c := New[int, int]()
	for i := range 64 {
		c.Set(i, i)
	}
	c.Range(func(int, int, error) bool { return true })

	var stop atomic.Bool
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				for i := range 64 {
					c.Delete(i)
					c.Swap(i, i*2)
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			c.Range(func(int, int, error) bool { return true })
		}
	}()
	time.Sleep(100 * time.Millisecond)
	stop.Store(true)
	wg.Wait()
}

// TestMemoryBounded churns a fixed key set in and out of the cache. We don't
// assert exact bytes; we assert that after N rounds of (Set, Get, Delete,
// Range-triggered promote), the resident entry count is bounded by the number
// of live keys. This would catch a regression where dirty-map bookkeeping
// leaked entries.
func TestMemoryBounded(t *testing.T) {
	c := New[int, int]()
	const liveKeys = 32
	const rounds = 2000

	for r := range rounds {
		for i := range liveKeys {
			c.Set(r*liveKeys+i, i)
		}
		// Delete the ones we just wrote except for `liveKeys` canonical keys.
		for i := range liveKeys {
			c.Delete(r*liveKeys + i)
		}
		for i := range liveKeys {
			c.Set(i, i)
		}
		// Trigger promote.
		c.Range(func(int, int, error) bool { return true })
	}

	count := 0
	c.Range(func(int, int, error) bool { count++; return true })
	if count > liveKeys {
		t.Fatalf("after %d rounds: %d live entries, want <= %d", rounds, count, liveKeys)
	}
}
