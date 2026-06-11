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
// expired, so the next goroutine that re-checks the entry after the leader's
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

// TestGet_StaleRefreshCASRace pounds on the Get slow-path CAS-retry that
// replaces a finalized-expired-with-stale loading with a fresh loading
// whose stale is a snapshot of the old value. Many concurrent Gets on the
// same key force CAS losses and thus exercise the retry.
func TestGet_StaleRefreshCASRace(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stress test in -short")
	}
	for range 200 {
		c := New[string, int](
			MaxAge(time.Microsecond),
			MaxStaleAge(time.Hour),
		)
		c.Set("k", 1)
		time.Sleep(time.Millisecond) // entry expired, stale valid

		const workers = 16
		var wg sync.WaitGroup
		wg.Add(workers)
		for range workers {
			go func() {
				defer wg.Done()
				c.Get("k", func() (int, error) {
					// Slow miss so concurrent Gets all pile into the
					// slow-path stale branch before any finalizes.
					time.Sleep(time.Millisecond)
					return 2, nil
				})
			}()
		}
		wg.Wait()
	}
}

// TestGet_ConcurrentStaleDuringInFlight exercises Get's slow-path branch
// where a second Get observes a stale-returning loading that is already
// in flight from the first Get. The second Get must return the stale
// without attempting to install a new loading.
func TestGet_ConcurrentStaleDuringInFlight(t *testing.T) {
	c := New[string, int](
		MaxAge(time.Millisecond),
		MaxStaleAge(time.Hour),
	)
	c.Set("k", 1)
	time.Sleep(10 * time.Millisecond) // entry now expired, stale still valid

	missRelease := make(chan struct{})
	missStart := make(chan struct{})
	var aDone sync.WaitGroup
	aDone.Add(1)
	go func() {
		defer aDone.Done()
		c.Get("k", func() (int, error) {
			close(missStart)
			<-missRelease
			return 2, nil
		})
	}()
	<-missStart

	// Second Get observes the first Get's in-flight loading and its stale.
	v, _, s := c.Get("k", func() (int, error) {
		t.Error("second Get's miss function should not run while first Get is in flight")
		return 3, nil
	})
	if s != Stale || v != 1 {
		t.Fatalf("second Get: v=%d s=%v, want v=1 Stale", v, s)
	}

	close(missRelease)
	aDone.Wait()
}

// TestClean_SkipsInFlightLoads verifies Clean leaves pending loads alone.
// An entry whose loading hasn't finalized yet has no expiry to evaluate;
// Clean must skip it.
func TestClean_SkipsInFlightLoads(t *testing.T) {
	c := New[string, int](MaxAge(time.Nanosecond))
	missRelease := make(chan struct{})
	missStarted := make(chan struct{})
	getDone := make(chan struct{})
	go func() {
		defer close(getDone)
		c.Get("k", func() (int, error) {
			close(missStarted)
			<-missRelease
			return 42, nil
		})
	}()
	<-missStarted
	// The entry exists in the trie with an in-flight loading. Clean must
	// observe !l.finalized() and skip.
	c.Clean()
	close(missRelease)
	<-getDone
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

// TestSwap_DeleteRangeChurn stresses Swap's unlocked fast path racing
// Delete's tombstone-then-unlink, with Range walking the trie throughout:
// Swap must observe tombstones and re-create keys (never resurrect a dying
// entry), and Range must tolerate entries being unlinked mid-walk. The
// interesting windows are ~nanoseconds wide; we stress from many angles for
// long enough that they land, leaving a generous budget for slow CI runners.
func TestSwap_DeleteRangeChurn(t *testing.T) {
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

	// Deleters churn the primed keys, repeatedly creating the tombstone +
	// unlink windows that Swap's fast path must handle.
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
	// that races with the deleters.
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
	// Churners add+delete NEW keys continuously so the trie's shape keeps
	// changing (inserts expand slots, deletes prune empty parents) while
	// Range walks it.
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
	// Range callers walk the trie throughout the churn.
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

// TestRange_DeleteSetChurn races Range against a goroutine that
// continuously Deletes and re-Sets the same keys: every key cycles through
// tombstone, physical unlink, and fresh re-creation while the walk is in
// flight. Correctness is no panic and no race-detector report.
func TestRange_DeleteSetChurn(t *testing.T) {
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

// TestGet_RacesSwapOnExpired targets Get's slow-path Hit: the fast path
// sees an expired entry (Miss), but by the time the slow path re-checks, a
// concurrent Swap has published a fresh value for the same key. If Get
// reports a Hit it must be the Swap's value, never the expired one. Hitting
// the exact timing window requires multiple iterations.
func TestGet_RacesSwapOnExpired(t *testing.T) {
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

// TestCompareAndSwap_RacesSet races CompareAndSwap against the first Set of
// the same key (plus a Range walk): the CAS may observe no entry, an
// in-flight entry, or the finalized value, and must succeed only in the
// last case. Race window; stress only.
func TestCompareAndSwap_RacesSet(t *testing.T) {
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

// TestSwap_DeleteSwapRangeCycles cycles Delete-then-Swap on a fixed key set
// while Range walks: each cycle drives an entry through tombstone, unlink,
// and re-creation, so Swap's slow path repeatedly encounters entries
// mid-deletion. Race-dependent; stress only.
func TestSwap_DeleteSwapRangeCycles(t *testing.T) {
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
// assert exact bytes; we assert that after N rounds of (Set, Delete, Range),
// the resident entry count is bounded by the number of live keys. This would
// catch a regression where Delete stopped physically unlinking entries.
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
		c.Range(func(int, int, error) bool { return true })
	}

	count := 0
	c.Range(func(int, int, error) bool { count++; return true })
	if count > liveKeys {
		t.Fatalf("after %d rounds: %d live entries, want <= %d", rounds, count, liveKeys)
	}
}

// TestSwap_NotLostDuringConcurrentDelete verifies that a Swap racing a
// Delete of the same key is never silently dropped. The dangerous
// interleaving: Delete tombstones the value slot, Swap observes the
// tombstone, and Delete's physical unlink races Swap's installation of the
// new value. A nil value slot is terminal — Swap must re-create the key as
// a fresh entry, never write into the dying one — so if Swap returns Miss
// ("I stored into nothing"), it linearized after the Delete and the swapped
// value must be visible afterwards.
//
// Regression test: an earlier version resurrected tombstones with a bare
// CAS not serialized with deleteEntryIf's unlink, losing the swapped value
// in ~0.03% of rounds.
func TestSwap_NotLostDuringConcurrentDelete(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stress test in -short")
	}
	const rounds = 100_000
	for i := range rounds {
		c := New[int, int]()
		c.Set(i, 0)

		var wg sync.WaitGroup
		wg.Add(2)
		var swapState KeyState
		go func() {
			defer wg.Done()
			c.Delete(i)
		}()
		go func() {
			defer wg.Done()
			_, _, swapState = c.Swap(i, 1)
		}()
		wg.Wait()

		if swapState == Miss {
			if v, _, s := c.TryGet(i); s.IsMiss() || v != 1 {
				t.Fatalf("round %d: Swap returned Miss (stored after the Delete) but TryGet=(%d,%v); the swapped value was silently dropped", i, v, s)
			}
		}
	}
}

// TestClean_DoesNotEvictFreshEntries runs Clean continuously against Gets
// on a cache with no MaxAge and no Deletes: nothing ever expires, so Clean
// must never remove anything and every completed Get must be a subsequent
// TryGet hit.
//
// Regression test: an earlier version created Get's entry with a nil value
// slot and CAS'd the loading in afterwards; in that window the fresh entry
// was indistinguishable from a Delete tombstone and Clean would unlink it,
// losing the cached value (and with it, request collapsing for that key).
func TestClean_DoesNotEvictFreshEntries(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stress test in -short")
	}
	c := New[int, int]()

	stop := make(chan struct{})
	var cleanWg sync.WaitGroup
	cleanWg.Add(1)
	go func() {
		defer cleanWg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				c.Clean()
				runtime.Gosched()
			}
		}
	}()

	const rounds = 200_000
	for i := range rounds {
		c.Get(i, func() (int, error) { return i, nil })
		if v, _, s := c.TryGet(i); s.IsMiss() || v != i {
			t.Fatalf("round %d: freshly loaded entry evicted by Clean: TryGet=(%d,%v)", i, v, s)
		}
	}
	close(stop)
	cleanWg.Wait()
}

// TestGet_DoesNotRedriveFreshlyFinalizedValue targets the slow-path window
// between Get's load of prev and entGet's own load of the slot: if the
// in-flight load it observed finalizes live in that window, Get must return
// the Hit rather than demote the fresh value to a stale and drive a
// redundant load. Every round arranges an expired-with-stale entry, races
// Gets against the first load's release, and asserts the miss function ran
// exactly once.
func TestGet_DoesNotRedriveFreshlyFinalizedValue(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping race-iteration test in -short")
	}
	for iter := range 2000 {
		c := New[int, int](MaxAge(time.Hour), MaxStaleAge(time.Hour))
		c.Set(iter, 1)
		c.Expire(iter) // expired with a valid self-stale

		// A driving Get returns the stale and runs its miss on a
		// background goroutine, so completion must be synchronized on:
		// missDone is closed by the (single) miss, and a redundant
		// second drive panics on the double close.
		release := make(chan struct{})
		missDone := make(chan struct{})
		miss := func() (int, error) {
			<-release // closed below; late drivers pass straight through
			close(missDone)
			return 2, nil
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			c.Get(iter, miss) // drives the refresh, returns Stale(1)
		}()
		go func() {
			defer wg.Done()
			runtime.Gosched()
			close(release)
		}()
		// Meanwhile, hammer Gets that race the finalization. None may
		// drive a second load: each must piggyback the stale or return
		// the fresh Hit.
		for range 4 {
			c.Get(iter, miss)
		}
		wg.Wait()
		<-missDone
	}
}

// TestExpire_NotLostToIdleExtension verifies that Expire racing an
// idle-extending read sticks: once both return, the key must be expired.
//
// Regression test: idle extension previously used a plain Store on the
// expiry, which could overwrite a concurrent Expire (read loads expiry,
// Expire stores now-1, read stores now+idle). Extension now CASes against
// the expiry it observed, so an interleaved Expire wins.
func TestExpire_NotLostToIdleExtension(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stress test in -short")
	}
	c := New[int, int](MaxIdleAge(time.Hour))
	const rounds = 100_000
	for i := range rounds {
		c.Set(i, 1)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			c.TryGet(i) // a Hit extends the idle expiry
		}()
		go func() {
			defer wg.Done()
			c.Expire(i)
		}()
		wg.Wait()
		if _, _, s := c.TryGet(i); s.IsHit() {
			t.Fatalf("round %d: Expire was overwritten by a concurrent idle extension", i)
		}
	}
}

// TestClean_ClaimWindowDoesNotResurrectDeadStale pins Clean's claim protocol
// against racing Gets: Clean claims an entry's expiry word before
// tombstoning, and a Get that lands between the claim and the tombstone
// snapshots that loading for a stale. The claimed word must read as "stale
// window certified closed" — never as something a stale window can be
// derived from — or the Get would serve a value past MaxAge+MaxStaleAge as
// a fresh Stale for up to another MaxStaleAge.
//
// The test performs Clean's claim by hand (same gate, same CAS) so the
// window between Clean's two CASes is held open deterministically.
//
// Regression test: Clean used to claim with -1, the born-expired sentinel,
// which newStale then re-anchored at now (born-expired words now carry
// their birth and anchor there, and a claimed word derives no window at
// all).
func TestClean_ClaimWindowDoesNotResurrectDeadStale(t *testing.T) {
	c := New[string, int](MaxAge(time.Millisecond), MaxStaleAge(time.Millisecond))
	c.Set("k", 1)
	time.Sleep(5 * time.Millisecond) // expired AND past the stale window

	if _, _, s := c.TryGet("k"); !s.IsMiss() {
		t.Fatalf("precondition: entry should be fully dead, got %v", s)
	}

	// Clean's claim, by hand: same gate, same CAS, protocol frozen between
	// Clean's two CASes.
	e := c.t.loadEntry("k")
	l := e.p.Load()
	expires := l.expires.Load()
	tn := now()
	if expired := expires != 0 && expires <= tn; !expired ||
		(l.stale != nil && !l.stale.expired(tn)) ||
		(l.err == nil && staleOpen(expires, tn, c.cfg.maxStaleAge)) {
		t.Fatalf("precondition: entry not Clean-eligible: expires=%d", expires)
	}
	if !l.expires.CompareAndSwap(expires, expiredClaimed) {
		t.Fatal("claim CAS failed")
	}

	// A Get in the claim window must not serve the dead value as a stale:
	// the stale snapshot must come up empty, so the Get blocks on (and
	// returns) the fresh load.
	if v, _, s := c.Get("k", func() (int, error) { return 2, nil }); v != 2 || s != Miss {
		t.Fatalf("Get in Clean's claim window: v=%d s=%v, want 2 Miss (a Stale here resurrects a certified-dead value)", v, s)
	}
	if l2 := e.p.Load(); l2 == nil || l2.stale != nil {
		t.Fatalf("the replacement loading must carry no stale snapshot of the claimed value, got %+v", l2)
	}

	// Finish Clean's protocol: the tombstone CAS must fail (the Get
	// replaced the slot) and the fresh value must survive.
	if e.p.CompareAndSwap(l, nil) {
		t.Fatal("Clean's tombstone CAS should have failed against the Get's replacement")
	}
	if v, _, s := c.TryGet("k"); v != 2 || !s.IsHit() {
		t.Fatalf("after claim window: TryGet=(%d,%v), want (2,Hit)", v, s)
	}
}

// TestClean_ClaimWindowVsGetRace hammers the window between Clean's two
// CASes (the expiry-word claim and the tombstone) with racing Gets. Every
// round populates keys whose value AND stale are certifiably dead (TryGet
// returned Miss), then races a real Clean against a Get scan: any Get that
// returns Stale resurrected a certified-dead value.
//
// Probabilistic tripwire for the protocol pinned deterministically by
// TestClean_ClaimWindowDoesNotResurrectDeadStale; before Clean claimed with
// a dedicated sentinel, this fired within a few seconds under -race.
func TestClean_ClaimWindowVsGetRace(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping race-iteration test in -short")
	}
	const (
		keys = 1024
		// Small enough that entries die within a round's sleep; large
		// enough that a wrongly re-anchored stale stays observable for the
		// Get that snapshotted it.
		staleAge = 5 * time.Millisecond
	)
	c := New[int, int](MaxAge(time.Nanosecond), MaxStaleAge(staleAge))
	miss := func() (int, error) { return 2, nil }
	for start := time.Now(); time.Since(start) < 3*time.Second; {
		for k := range keys {
			c.Set(k, 1)
		}
		time.Sleep(staleAge + 2*time.Millisecond)
		for k := range keys {
			for {
				if _, _, s := c.TryGet(k); s.IsMiss() {
					break // certified: expired and stale dead
				}
			}
		}

		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Clean()
		}()
		for k := range keys {
			if v, _, s := c.Get(k, miss); s == Stale {
				t.Fatalf("key %d: Get racing Clean returned a certified-dead value as a stale: v=%d", k, v)
			}
		}
		wg.Wait()
	}
}

// TestExpire_DoesNotOverwriteCleanClaim pins Expire against Clean's claim
// window: once Clean has claimed an entry's expiry word (certifying the
// value expired and its stale window closed), a racing Expire must leave
// the claim in place — never replace it with a fresh timestamp that a Get
// in the window could re-derive a stale window from. Same hand-driven
// protocol as TestClean_ClaimWindowDoesNotResurrectDeadStale.
//
// Regression test: Expire used to blindly store now()-1 over any finalized
// word, including expiredClaimed; a Get between Clean's two CASes then
// served the certified-dead value as a fresh Stale.
func TestExpire_DoesNotOverwriteCleanClaim(t *testing.T) {
	c := New[string, int](MaxAge(time.Millisecond), MaxStaleAge(time.Millisecond))
	c.Set("k", 1)
	time.Sleep(5 * time.Millisecond) // expired AND past the stale window

	if _, _, s := c.TryGet("k"); !s.IsMiss() {
		t.Fatalf("precondition: entry should be fully dead, got %v", s)
	}

	e := c.t.loadEntry("k")
	l := e.p.Load()
	expires := l.expires.Load()
	if !l.expires.CompareAndSwap(expires, expiredClaimed) { // Clean's claim
		t.Fatal("claim CAS failed")
	}

	c.Expire("k") // lands between Clean's two CASes; must be a no-op

	if got := l.expires.Load(); got != expiredClaimed {
		t.Fatalf("Expire overwrote Clean's claim: expires=%d, want expiredClaimed", got)
	}
	if v, _, s := c.Get("k", func() (int, error) { return 2, nil }); v != 2 || s != Miss {
		t.Fatalf("Get in the claim window: (%d, %v), want (2, Miss)", v, s)
	}
}

// TestSwap_OverFinalizedErroredWithStale is TestSwap_OverErroredWithStale
// with the race pinned the other way: the errored load has finalized by
// the time Swap displaces it. The classification must agree with TryGet:
// the valid stale, not the error, is the prior value.
//
// Regression test: the defer's finalized classification used to assign the
// stale and then fall through to overwrite it with (v, err, Hit); the
// sibling test only exercises the unfinalized branch (Swap usually beats
// the async setve), so the finalized path went unexercised.
func TestSwap_OverFinalizedErroredWithStale(t *testing.T) {
	c := New[string, int](MaxStaleAge(time.Hour))
	c.Set("k", 1)
	c.Expire("k")
	c.Get("k", func() (int, error) { return 0, errors.New("boom") })

	// Wait for the async setve to finalize the errored loading.
	e := c.t.loadEntry("k")
	for l := e.p.Load(); l == nil || !l.finalized(); l = e.p.Load() {
		runtime.Gosched()
	}

	if v, err, s := c.TryGet("k"); s != Stale || v != 1 || err != nil {
		t.Fatalf("TryGet: (%d, %v, %v), want (1, nil, Stale)", v, err, s)
	}
	old, oldErr, oldS := c.Swap("k", 2)
	if oldS != Stale || old != 1 || oldErr != nil {
		t.Fatalf("Swap: (%d, %v, %v), want (1, nil, Stale) to agree with TryGet", old, oldErr, oldS)
	}
}

// TestSwap_OverFinalizedErroredNoStale covers the remaining finalized
// classification arm: with no stale to return, an unexpired cached error
// is itself the prior result, and Swap reports it as a Hit carrying the
// error — agreeing with TryGet.
func TestSwap_OverFinalizedErroredNoStale(t *testing.T) {
	c := New[string, int]() // errors cache forever by default
	// The driving Get waits for its load, so the error is finalized here.
	c.Get("k", func() (int, error) { return 0, errors.New("boom") })

	if _, err, s := c.TryGet("k"); err == nil || !s.IsHit() {
		t.Fatalf("TryGet: err=%v s=%v, want the cached error as a Hit", err, s)
	}
	old, oldErr, oldS := c.Swap("k", 2)
	if old != 0 || oldErr == nil || !oldS.IsHit() {
		t.Fatalf("Swap: (%d, %v, %v), want (0, boom, Hit) to agree with TryGet", old, oldErr, oldS)
	}
}

// TestDelete_ConcurrentlyFinalizedLoadReportsHit pins Delete's documented
// classification race: Delete samples the clock before tombstoning, but
// reads the loading's finalized state afterwards, so a load finalizing
// between the two is reported as the removed value (a Hit) even though no
// read ever observed it cached. This is Delete's exact code sequence with
// the window held open; the returned triple is best effort there (see
// Delete's doc), and the cache state itself is unaffected.
func TestDelete_ConcurrentlyFinalizedLoadReportsHit(t *testing.T) {
	c := New[string, int](MaxAge(time.Hour))
	missRelease := make(chan struct{})
	getDone := make(chan struct{})
	go func() {
		defer close(getDone)
		c.Get("k", func() (int, error) { <-missRelease; return 7, nil })
	}()
	var e *ent[string, int]
	for e = c.t.loadEntry("k"); e == nil; e = c.t.loadEntry("k") {
		runtime.Gosched()
	}

	// Delete's sequence (see Cache.Delete), paused mid-window:
	n := now()
	was := entDel(e)
	c.t.deleteEntryIf("k", entDead[string, int])
	close(missRelease) // the miss finalizes the already-detached loading
	<-getDone

	if v, _, s := loadingTryGet(was, n, 0, c.cfg.maxStaleAge); !s.IsHit() || v != 7 {
		t.Fatalf("classifying the concurrently finalized loading: (%d, %v), want (7, Hit) per Delete's documented race", v, s)
	}
	if _, _, s := c.TryGet("k"); !s.IsMiss() {
		t.Fatalf("the key must be gone regardless: %v, want Miss", s)
	}
}

// TestGet_CollapsedErrorWaitersRedrive pins the documented MaxErrorAge(0)
// collapsing behavior (see MaxErrorAge): Gets that collapsed onto a load
// that then errors do not share the error; each waiter re-drives its own
// load in turn, so one erroring wave of N collapsed Gets issues N loads.
func TestGet_CollapsedErrorWaitersRedrive(t *testing.T) {
	c := New[string, int](MaxErrorAge(0))
	var calls atomic.Int32
	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	miss := func() (int, error) {
		if calls.Add(1) == 1 {
			once.Do(func() { close(started) })
			<-release
		}
		return 0, errors.New("boom")
	}
	const waiters = 8
	var wg sync.WaitGroup
	wg.Add(waiters)
	for range waiters {
		go func() { defer wg.Done(); c.Get("k", miss) }()
	}
	<-started
	time.Sleep(20 * time.Millisecond) // let the others queue on the leader
	close(release)
	wg.Wait()
	if n := calls.Load(); n != waiters {
		t.Fatalf("miss ran %d times for %d collapsed Gets; with errors uncached, every Get drives exactly one load", n, waiters)
	}
}

// armExpiresPairingHook installs expiresPairingHook such that exactly the
// first read to pass through it blocks until release is closed; all later
// reads pass straight through. The hook is cleared when the test ends.
func armExpiresPairingHook(t *testing.T) (entered, release chan struct{}) {
	t.Helper()
	entered = make(chan struct{})
	release = make(chan struct{})
	var fired atomic.Bool
	expiresPairingHook = func() {
		if fired.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
	}
	t.Cleanup(func() { expiresPairingHook = nil })
	return entered, release
}

// TestTryGet_PairedReadVsIdleExtension pins the coherence of the (expiry
// word, clock) pair a read classifies with: a reader descheduled between
// loading the word and sampling the clock, with an idle extension landing
// in that window, would otherwise classify the dead pre-extension word at
// a fresh clock and report Stale (with MaxStaleAge) or Miss (without) for
// an entry that was live at every instant of its call — and a later TryGet
// returns Hit with no intervening write, a history no sequential order
// explains. expiresNow re-confirms the word after sampling the clock, so
// the read classifies the extended (live) word instead.
//
// The hook holds the deschedule window open deterministically, like the
// hand-driven claim-window tests above.
//
// Regression test: loadingTryGet and entGet used to classify whatever word
// they loaded first at whatever clock they sampled next.
func TestTryGet_PairedReadVsIdleExtension(t *testing.T) {
	const ttl = 100 * time.Millisecond
	for _, tt := range []struct {
		name string
		opts []Opt
	}{
		{"no_stale_age", []Opt{MaxAge(ttl), MaxIdleAge(time.Hour)}},
		{"with_stale_age", []Opt{MaxAge(ttl), MaxIdleAge(time.Hour), MaxStaleAge(time.Hour)}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := New[string, int](tt.opts...)
			c.Set("k", 1)

			entered, release := armExpiresPairingHook(t)
			type res struct {
				v int
				s KeyState
			}
			r1c := make(chan res, 1)
			go func() {
				v, _, s := c.TryGet("k") // loads the initial expiry, parks in the hook
				r1c <- res{v, s}
			}()
			<-entered

			// A second TryGet (the hook is one-shot) hits and extends the
			// entry to ~now+1h; the entry is now live well past the
			// original expiry.
			if v, _, s := c.TryGet("k"); s != Hit || v != 1 {
				t.Fatalf("extending TryGet: (%d, %v), want (1, Hit)", v, s)
			}
			// Let real time pass the original expiry while the first
			// reader is still parked between its two loads.
			time.Sleep(ttl + 50*time.Millisecond)
			close(release)
			r1 := <-r1c

			if v, _, s := c.TryGet("k"); s != Hit || v != 1 {
				t.Fatalf("entry must still be live after the extension: (%d, %v)", v, s)
			}
			if r1.s != Hit || r1.v != 1 {
				t.Fatalf("paired read returned (%d, %v) for an entry that was live throughout its call, want (1, Hit)", r1.v, r1.s)
			}
		})
	}
}

// TestTryGet_PairedReadVsExpire pins the safe direction of the same window:
// otherwise the word only ever moves backward (Expire, Clean's claim), and
// a parked reader lands on one side of the race or the other — Hit per the
// pre-Expire live word (its call overlaps the Expire, so ordering the read
// first explains the history) or Miss per the expired word. Whichever side
// it lands on, no stale is conjured for an entry with no stale configured.
func TestTryGet_PairedReadVsExpire(t *testing.T) {
	c := New[string, int](MaxAge(time.Hour))
	c.Set("k", 1)

	entered, release := armExpiresPairingHook(t)
	type res struct {
		v int
		s KeyState
	}
	r1c := make(chan res, 1)
	go func() {
		v, _, s := c.TryGet("k")
		r1c <- res{v, s}
	}()
	<-entered

	c.Expire("k") // moves the word backward while the reader is parked
	close(release)
	r1 := <-r1c

	if r1.s == Stale {
		t.Fatalf("no stale is configured; got (%d, %v)", r1.v, r1.s)
	}
	if _, _, s := c.TryGet("k"); !s.IsMiss() {
		t.Fatalf("after Expire: %v, want Miss", s)
	}
}

// TestSwap_OverBornExpired pins the born-expired classification cell of
// Swap's displaced-value report against TryGet: a displaced born-expired
// value (negated-birth expiry word) is the prior value (Stale) while its
// birth-anchored window is open, and Miss after — exactly as TryGet
// reports the same state.
func TestSwap_OverBornExpired(t *testing.T) {
	t.Run("window_open", func(t *testing.T) {
		c := New[string, int](MaxAge(0), MaxStaleAge(time.Hour))
		c.Set("k", 1)
		if v, _, s := c.TryGet("k"); s != Stale || v != 1 {
			t.Fatalf("TryGet: (%d, %v), want (1, Stale)", v, s)
		}
		old, oldErr, oldS := c.Swap("k", 2)
		if oldS != Stale || old != 1 || oldErr != nil {
			t.Fatalf("Swap: (%d, %v, %v), want (1, nil, Stale) to agree with TryGet", old, oldErr, oldS)
		}
	})
	t.Run("window_closed", func(t *testing.T) {
		c := New[string, int](MaxAge(0), MaxStaleAge(30*time.Millisecond))
		c.Set("k", 1)
		time.Sleep(60 * time.Millisecond)
		if _, _, s := c.TryGet("k"); !s.IsMiss() {
			t.Fatalf("TryGet: %v, want Miss", s)
		}
		if _, _, oldS := c.Swap("k", 2); oldS != Miss {
			t.Fatalf("Swap: %v, want Miss to agree with TryGet", oldS)
		}
	})
}
