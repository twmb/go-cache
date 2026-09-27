package cache

import (
	"hash/maphash"
	"sync"
	"sync/atomic"
	"unsafe"
)

// trie is a concurrent hash-trie map, modeled on Go's internal/sync
// HashTrieMap. Loads are lock-free. Inserts and deletes lock only the
// indirect node that holds the slot being changed. Deletes prune empty
// indirect nodes bottom-up, locking hand over hand from child to parent.
//
// Unlike internal/sync, we only have userspace tools: hash/maphash gives us
// the runtime's hasher for any comparable type.
type trie[K comparable, V any] struct {
	root   atomic.Pointer[trieIndirect[K, V]] // nil until first insert
	initMu sync.Mutex
	seed   maphash.Seed

	// hashFn, if non-nil, remaps every hash so that tests can force
	// collisions. It takes the hash rather than the key: calling a func
	// value with the key would make every key escape to the heap.
	hashFn func(uintptr) uintptr
}

const (
	trieBranchingLog2 = 4
	trieBranching     = 1 << trieBranchingLog2
	trieBranchingMask = trieBranching - 1
)

// ptrBits is the number of hash bits we use: the trie is at most
// ptrBits/trieBranchingLog2 levels deep.
const ptrBits = uint(unsafe.Sizeof(uintptr(0))) * 8

// trieNode is the header shared by trieIndirect and trieEntry; isEntry
// says which one a *trieNode actually is.
type trieNode[K comparable, V any] struct {
	isEntry bool
}

type trieIndirect[K comparable, V any] struct {
	trieNode[K, V]
	dead     atomic.Bool // set once pruned; the node must not be written again
	mu       sync.Mutex
	parent   *trieIndirect[K, V]
	children [trieBranching]atomic.Pointer[trieNode[K, V]]
}

// trieEntry is a leaf. Keys whose full hashes are equal are chained through
// overflow. The trie never reads p; the cache owns what it means (see
// entDel).
type trieEntry[K comparable, V any] struct {
	trieNode[K, V]
	overflow atomic.Pointer[trieEntry[K, V]]
	key      K
	p        atomic.Pointer[V]
}

func newTrieIndirect[K comparable, V any](parent *trieIndirect[K, V]) *trieIndirect[K, V] {
	return &trieIndirect[K, V]{parent: parent}
}

func newTrieEntry[K comparable, V any](key K, value *V) *trieEntry[K, V] {
	e := &trieEntry[K, V]{key: key}
	e.isEntry = true
	if value != nil {
		e.p.Store(value)
	}
	return e
}

func (n *trieNode[K, V]) entry() *trieEntry[K, V] {
	return (*trieEntry[K, V])(unsafe.Pointer(n))
}

func (n *trieNode[K, V]) indirect() *trieIndirect[K, V] {
	return (*trieIndirect[K, V])(unsafe.Pointer(n))
}

func (i *trieIndirect[K, V]) empty() bool {
	for j := range i.children {
		if i.children[j].Load() != nil {
			return false
		}
	}
	return true
}

// initRoot returns the root, creating it and the hash seed on first use.
// The seed is written before the root is published, so anything that loads
// a non-nil root can hash.
func (t *trie[K, V]) initRoot() *trieIndirect[K, V] {
	if i := t.root.Load(); i != nil {
		return i
	}
	t.initMu.Lock()
	defer t.initMu.Unlock()
	if i := t.root.Load(); i != nil {
		return i
	}
	if t.hashFn == nil {
		t.seed = maphash.MakeSeed()
	}
	i := newTrieIndirect[K, V](nil)
	t.root.Store(i)
	return i
}

func (t *trie[K, V]) hash(k K) uintptr {
	h := uintptr(maphash.Comparable(t.seed, k))
	if t.hashFn != nil {
		h = t.hashFn(h)
	}
	return h
}

// loadEntry returns the entry for k, or nil if there is none.
func (t *trie[K, V]) loadEntry(k K) *trieEntry[K, V] {
	i := t.root.Load()
	if i == nil {
		return nil
	}
	h := t.hash(k)
	for shift := ptrBits; shift != 0; {
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
	panic("cache: trie ran out of hash bits")
}

// loadOrStoreEntry returns the existing entry for k and true, or inserts a
// new entry holding value (which may be nil) and returns it and false.
func (t *trie[K, V]) loadOrStoreEntry(k K, value *V) (*trieEntry[K, V], bool) {
	t.initRoot()
	h := t.hash(k)
	var (
		i     *trieIndirect[K, V]
		shift uint
		slot  *atomic.Pointer[trieNode[K, V]]
		n     *trieNode[K, V]
	)
	for {
		// Find the slot we would insert into without locking, then lock
		// its node and retry if the slot has since become an indirect or
		// the node was pruned.
		i = t.root.Load()
		shift = ptrBits
		for {
			if shift == 0 {
				panic("cache: trie ran out of hash bits")
			}
			shift -= trieBranchingLog2
			slot = &i.children[(h>>shift)&trieBranchingMask]
			n = slot.Load()
			if n == nil {
				break
			}
			if n.isEntry {
				for e := n.entry(); e != nil; e = e.overflow.Load() {
					if e.key == k {
						return e, true
					}
				}
				break
			}
			i = n.indirect()
		}
		i.mu.Lock()
		n = slot.Load()
		if (n == nil || n.isEntry) && !i.dead.Load() {
			break
		}
		i.mu.Unlock()
	}
	defer i.mu.Unlock()

	var old *trieEntry[K, V]
	if n != nil {
		old = n.entry()
		for e := old; e != nil; e = e.overflow.Load() {
			if e.key == k {
				return e, true
			}
		}
	}
	e := newTrieEntry(k, value)
	if old == nil {
		slot.Store(&e.trieNode)
	} else {
		slot.Store(t.expand(old, e, h, shift, i))
	}
	return e, false
}

// expand returns the node to replace old's slot with so that both old and
// new are reachable: new chained in front of old if their full hashes are
// equal, otherwise indirect nodes down to the first hash bits that differ.
func (t *trie[K, V]) expand(old, new *trieEntry[K, V], newHash uintptr, shift uint, parent *trieIndirect[K, V]) *trieNode[K, V] {
	oldHash := t.hash(old.key)
	if oldHash == newHash {
		new.overflow.Store(old)
		return &new.trieNode
	}
	top := newTrieIndirect(parent)
	cur := top
	for {
		if shift == 0 {
			panic("cache: trie ran out of hash bits")
		}
		shift -= trieBranchingLog2
		oi := (oldHash >> shift) & trieBranchingMask
		ni := (newHash >> shift) & trieBranchingMask
		if oi != ni {
			cur.children[oi].Store(&old.trieNode)
			cur.children[ni].Store(&new.trieNode)
			return &top.trieNode
		}
		next := newTrieIndirect(cur)
		cur.children[oi].Store(&next.trieNode)
		cur = next
	}
}

// deleteEntryIf unlinks and returns the entry for k if pred, evaluated
// while the entry's node is locked, returns true. A nil pred always
// deletes.
func (t *trie[K, V]) deleteEntryIf(k K, pred func(*trieEntry[K, V]) bool) *trieEntry[K, V] {
	i := t.root.Load()
	if i == nil {
		return nil
	}
	h := t.hash(k)
	var (
		shift uint
		slot  *atomic.Pointer[trieNode[K, V]]
		n     *trieNode[K, V]
	)
	for {
		i = t.root.Load()
		shift = ptrBits
		for {
			if shift == 0 {
				panic("cache: trie ran out of hash bits")
			}
			shift -= trieBranchingLog2
			slot = &i.children[(h>>shift)&trieBranchingMask]
			n = slot.Load()
			if n == nil {
				return nil
			}
			if n.isEntry {
				break
			}
			i = n.indirect()
		}
		i.mu.Lock()
		if i.dead.Load() {
			i.mu.Unlock()
			continue
		}
		n = slot.Load()
		if n == nil {
			i.mu.Unlock()
			return nil
		}
		if n.isEntry {
			break
		}
		// A concurrent insert expanded the slot; k may now be deeper.
		i.mu.Unlock()
	}

	head := n.entry()
	var prev, target *trieEntry[K, V]
	for e := head; e != nil; prev, e = e, e.overflow.Load() {
		if e.key == k {
			target = e
			break
		}
	}
	if target == nil || pred != nil && !pred(target) {
		i.mu.Unlock()
		return nil
	}
	if prev != nil {
		prev.overflow.Store(target.overflow.Load())
		i.mu.Unlock()
		return target
	}
	if next := target.overflow.Load(); next != nil {
		slot.Store(&next.trieNode)
		i.mu.Unlock()
		return target
	}
	slot.Store(nil)

	// Prune now-empty indirect nodes up toward the root. We hold i's lock
	// until i is marked dead, so an insert that locks i afterward sees
	// dead and retries from the root.
	for i.parent != nil && i.empty() {
		shift += trieBranchingLog2
		parent := i.parent
		parent.mu.Lock()
		i.dead.Store(true)
		parent.children[(h>>shift)&trieBranchingMask].Store(nil)
		i.mu.Unlock()
		i = parent
	}
	i.mu.Unlock()
	return target
}

// walk calls f on every entry until f returns false. We do not snapshot:
// concurrent changes may or may not be seen, but no key is visited twice.
func (t *trie[K, V]) walk(f func(*trieEntry[K, V]) bool) {
	if i := t.root.Load(); i != nil {
		walkIndirect(i, f)
	}
}

func walkIndirect[K comparable, V any](i *trieIndirect[K, V], f func(*trieEntry[K, V]) bool) bool {
	for j := range i.children {
		n := i.children[j].Load()
		if n == nil {
			continue
		}
		if !n.isEntry {
			if !walkIndirect(n.indirect(), f) {
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

// clear swaps in an empty root. Anything still working on the old tree
// finishes on the old tree.
func (t *trie[K, V]) clear() {
	if t.root.Load() != nil {
		t.root.Store(newTrieIndirect[K, V](nil))
	}
}
