package cache

import (
	"runtime"
	"testing"
	"time"
)

func TestAutoClean_RunsAndCleansExpired(t *testing.T) {
	c := New[string, int](
		MaxAge(time.Millisecond),
		AutoCleanInterval(2*time.Millisecond),
	)
	defer c.StopAutoClean()

	c.Set("a", 1)
	waitEntries(t, c, 0)
}

// waitEntries waits until the trie holds n entries.
func waitEntries[K comparable, V any](t *testing.T, c *Cache[K, V], n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for trieEntries(c) != n {
		if time.Now().After(deadline) {
			t.Fatalf("trie has %d entries, want %d", trieEntries(c), n)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestAutoClean_DisabledWhenStaleAgeNegative(t *testing.T) {
	// AutoCleanInterval is ignored when MaxStaleAge < 0 (stales kept forever).
	// We verify no goroutine is spawned by confirming StopAutoClean is a no-op
	// on a cache whose quitClean channel was never created.
	c := New[string, int](
		MaxAge(time.Millisecond),
		MaxStaleAge(-1),
		AutoCleanInterval(time.Millisecond),
	)
	if c.quitClean != nil {
		t.Fatal("quitClean channel should not be allocated when MaxStaleAge<0")
	}
	c.StopAutoClean() // no-op must not panic
}

func TestAutoClean_StopIdempotent(t *testing.T) {
	c := New[string, int](
		MaxAge(time.Millisecond),
		AutoCleanInterval(time.Millisecond),
	)
	c.StopAutoClean()
	c.StopAutoClean() // sync.Once guards the close; second call must be safe.
}

func TestAutoClean_ZeroIntervalNoGoroutine(t *testing.T) {
	// autoCleanInterval == 0 means disabled; no goroutine, no channel.
	c := New[string, int](MaxAge(time.Second))
	if c.quitClean != nil {
		t.Fatal("quitClean channel should not be allocated with zero interval")
	}
	c.StopAutoClean()
}

// Once StopAutoClean returns, no Clean runs.
func TestAutoClean_StopHaltsCleaning(t *testing.T) {
	c := New[string, int](
		MaxAge(time.Millisecond),
		AutoCleanInterval(time.Millisecond),
	)
	c.Set("a", 1)
	waitEntries(t, c, 0)
	c.StopAutoClean()

	c.Set("b", 2)
	time.Sleep(20 * time.Millisecond)
	if n := trieEntries(c); n != 1 {
		t.Fatalf("trie has %d entries after StopAutoClean, want 1", n)
	}
	c.Clean()
	if n := trieEntries(c); n != 0 {
		t.Fatalf("trie has %d entries after Clean, want 0", n)
	}
}

// A cache that is never stopped is still garbage collected, and its clean
// goroutine exits.
func TestAutoClean_ExitsWhenCacheCollected(t *testing.T) {
	for _, newDone := range []func() chan struct{}{
		func() chan struct{} { return New[int, int](AutoCleanInterval(time.Millisecond)).cleanDone },
		func() chan struct{} { return NewItem[int](AutoCleanInterval(time.Millisecond)).c.cleanDone },
		func() chan struct{} { return NewSet[int](AutoCleanInterval(time.Millisecond)).c.cleanDone },
	} {
		done := newDone()
		deadline := time.Now().Add(10 * time.Second)
		for exited := false; !exited; {
			runtime.GC()
			select {
			case <-done:
				exited = true
			case <-time.After(10 * time.Millisecond):
				if time.Now().After(deadline) {
					t.Fatal("clean goroutine did not exit after its cache became unreachable")
				}
			}
		}
	}
}
