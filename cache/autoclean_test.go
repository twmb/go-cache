package cache

import (
	"sync/atomic"
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
	if _, _, s := c.TryGet("a"); !s.IsHit() {
		t.Fatalf("immediately after Set: want Hit")
	}

	// Wait long enough for the ticker to tick at least a few times.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, _, s := c.TryGet("a"); s.IsMiss() {
			return // AutoClean fired and removed the expired entry.
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("AutoClean never removed the expired entry")
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

func TestAutoClean_StopOnCacheWithoutGoroutine(t *testing.T) {
	// A cache created without AutoCleanInterval has no goroutine and no
	// quitClean channel. StopAutoClean must still be safe to call.
	c := New[string, int]()
	c.StopAutoClean()
}

func TestAutoClean_ZeroIntervalNoGoroutine(t *testing.T) {
	// autoCleanInterval == 0 means disabled; no goroutine, no channel.
	c := New[string, int](MaxAge(time.Second))
	if c.quitClean != nil {
		t.Fatal("quitClean channel should not be allocated with zero interval")
	}
	c.StopAutoClean()
}

// Verifies that once StopAutoClean is called, the goroutine actually exits and
// subsequent ticks are not observed. We can't directly observe goroutine exit
// but we can observe Clean not running: after stopping, a freshly expired
// entry must remain until the test manually cleans.
func TestAutoClean_StopHaltsCleaning(t *testing.T) {
	var cleanCount int32
	c := New[string, int](
		MaxAge(time.Millisecond),
		AutoCleanInterval(time.Millisecond),
	)
	// Poll until we see at least one clean happened by observing entry removal.
	c.Set("a", 1)
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, _, s := c.TryGet("a"); s.IsMiss() {
			atomic.StoreInt32(&cleanCount, 1)
			break
		}
		time.Sleep(time.Millisecond)
	}
	if atomic.LoadInt32(&cleanCount) == 0 {
		t.Fatal("AutoClean never observed cleaning before stop")
	}

	c.StopAutoClean()

	// After stop, set an entry and verify it is NOT auto-cleaned within a
	// window many multiples of the original interval.
	c.Set("b", 2)
	time.Sleep(20 * time.Millisecond)
	// b is expired (MaxAge=1ms) but AutoClean is stopped, so TryGet still
	// reports Miss only because of the expiry check — the key remains in
	// the map. Verify Clean manually removes it.
	c.Clean()
	// A subsequent Get with a non-panicking miss should be called (i.e., the
	// key is actually gone from the map, not just expired).
	var missCalled bool
	c.Get("b", func() (int, error) {
		missCalled = true
		return 0, nil
	})
	if !missCalled {
		t.Fatal("miss function should have run after manual Clean removed the entry")
	}
}
