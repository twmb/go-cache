package cache

import (
	"errors"
	"math"
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
// stale is still within its window. Born-expired entries (MaxAge(0) stores
// the -1 expired sentinel) are the regression case: their MaxAge+MaxStaleAge
// grace would otherwise anchor at the epoch instead of at the entry, so
// Clean evicted stales that TryGet was still serving.
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
// entries (the expiredBorn sentinel: MaxAge(0) / MaxErrorAge(0)), whose -1
// expiry word carries no entry-anchored timeline to add the MaxStaleAge
// grace window to. Reclaim is gated on the stale instead: such an entry is
// removed exactly when no valid stale remains (i.e. as soon as TryGet
// reports Miss), regardless of process uptime.
//
// Regression test: the grace window used to be computed as
// satAdd(-1, maxStaleAge), anchoring it at the process epoch — born-expired
// stale-less entries (error churn under MaxErrorAge(0)) were unreclaimable
// until process uptime exceeded MaxStaleAge, and forever for huge stale
// ages.
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
