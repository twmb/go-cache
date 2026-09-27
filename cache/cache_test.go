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
		wg.Go(func() { c.Range(func(string, int, error) bool { return false }) })
		wg.Go(func() { c.Delete(k) })
		wg.Go(func() { c.Set(k, 10) })
		wg.Go(func() { c.Expire(k) })
		wg.Go(func() { c.Set(k, 10) })
		wg.Go(func() { c.Range(func(string, int, error) bool { return false }) })
		wg.Go(func() { c.Delete(k) })
		wg.Go(func() { c.Set(k, 10) })
		wg.Go(func() { c.Expire(k) })
		wg.Go(func() { c.Set(k, 10) })
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
		// expired. We wait for the prior miss to finish and update our
		// value to baz, and block the new miss so that it cannot finish
		// before Get checks for a stale.
		waitFinalized(t, &c.t, "foo")
		ch = make(chan struct{})
		defer close(ch)
		v, err, s = c.Get("foo", func() (string, error) {
			<-ch
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
		waitFinalized(t, &c.t, "foo")

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
		close(ch)
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
		if n := trieEntries(c); n != 4 {
			t.Errorf("Clean left %d entries, want 4", n)
		}
	}
}

func TestMaxIdleAge(t *testing.T) {
	expiresOf := func(c *Cache[string, string]) int64 {
		return c.t.loadEntry("foo").p.Load().expires.Load()
	}
	// tick ensures the next now() differs from the last.
	tick := func() { time.Sleep(time.Millisecond) }

	// Hits from TryGet and Get extend. A 6.4s idle age has a 100ms slack
	// (age/64), so we wait past it between hits.
	t.Run("alone", func(t *testing.T) {
		const idle = 6400 * time.Millisecond
		pastSlack := func() { time.Sleep(idle/64 + 10*time.Millisecond) }
		c := New[string, string](MaxIdleAge(idle))
		c.Get("foo", func() (string, error) { return "bar", nil })
		w0 := expiresOf(c)
		pastSlack()
		v, err, s := c.TryGet("foo")
		vcheck(t, got[string]{v, err, s}, got[string]{"bar", nil, Hit})
		w1 := expiresOf(c)
		if w1 <= w0 {
			t.Fatalf("TryGet did not extend: %d <= %d", w1, w0)
		}
		pastSlack()
		v, err, s = c.Get("foo", func() (string, error) { return "baz", nil })
		vcheck(t, got[string]{v, err, s}, got[string]{"bar", nil, Hit})
		if w2 := expiresOf(c); w2 <= w1 {
			t.Fatalf("Get did not extend: %d <= %d", w2, w1)
		}
	})

	// Without accesses, the idle age is the TTL.
	t.Run("lapses", func(t *testing.T) {
		const idle = 20 * time.Millisecond
		c := New[string, string](MaxIdleAge(idle))
		c.Get("foo", func() (string, error) { return "bar", nil })
		time.Sleep(3 * idle)
		v, err, s := c.TryGet("foo")
		vcheck(t, got[string]{v, err, s}, got[string]{"", nil, Miss})
	})

	// With MaxAge, an access resets the expiry to now + idle age, whether
	// that is later or earlier than the MaxAge expiry.
	t.Run("with_max_age", func(t *testing.T) {
		c := New[string, string](MaxAge(time.Minute), MaxIdleAge(time.Hour))
		c.Get("foo", func() (string, error) { return "bar", nil })
		w0 := expiresOf(c)
		c.TryGet("foo")
		if w1 := expiresOf(c); w1 < w0+int64(50*time.Minute) {
			t.Fatalf("access did not extend past MaxAge: %d vs %d", w1, w0)
		}

		c = New[string, string](MaxAge(time.Hour), MaxIdleAge(time.Minute))
		c.Get("foo", func() (string, error) { return "bar", nil })
		w0 = expiresOf(c)
		c.TryGet("foo")
		if w1 := expiresOf(c); w1 > w0-int64(50*time.Minute) {
			t.Fatalf("access did not shorten to the idle age: %d vs %d", w1, w0)
		}
	})

	// MaxIdleAge doesn't extend errors.
	t.Run("no_extend_errors", func(t *testing.T) {
		c := New[string, string](MaxAge(time.Hour), MaxIdleAge(time.Hour))
		c.Get("foo", func() (string, error) { return "", errors.New("err") })
		w0 := expiresOf(c)
		tick()
		v, err, s := c.TryGet("foo")
		vcheck(t, got[string]{v, err, s}, got[string]{"", errors.New("err"), Hit})
		c.Get("foo", func() (string, error) { return "bar", nil })
		if w1 := expiresOf(c); w1 != w0 {
			t.Fatalf("errored entry was extended: %d != %d", w1, w0)
		}
	})

	// MaxIdleAge must not resurrect entries that finalize already expired:
	// with MaxAge(0), the Get that drives the load returns the value but
	// does not extend it.
	t.Run("no_resurrect_expired", func(t *testing.T) {
		c := New[string, string](MaxAge(0), MaxIdleAge(time.Hour))
		v, err, s := c.Get("foo", func() (string, error) { return "bar", nil })
		vcheck(t, got[string]{v, err, s}, got[string]{"bar", nil, Miss})
		v, err, s = c.TryGet("foo")
		vcheck(t, got[string]{v, err, s}, got[string]{"", nil, Miss})
	})

	// Range doesn't extend.
	t.Run("range_no_extend", func(t *testing.T) {
		c := New[string, string](MaxAge(time.Hour), MaxIdleAge(time.Hour))
		c.Get("foo", func() (string, error) { return "bar", nil })
		w0 := expiresOf(c)
		tick()
		var n int
		c.Range(func(string, string, error) bool { n++; return true })
		if n != 1 {
			t.Fatalf("Range saw %d keys, want 1", n)
		}
		if w1 := expiresOf(c); w1 != w0 {
			t.Fatalf("Range extended: %d != %d", w1, w0)
		}
	})
}

// TestMaxErrorAge_DefaultsToIdleInitialTTL checks that with only MaxIdleAge
// set, a load error is cached for the idle age (not forever), and the next
// Get after it lapses retries.
func TestMaxErrorAge_DefaultsToIdleInitialTTL(t *testing.T) {
	boom := errors.New("boom")
	{
		c := New[string, int](MaxIdleAge(time.Hour))
		c.Get("k", func() (int, error) { return 0, boom })
		if _, err, s := c.TryGet("k"); err == nil || !s.IsHit() {
			t.Fatalf("TryGet within the idle age: err=%v s=%v, want the cached error as a Hit", err, s)
		}
		if w := c.t.loadEntry("k").p.Load().expires.Load(); w <= 0 {
			t.Fatalf("error expiry = %d, want the idle age from now", w)
		}
	}

	const idle = 20 * time.Millisecond
	c := New[string, int](MaxIdleAge(idle))
	c.Get("k", func() (int, error) { return 0, boom })
	time.Sleep(3 * idle)
	if _, _, s := c.TryGet("k"); !s.IsMiss() {
		t.Fatalf("TryGet after the idle age: %v, want Miss", s)
	}
	var retried bool
	v, err, s := c.Get("k", func() (int, error) { retried = true; return 7, nil })
	if !retried || v != 7 || err != nil || s != Miss {
		t.Fatalf("Get after the error lapsed: retried=%v (%d, %v, %v), want a fresh (7, nil, Miss) load", retried, v, err, s)
	}
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

// waitFinalized waits until k's value is not loading.
func waitFinalized[K comparable, V any](t *testing.T, tr *trie[K, loading[V]], k K) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if e := tr.loadEntry(k); e != nil {
			if l := e.p.Load(); l != nil && l.finalized() {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%v never finished loading", k)
		}
		time.Sleep(time.Millisecond)
	}
}

// trieEntries counts every entry in the trie, including deleted entries and
// entries that are invisible to Range and TryGet.
func trieEntries[K comparable, V any](c *Cache[K, V]) (n int) {
	c.t.walk(func(*ent[K, V]) bool { n++; return true })
	return n
}

// TestClean_ReclaimsBornExpired verifies Clean's handling of born-expired
// entries (MaxAge(0) / MaxErrorAge(0), whose negative expiry word carries
// the negated birth): an entry is removed exactly when no read can see it
// anymore (as soon as TryGet reports Miss). That is immediately for errored
// stale-less entries, and when the birth-anchored stale window closes for
// values, regardless of process uptime.
//
// Regression test: born-expired entries used to store a bare -1 sentinel,
// and the grace window was computed as satAdd(-1, maxStaleAge), anchoring
// it at the process epoch: born-expired stale-less entries (error churn
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
// (MaxAge(0)) stale window is anchored at the value's birth (carried by
// the negated-birth expiry word) for every read and refresh. Once the
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
	start := time.Now()
	c.Set("k", 1)

	// Within the window, the value is servable as a stale. We only check
	// if we were not descheduled past the window.
	if v, _, s := c.TryGet("k"); time.Since(start) < age && (s != Stale || v != 1) {
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
// itself expires, every read serves that value as the stale: not the
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
	waitFinalized(t, &c.t, "k")
	time.Sleep(2 * ttl) // v2 expired

	if v, _, s := c.TryGet("k"); s != Stale || v != 2 {
		t.Fatalf("TryGet after v2 expired: (%d, %v), want (2, Stale): the freshest dead value", v, s)
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
	const age = time.Minute
	c := New[string, int](MaxAge(time.Millisecond), MaxIdleAge(time.Hour), MaxStaleAge(age))
	c.Set("k", 1)
	l := c.t.loadEntry("k").p.Load()
	w0 := l.expires.Load()
	if _, _, s := c.TryGet("k"); s != Hit {
		// We were descheduled past the 1ms MaxAge; extend manually.
		l.expires.Store(now() + int64(time.Hour))
	}
	w1 := l.expires.Load()
	if w1 <= w0+int64(age) {
		t.Fatalf("precondition: extension %d not past the initial window end %d", w1, w0+int64(age))
	}

	// Half way into the window after the extended expiry, the initial
	// window is long closed.
	if v, _, s := loadingTryGet(l, w1+int64(age)/2, 0, age); s != Stale || v != 1 {
		t.Fatalf("read inside the extended entry's window: (%d, %v), want (1, Stale)", v, s)
	}
	if _, _, s := loadingTryGet(l, w1+int64(age), 0, age); s != Miss {
		t.Fatalf("read after the extended entry's window: %v, want Miss", s)
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
	start := time.Now()
	c.Expire("k")
	if v, _, s := c.TryGet("k"); time.Since(start) < age && (s != Stale || v != 1) {
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
// generation) is still inside its window is visible to reads (TryGet
// serves the companion) so Clean must not reclaim it.
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
	if st, ok := maybeNewStale(c.t.loadEntry("k").p.Load(), age); ok {
		t.Fatalf("maybeNewStale snapshotted a claimed loading: %+v", st)
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

// An idle extension that would move the expiry by less than the slack is
// skipped; a larger one is written.
func TestMaxIdleAge_Slack(t *testing.T) {
	const age = time.Hour
	l := &loading[int]{}
	base := now() + int64(age)
	l.expires.Store(base)

	l.extend(base, base-int64(age)+int64(maxIdleSlack)/2, age)
	if got := l.expires.Load(); got != base {
		t.Fatalf("extension within the slack was written: %d != %d", got, base)
	}
	n := base - int64(age) + 2*int64(maxIdleSlack)
	l.extend(base, n, age)
	if got := l.expires.Load(); got != n+int64(age) {
		t.Fatalf("extension past the slack: expiry %d, want %d", got, n+int64(age))
	}

	// A small age uses age/64 as the slack.
	const small = 32 * time.Millisecond
	l.expires.Store(base)
	n = base - int64(small) + int64(small)/32
	l.extend(base, n, small)
	if got := l.expires.Load(); got != n+int64(small) {
		t.Fatalf("small-age extension past age/64: expiry %d, want %d", got, n+int64(small))
	}
}

// Lookups must not make the key escape: a key converted from a []byte, as
// with a builtin map lookup, must not allocate.
func TestLookupKeyDoesNotEscape(t *testing.T) {
	c := New[string, int](MaxAge(time.Hour))
	c.Set("abc", 1)
	b := []byte("abc")
	for name, f := range map[string]func(){
		"TryGet":           func() { c.TryGet(string(b)) },
		"Expire":           func() { c.Expire(string(b) + "x") },
		"Delete":           func() { c.Delete(string(b) + "x") },
		"CompareAndSwap":   func() { c.CompareAndSwap(string(b), 2, 3) },
		"CompareAndDelete": func() { c.CompareAndDelete(string(b), 2) },
	} {
		if n := testing.AllocsPerRun(100, f); n != 0 {
			t.Errorf("%s allocated %v times, want 0", name, n)
		}
	}
}

// Clean does not allocate, however much it deletes.
func TestCleanDoesNotAllocate(t *testing.T) {
	c := New[int, int](MaxAge(0))
	for i := range 1000 {
		c.Set(i, i)
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	c.Clean()
	runtime.ReadMemStats(&after)
	if n := after.Mallocs - before.Mallocs; n != 0 {
		t.Errorf("Clean deleting 1000 entries allocated %d times, want 0", n)
	}
	if n := trieEntries(c); n != 0 {
		t.Fatalf("Clean left %d entries, want 0", n)
	}
}

// A refresh after an error that has no stale carries no stale, even with
// stale values enabled.
func TestRefreshAfterErrorWithoutStale(t *testing.T) {
	c := New[int, int](MaxStaleAge(time.Hour), MaxErrorAge(0))
	if _, err, s := c.Get(1, func() (int, error) { return 0, errors.New("boom") }); err == nil || s != Miss {
		t.Fatalf("first Get: (%v, %v), want (boom, Miss)", err, s)
	}
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Get(1, func() (int, error) { <-release; return 2, nil })
	}()
	waitForState(t, c, 1, func(l *loading[int]) bool { return !l.finalized() })
	if st := c.t.loadEntry(1).p.Load().stale(); st != nil {
		t.Fatalf("refresh after an error with no stale carries stale %v", st.v)
	}
	close(release)
	<-done
	if v, _, s := c.TryGet(1); v != 2 || s != Hit {
		t.Fatalf("TryGet: (%d, %v), want (2, Hit)", v, s)
	}
}

// waitForState waits until k's loading satisfies ok.
func waitForState(t *testing.T, c *Cache[int, int], k int, ok func(*loading[int]) bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if e := c.t.loadEntry(k); e != nil {
			if l := e.p.Load(); l != nil && ok(l) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("loading never reached the wanted state")
		}
		time.Sleep(time.Millisecond)
	}
}
