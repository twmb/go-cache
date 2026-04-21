package cache

import (
	"hash/maphash"
	"sync"
	"sync/atomic"
	"unsafe"
)

// trie is a concurrent hash-trie map, modeled on Go's internal/sync
// HashTrieMap. It is intended to replace Cache's read/dirty backing store.
// For now it is standalone and only exercised by trie_test.go.
//
// Design notes:
//
//   - The root is a lazily-allocated indirect node, atomically swapped on
//     Clear so readers that already loaded the root pointer keep operating
//     on the old tree.
//
//   - Interior nodes ("indirect") hold trieBranching atomic child pointers.
//     Leaf nodes ("entry") hold a key, an atomic value slot (*V), and an
//     atomic overflow pointer for hash-collision chains.
//
//   - Load is lock-free: walk the tree with atomic loads, follow overflow
//     on hash collision.
//
//   - LoadOrStore and Delete take the *parent* indirect node's mutex
//     (per-bucket locking) during mutation. After Delete, empty parents
//     are pruned bottom-up while locks are held.
//
//   - Hash and pointer-size primitives come from userspace: hash/maphash
//     for the runtime's typed hasher, unsafe.Sizeof(uintptr(0)) for the
//     hash bit budget. No internal/abi, no internal/goarch.

const (
	trieBranchingLog2 = 4
	trieBranching     = 1 << trieBranchingLog2
	trieBranchingMask = trieBranching - 1
)

// ptrBits is the number of hash bits consumed per trie walk. Using uintptr
// gives one hasher call on both 32-bit (32 bits consumed) and 64-bit (64
// bits consumed) platforms; on 32-bit, maphash.Comparable internally calls
// the runtime hasher twice to synthesize 64 bits, of which we keep the low
// uintptr — a cheap extra hash call on rare hardware.
const ptrBits = uint(unsafe.Sizeof(uintptr(0))) * 8

// trieNode is the common header for both leaf (entry) and interior
// (indirect) nodes. The layout is carefully matched so a *trieNode can be
// unsafe-cast to either concrete type via the isEntry discriminator.
type trieNode[K comparable, V any] struct {
	isEntry bool
}

// trieIndirect is an interior node with trieBranching atomic child slots.
type trieIndirect[K comparable, V any] struct {
	trieNode[K, V]
	dead     atomic.Bool
	mu       sync.Mutex
	parent   *trieIndirect[K, V]
	children [trieBranching]atomic.Pointer[trieNode[K, V]]
}

// trieEntry is a leaf node. The value slot p is an atomic pointer; a nil
// value slot means the entry is tombstoned but still wired into the trie
// (callers can atomically replace nil with a fresh value). The overflow
// chain handles hash-equal keys (full hash collisions).
type trieEntry[K comparable, V any] struct {
	trieNode[K, V]
	overflow atomic.Pointer[trieEntry[K, V]]
	key      K
	p        atomic.Pointer[V]
}

func newTrieIndirect[K comparable, V any](parent *trieIndirect[K, V]) *trieIndirect[K, V] {
	n := &trieIndirect[K, V]{parent: parent}
	n.isEntry = false
	return n
}

func newTrieEntry[K comparable, V any](key K, value *V) *trieEntry[K, V] {
	e := &trieEntry[K, V]{key: key}
	e.isEntry = true
	if value != nil {
		e.p.Store(value)
	}
	return e
}

// entry unsafe-casts a trieNode to its leaf form. Every call site checks
// n.isEntry first, so no runtime guard is needed here.
func (n *trieNode[K, V]) entry() *trieEntry[K, V] {
	return (*trieEntry[K, V])(unsafe.Pointer(n))
}

// indirect unsafe-casts a trieNode to its interior form. Every call site
// checks !n.isEntry first, so no runtime guard is needed here.
func (n *trieNode[K, V]) indirect() *trieIndirect[K, V] {
	return (*trieIndirect[K, V])(unsafe.Pointer(n))
}

// empty reports whether all child slots of i are nil.
func (i *trieIndirect[K, V]) empty() bool {
	for j := range i.children {
		if i.children[j].Load() != nil {
			return false
		}
	}
	return true
}

type trie[K comparable, V any] struct {
	inited atomic.Bool
	initMu sync.Mutex
	root   atomic.Pointer[trieIndirect[K, V]]
	seed   maphash.Seed

	// hashFn, if non-nil, overrides maphash.Comparable for tests that need
	// to construct hash collisions deterministically. Never set in
	// production code.
	hashFn func(K) uintptr
}

func (t *trie[K, V]) init() {
	if t.inited.Load() {
		return
	}
	t.initMu.Lock()
	defer t.initMu.Unlock()
	if t.inited.Load() {
		return
	}
	if t.hashFn == nil {
		t.seed = maphash.MakeSeed()
	}
	t.root.Store(newTrieIndirect[K, V](nil))
	t.inited.Store(true)
}

func (t *trie[K, V]) hash(k K) uintptr {
	if t.hashFn != nil {
		return t.hashFn(k)
	}
	return uintptr(maphash.Comparable(t.seed, k))
}

// loadEntry returns the entry for k, or nil if the key is not present. The
// lookup is lock-free: atomic loads walk the tree, then a final walk over
// the overflow chain checks for an exact key match.
func (t *trie[K, V]) loadEntry(k K) *trieEntry[K, V] {
	if !t.inited.Load() {
		return nil
	}
	h := t.hash(k)
	i := t.root.Load()
	shift := ptrBits
	for shift != 0 {
		shift -= trieBranchingLog2
		n := i.children[(h>>shift)&trieBranchingMask].Load()
		if n == nil {
			return nil
		}
		if n.isEntry {
			for e := n.entry(); e != nil; e = e.overflow.Load() {
				if e.key == k {
					return e
				}
			}
			return nil
		}
		i = n.indirect()
	}
	// Unreachable: the tree depth is bounded by ptrBits / trieBranchingLog2
	// (16 levels on 64-bit, 8 on 32-bit), so the loop always returns before
	// shift reaches 0. Kept as a defensive assertion in case the tree is
	// ever corrupted.
	panic("cache: trie ran out of hash bits in loadEntry")
}

// loadOrStoreEntry returns the existing entry for k if present. Otherwise it
// inserts a new entry initialized with value (which may be nil, meaning the
// caller will fill the value slot later) and returns it. The second return
// is true if an existing entry was loaded, false if a new one was stored.
//
// The insertion path takes the parent indirect node's lock. On hash prefix
// collision (two distinct keys sharing the same hash bits at the current
// depth) we build out indirection nodes until the bits diverge, via expand.
func (t *trie[K, V]) loadOrStoreEntry(k K, value *V) (*trieEntry[K, V], bool) {
	t.init()
	h := t.hash(k)
	var i *trieIndirect[K, V]
	var shift uint
	var slot *atomic.Pointer[trieNode[K, V]]
	var n *trieNode[K, V]
	for {
		i = t.root.Load()
		shift = ptrBits
		haveInsertPoint := false
		for shift != 0 {
			shift -= trieBranchingLog2
			slot = &i.children[(h>>shift)&trieBranchingMask]
			n = slot.Load()
			if n == nil {
				haveInsertPoint = true
				break
			}
			if n.isEntry {
				for e := n.entry(); e != nil; e = e.overflow.Load() {
					if e.key == k {
						return e, true
					}
				}
				haveInsertPoint = true
				break
			}
			i = n.indirect()
		}
		if !haveInsertPoint {
			// Unreachable: see the note in loadEntry above.
			panic("cache: trie ran out of hash bits in loadOrStoreEntry")
		}
		i.mu.Lock()
		n = slot.Load()
		if (n == nil || n.isEntry) && !i.dead.Load() {
			break
		}
		i.mu.Unlock()
	}
	defer i.mu.Unlock()

	var oldEntry *trieEntry[K, V]
	if n != nil {
		oldEntry = n.entry()
		for e := oldEntry; e != nil; e = e.overflow.Load() {
			if e.key == k {
				return e, true
			}
		}
	}

	newEntry := newTrieEntry(k, value)
	if oldEntry == nil {
		slot.Store(&newEntry.trieNode)
	} else {
		slot.Store(t.expand(oldEntry, newEntry, h, shift, i))
	}
	return newEntry, false
}

// expand resolves an insert where a leaf already sits at the target slot
// but holds a different key. If the old and new keys hash identically, we
// chain the old entry onto the new entry's overflow list. Otherwise we
// build out indirect nodes until the next differing hash bit, then place
// both entries in distinct children.
func (t *trie[K, V]) expand(oldEntry, newEntry *trieEntry[K, V], newHash uintptr, shift uint, parent *trieIndirect[K, V]) *trieNode[K, V] {
	oldHash := t.hash(oldEntry.key)
	if oldHash == newHash {
		newEntry.overflow.Store(oldEntry)
		return &newEntry.trieNode
	}
	top := newTrieIndirect(parent)
	cur := top
	for {
		if shift == 0 {
			// Unreachable: oldHash and newHash differ (same-hash case
			// returned above via the overflow chain), so some bit in
			// [0, shift) must differ and the loop exits before shift=0.
			panic("cache: trie ran out of hash bits in expand")
		}
		shift -= trieBranchingLog2
		oi := (oldHash >> shift) & trieBranchingMask
		ni := (newHash >> shift) & trieBranchingMask
		if oi != ni {
			cur.children[oi].Store(&oldEntry.trieNode)
			cur.children[ni].Store(&newEntry.trieNode)
			break
		}
		next := newTrieIndirect(cur)
		cur.children[oi].Store(&next.trieNode)
		cur = next
	}
	return &top.trieNode
}

// deleteEntry removes the entry for k from the trie. Returns the removed
// entry (or nil). Empty interior ancestors are pruned bottom-up.
func (t *trie[K, V]) deleteEntry(k K) *trieEntry[K, V] {
	if !t.inited.Load() {
		return nil
	}
	h := t.hash(k)

	// Walk to the entry, then lock the parent indirect node. We retry if we
	// see the parent has been marked dead by concurrent pruning.
	var i *trieIndirect[K, V]
	var shift uint
	var slot *atomic.Pointer[trieNode[K, V]]
	var n *trieNode[K, V]
	for {
		i = t.root.Load()
		shift = ptrBits
		found := false
		for shift != 0 {
			shift -= trieBranchingLog2
			slot = &i.children[(h>>shift)&trieBranchingMask]
			n = slot.Load()
			if n == nil {
				return nil
			}
			if n.isEntry {
				found = true
				break
			}
			i = n.indirect()
		}
		if !found {
			// Unreachable: see the note in loadEntry above.
			panic("cache: trie ran out of hash bits in deleteEntry")
		}
		i.mu.Lock()
		if i.dead.Load() {
			i.mu.Unlock()
			continue
		}
		n = slot.Load()
		if n == nil || !n.isEntry {
			i.mu.Unlock()
			return nil
		}
		break
	}

	// Under i.mu, remove k from the overflow chain. If k was the head and
	// had no overflow, slot becomes nil; otherwise slot gets the new head.
	head := n.entry()
	removed, newHead, found := trieRemoveFromChain(head, k)
	if !found {
		i.mu.Unlock()
		return nil
	}
	if newHead != nil {
		slot.Store(&newHead.trieNode)
		i.mu.Unlock()
		return removed
	}
	slot.Store(nil)

	// Prune empty parents. Walk up while holding locks as a hand-over-hand
	// from child to parent.
	for i.parent != nil && i.empty() {
		if shift == ptrBits {
			// Unreachable: pruning walks strictly upward from a leaf, so
			// shift is always < ptrBits when we enter (we consumed at
			// least trieBranchingLog2 bits on descent). Kept as a
			// defensive assertion.
			panic("cache: trie shift overflow in deleteEntry pruning")
		}
		shift += trieBranchingLog2
		parent := i.parent
		parent.mu.Lock()
		i.dead.Store(true)
		parent.children[(h>>shift)&trieBranchingMask].Store(nil)
		i.mu.Unlock()
		i = parent
	}
	i.mu.Unlock()
	return removed
}

// trieRemoveFromChain walks the overflow chain starting at head, removes
// the entry whose key equals k, and returns the removed entry, the new
// chain head (which may be nil if the only entry was the head), and
// whether an entry was actually removed.
func trieRemoveFromChain[K comparable, V any](head *trieEntry[K, V], k K) (removed, newHead *trieEntry[K, V], found bool) {
	if head.key == k {
		return head, head.overflow.Load(), true
	}
	prev := head
	for {
		next := prev.overflow.Load()
		if next == nil {
			return nil, head, false
		}
		if next.key == k {
			prev.overflow.Store(next.overflow.Load())
			return next, head, true
		}
		prev = next
	}
}

// walk iterates every entry in the trie, calling f for each. If f returns
// false, iteration stops. No snapshot is taken; concurrent mutations may
// or may not be observed. A given key is never visited more than once.
func (t *trie[K, V]) walk(f func(*trieEntry[K, V]) bool) {
	if !t.inited.Load() {
		return
	}
	trieWalkIndirect(t.root.Load(), f)
}

func trieWalkIndirect[K comparable, V any](i *trieIndirect[K, V], f func(*trieEntry[K, V]) bool) bool {
	for j := range i.children {
		n := i.children[j].Load()
		if n == nil {
			continue
		}
		if !n.isEntry {
			if !trieWalkIndirect(n.indirect(), f) {
				return false
			}
			continue
		}
		for e := n.entry(); e != nil; e = e.overflow.Load() {
			if !f(e) {
				return false
			}
		}
	}
	return true
}

// clear replaces the root with a fresh empty indirect node. Concurrent
// readers that already loaded the old root keep walking it; the old tree
// becomes unreachable once all such readers finish.
func (t *trie[K, V]) clear() {
	t.init()
	t.root.Store(newTrieIndirect[K, V](nil))
}
