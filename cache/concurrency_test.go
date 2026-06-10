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
