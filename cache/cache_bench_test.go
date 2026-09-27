package cache

import (
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func benchGetHit(b *testing.B, opts ...Opt) {
	c := New[int, int](opts...)
	defer c.StopAutoClean()
	const keys = 1024
	for i := range keys {
		c.Set(i, i)
	}
	miss := func() (int, error) { return 0, nil }
	b.RunParallel(func(pb *testing.PB) {
		var i int
		for pb.Next() {
			c.Get(i%keys, miss)
			i++
		}
	})
}

func BenchmarkGetHit(b *testing.B)        { benchGetHit(b) }
func BenchmarkGetHitMaxAge(b *testing.B)  { benchGetHit(b, MaxAge(time.Hour)) }
func BenchmarkGetHitIdleAge(b *testing.B) { benchGetHit(b, MaxIdleAge(time.Hour)) }

func BenchmarkGetHitOneKey(b *testing.B)        { benchGetHitOneKey(b) }
func BenchmarkGetHitOneKeyIdleAge(b *testing.B) { benchGetHitOneKey(b, MaxIdleAge(time.Hour)) }

func benchGetHitOneKey(b *testing.B, opts ...Opt) {
	c := New[int, int](opts...)
	c.Set(0, 0)
	miss := func() (int, error) { return 0, nil }
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			c.Get(0, miss)
		}
	})
}

func BenchmarkTryGetHitMaxAge(b *testing.B) {
	c := New[int, int](MaxAge(time.Hour))
	const keys = 1024
	for i := range keys {
		c.Set(i, i)
	}
	b.RunParallel(func(pb *testing.PB) {
		var i int
		for pb.Next() {
			c.TryGet(i % keys)
			i++
		}
	})
}

func BenchmarkGetMissUnique(b *testing.B) {
	c := New[int, int]()
	var ctr atomic.Int64
	miss := func() (int, error) { return 0, nil }
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			c.Get(int(ctr.Add(1)), miss)
		}
	})
}

func BenchmarkGetRefreshExpired(b *testing.B) {
	c := New[int, int](MaxAge(0))
	miss := func() (int, error) { return 0, nil }
	b.RunParallel(func(pb *testing.PB) {
		var i int
		for pb.Next() {
			c.Get(i%1024, miss)
			i++
		}
	})
}

func BenchmarkSetExisting(b *testing.B) {
	c := New[int, int](MaxAge(time.Hour))
	const keys = 1024
	for i := range keys {
		c.Set(i, i)
	}
	b.RunParallel(func(pb *testing.PB) {
		var i int
		for pb.Next() {
			c.Set(i%keys, i)
			i++
		}
	})
}

func BenchmarkDeleteSet(b *testing.B) {
	c := New[int, int]()
	var ctr atomic.Int64
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			k := int(ctr.Add(1)) % 4096
			c.Set(k, k)
			c.Delete(k)
		}
	})
}

func BenchmarkClean(b *testing.B) {
	c := New[int, int](MaxAge(time.Hour))
	for i := range 4096 {
		c.Set(i, i)
	}
	b.ResetTimer()
	for range b.N {
		c.Clean()
	}
}

// BenchmarkGetRefreshStale measures a refresh that returns the stale value
// while the load runs in the background, waiting for each load to finish.
func BenchmarkGetRefreshStale(b *testing.B) {
	c := New[int, int](MaxAge(0), MaxStaleAge(time.Hour))
	c.Set(0, 0)
	miss := func() (int, error) { return 0, nil }
	for range b.N {
		c.Get(0, miss)
		for !c.t.loadEntry(0).p.Load().finalized() {
			runtime.Gosched()
		}
	}
}

// BenchmarkGetHitCapturingMiss is GetHit with the miss closure a real caller
// writes: one that captures the key.
func BenchmarkGetHitCapturingMiss(b *testing.B) {
	c := New[int, int](MaxAge(time.Hour))
	const keys = 1024
	for i := range keys {
		c.Set(i, i)
	}
	b.RunParallel(func(pb *testing.PB) {
		var i int
		for pb.Next() {
			k := i % keys
			c.Get(k, func() (int, error) { return k, nil })
			i++
		}
	})
}
