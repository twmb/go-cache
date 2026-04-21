// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cache

import (
	"math/rand"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"testing/quick"
)

type mapOp string

const (
	opLoad             = mapOp("Load")
	opStore            = mapOp("Store")
	opLoadOrStore      = mapOp("LoadOrStore")
	opLoadAndDelete    = mapOp("LoadAndDelete")
	opDelete           = mapOp("Delete")
	opSwap             = mapOp("Swap")
	opCompareAndSwap   = mapOp("CompareAndSwap")
	opCompareAndDelete = mapOp("CompareAndDelete")
	opClear            = mapOp("Clear")
)

var mapOps = [...]mapOp{
	opLoad,
	opStore,
	opLoadOrStore,
	opLoadAndDelete,
	opDelete,
	opSwap,
	opCompareAndSwap,
	opCompareAndDelete,
	opClear,
}

// mapCall is a quick.Generator for calls on mapInterface.
type mapCall struct {
	op mapOp
	k  string
	v  any
}

func (c mapCall) apply(m mapInterface[string]) (any, bool) {
	switch c.op {
	case opLoad:
		return m.Load(c.k)
	case opStore:
		m.Store(c.k, c.v)
		return nil, false
	case opLoadOrStore:
		return m.LoadOrStore(c.k, c.v)
	case opLoadAndDelete:
		return m.LoadAndDelete(c.k)
	case opDelete:
		m.Delete(c.k)
		return nil, false
	case opSwap:
		return m.Swap(c.k, c.v)
	case opCompareAndSwap:
		if m.CompareAndSwap(c.k, c.v, rand.Int()) {
			m.Delete(c.k)
			return c.v, true
		}
		return nil, false
	case opCompareAndDelete:
		if m.CompareAndDelete(c.k, c.v) {
			if _, ok := m.Load(c.k); !ok {
				return nil, true
			}
		}
		return nil, false
	case opClear:
		m.Clear()
		return nil, false
	default:
		panic("invalid mapOp")
	}
}

type mapResult struct {
	value any
	ok    bool
}

func randValue(r *rand.Rand) string {
	b := make([]byte, r.Intn(4))
	for i := range b {
		b[i] = 'a' + byte(rand.Intn(26))
	}
	return string(b)
}

func (mapCall) Generate(r *rand.Rand, size int) reflect.Value {
	c := mapCall{op: mapOps[rand.Intn(len(mapOps))], k: randValue(r)}
	switch c.op {
	case opStore, opLoadOrStore:
		c.v = randValue(r)
	}
	return reflect.ValueOf(c)
}

func applyCalls(m mapInterface[string], calls []mapCall) (results []mapResult, final map[string]any) {
	for _, c := range calls {
		v, ok := c.apply(m)
		results = append(results, mapResult{v, ok})
	}

	final = make(map[string]any)
	m.Range(func(k string, v any) bool {
		final[k] = v
		return true
	})

	return results, final
}

func applyCache(calls []mapCall) ([]mapResult, map[string]any) {
	return applyCalls(new(CacheMap[string]), calls)
}

func applyRWMutexMap(calls []mapCall) ([]mapResult, map[string]any) {
	return applyCalls(new(RWMutexMap[string]), calls)
}

func TestCacheMatchesRWMutex(t *testing.T) {
	if err := quick.CheckEqual(applyCache, applyRWMutexMap, nil); err != nil {
		t.Error(err)
	}
}

func TestConcurrentRange(t *testing.T) {
	const mapSize = 1 << 10

	var m CacheMap[int64]
	for n := int64(1); n <= mapSize; n++ {
		m.Store(n, int64(n))
	}

	done := make(chan struct{})
	var wg sync.WaitGroup
	defer func() {
		close(done)
		wg.Wait()
	}()
	for g := int64(runtime.GOMAXPROCS(0)); g > 0; g-- {
		r := rand.New(rand.NewSource(g))
		wg.Add(1)
		go func(g int64) {
			defer wg.Done()
			for i := int64(0); ; i++ {
				select {
				case <-done:
					return
				default:
				}
				for n := int64(1); n < mapSize; n++ {
					if r.Int63n(mapSize) == 0 {
						m.Store(n, n*i*g)
					} else {
						m.Load(n)
					}
				}
			}
		}(g)
	}

	iters := 1 << 10
	if testing.Short() {
		iters = 16
	}
	for n := iters; n > 0; n-- {
		seen := make(map[int64]bool, mapSize)

		m.Range(func(k int64, vi any) bool {
			v := vi.(int64)
			if v%k != 0 {
				t.Fatalf("while Storing multiples of %v, Range saw value %v", k, v)
			}
			if seen[k] {
				t.Fatalf("Range visited key %v twice", k)
			}
			seen[k] = true
			return true
		})

		if len(seen) != mapSize {
			t.Fatalf("Range visited %v elements of %v-element Map", len(seen), mapSize)
		}
	}
}

func TestIssue40999(t *testing.T) {
	var m CacheMap[*int]

	// Since the miss-counting in missLocked (via Delete)
	// compares the miss count with len(m.dirty),
	// add an initial entry to bias len(m.dirty) above the miss count.
	m.Store(nil, struct{}{})

	var finalized uint32

	// Set finalizers that count for collected keys. A non-zero count
	// indicates that keys have not been leaked.
	for atomic.LoadUint32(&finalized) == 0 {
		p := new(int)
		runtime.SetFinalizer(p, func(*int) {
			atomic.AddUint32(&finalized, 1)
		})
		m.Store(p, struct{}{})
		m.Delete(p)
		runtime.GC()
	}
}

func TestMapRangeNestedCall(t *testing.T) { // Issue 46399
	var m CacheMap[int]
	for i, v := range [3]string{"hello", "world", "Go"} {
		m.Store(i, v)
	}
	m.Range(func(key int, value any) bool {
		m.Range(func(key int, value any) bool {
			// We should be able to load the key offered in the Range callback,
			// because there are no concurrent Delete involved in this tested map.
			if v, ok := m.Load(key); !ok || !reflect.DeepEqual(v, value) {
				t.Fatalf("Nested Range loads unexpected value, got %+v want %+v", v, value)
			}

			// We didn't keep 42 and a value into the map before, if somehow we loaded
			// a value from such a key, meaning there must be an internal bug regarding
			// nested range in the Map.
			if _, loaded := m.LoadOrStore(42, "dummy"); loaded {
				t.Fatalf("Nested Range loads unexpected value, want store a new value")
			}

			// Try to Store then LoadAndDelete the corresponding value with the key
			// 42 to the Map. In this case, the key 42 and associated value should be
			// removed from the Map. Therefore any future range won't observe key 42
			// as we checked in above.
			val := "CacheMap"
			m.Store(42, val)
			if v, loaded := m.LoadAndDelete(42); !loaded || !reflect.DeepEqual(v, val) {
				t.Fatalf("Nested Range loads unexpected value, got %v, want %v", v, val)
			}
			return true
		})

		// Remove key from Map on-the-fly.
		m.Delete(key)
		return true
	})

	// After a Range of Delete, all keys should be removed and any
	// further Range won't invoke the callback. Hence length remains 0.
	length := 0
	m.Range(func(key int, value any) bool {
		length++
		return true
	})

	if length != 0 {
		t.Fatalf("Unexpected CacheMap size, got %v want %v", length, 0)
	}
}

func TestCompareAndSwap_NonExistingKey(t *testing.T) {
	var m CacheMap[int]
	if m.CompareAndSwap(0, nil, 42) {
		// See https://go.dev/issue/51972#issuecomment-1126408637.
		t.Fatalf("CompareAndSwap on an non-existing key succeeded")
	}
}

// TestConcurrentClear — adapted from sync/map_test.go. Spawns 10 writers,
// 10 readers, and 10 Clear goroutines; correctness is that nothing panics
// or trips the race detector.
func TestConcurrentClear(t *testing.T) {
	var m CacheMap[int]

	var wg sync.WaitGroup
	wg.Add(30)

	for i := range 10 {
		go func(k, v int) {
			defer wg.Done()
			m.Store(k, v)
		}(i, i*10)
	}
	for i := range 10 {
		go func(k int) {
			defer wg.Done()
			if _, ok := m.Load(k); ok {
				_ = ok
			}
		}(i)
	}
	for range 10 {
		go func() {
			defer wg.Done()
			m.Clear()
		}()
	}

	wg.Wait()

	// After all Clears and Stores have run, the map may be empty or contain a
	// subset of the Stores depending on interleaving. Verify no phantom keys
	// (keys we never Stored) materialized.
	m.Range(func(k int, _ any) bool {
		if k < 0 || k >= 10 {
			t.Errorf("Range after concurrent Clear/Store saw key %d, never Stored", k)
		}
		return true
	})
}

// TestMapClearOneAllocation — adapted from sync/map_test.go. Cache.Clear
// replaces the trie root with one fresh indirect node, which should be the
// only allocation.
func TestMapClearOneAllocation(t *testing.T) {
	var m CacheMap[int]
	// Prime so the trie has been initialized; Clear of an uninitialized
	// trie still calls init which allocates more.
	m.Store(0, 0)
	allocs := testing.AllocsPerRun(10, func() {
		m.Clear()
	})
	if allocs > 1 {
		t.Errorf("AllocsPerRun of Clear = %v; want 1", allocs)
	}
}

// TestMapRangeNoAllocations — adapted from sync/map_test.go. Range must not
// allocate.
func TestMapRangeNoAllocations(t *testing.T) {
	var m CacheMap[int]
	allocs := testing.AllocsPerRun(10, func() {
		m.Range(func(k int, _ any) bool {
			return true
		})
	})
	if allocs > 0 {
		t.Errorf("AllocsPerRun of Range = %v; want 0", allocs)
	}
}
