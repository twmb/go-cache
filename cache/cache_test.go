package cache

import (
	"errors"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type got[V any] struct {
	v   V
	err error
	s   KeyState
}

func vcheck[V comparable](t *testing.T, g, e got[V]) {
	t.Helper()
	if g.v != e.v {
		t.Errorf("got %v != exp %v", g.v, e.v)
	}
	if gotErr, expErr := g.err != nil, e.err != nil; gotErr != expErr {
		t.Errorf("got err? %v (%v) != exp err? %v (%v)", gotErr, g.err, expErr, e.err)
	}
	if g.s != e.s {
		t.Errorf("got key state? %v != exp key state? %v", g.s, e.s)
	}
}

func TestGet2x(t *testing.T) {
	var c Cache[string, int]

	{
		i, _, _ := c.Get("foo", func() (int, error) {
			return 3, nil
		})
		if i != 3 {
			t.Errorf("got %d != exp 3", i)
		}
	}

	{
		i, _, _ := c.Get("foo", func() (int, error) {
			panic("should have been cached")
		})
		if i != 3 {
			t.Errorf("got %d != exp 3", i)
		}
	}
}

func TestCollapsedGet(t *testing.T) {
	const niter = 1000

	var (
		c     Cache[string, *int]
		r     = new(int)
		calls int64
		ps    = make(chan *int)
	)
	for range niter {
		go func() {
			p, _, _ := c.Get("foo", func() (*int, error) {
				if atomic.AddInt64(&calls, 1) != 1 {
					t.Error("closure called multiple times")
				}
				time.Sleep(50 * time.Millisecond)
				return r, nil
			})
			ps <- p
		}()
	}
	for range niter {
		p := <-ps
		if p != r {
			t.Error("pointer mismatch")
		}
	}
}

func TestSetExpire(t *testing.T) {
	var c Cache[string, int]

	k := "a"

	check := func(got got[int], exp int) {
		if got.v != exp {
			t.Errorf("got %d != exp %d", got.v, exp)
		}
		if got.err != nil {
			t.Errorf("unexpected err: %v", got.err)
		}
	}

	ch := make(chan struct{})
	vch := make(chan got[int], 1)
	go func() {
		v, err, s := c.Get(k, func() (int, error) {
			ch <- struct{}{}
			<-ch
			return 0, errors.New("foo")
		})
		vch <- got[int]{v, err, s}
	}()

	<-ch
	c.Set(k, 4)
	ch <- struct{}{}
	check(<-vch, 4)
	v, err, s := c.Get(k, func() (int, error) { panic("unreachable") })
	check(got[int]{v, err, s}, 4)

	c.Set(k, 10)
	v, err, s = c.Get(k, func() (int, error) { panic("unreachable") })
	check(got[int]{v, err, s}, 10)

	c.Range(func(string, int, error) bool { return false })
	c.Delete(k)
	c.Set(k, 10)

	for range 2 {
		var wg sync.WaitGroup
		wg.Add(10)
		go func() { defer wg.Done(); c.Range(func(string, int, error) bool { return false }) }()
		go func() { defer wg.Done(); c.Delete(k) }()
		go func() { defer wg.Done(); c.Set(k, 10) }()
		go func() { defer wg.Done(); c.Expire(k) }()
		go func() { defer wg.Done(); c.Set(k, 10) }()
		go func() { defer wg.Done(); c.Range(func(string, int, error) bool { return false }) }()
		go func() { defer wg.Done(); c.Delete(k) }()
		go func() { defer wg.Done(); c.Set(k, 10) }()
		go func() { defer wg.Done(); c.Expire(k) }()
		go func() { defer wg.Done(); c.Set(k, 10) }()
		wg.Wait()
		v, err, s = c.Get(k, func() (int, error) { return 10, nil })
		check(got[int]{v, err, s}, 10)
	}

	c.Expire("asdf")
}

func TestExpires(t *testing.T) {
	// Expire things immediately: we expect all gets to be misses.
	{
		c := New[string, string](
			MaxAge(time.Nanosecond),
		)

		v, err, s := c.Get("foo", func() (string, error) {
			return "bar", nil
		})
		vcheck(t, got[string]{v, err, s}, got[string]{"bar", nil, Miss})

		v, err, s = c.Get("foo", func() (string, error) {
			return "baz", nil
		})
		vcheck(t, got[string]{v, err, s}, got[string]{"baz", nil, Miss})
	}

	// Expire immediately, but have stales.
	{
		c := New[string, string](
			MaxAge(time.Nanosecond),
			MaxStaleAge(24*time.Hour),
		)

		c.Get("foo", func() (string, error) { return "bar", nil }) // prime the stale
		time.Sleep(2 * time.Nanosecond)

		// Here, we check that the stale is returned while we load.
		ch := make(chan struct{})
		v, err, s := c.Get("foo", func() (string, error) {
			<-ch
			return "baz", nil
		})
		vcheck(t, got[string]{v, err, s}, got[string]{"bar", nil, Stale})
		close(ch)

		// Here, we check that the stale is returned because it is
		// expired.  We sleep 10ms to allow the goroutines running the
		// prior miss to finish and update our value to baz.
		time.Sleep(10 * time.Millisecond)
		v, err, s = c.Get("foo", func() (string, error) {
			return "bar", nil
		})
		vcheck(t, got[string]{v, err, s}, got[string]{"baz", nil, Stale})
	}

	// Return stale while value is an error.
	{
		c := New[string, string](
			MaxStaleAge(-1),
		)
		c.Get("foo", func() (string, error) { return "bar", nil })
		time.Sleep(time.Millisecond)
		c.Expire("foo")
		time.Sleep(time.Millisecond)
		c.Get("foo", func() (string, error) { return "", errors.New("foo") })
		time.Sleep(time.Millisecond)

		// The cache is primed: foo exists, it was expired, and we
		// injected an error such that the next loads should be stale.

		v, err, s := c.TryGet("foo")
		vcheck(t, got[string]{v, err, s}, got[string]{"bar", nil, Stale})

		ch := make(chan struct{})
		v, err, s = c.Get("foo", func() (string, error) {
			<-ch
			return "baz", nil
		})
		vcheck(t, got[string]{v, err, s}, got[string]{"bar", nil, Stale})

		v, err, s = c.TryGet("foo")
		vcheck(t, got[string]{v, err, s}, got[string]{"bar", nil, Stale})
		ch <- struct{}{}
	}

	// Misc. bits.
	{
		c := New[string, string](MaxStaleAge(-1))
		c.Get("foo", func() (string, error) { return "bar", nil })
		c.Range(func(string, string, error) bool { return true })
		c.Delete("foo")
		v, err, s := c.Get("foo", func() (string, error) { return "baz", nil })
		vcheck(t, got[string]{v, err, s}, got[string]{"baz", nil, Miss})
	}

	// Set a stale value, and get it.
	{
		c := New[string, string](MaxStaleAge(-1))
		c.Set("foo", "biz")
		c.Expire("foo")
		v, err, s := c.TryGet("foo")
		vcheck(t, got[string]{v, err, s}, got[string]{"biz", nil, Stale})
	}

	// Stale value expiry.
	{
		// We expire foo, and it is not used even though it is a stale
		// value.
		c := New[string, string](MaxStaleAge(1))
		c.Set("foo", "bar")
		c.Expire("foo")
		time.Sleep(5 * time.Nanosecond) // past max stale age
		v, err, s := c.TryGet("foo")
		vcheck(t, got[string]{v, err, s}, got[string]{"", nil, Miss})
		v, err, s = c.Get("foo", func() (string, error) { return "baz", nil })
		vcheck(t, got[string]{v, err, s}, got[string]{"baz", nil, Miss})

		// TryGet returns immediately with miss if we are loading and
		// stale is expired.
		c.Expire("foo")
		time.Sleep(2 * time.Nanosecond)
		ch := make(chan struct{})
		go func() {
			defer close(ch)
			c.Get("foo", func() (string, error) {
				ch <- struct{}{}
				<-ch
				return "asdf", nil
			})
		}()
		<-ch // ensure we are in Get, then we block it
		v, err, s = c.TryGet("foo")
		ch <- struct{}{} // unblock get
		vcheck(t, got[string]{v, err, s}, got[string]{"", nil, Miss})

		// We expire foo, and return an error. The error is returned
		// because our stale is expired.
		<-ch // ensure Get has returned
		c.Expire("foo")
		time.Sleep(2 * time.Nanosecond)
		c.Get("foo", func() (string, error) { return "", errors.New("err") })
		v, err, s = c.Get("foo", func() (string, error) { panic("unreachable") })
		vcheck(t, got[string]{v, err, s}, got[string]{"", errors.New("err"), Hit})
	}

	// Values deleted immediately.
	{
		c := New[string, string](MaxAge(0))

		gch := make(chan got[string], 10)
		ch := make(chan struct{})
		for range 10 {
			go func() {
				v, err, s := c.Get("foo", func() (string, error) {
					<-ch
					return "bar", nil
				})
				gch <- got[string]{v, err, s}
			}()
		}

		// We give the Get's a chance to stack, but we do not sleep
		// long, so not all Get's could be stacked at once.
		time.Sleep(100 * time.Millisecond)
		close(ch)
		for range 10 {
			g := <-gch
			if g.s == Miss {
				vcheck(t, g, got[string]{"bar", nil, Miss})
			} else {
				vcheck(t, g, got[string]{"bar", nil, Hit})
			}
		}

		// We expect a miss because nothing persists.
		v, err, s := c.TryGet("foo")
		vcheck(t, got[string]{v, err, s}, got[string]{"", nil, Miss})

	}

	// Errors are deleted immediately.
	for _, d := range []time.Duration{-1, 0, 1} {
		c := New[string, string](MaxErrorAge(d))
		v, err, s := c.Get("foo", func() (string, error) { return "", errors.New("foo") })
		vcheck(t, got[string]{v, err, s}, got[string]{"", errors.New("foo"), Miss})
		v, err, s = c.Get("foo", func() (string, error) { return "bar", nil })
		vcheck(t, got[string]{v, err, s}, got[string]{"bar", nil, Miss})
	}

	// Clean expired values.
	{
		c := New[string, string]()
		c.Set("1", "foo")
		c.Set("2", "foo")
		c.Set("3", "foo")
		c.Set("4", "foo")
		c.Set("5", "foo")
		c.Set("6", "foo")
		c.Set("7", "foo")
		c.Set("8", "foo")
		c.Set("9", "foo")
		c.Expire("1")
		c.Expire("3")
		c.Expire("5")
		c.Expire("7")
		c.Expire("9")
		c.Clean()

		exp := map[string]bool{
			"2": true,
			"4": true,
			"6": true,
			"8": true,
		}
		c.Range(func(k, v string, err error) bool {
			if !exp[k] {
				t.Errorf("unexpected key %s remaining", k)
			}
			delete(exp, k)
			if v != "foo" {
				t.Errorf("unexpected value %s", v)
			}
			if err != nil {
				t.Errorf("unexpected err %v", err)
			}
			return true
		})
		if len(exp) != 0 {
			t.Errorf("unexpected values remain: %v", exp)
		}
	}
}

func TestMaxIdleAge(t *testing.T) {
	// Scale all time windows generously — these tests are timing-based and
	// fail under heavy -race/stress load if the sleep/access margin is thin.

	// MaxIdleAge alone: entry stays alive while accessed within the window,
	// expires after inactivity.
	t.Run("alone", func(t *testing.T) {
		const idle = 200 * time.Millisecond
		c := New[string, string](MaxIdleAge(idle))
		c.Get("foo", func() (string, error) { return "bar", nil })
		time.Sleep(40 * time.Millisecond)

		// Access within the idle window — should still be a hit and extend.
		for range 5 {
			v, err, s := c.TryGet("foo")
			vcheck(t, got[string]{v, err, s}, got[string]{"bar", nil, Hit})
			time.Sleep(40 * time.Millisecond) // ≪ idle, plenty of margin
		}

		// Now stop accessing. After idle of inactivity, it should expire.
		time.Sleep(idle + 50*time.Millisecond)
		v, err, s := c.TryGet("foo")
		vcheck(t, got[string]{v, err, s}, got[string]{"", nil, Miss})
	})

	// MaxIdleAge + MaxAge: initial expiry uses MaxAge, subsequent accesses
	// extend using MaxIdleAge.
	t.Run("with_max_age", func(t *testing.T) {
		const ttl = 200 * time.Millisecond
		c := New[string, string](MaxAge(ttl), MaxIdleAge(ttl))
		c.Get("foo", func() (string, error) { return "bar", nil })

		// Keep alive well past the original MaxAge.
		for range 5 {
			time.Sleep(40 * time.Millisecond)
			v, err, s := c.Get("foo", func() (string, error) { return "baz", nil })
			vcheck(t, got[string]{v, err, s}, got[string]{"bar", nil, Hit})
		}

		// Stop accessing, should expire.
		time.Sleep(ttl + 50*time.Millisecond)
		v, err, s := c.TryGet("foo")
		vcheck(t, got[string]{v, err, s}, got[string]{"", nil, Miss})
	})

	// MaxIdleAge doesn't extend errors.
	t.Run("no_extend_errors", func(t *testing.T) {
		const ttl = 200 * time.Millisecond
		c := New[string, string](MaxAge(ttl), MaxIdleAge(ttl))
		c.Get("foo", func() (string, error) { return "", errors.New("err") })
		time.Sleep(40 * time.Millisecond)

		// Access the errored entry — it should not be extended.
		v, err, s := c.TryGet("foo")
		vcheck(t, got[string]{v, err, s}, got[string]{"", errors.New("err"), Hit})

		// Wait past expiry — should be gone.
		time.Sleep(ttl + 50*time.Millisecond)
		v, err, s = c.TryGet("foo")
		vcheck(t, got[string]{v, err, s}, got[string]{"", nil, Miss})
	})

	// MaxIdleAge must not resurrect entries that finalize already expired:
	// with MaxAge(0), caching is disabled entirely and the Get that drives
	// (or piggybacks on) the load gets a courtesy value, not an
	// idle-extending access. Regression test: the extension previously
	// fired on the waited-expired path and revived the value for the idle
	// window.
	t.Run("no_resurrect_expired", func(t *testing.T) {
		c := New[string, string](MaxAge(0), MaxIdleAge(time.Hour))
		v, err, s := c.Get("foo", func() (string, error) { return "bar", nil })
		vcheck(t, got[string]{v, err, s}, got[string]{"bar", nil, Miss})
		v, err, s = c.TryGet("foo")
		vcheck(t, got[string]{v, err, s}, got[string]{"", nil, Miss})
	})

	// Range doesn't extend.
	t.Run("range_no_extend", func(t *testing.T) {
		const ttl = 200 * time.Millisecond
		c := New[string, string](MaxAge(ttl), MaxIdleAge(ttl))
		c.Get("foo", func() (string, error) { return "bar", nil })

		// Range over entries repeatedly — should NOT extend expiry.
		for range 3 {
			time.Sleep(40 * time.Millisecond)
			c.Range(func(string, string, error) bool { return true })
		}

		// Should be expired by now (>ttl since creation, range didn't extend).
		time.Sleep(ttl + 50*time.Millisecond)
		v, err, s := c.TryGet("foo")
		vcheck(t, got[string]{v, err, s}, got[string]{"", nil, Miss})
	})
}

// TestHugeAges verifies expiry arithmetic saturates rather than wrapping:
// absurd-but-valid Durations must mean "effectively forever", not "born
// expired" (or, for Clean, "evict everything").
func TestHugeAges(t *testing.T) {
	huge := time.Duration(math.MaxInt64)

	// MaxAge: now + ttl must not wrap negative.
	{
		c := New[string, int](MaxAge(huge))
		c.Set("k", 1)
		if v, _, s := c.TryGet("k"); !s.IsHit() || v != 1 {
			t.Fatalf("TryGet under huge MaxAge: v=%d s=%v, want 1 Hit", v, s)
		}
	}

	// MaxStaleAge: Clean's expires + staleAge must not wrap negative, and
	// the stale's own expiry must not be born wrapped.
	{
		c := New[string, int](MaxAge(time.Nanosecond), MaxStaleAge(huge))
		c.Set("k", 1)
		time.Sleep(time.Millisecond) // main entry expired, stale effectively forever
		c.Clean()                    // must not evict
		if v, _, s := c.TryGet("k"); s != Stale || v != 1 {
			t.Fatalf("TryGet after Clean under huge MaxStaleAge: v=%d s=%v, want 1 Stale", v, s)
		}
	}

	// MaxIdleAge: the extension n + idle must not wrap negative.
	{
		c := New[string, int](MaxIdleAge(huge))
		c.Set("k", 1)
		c.TryGet("k") // a Hit extends; the extension must saturate
		if v, _, s := c.TryGet("k"); !s.IsHit() || v != 1 {
			t.Fatalf("TryGet after huge idle extension: v=%d s=%v, want 1 Hit", v, s)
		}
	}
}

// TestClean_RespectsValidStales verifies Clean never removes an entry whose
// stale window is still open. Born-expired entries (MaxAge(0) stores a
// negative, birth-carrying word) are the regression case: their
// MaxAge+MaxStaleAge grace used to anchor at the epoch instead of at the
// entry, so Clean evicted stales that TryGet was still serving.
func TestClean_RespectsValidStales(t *testing.T) {
	c := New[string, int](MaxAge(0), MaxStaleAge(50*time.Millisecond))
	c.Set("k", 1)

	c.Clean() // stale still valid: must not evict
	if v, _, s := c.TryGet("k"); s != Stale || v != 1 {
		t.Fatalf("TryGet after early Clean: v=%d s=%v, want 1 Stale", v, s)
	}

	time.Sleep(60 * time.Millisecond) // stale now expired
	c.Clean()                         // must evict
	if _, _, s := c.TryGet("k"); !s.IsMiss() {
		t.Fatalf("TryGet after late Clean: s=%v, want Miss", s)
	}
}

// trieEntries counts every entry wired into the trie, including tombstoned
// shells and entries that are invisible to Range and TryGet.
func trieEntries[K comparable, V any](c *Cache[K, V]) (n int) {
	c.t.walk(func(*ent[K, V]) bool { n++; return true })
	return n
}

// TestClean_ReclaimsBornExpired verifies Clean's handling of born-expired
// entries (MaxAge(0) / MaxErrorAge(0), whose negative expiry word carries
// the negated birth): an entry is removed exactly when no read can see it
// anymore (i.e. as soon as TryGet reports Miss) — immediately for errored
// stale-less entries, and exactly when the birth-anchored stale window
// closes for values — regardless of process uptime.
//
// Regression test: born-expired entries used to store a bare -1 sentinel,
// and the grace window was computed as satAdd(-1, maxStaleAge), anchoring
// it at the process epoch — born-expired stale-less entries (error churn
// under MaxErrorAge(0)) were unreclaimable until process uptime exceeded
// MaxStaleAge, and forever for huge stale ages.
func TestClean_ReclaimsBornExpired(t *testing.T) {
	t.Run("errored_no_stale", func(t *testing.T) {
		c := New[string, int](MaxErrorAge(0), MaxStaleAge(time.Hour))
		c.Get("k", func() (int, error) { return 0, errors.New("boom") })
		if _, _, s := c.TryGet("k"); !s.IsMiss() {
			t.Fatalf("precondition: errored entry should be invisible, got %v", s)
		}
		c.Clean()
		if n := trieEntries(c); n != 0 {
			t.Fatalf("Clean left %d dead invisible born-expired entries in the trie", n)
		}
	})

	t.Run("huge_stale_age", func(t *testing.T) {
		c := New[string, int](MaxErrorAge(0), MaxStaleAge(time.Duration(math.MaxInt64)))
		c.Get("k", func() (int, error) { return 0, errors.New("boom") })
		if _, _, s := c.TryGet("k"); !s.IsMiss() {
			t.Fatalf("precondition: errored entry should be invisible, got %v", s)
		}
		c.Clean()
		if n := trieEntries(c); n != 0 {
			t.Fatalf("Clean left %d dead entries (huge MaxStaleAge made them permanent)", n)
		}
	})

	t.Run("keeps_valid_stale", func(t *testing.T) {
		c := New[string, int](MaxAge(0), MaxStaleAge(time.Hour))
		c.Set("k", 1) // born expired; stale window anchored at the store
		if _, _, s := c.TryGet("k"); s != Stale {
			t.Fatalf("precondition: want Stale, got %v", s)
		}
		c.Clean()
		if v, _, s := c.TryGet("k"); s != Stale || v != 1 {
			t.Fatalf("Clean evicted a born-expired entry with a valid stale: TryGet=(%d,%v)", v, s)
		}
		if n := trieEntries(c); n != 1 {
			t.Fatalf("trie entries = %d, want 1", n)
		}
	})

	t.Run("reclaims_dead_stale", func(t *testing.T) {
		c := New[string, int](MaxAge(0), MaxStaleAge(5*time.Millisecond))
		c.Set("k", 1)
		time.Sleep(10 * time.Millisecond) // stale window passed
		if _, _, s := c.TryGet("k"); !s.IsMiss() {
			t.Fatalf("precondition: want Miss, got %v", s)
		}
		c.Clean()
		if n := trieEntries(c); n != 0 {
			t.Fatalf("Clean left %d entries whose stale window had passed", n)
		}
	})
}

// TestStale_BornExpiredAnchorsAtBirth verifies that a born-expired value's
// (MaxAge(0)) stale window is anchored at the value's birth — carried by
// the negated-birth expiry word — for every read and refresh. Once the
// window passes, the value is gone for good: a later Get drives a fresh
// load rather than serving the dead value.
//
// Regression test: born-expired values used to store a bare -1 sentinel,
// destroying the birth; a Get-driven refresh arbitrarily later re-anchored
// the dead value's stale at the refresh (newStale's expires<=0 fallback),
// serving a value of unbounded age as a fresh Stale right after TryGet
// certified Miss.
func TestStale_BornExpiredAnchorsAtBirth(t *testing.T) {
	const age = 50 * time.Millisecond
	c := New[string, int](MaxAge(0), MaxStaleAge(age))
	c.Set("k", 1)

	// Within the window, the value is servable as a stale.
	if v, _, s := c.TryGet("k"); s != Stale || v != 1 {
		t.Fatalf("TryGet within the window: (%d, %v), want (1, Stale)", v, s)
	}

	time.Sleep(2 * age) // window passed; the value is certifiably dead
	if _, _, s := c.TryGet("k"); !s.IsMiss() {
		t.Fatalf("TryGet after the window: %v, want Miss", s)
	}
	v, _, s := c.Get("k", func() (int, error) { return 2, nil })
	if v != 2 || s != Miss {
		t.Fatalf("Get after the window: (%d, %v), want (2, Miss): the dead value must not be re-anchored at the refresh", v, s)
	}
}

// TestStale_GetLoadedValueServesSelfStale verifies the stale window applies
// uniformly regardless of how the value was written: a Get-loaded value,
// once expired, is served as Stale by TryGet for MaxStaleAge, exactly like
// a Set-loaded value.
//
// Regression test: only Set/Swap-created loadings used to carry a stale
// snapshot of themselves; Get-loaded values had none (their stale field is
// the previous generation's refresh companion), so TryGet reported Miss
// the instant they expired while a Get-driven refresh of the same state
// served the Stale.
func TestStale_GetLoadedValueServesSelfStale(t *testing.T) {
	const ttl = 20 * time.Millisecond
	c := New[string, int](MaxAge(ttl), MaxStaleAge(time.Hour))
	c.Get("k", func() (int, error) { return 1, nil })
	time.Sleep(2 * ttl)
	if v, _, s := c.TryGet("k"); s != Stale || v != 1 {
		t.Fatalf("TryGet on an expired Get-loaded value: (%d, %v), want (1, Stale)", v, s)
	}
}

// TestStale_ServesLatestGeneration verifies that once a refreshed value
// itself expires, every read serves that value as the stale — not the
// generation before it.
//
// Regression test: a Get-created loading's stale field is the previous
// generation's snapshot; TryGet used to serve it (v1) after the refreshed
// value (v2) expired, while a Get-driven refresh of the same state
// snapshotted and served v2.
func TestStale_ServesLatestGeneration(t *testing.T) {
	const ttl = 30 * time.Millisecond
	c := New[string, int](MaxAge(ttl), MaxStaleAge(time.Hour))
	c.Set("k", 1)
	time.Sleep(ttl + 10*time.Millisecond) // v1 expired; within its stale window

	// Drive a refresh to v2; the driving Get returns the v1 stale while
	// its (held-open) miss runs.
	release := make(chan struct{})
	if v, _, s := c.Get("k", func() (int, error) { <-release; return 2, nil }); s != Stale || v != 1 {
		t.Fatalf("refreshing Get: (%d, %v), want (1, Stale)", v, s)
	}
	close(release)
	time.Sleep(ttl + 20*time.Millisecond) // v2 finalized, then expired

	if v, _, s := c.TryGet("k"); s != Stale || v != 2 {
		t.Fatalf("TryGet after v2 expired: (%d, %v), want (2, Stale) — the freshest dead value", v, s)
	}
}

// TestMaxIdleAge_StaleWindowFollowsExtensions verifies that the stale
// window of an idle-extended entry anchors at the entry's actual death
// (its last extension lapsing), not at its initial expiry: derived from
// the expiry word, the window moves with every extension.
//
// Regression test: the self-stale used to be a snapshot frozen at
// creation, anchored at the initial expiry. Extensions moved only the
// expiry word, so an entry kept hot past initialExpiry+MaxStaleAge idled
// out with its stale window already closed: TryGet reported Miss the
// instant the entry expired while Get served the Stale.
func TestMaxIdleAge_StaleWindowFollowsExtensions(t *testing.T) {
	const (
		ttl = 200 * time.Millisecond
		age = 400 * time.Millisecond
	)
	c := New[string, int](MaxAge(ttl), MaxIdleAge(ttl), MaxStaleAge(age))
	c.Set("k", 1)

	// Keep the entry hot well past the initial expiry + age (600ms after
	// the store), so a creation-frozen window would be long dead.
	for range 8 { // ~640ms of extensions
		time.Sleep(80 * time.Millisecond)
		if _, _, s := c.TryGet("k"); !s.IsHit() {
			t.Fatal("entry should still be live while being extended")
		}
	}
	time.Sleep(ttl + 100*time.Millisecond) // idle out: died ~100ms ago

	// ~100ms into the 400ms post-death window: still a Stale.
	if v, _, s := c.TryGet("k"); s != Stale || v != 1 {
		t.Fatalf("TryGet shortly after an idle-extended entry expired: (%d, %v), want (1, Stale)", v, s)
	}
	time.Sleep(age) // window passed
	if _, _, s := c.TryGet("k"); !s.IsMiss() {
		t.Fatalf("TryGet after the stale window: %v, want Miss", s)
	}
}

// TestExpire_DoesNotResurrectDeadStale verifies that Expire on an entry
// whose value and stale window are both already dead is a no-op: the next
// Get drives a fresh load rather than serving the dead value.
//
// Regression test: Expire used to blindly store now()-1, moving an
// already-expired word forward; the next Get-driven refresh anchored a
// fresh stale window at the Expire and served the dead value as Stale for
// up to another MaxStaleAge.
func TestExpire_DoesNotResurrectDeadStale(t *testing.T) {
	const ttl, age = 5 * time.Millisecond, 5 * time.Millisecond
	c := New[string, int](MaxAge(ttl), MaxStaleAge(age))
	c.Set("k", 1)
	time.Sleep(30 * time.Millisecond) // value expired AND stale window passed
	if _, _, s := c.TryGet("k"); !s.IsMiss() {
		t.Fatalf("precondition: want Miss, got %v", s)
	}
	c.Expire("k") // invalidating a dead key must not revive anything
	if v, _, s := c.Get("k", func() (int, error) { return 2, nil }); v != 2 || s != Miss {
		t.Fatalf("Get after expiring a dead entry: (%d, %v), want (2, Miss)", v, s)
	}
}

// TestExpire_StaleWindowAnchorsAtExpire verifies the uniform stale-window
// anchor for manually expired values: the window opens at the Expire and
// closes MaxStaleAge later, for TryGet and Get-driven refreshes alike.
//
// Regression test: TryGet used to serve a frozen creation-time snapshot
// whose window was anchored at the original expiry (an hour out here),
// outliving the documented Expire-anchored window.
func TestExpire_StaleWindowAnchorsAtExpire(t *testing.T) {
	const age = 50 * time.Millisecond
	c := New[string, int](MaxAge(time.Hour), MaxStaleAge(age))
	c.Set("k", 1)
	c.Expire("k")
	if v, _, s := c.TryGet("k"); s != Stale || v != 1 {
		t.Fatalf("TryGet just after Expire: (%d, %v), want (1, Stale)", v, s)
	}
	time.Sleep(2 * age)
	if _, _, s := c.TryGet("k"); !s.IsMiss() {
		t.Fatalf("TryGet after the post-Expire window: %v, want Miss", s)
	}
}

// TestExpire_MidWindowDoesNotReanchor verifies Expire is a no-op on an
// entry that is expired but still inside its stale window: the expiry word
// (the window's anchor) must be byte-identical after the call.
// TestExpire_DoesNotResurrectDeadStale covers the window-already-closed
// case; here a forward re-anchor would extend a dead value's servable life
// mid-window.
func TestExpire_MidWindowDoesNotReanchor(t *testing.T) {
	t.Run("ttl_expired", func(t *testing.T) {
		c := New[string, int](MaxAge(20*time.Millisecond), MaxStaleAge(time.Hour))
		c.Set("k", 1)
		time.Sleep(40 * time.Millisecond) // expired; window open for ~1h
		if _, _, s := c.TryGet("k"); s != Stale {
			t.Fatalf("precondition: want Stale, got %v", s)
		}
		l := c.t.loadEntry("k").p.Load()
		w0 := l.expires.Load()
		c.Expire("k")
		if w1 := l.expires.Load(); w1 != w0 {
			t.Fatalf("Expire moved an already-expired word: %d -> %d (re-anchors the stale window)", w0, w1)
		}
	})
	t.Run("born_expired", func(t *testing.T) {
		c := New[string, int](MaxAge(0), MaxStaleAge(time.Hour))
		c.Set("k", 1) // word carries the negated birth
		if _, _, s := c.TryGet("k"); s != Stale {
			t.Fatalf("precondition: want Stale, got %v", s)
		}
		l := c.t.loadEntry("k").p.Load()
		w0 := l.expires.Load()
		if w0 >= 0 {
			t.Fatalf("precondition: want a negated-birth word, got %d", w0)
		}
		c.Expire("k")
		if w1 := l.expires.Load(); w1 != w0 {
			t.Fatalf("Expire destroyed the birth anchor: %d -> %d", w0, w1)
		}
	})
}

// TestClean_KeepsExpiredErrorWithValidCompanion pins Clean's gate for
// errored entries: an expired error whose refresh companion (the previous
// generation) is still inside its window is visible to reads — TryGet
// serves the companion — so Clean must not reclaim it.
func TestClean_KeepsExpiredErrorWithValidCompanion(t *testing.T) {
	c := New[string, int](MaxStaleAge(time.Hour), MaxErrorAge(20*time.Millisecond))
	c.Set("k", 1)
	c.Expire("k")
	// Drive a refresh that errors; the new loading carries companion=1.
	c.Get("k", func() (int, error) { return 0, errors.New("boom") })
	e := c.t.loadEntry("k")
	for l := e.p.Load(); l == nil || !l.finalized(); l = e.p.Load() {
		runtime.Gosched()
	}
	time.Sleep(40 * time.Millisecond) // error now expired; companion valid ~1h

	if v, _, s := c.TryGet("k"); s != Stale || v != 1 {
		t.Fatalf("precondition: TryGet=(%d,%v), want (1, Stale) via the companion", v, s)
	}
	c.Clean()
	if v, _, s := c.TryGet("k"); s != Stale || v != 1 {
		t.Fatalf("Clean reclaimed an entry whose companion was still servable: TryGet=(%d,%v)", v, s)
	}
	if n := trieEntries(c); n != 1 {
		t.Fatalf("trie entries = %d, want 1", n)
	}
}

// TestStale_WindowBoundaries drives the exact-nanosecond boundaries of the
// derived stale window through loadingTryGet with hand-picked clocks (the
// n64 parameter pins the clock deterministically, exactly as Range and
// Delete do) and checks the window definitions agree bit-for-bit across
// the read path (staleOpen), the refresh companion (newStale), and
// stale.expired: the window is [expiry, expiry+age), half-open at every
// site, for positive, negated-birth, and claimed words.
func TestStale_WindowBoundaries(t *testing.T) {
	const age = 100 * time.Millisecond
	a := int64(age)
	c := New[string, int](MaxAge(time.Hour), MaxStaleAge(age))
	c.Set("k", 1)
	l := c.t.loadEntry("k").p.Load()

	check := func(n int64, wantS KeyState, wantV int, what string) {
		t.Helper()
		v, _, s := loadingTryGet(l, n, 0, age)
		if s != wantS || (wantS != Miss && v != wantV) {
			t.Fatalf("%s: loadingTryGet=(%d,%v), want (%d,%v)", what, v, s, wantV, wantS)
		}
	}

	// Positive word: expiry E, window [E, E+age).
	E := now() - 1
	l.expires.Store(E)
	check(E-1, Hit, 1, "one ns before expiry")
	check(E, Stale, 1, "the expiry instant itself")
	check(E+a-1, Stale, 1, "last open ns of the window")
	check(E+a, Miss, 0, "the window-close instant")

	if st := newStale(1, E, age); st.expires != E+a {
		t.Fatalf("companion window end %d != read-path window end %d", st.expires, E+a)
	} else if st.expired(E+a-1) || !st.expired(E+a) {
		t.Fatal("companion boundary disagrees with the read-path boundary")
	}
	if !staleOpen(E, E+a-1, age) || staleOpen(E, E+a, age) {
		t.Fatal("staleOpen boundary inconsistent with itself")
	}

	// Negated-birth word: anchor B, window [B, B+age).
	B := now()
	l.expires.Store(-B)
	check(B+a-1, Stale, 1, "born-expired: last open ns")
	check(B+a, Miss, 0, "born-expired: window-close instant")
	if st := newStale(1, -B, age); st.expires != B+a {
		t.Fatalf("born-expired companion end %d, want %d", st.expires, B+a)
	}

	// Claimed word: no window, ever; and no companion may be derived.
	l.expires.Store(expiredClaimed)
	check(B, Miss, 0, "claimed word")
	if staleOpen(expiredClaimed, 1, age) {
		t.Fatal("staleOpen derived a window from a claimed word")
	}
	if st := entMaybeNewStale(c.t.loadEntry("k"), age); st != nil {
		t.Fatalf("entMaybeNewStale snapshotted a claimed loading: %+v", st)
	}
}

// TestCompareAndSwapAgreesWithTryGet verifies that CompareAndSwap and
// CompareAndDelete only match live values: expired entries and errored
// entries are "not there" per TryGet, so comparing against them must fail.
func TestCompareAndSwapAgreesWithTryGet(t *testing.T) {
	t.Run("expired", func(t *testing.T) {
		c := New[string, int](MaxAge(time.Hour))
		c.Set("k", 1)
		c.Expire("k")
		if _, _, s := c.TryGet("k"); !s.IsMiss() {
			t.Fatal("expired entry should TryGet Miss")
		}
		if c.CompareAndSwap("k", 1, 2) {
			t.Error("CompareAndSwap matched an expired value")
		}
		if c.CompareAndDelete("k", 1) {
			t.Error("CompareAndDelete matched an expired value")
		}
	})

	t.Run("errored", func(t *testing.T) {
		c := New[string, int]()
		c.Get("k", func() (int, error) { return 0, errors.New("boom") })
		// The errored loading's v is the zero int; CAS against 0 must not
		// treat that as a cached value.
		if c.CompareAndSwap("k", 0, 2) {
			t.Error("CompareAndSwap matched an errored entry's placeholder value")
		}
		if c.CompareAndDelete("k", 0) {
			t.Error("CompareAndDelete matched an errored entry's placeholder value")
		}
	})

	t.Run("live", func(t *testing.T) {
		c := New[string, int](MaxAge(time.Hour))
		c.Set("k", 1)
		if !c.CompareAndSwap("k", 1, 2) {
			t.Error("CompareAndSwap failed on a live matching value")
		}
		if !c.CompareAndDelete("k", 2) {
			t.Error("CompareAndDelete failed on a live matching value")
		}
	})
}
