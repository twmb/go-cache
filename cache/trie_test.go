package cache

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
)

// Helper: store or replace the value slot with a fresh pointer carrying v.
// The trie owns the entry; the caller owns the value pointer identity.
func triePut[K comparable, V any](t *trie[K, V], k K, v V) *V {
	p := &v
	e, _ := t.loadOrStoreEntry(k, p)
	e.p.Store(p)
	return p
}

func trieGet[K comparable, V any](t *trie[K, V], k K) (V, bool) {
	e := t.loadEntry(k)
	if e == nil {
		return *new(V), false
	}
	p := e.p.Load()
	if p == nil {
		return *new(V), false
	}
	return *p, true
}

func TestTrie_ZeroValueLoad(t *testing.T) {
	var tr trie[string, int]
	if e := tr.loadEntry("missing"); e != nil {
		t.Fatalf("loadEntry on zero-value trie returned %v, want nil", e)
	}
	var visited int
	tr.walk(func(*trieEntry[string, int]) bool { visited++; return true })
	if visited != 0 {
		t.Fatalf("walk on zero-value trie visited %d, want 0", visited)
	}
	if e := tr.deleteEntry("missing"); e != nil {
		t.Fatalf("deleteEntry on zero-value trie returned %v, want nil", e)
	}
}

func TestTrie_StoreLoad(t *testing.T) {
	var tr trie[string, int]
	triePut(&tr, "a", 1)
	triePut(&tr, "b", 2)
	if v, ok := trieGet(&tr, "a"); !ok || v != 1 {
		t.Fatalf("get a: v=%d ok=%v, want 1 true", v, ok)
	}
	if v, ok := trieGet(&tr, "b"); !ok || v != 2 {
		t.Fatalf("get b: v=%d ok=%v, want 2 true", v, ok)
	}
	if _, ok := trieGet(&tr, "c"); ok {
		t.Fatal("get c should be missing")
	}
}

func TestTrie_LoadOrStoreExisting(t *testing.T) {
	var tr trie[string, int]
	v1 := 1
	e1, loaded := tr.loadOrStoreEntry("k", &v1)
	if loaded {
		t.Fatal("first loadOrStoreEntry should not report loaded")
	}
	v2 := 2
	e2, loaded := tr.loadOrStoreEntry("k", &v2)
	if !loaded {
		t.Fatal("second loadOrStoreEntry should report loaded")
	}
	if e1 != e2 {
		t.Fatal("loadOrStoreEntry should return the same entry object on subsequent calls")
	}
	// The stored value is v1's pointer; v2 was ignored.
	if got := *e2.p.Load(); got != 1 {
		t.Fatalf("stored value = %d, want 1 (new value must not overwrite)", got)
	}
}

func TestTrie_LoadOrStoreWithNilValue(t *testing.T) {
	// Cache always creates entries with a value in place (a nil slot means
	// tombstoned), but the trie itself is value-agnostic: verify
	// loadOrStoreEntry(k, nil) is legal and the entry.p is nil.
	var tr trie[string, int]
	e, loaded := tr.loadOrStoreEntry("k", nil)
	if loaded {
		t.Fatal("expected not-loaded for first insert")
	}
	if e.p.Load() != nil {
		t.Fatal("nil-initialized entry should have nil value slot")
	}
	v := 42
	e.p.Store(&v)
	if got, ok := trieGet(&tr, "k"); !ok || got != 42 {
		t.Fatalf("after Store: got=%d ok=%v, want 42 true", got, ok)
	}
}

func TestTrie_Delete(t *testing.T) {
	var tr trie[string, int]
	triePut(&tr, "a", 1)
	triePut(&tr, "b", 2)

	e := tr.deleteEntry("a")
	if e == nil || e.key != "a" {
		t.Fatalf("deleteEntry(a) = %v, want entry for a", e)
	}
	if _, ok := trieGet(&tr, "a"); ok {
		t.Fatal("after delete, a should be missing")
	}
	if _, ok := trieGet(&tr, "b"); !ok {
		t.Fatal("after delete of a, b should still be present")
	}
	if e := tr.deleteEntry("a"); e != nil {
		t.Fatalf("second deleteEntry(a) = %v, want nil", e)
	}
}

func TestTrie_DeletePrunesEmptyParents(t *testing.T) {
	// Insert many keys so the tree grows, then delete them all. After, the
	// root should be empty (all children nil).
	var tr trie[int, int]
	const n = 1000
	for i := range n {
		triePut(&tr, i, i)
	}
	for i := range n {
		if e := tr.deleteEntry(i); e == nil {
			t.Fatalf("deleteEntry(%d) unexpectedly returned nil", i)
		}
	}
	root := tr.root.Load()
	for i := range root.children {
		if n := root.children[i].Load(); n != nil {
			t.Fatalf("root.children[%d] = %v, want nil (pruning should have cleared)", i, n)
		}
	}
}

func TestTrie_Walk(t *testing.T) {
	var tr trie[int, int]
	const n = 256
	for i := range n {
		triePut(&tr, i, i*10)
	}
	seen := make(map[int]int)
	tr.walk(func(e *trieEntry[int, int]) bool {
		if p := e.p.Load(); p != nil {
			seen[e.key] = *p
		}
		return true
	})
	if len(seen) != n {
		t.Fatalf("walk saw %d entries, want %d", len(seen), n)
	}
	for k, v := range seen {
		if v != k*10 {
			t.Fatalf("seen[%d] = %d, want %d", k, v, k*10)
		}
	}
}

func TestTrie_WalkEarlyStop(t *testing.T) {
	var tr trie[int, int]
	for i := range 100 {
		triePut(&tr, i, i)
	}
	var count int
	tr.walk(func(*trieEntry[int, int]) bool {
		count++
		return count < 5
	})
	if count != 5 {
		t.Fatalf("walk visited %d, want 5 (early stop)", count)
	}
}

func TestTrie_Clear(t *testing.T) {
	var tr trie[int, int]
	for i := range 100 {
		triePut(&tr, i, i)
	}
	tr.clear()
	for i := range 100 {
		if _, ok := trieGet(&tr, i); ok {
			t.Fatalf("after clear, key %d should be missing", i)
		}
	}
	var count int
	tr.walk(func(*trieEntry[int, int]) bool { count++; return true })
	if count != 0 {
		t.Fatalf("walk after clear visited %d, want 0", count)
	}
	// Trie is still usable after clear.
	triePut(&tr, 1, 1)
	if v, ok := trieGet(&tr, 1); !ok || v != 1 {
		t.Fatalf("reuse after clear: v=%d ok=%v", v, ok)
	}
}

// TestTrie_ManyKeys exercises the expand path repeatedly: random keys will
// routinely collide on short hash prefixes, forcing the trie to extend its
// depth. 20k keys is enough that most hash prefixes of length 4 will
// collide multiple times.
func TestTrie_ManyKeys(t *testing.T) {
	var tr trie[int, int]
	const n = 20_000
	keys := rand.Perm(n)
	for _, k := range keys {
		triePut(&tr, k, k*2)
	}
	for _, k := range keys {
		v, ok := trieGet(&tr, k)
		if !ok || v != k*2 {
			t.Fatalf("lookup(%d): v=%d ok=%v, want %d true", k, v, ok, k*2)
		}
	}
	// Delete half and re-verify.
	for i, k := range keys {
		if i%2 == 0 {
			tr.deleteEntry(k)
		}
	}
	for i, k := range keys {
		v, ok := trieGet(&tr, k)
		switch i % 2 {
		case 0:
			if ok {
				t.Fatalf("deleted key %d still present: v=%d", k, v)
			}
		case 1:
			if !ok || v != k*2 {
				t.Fatalf("surviving key %d: v=%d ok=%v, want %d true", k, v, ok, k*2)
			}
		}
	}
}

// TestTrie_FullHashCollision verifies the overflow-chain behavior by forcing
// two distinct keys to hash identically via the test-only hashFn hook.
func TestTrie_FullHashCollision(t *testing.T) {
	var tr trie[string, int]
	// Collide everything in a single hash bucket.
	tr.hashFn = func(string) uintptr { return 0xdeadbeef }

	const n = 10
	for i := range n {
		k := fmt.Sprintf("k%d", i)
		triePut(&tr, k, i)
	}
	// All n entries share one overflow chain under the same leaf slot.
	for i := range n {
		k := fmt.Sprintf("k%d", i)
		v, ok := trieGet(&tr, k)
		if !ok || v != i {
			t.Fatalf("get %q: v=%d ok=%v, want %d true", k, v, ok, i)
		}
	}
	// Delete one from the middle; the rest remain.
	tr.deleteEntry("k5")
	if _, ok := trieGet(&tr, "k5"); ok {
		t.Fatal("k5 should be deleted")
	}
	for i := range n {
		if i == 5 {
			continue
		}
		k := fmt.Sprintf("k%d", i)
		if _, ok := trieGet(&tr, k); !ok {
			t.Fatalf("%q should survive delete of k5", k)
		}
	}
}

// TestTrie_FullHashCollisionDeleteHead deletes the head of the overflow
// chain. The chain head is what the slot points at, so slot must be
// updated to the next entry.
func TestTrie_FullHashCollisionDeleteHead(t *testing.T) {
	var tr trie[string, int]
	tr.hashFn = func(string) uintptr { return 0x42 }
	triePut(&tr, "a", 1)
	triePut(&tr, "b", 2)
	triePut(&tr, "c", 3)

	// Delete the most-recently-inserted (head of chain, per expand logic).
	if e := tr.deleteEntry("c"); e == nil || e.key != "c" {
		t.Fatalf("delete c: %v", e)
	}
	for _, k := range []string{"a", "b"} {
		if _, ok := trieGet(&tr, k); !ok {
			t.Fatalf("%s missing after delete of chain head", k)
		}
	}
}

func TestTrie_LoadOrStoreDuringCollision(t *testing.T) {
	// After a collision chain exists, loadOrStoreEntry for an already-present
	// key must find it via the overflow walk, not append a duplicate.
	var tr trie[string, int]
	tr.hashFn = func(string) uintptr { return 0xabcd }
	triePut(&tr, "x", 1)
	triePut(&tr, "y", 2)
	v := 999
	e, loaded := tr.loadOrStoreEntry("x", &v)
	if !loaded {
		t.Fatal("second loadOrStoreEntry(x) should report loaded")
	}
	if got := *e.p.Load(); got != 1 {
		t.Fatalf("existing entry value=%d, want 1 (new value must be ignored)", got)
	}
}

// TestTrie_ConcurrentStoreLoad runs many goroutines performing
// loadOrStoreEntry and loadEntry over disjoint key ranges, verifying no
// race and eventual consistency.
func TestTrie_ConcurrentStoreLoad(t *testing.T) {
	var tr trie[int, int]
	const workers = 8
	const perWorker = 2000

	var wg sync.WaitGroup
	wg.Add(workers)
	for w := range workers {
		go func(w int) {
			defer wg.Done()
			base := w * perWorker
			for i := range perWorker {
				triePut(&tr, base+i, base+i)
			}
			for i := range perWorker {
				v, ok := trieGet(&tr, base+i)
				if !ok || v != base+i {
					t.Errorf("worker %d, key %d: v=%d ok=%v", w, base+i, v, ok)
					return
				}
			}
		}(w)
	}
	wg.Wait()
}

// TestTrie_ConcurrentSameKey hammers the same key with many goroutines
// doing loadOrStoreEntry. Exactly one must report loaded=false; all others
// must observe the same entry object.
func TestTrie_ConcurrentSameKey(t *testing.T) {
	const workers = 64
	for iter := range 100 {
		var tr trie[int, int]
		var wg sync.WaitGroup
		wg.Add(workers)
		entries := make([]*trieEntry[int, int], workers)
		loadeds := make([]bool, workers)
		for w := range workers {
			go func(w int) {
				defer wg.Done()
				v := w
				e, loaded := tr.loadOrStoreEntry(iter, &v)
				entries[w] = e
				loadeds[w] = loaded
			}(w)
		}
		wg.Wait()

		var inserted int
		for _, l := range loadeds {
			if !l {
				inserted++
			}
		}
		if inserted != 1 {
			t.Fatalf("iter %d: %d inserters, want exactly 1", iter, inserted)
		}
		first := entries[0]
		for i, e := range entries {
			if e != first {
				t.Fatalf("iter %d: worker %d saw entry %p, want %p", iter, i, e, first)
			}
		}
	}
}

// TestTrie_ConcurrentDeletePrune exercises concurrent deletes across many
// keys so parent pruning fires from many angles simultaneously.
func TestTrie_ConcurrentDeletePrune(t *testing.T) {
	var tr trie[int, int]
	const n = 10_000
	for i := range n {
		triePut(&tr, i, i)
	}
	const workers = 8
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := range workers {
		go func(w int) {
			defer wg.Done()
			for i := w; i < n; i += workers {
				tr.deleteEntry(i)
			}
		}(w)
	}
	wg.Wait()
	for i := range n {
		if _, ok := trieGet(&tr, i); ok {
			t.Fatalf("key %d should be deleted", i)
		}
	}
	// After all deletions, root should be empty.
	if root := tr.root.Load(); !root.empty() {
		t.Fatal("root not pruned: has surviving children")
	}
}

// TestTrie_RangeDuringMutation runs walk concurrently with store and
// delete. No snapshot is promised; we assert only no-crash + that every
// observed key-value pair is plausible (key was stored, value is key*2 or
// nil).
func TestTrie_RangeDuringMutation(t *testing.T) {
	var tr trie[int, int]
	const n = 2000
	for i := range n {
		triePut(&tr, i, i*2)
	}

	var stop atomic.Bool
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			i := rand.IntN(n)
			triePut(&tr, i, i*2)
			tr.deleteEntry(i)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			tr.walk(func(e *trieEntry[int, int]) bool {
				p := e.p.Load()
				if p != nil && *p != e.key*2 {
					t.Errorf("invariant break: key=%d value=%d", e.key, *p)
					return false
				}
				return true
			})
		}
	}()

	// Let the race run briefly.
	for range 50_000 {
		i := rand.IntN(n)
		if _, ok := trieGet(&tr, i); ok {
			// just a read
		}
	}
	stop.Store(true)
	wg.Wait()
}

// TestTrie_DeleteMissingInCollisionChain walks an overflow chain in search
// of a key that is never present, exercising deleteEntryIf's
// "walk the chain to the end without finding k" branch.
func TestTrie_DeleteMissingInCollisionChain(t *testing.T) {
	var tr trie[string, int]
	tr.hashFn = func(string) uintptr { return 0x1234 }
	triePut(&tr, "a", 1)
	triePut(&tr, "b", 2)
	triePut(&tr, "c", 3)
	// "missing" collides with a/b/c but was never stored.
	if e := tr.deleteEntry("missing"); e != nil {
		t.Fatalf("deleteEntry of never-stored colliding key returned %v, want nil", e)
	}
	// Original entries survive.
	for _, k := range []string{"a", "b", "c"} {
		if _, ok := trieGet(&tr, k); !ok {
			t.Fatalf("%s should survive delete of missing colliding key", k)
		}
	}
}

// TestTrie_DeleteRaceRetries stresses deleteEntry's lock-then-retry paths:
// the "parent is dead" retry and the "slot became nil after lock"
// early-return. Both fire when a concurrent delete prunes or empties the
// slot between our unlocked walk and our mu.Lock.
//
// The stress needs sustained concurrent deletes on many hash-adjacent
// keys so that pruning and slot clearing happen continuously.
func TestTrie_DeleteRaceRetries(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stress test in -short")
	}
	const workers = 16
	const keySpace = 256
	for range 200 {
		var tr trie[int, int]
		// Prime: every key is present.
		for i := range keySpace {
			triePut(&tr, i, i)
		}
		var wg sync.WaitGroup
		wg.Add(workers * 2)
		// Deleter swarms.
		for w := range workers {
			go func(w int) {
				defer wg.Done()
				for i := w; i < keySpace; i += workers {
					tr.deleteEntry(i)
				}
			}(w)
		}
		// Concurrent deleter attempting same keys (likely to race on lock).
		for w := range workers {
			go func(w int) {
				defer wg.Done()
				for i := keySpace - 1 - w; i >= 0; i -= workers {
					tr.deleteEntry(i)
				}
			}(w)
		}
		wg.Wait()
	}
}

// TestTrie_DeleteRaceAgainstExpand targets deleteEntryIf's "slot changed to
// non-entry after lock" branch. An indirect-node split (expand) during the
// window between delete's unlocked walk and its parent.Lock forces the
// reload to see a non-entry node, which must trigger a retry from the root
// (matching internal/sync.HashTrieMap's find), not a give-up: "a" is present
// for the whole round until the delete removes it, so deleteEntry("a") must
// always find and return it.
//
// We use a controlled hashFn that collides a few keys in the top bits but
// not all bits, so inserting them triggers expand. Concurrent insert+delete
// races through the expand window.
func TestTrie_DeleteRaceAgainstExpand(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stress test in -short")
	}
	// Keys whose hashes match in the top 4 bits but diverge later: they
	// share a slot at depth 1 and a leaf at depth 2 (via overflow then
	// expand).
	hashes := map[string]uintptr{
		"a": 0xA000000000000000,
		"b": 0xA000000000000001,
		"c": 0xA000000000000002,
		"d": 0xB000000000000000,
	}
	for i := range 20_000 {
		var tr trie[string, int]
		tr.hashFn = func(k string) uintptr { return hashes[k] }
		triePut(&tr, "a", 1)
		triePut(&tr, "d", 4)

		var wg sync.WaitGroup
		wg.Add(3)
		var deleted *trieEntry[string, int]
		go func() {
			defer wg.Done()
			triePut(&tr, "b", 2)
		}()
		go func() {
			defer wg.Done()
			triePut(&tr, "c", 3)
		}()
		go func() {
			defer wg.Done()
			deleted = tr.deleteEntry("a")
		}()
		wg.Wait()

		if deleted == nil || deleted.key != "a" {
			t.Fatalf("round %d: deleteEntry(a) = %v, want the entry for a (delete must retry across a concurrent expand)", i, deleted)
		}
		if e := tr.loadEntry("a"); e != nil {
			t.Fatalf("round %d: a still present after deleteEntry returned it", i)
		}
		for _, k := range []string{"b", "c", "d"} {
			if _, ok := trieGet(&tr, k); !ok {
				t.Fatalf("round %d: %s missing after delete of a", i, k)
			}
		}
	}
}

// TestTrie_DeleteDuringConcurrentStore: a goroutine repeatedly deletes key
// K while another repeatedly stores it. Both succeed; the final state is
// "either present or absent" and neither crashes.
func TestTrie_DeleteDuringConcurrentStore(t *testing.T) {
	var tr trie[int, int]
	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			triePut(&tr, 7, 42)
		}
	}()
	go func() {
		defer wg.Done()
		for !stop.Load() {
			tr.deleteEntry(7)
		}
	}()
	for range 100_000 {
		trieGet(&tr, 7)
	}
	stop.Store(true)
	wg.Wait()
}
