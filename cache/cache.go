// Package cache provides a concurrency safe, mostly lock-free, singleflight
// request collapsing generic cache with support for stale values.
//
// The internal storage is a concurrent hash-trie (see trie.go), inspired by
// Go's internal/sync.HashTrieMap. Loads are lock-free; inserts and deletes
// take a per-bucket (not global) mutex. The cache layers singleflight,
// stale values, TTLs, idle-age extension, and error caching on top.
//
// Functions in this API that return a KeyState return the state last, rather
// than the error last: the value and error are cached as a single internal
// unit and can be thought of as a single value.
//
// This package provides three similar types: Cache, Set, and Item. Set and
// Item are key-only and value-only caches. You may want a key-only cache if
// you want to ensure logic is ran once / periodically on expiry for every key.
// You may want a value-only cache as a singleton that is updated as needed.
package cache

import (
	"sync"
	"sync/atomic"
	"time"
)

// KeyState is returned from cache operations to indicate how a key existed in
// the map, if at all.
type KeyState uint8

// IsHit returns whether the key state is Hit or Stale, meaning that the value
// is loaded.
func (s KeyState) IsHit() bool { return s != Miss }

// IsMiss returns if the key state is Miss.
func (s KeyState) IsMiss() bool { return s == Miss }

const (
	// Miss indicates that the key was not present in the map.
	Miss KeyState = iota
	// Hit indicates that the key was present in the map and we are using
	// the latest values for the key.
	Hit
	// Stale indicates that the key was present in the map, but either was
	// expired or had an error, and we returned a valid stale value.
	Stale
)

type (
	stale[V any] struct {
		v       V
		expires int64 // nano at which this stale ent is unusable, if non-zero
	}
	loading[V any] struct {
		v       V
		err     error
		expires atomic.Int64 // nano at which this ent is unusable, if non-zero
		stale   *stale[V]
		state   atomic.Uint32

		wg sync.WaitGroup
		mu sync.Mutex
	}

	// Cache caches comparable keys to arbitrary values. By default, the
	// cache grows without bounds and all keys persist forever. These
	// limits can be changed with options that are passed to New.
	Cache[K comparable, V any] struct {
		t trie[K, loading[V]]

		cfg cfg

		quitOnce  sync.Once
		quitClean chan struct{}
	}

	cfg struct {
		maxAge            time.Duration
		maxStaleAge       time.Duration
		maxErrAge         time.Duration
		maxIdleAge        time.Duration
		ageSet            bool
		errAgeSet         bool
		autoCleanInterval time.Duration
	}

	opt struct{ fn func(*cfg) }

	// Opt configures a cache.
	Opt interface {
		apply(*cfg)
	}
)

const (
	stateLoading uint32 = iota
	stateFinalized
)

// ent is the concrete type of cache entries living in the trie. Using a
// type alias (Go 1.24+) lets us refer to entries by a short name while the
// trie stays a generic hash-trie with no cache-specific logic.
type ent[K comparable, V any] = trieEntry[K, loading[V]]

func now() int64 { return time.Now().UnixNano() }

func (cfg *cfg) newExpires(err error) int64 {
	var ttl time.Duration
	var del0 bool
	if err != nil && cfg.errAgeSet {
		ttl = cfg.maxErrAge
		del0 = cfg.maxErrAge <= 0
	} else {
		ttl = cfg.maxAge
		if ttl == 0 && !cfg.ageSet && cfg.maxIdleAge > 0 {
			ttl = cfg.maxIdleAge
		}
		del0 = cfg.ageSet && cfg.maxAge <= 0
	}
	if del0 {
		return -1
	}
	if ttl == 0 {
		return 0
	}
	return time.Now().Add(ttl).UnixNano()
}

func (o opt) apply(c *cfg) { o.fn(c) }

// MaxAge sets the maximum age that values are cached for. By default, entries
// are cached forever. Using this option with 0 disables caching entirely,
// which allows this "cache" to be used as a way to collapse simultaneous
// queries for the same key.
//
// Using this does *not* start a goroutine that periodically cleans the cache.
// Instead, the values will persist but are "dead" and cannot be queried. You
// can forcefully clean the cache with relevant Cache methods.
//
// You can opt in to values expiring but still being queryable with the
// MaxStaleAge option.
func MaxAge(age time.Duration) Opt { return opt{fn: func(c *cfg) { c.maxAge, c.ageSet = age, true }} }

// MaxStaleAge opts in to stale values and sets how long they will persist
// after an entry has expired (so, total age is MaxAge + MaxStaleAge). A stale
// value is the previous successfully cached value that is returned while the
// value is being refreshed (a new value is being queried). As well, the stale
// value is returned while the refreshed value is erroring. This option is
// useless without MaxAge.
//
// A special value of -1 allows stale values to be returned indefinitely.
func MaxStaleAge(age time.Duration) Opt { return opt{fn: func(c *cfg) { c.maxStaleAge = age }} }

// MaxErrorAge sets the age to persist load errors. If not specified, the
// default is MaxAge. Using this option with 0 disables caching errors
// entirely.
func MaxErrorAge(age time.Duration) Opt {
	return opt{fn: func(c *cfg) { c.maxErrAge, c.errAgeSet = age, true }}
}

// MaxIdleAge opts in to extending an entry's expiry on each successful
// access. Each time Get or TryGet returns a Hit without an error, the
// entry's expiry is reset to now + age. If MaxAge is not set, the idle
// age is also used as the initial TTL.
func MaxIdleAge(age time.Duration) Opt {
	return opt{fn: func(c *cfg) { c.maxIdleAge = age }}
}

// AutoCleanInterval begins a goroutine that calls Clean every interval. The
// goroutine can be quit with StopAutoClean. It is recommended to use an
// interval that is less than your MaxAge + MaxStaleAge. Values are only
// candidates to be cleaned after the max age has elapsed. At worst, a value
// may persist for a total of MaxAge + MaxStaleAge + AutoCleanInterval.
func AutoCleanInterval(interval time.Duration) Opt {
	return opt{fn: func(c *cfg) { c.autoCleanInterval = interval }}
}

// New returns a new cache, with the optional overrides configuring cache
// semantics. If you do not need to configure a cache at all, the zero value
// cache is valid and usable.
func New[K comparable, V any](opts ...Opt) *Cache[K, V] {
	var cfg cfg
	for _, opt := range opts {
		opt.apply(&cfg)
	}
	c := &Cache[K, V]{
		cfg: cfg,
	}

	if cfg.autoCleanInterval > 0 && c.cfg.maxStaleAge >= 0 {
		c.quitClean = make(chan struct{})
		go func() {
			ticker := time.NewTicker(cfg.autoCleanInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
				case <-c.quitClean:
					return
				}
				c.Clean()
			}
		}()
	}
	return c
}

// Get returns the cache value for k, running the miss function in a goroutine
// if the key is not yet cached. If stale values are enabled, the currently
// cached value has an error, and there is an unexpired stale value, this
// returns the stale value and no error.
func (c *Cache[K, V]) Get(k K, miss func() (V, error)) (v V, err error, s KeyState) {
	// Fast path: if the entry already exists and holds a live value, use
	// it without any locking.
	if e := c.t.loadEntry(k); e != nil {
		if v, err, s = entGet(e, c.cfg.maxIdleAge); s == Hit {
			return v, err, s
		}
	}

	// Slow path: acquire or create the entry and install a fresh loading
	// if it has no live value. loadOrStoreEntry takes the trie's per-bucket
	// lock for the insert/replace window.
	e, _ := c.t.loadOrStoreEntry(k, nil)

	// Either the entry is newly created (p=nil), tombstoned (p=nil from a
	// prior Delete), or holds a live loading from a concurrent Get/Set. We
	// loop to handle the race where p is non-nil but expired/errored — we
	// want to replace it with a fresh loading and carry forward a stale.
	var l *loading[V]
	for {
		prev := e.p.Load()
		if prev == nil {
			l = &loading[V]{}
			l.wg.Add(1)
			if e.p.CompareAndSwap(nil, l) {
				break
			}
			continue
		}
		// There is an existing value. Use entGet to decide whether to
		// wait for it (finalized hit), return a stale (finalized but
		// expired/errored with a stale), or replace (expired with no
		// stale). For the replace case, we need a fresh loading whose
		// stale carries the old value.
		if v, err, s = entGet(e, c.cfg.maxIdleAge); s == Hit {
			return v, err, s
		}
		if s == Stale {
			// We already have a valid stale to return; a concurrent
			// goroutine is (or will be) driving the load, and we can
			// piggyback. But we must ensure *someone* is driving the
			// load — if the prev loading is finalized but expired and
			// has a stale, entGet returned Stale but nothing is yet
			// refreshing. Install a fresh loading now.
			//
			// Concretely: if prev is finalized, we race to replace it.
			// If prev is still loading (not finalized), another Get
			// already triggered a miss; entGet returned prev.stale and
			// that caller will finalize.
			if prev.finalized() {
				l = &loading[V]{
					stale: entMaybeNewStale(e, c.cfg.maxStaleAge),
				}
				l.wg.Add(1)
				if !e.p.CompareAndSwap(prev, l) {
					continue
				}
				break
			}
			// Loading in flight; return the stale from entGet.
			return v, err, s
		}
		// Miss: prev is finalized, expired, and has no valid stale (or
		// prev is tombstoned and slipped through the nil check above via
		// race). Install a fresh loading with a stale snapshot from prev.
		l = &loading[V]{
			stale: entMaybeNewStale(e, c.cfg.maxStaleAge),
		}
		l.wg.Add(1)
		if e.p.CompareAndSwap(prev, l) {
			break
		}
	}

	go func() { v, err := miss(); l.setve(v, err, c.cfg.newExpires(err)) }()

	// Wait for our loading to finalize, then return the value. Waiting via
	// l.wg (our own) synchronizes with whoever called wg.Done — the miss
	// goroutine's setve or a racing Swap's defer — even if e.p has since
	// been replaced by another writer.
	v, err, s = entGet(e, c.cfg.maxIdleAge)
	switch s {
	case Miss, Hit:
		l.wg.Wait()
		return l.v, l.err, Miss
	}
	return v, err, Stale
}

// TryGet returns the value for the given key if it is cached. This returns
// either the currently stored value, or the current stale if the load errored,
// or the load error if there is no stale. If nothing is cached, or what is
// cached is expired, this returns Miss.
func (c *Cache[K, V]) TryGet(k K) (v V, err error, _ KeyState) {
	e := c.t.loadEntry(k)
	if e == nil {
		return v, err, Miss
	}
	return entTryGet(e, 0, c.cfg.maxIdleAge)
}

// Delete deletes the value for a key and returns the prior value, if stored
// (i.e. the return from TryGet).
//
// Delete physically removes the entry from the trie. If a concurrent Get is
// holding a reference to the deleted entry via its in-flight loading, that
// Get still completes with the miss function's result; a new Get arriving
// after Delete creates a fresh entry and may drive an independent miss.
func (c *Cache[K, V]) Delete(k K) (v V, err error, _ KeyState) {
	e := c.t.loadEntry(k)
	if e == nil {
		return v, err, Miss
	}
	v, err, s := entTryGet(e, 0, 0)
	// Tombstone the value slot first so any lingering in-flight readers
	// see nothing. Then physically remove the entry from the trie so that
	// the key (and any memory it refers to) is eligible for GC.
	entDel(e)
	c.t.deleteEntryIf(k, func(ee *ent[K, V]) bool {
		// Only remove if nobody resurrected the slot under the lock. A
		// concurrent Swap between entDel above and the lock below may
		// have installed a fresh loading; leave that entry alone.
		return ee.p.Load() == nil
	})
	return v, err, s
}

// Expire sets a stored value to expire immediately, meaning the next Get will
// be a miss. If stale values are enabled, the next Get will trigger the miss
// function but still allow the now-stale value to be returned.
//
// Expire only affects entries that have finished loading. Calling Expire for a
// key whose miss function is still running is a no-op; the in-flight load will
// complete with its normal TTL and is not canceled or shortened.
func (c *Cache[K, V]) Expire(k K) {
	e := c.t.loadEntry(k)
	if e == nil {
		return
	}
	if l := entLoad(e); l != nil && l.finalized() {
		l.expires.Store(now() - 1)
	}
}

// Range calls fn for every cached value. If fn returns false, iteration stops.
func (c *Cache[K, V]) Range(fn func(K, V, error) bool) {
	// When ranging, repeated time.Now() calls add up, so we get the
	// current time when we enter range and avoid it in all tryGet calls.
	tn := now()
	c.t.walk(func(e *ent[K, V]) bool {
		v, err, s := entTryGet(e, tn, 0)
		if s.IsMiss() {
			return true
		}
		return fn(e.key, v, err)
	})
}

// Clean deletes all expired values from the cache. A value is expired if
// MaxAge is used and the entry is older than the max age, or if you manually
// expired a key. If MaxStaleAge is used and not -1, the entry must be older
// than MaxAge + MaxStaleAge. If MaxStaleAge is -1, Clean returns immediately.
//
// Clean also physically removes entries that were tombstoned by prior Delete
// calls, so the trie's memory footprint does not grow unboundedly with
// delete churn.
func (c *Cache[K, V]) Clean() {
	if c.cfg.maxStaleAge < 0 {
		return
	}
	tn := now()

	// First pass: tombstone expired entries. Collect keys that need
	// physical removal (both newly-tombstoned and pre-existing tombstones).
	var toPrune []K
	c.t.walk(func(e *ent[K, V]) bool {
		l := e.p.Load()
		if l == nil {
			toPrune = append(toPrune, e.key)
			return true
		}
		if !l.finalized() {
			return true
		}
		expires := l.expires.Load()
		if expires != 0 && tn > expires+int64(c.cfg.maxStaleAge) {
			if e.p.CompareAndSwap(l, nil) {
				toPrune = append(toPrune, e.key)
			}
		}
		return true
	})

	// Second pass: remove tombstoned entries, but only if they are still
	// tombstoned under the trie's bucket lock. A concurrent Swap may have
	// resurrected the entry with a fresh loading in the meantime; in that
	// case we leave it alone.
	for _, k := range toPrune {
		c.t.deleteEntryIf(k, func(e *ent[K, V]) bool {
			return e.p.Load() == nil
		})
	}
}

// Clear deletes all keys from the cache, resetting it to an empty state.
func (c *Cache[K, V]) Clear() {
	c.t.clear()
}

// StopAutoClean stops the auto clean goroutine that is began with the
// AutoClean option.
func (c *Cache[K, V]) StopAutoClean() {
	c.quitOnce.Do(func() {
		if c.quitClean != nil {
			close(c.quitClean)
		}
	})
}

// Swap sets a value for a key. If the key is currently loading via Get, the
// load is canceled and Get returns the value from Swap. This returns the
// previously stored value, or the previous stale if the load errored, or the
// previous error if there is no stale. If nothing was cached, this returns
// Miss.
func (c *Cache[K, V]) Swap(k K, v V) (old V, oldErr error, oldState KeyState) {
	l := c.finalizedLoading(v)

	var was *loading[V]
	defer func() {
		if was == nil {
			return
		}

		// The following block is similar to setve, but expanded to
		// return our prior value (or stale, or err) if it exists.
		n := now()
		if !was.finalized() {
			was.mu.Lock()
			if !was.finalized() {
				if was.stale != nil && !was.stale.expired(n) {
					old, oldState = was.stale.v, Stale
				}
				was.v = v
				was.expires.Store(c.cfg.newExpires(nil))
				was.state.Store(stateFinalized)
				was.wg.Done()
				was.mu.Unlock()
				return
			}
			was.mu.Unlock()
		}
		if expired := was.expired(n); was.err != nil || expired {
			if was.stale != nil && !was.stale.expired(n) {
				old, oldState = was.stale.v, Stale
			}
			if expired {
				return
			}
		}
		old, oldErr, oldState = was.v, was.err, Hit
	}()

	// Fast path: if the entry already exists, CAS the value slot directly
	// without going through the trie's bucket lock.
	if e := c.t.loadEntry(k); e != nil {
		rm := e.p.Load()
		was = rm
		if e.p.CompareAndSwap(rm, l) {
			return old, oldErr, oldState
		}
		// Lost the CAS; fall into the slow path where we can reason
		// about the entry under the trie's bucket lock.
	}

	// Slow path: get or create an entry and swap in our value. The trie's
	// bucket lock ensures creation is exclusive; Swap on the value slot is
	// atomic.
	e, _ := c.t.loadOrStoreEntry(k, l)
	// If the entry was freshly created with value=l, we're done. Otherwise
	// atomically swap in l and capture the previous loading.
	for {
		rm := e.p.Load()
		if rm == l {
			// We are the entry's installer; nothing was there before.
			was = nil
			return old, oldErr, oldState
		}
		was = rm
		if e.p.CompareAndSwap(rm, l) {
			return old, oldErr, oldState
		}
	}
}

func (l *loading[V]) setve(v V, err error, expires int64) {
	if l.finalized() {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.finalized() {
		return
	}
	l.v, l.err = v, err
	l.expires.Store(expires)
	l.state.Store(stateFinalized)
	l.wg.Done()
}

// Set sets a value for a key. If the key is currently loading via Get, the
// load is canceled and Get returns the value from Set.
func (c *Cache[K, V]) Set(k K, v V) {
	c.Swap(k, v)
}

// CompareAndSwap swaps the old and new values for k if the value has finished
// loading and the value is equal to old. The type V must be comparable.
func (c *Cache[K, V]) CompareAndSwap(k K, old, new V) bool {
	e := c.t.loadEntry(k)
	if e == nil {
		return false
	}
	return c.tryCAS(e, old, new, true)
}

// CompareAndDelete deletes the entry for k if the value has finished loading
// and the value is equal to old. The type V must be comparable.
func (c *Cache[K, V]) CompareAndDelete(k K, old V) bool {
	e := c.t.loadEntry(k)
	if e == nil {
		return false
	}
	return c.tryCAS(e, old, old, false)
}

func (c *Cache[K, V]) tryCAS(e *ent[K, V], old, new V, useNew bool) bool {
	l := e.p.Load()
	if l == nil || !l.onlyFinalized() || any(l.v) != any(old) {
		return false
	}
	var l2 *loading[V]
	if useNew {
		l2 = c.finalizedLoading(new)
	}
	for {
		if e.p.CompareAndSwap(l, l2) {
			return true
		}
		l = e.p.Load()
		if l == nil || !l.onlyFinalized() || any(l.v) != any(old) {
			return false
		}
	}
}

///////////////////
// ENTRY HELPERS //
///////////////////

func (c *Cache[K, V]) finalizedLoading(v V) *loading[V] {
	l := &loading[V]{
		v: v,
	}
	expires := c.cfg.newExpires(nil)
	if expires != 0 || c.cfg.maxStaleAge != 0 {
		l.expires.Store(expires)
		if c.cfg.maxStaleAge != 0 {
			// newStale handles expires <= 0 by basing the stale lifespan
			// on now, so an infinite-TTL + MaxStaleAge combo produces a
			// usable (non-epoch-born) stale for manual Expire to surface.
			l.stale = newStale(v, expires, c.cfg.maxStaleAge)
		}
	}
	l.state.Store(stateFinalized)
	return l
}

func (l *loading[V]) finalized() bool     { return l.state.Load() != 0 }
func (l *loading[V]) onlyFinalized() bool { return l.state.Load() == 1 }

// entDel tombstones the entry by atomically setting its value slot to nil.
// The entry itself remains in the trie; Clean later removes tombstones
// physically. This matches the old "e.del then promote" pattern so that a
// concurrent Swap can resurrect the entry by re-populating the value slot.
func entDel[K comparable, V any](e *ent[K, V]) {
	for {
		p := e.p.Load()
		if p == nil {
			return
		}
		if e.p.CompareAndSwap(p, nil) {
			return
		}
	}
}

// entLoad returns the entry's current loading, or nil if the slot is
// tombstoned. Callers guarantee e is non-nil.
func entLoad[K comparable, V any](e *ent[K, V]) *loading[V] {
	return e.p.Load()
}

func (l *loading[V]) expired(n int64) bool {
	expires := l.expires.Load()
	return expires != 0 && expires <= n // 0 means either miss not resolved, or no max age
}

func (s *stale[V]) expired(n int64) bool {
	expires := s.expires
	return expires != 0 && expires <= n
}

// entMaybeNewStale derives a new stale snapshot from the entry's current
// loading, for use when installing a replacement loading.
//
//   - if entry has no live loading, no stale, return nil
//   - if no stale age, we are not using stales, return nil
//   - if the loading is still pending, return nil (no value to snapshot)
//   - if loading has an error, return the prior stale
//   - if age is < 0, return new unexpiring stale
//   - else, return new stale with prior expiry + stale age
func entMaybeNewStale[K comparable, V any](e *ent[K, V], age time.Duration) *stale[V] {
	if age == 0 {
		return nil
	}
	l := entLoad(e)
	if l == nil || !l.finalized() {
		return nil
	}
	if l.err != nil {
		return l.stale
	}
	return newStale(l.v, l.expires.Load(), age)
}

// newStale returns a fresh stale; age must be non-zero.
//
// expires is the main entry's expiry nano. If it is <= 0 (either 0 meaning
// the main entry never expires, or -1 meaning it expired immediately), we
// base the stale's lifespan on now so the stale is not born expired at the
// Unix epoch.
func newStale[V any](v V, expires int64, age time.Duration) *stale[V] {
	if age < 0 {
		return &stale[V]{v: v}
	}
	if expires <= 0 {
		expires = now()
	}
	return &stale[V]{v, expires + int64(age)}
}

// entGet always returns the value or the stale value. We do not check if our
// value is expired: we call this at the end of Get, we must always return
// something even if it is to be immediately expired.
func entGet[K comparable, V any](e *ent[K, V], extend time.Duration) (v V, err error, state KeyState) {
	l := entLoad(e)
	var waited bool
	if l == nil {
		return v, err, state
	}
	if !l.finalized() {
		if l.stale != nil && !l.stale.expired(now()) {
			return l.stale.v, nil, Stale
		}
		l.wg.Wait()
		waited = true
	}

	// If we did not wait and our entry is expired (value or error), or if
	// our entry is not expired but has errored, we potentially return the
	// stale entry.
	//
	// If we waited, we could immediately be expired due to time sync, or
	// if the user is configured to never cache and they're just using
	// request collapsing: we still want to return the now expired value.
	n := now()
	if (!waited && l.expired(n)) || l.err != nil {
		if l.stale != nil && !l.stale.expired(n) {
			return l.stale.v, nil, Stale
		}
		// The stale value is expired: if our entry is not expired,
		// this must be an error we waited on.
		if !l.expired(n) {
			return l.v, l.err, Hit
		}
		return v, err, state
	}
	if extend > 0 && l.err == nil {
		l.expires.Store(n + int64(extend))
	}
	return l.v, l.err, Hit
}

func entTryGet[K comparable, V any](e *ent[K, V], n64 int64, extend time.Duration) (v V, err error, state KeyState) {
	l := entLoad(e)
	if l == nil {
		return v, err, state
	}
	// Fast path: finalized, no expiry, no error — skip time checks.
	if l.onlyFinalized() && l.expires.Load() == 0 && l.err == nil {
		return l.v, nil, Hit
	}
	if n64 == 0 {
		n64 = now()
	}
	n := n64

	// If we are loading but there is a valid stale, return it, otherwise
	// return immediately: no get.
	if !l.finalized() {
		if l.stale != nil && !l.stale.expired(n) {
			return l.stale.v, nil, Stale
		}
		return v, err, state
	}

	// If we have an error or we are expired, we maybe return the stale.
	if expired := l.expired(n); l.err != nil || expired {
		if l.stale != nil && !l.stale.expired(n) {
			return l.stale.v, nil, Stale
		}
		if expired {
			return v, err, state
		}
	}
	if extend > 0 && l.err == nil {
		l.expires.Store(n + int64(extend))
	}
	return l.v, l.err, Hit
}

//////////
// ITEM //
//////////

// Item caches a single item. By default, it persists forever once loaded.
// Cache semantics for the item can be changed with options that are passed to
// NewItem.
type Item[V any] struct {
	c Cache[struct{}, V]
}

// NewItem returns a new Item, with the optional overrides configuring cache
// semantics. If you do not need to configure an item at all, the zero value
// item is valid and usable.
func NewItem[V any](opts ...Opt) *Item[V] {
	var c cfg
	for _, opt := range opts {
		opt.apply(&c)
	}
	return &Item[V]{
		c: Cache[struct{}, V]{
			cfg: c,
		},
	}
}

// Get returns the currently cached value, running the miss function in a
// goroutine if the item is not yet cached. If stale values are enabled, the
// currently cached value has an error, and there is an unexpired stale value,
// this returns the stale value and no error.
func (i *Item[V]) Get(miss func() (V, error)) (v V, err error, state KeyState) {
	return i.c.Get(struct{}{}, miss)
}

// TryGet returns the value the item if it is cached. This returns either the
// currently loaded value, or the current stale if the load errored, or the
// load error if there is no stale. If nothing is cached, or what is cached is
// expired, this returns Miss.
func (i *Item[V]) TryGet() (v V, err error, state KeyState) {
	return i.c.TryGet(struct{}{})
}

// Delete deletes the value for the item and returns the prior value, if loaded
// (i.e., the return from TryGet).
func (i *Item[V]) Delete() (V, error, KeyState) {
	return i.c.Delete(struct{}{})
}

// Expire sets the item to expire immediately, meaning the next call to Get
// will be a miss. If stale values are enabled, the next Get will trigger the
// miss function but still allow the now-stale value to be returned.
//
// Expire only affects an item that has finished loading. Calling Expire while
// the miss function is still running is a no-op; the in-flight load will
// complete with its normal TTL and is not canceled or shortened.
func (i *Item[V]) Expire() {
	i.c.Expire(struct{}{})
}

// Set sets the value for the item. If the item is currently loading via Get,
// the load is canceled and Get returns the value from Set.
func (i *Item[V]) Set(v V) {
	i.c.Set(struct{}{}, v)
}

// Swap sets the value for the item. If the item is currently loading via Get,
// the load is canceled and Get returns the value from Set. This returns the
// previously stored value, or the previous stale if the load errored, or the
// previous error if there is no stale. If nothing is cached, this returns
// Miss.
func (i *Item[V]) Swap(v V) (old V, oldErr error, oldState KeyState) {
	return i.c.Swap(struct{}{}, v)
}

// CompareAndSwap swaps the old and new values if the value has finished
// loading and the value is equal to old. The type V must be comparable.
func (i *Item[V]) CompareAndSwap(old, new V) (swapped bool) {
	return i.c.CompareAndSwap(struct{}{}, old, new)
}

// CompareAndDelete deletes the item if the value has finished loading and the
// value is equal to old. The type V must be comparable.
func (i *Item[V]) CompareAndDelete(old V) (deleted bool) {
	return i.c.CompareAndDelete(struct{}{}, old)
}

// Clear deletes the cached item, resetting the item to an empty state.
func (i *Item[V]) Clear() {
	i.c.Clear()
}

/////////
// SET //
/////////

// Set caches a set of keys. By default, the set grows without bounds and all
// keys persist forever. These limits can be changed with options that are
// passed to NewSet.
type Set[K comparable] struct {
	c Cache[K, struct{}]
}

// NewSet returns a new Set, with the optional overrides configuring cache
// semantics. If you do not need to configure an set at all, the zero value set
// is valid and usable.
func NewSet[K comparable](opts ...Opt) *Set[K] {
	var c cfg
	for _, opt := range opts {
		opt.apply(&c)
	}
	return &Set[K]{
		c: Cache[K, struct{}]{
			cfg: c,
		},
	}
}

// Get ensures the key is cached, running the miss function in a goroutine if
// the key is not yet cached. If stale keys are enabled, the currently cached
// key has an error, and there is a stale key, this returns with no error and a
// Stale key state.
func (s *Set[K]) Get(k K, miss func() error) (err error, state KeyState) {
	_, err, state = s.c.Get(k, func() (struct{}, error) {
		return struct{}{}, miss()
	})
	return err, state
}

// TryGet any error for the given key if it is cached. This returns either the
// currently stored nil error, or if the current store has an error, the stale
// nil error if present, otherwise the current error. If nothing is cached, or
// what is cached is expired, this returns Miss.
func (s *Set[K]) TryGet(k K) (err error, state KeyState) {
	_, err, state = s.c.TryGet(k)
	return err, state
}

// Delete deletes the key and returns the prior stored error if it existed
// (i.e., the return from TryGet).
func (s *Set[K]) Delete(k K) (error, KeyState) {
	_, err, state := s.c.Delete(k)
	return err, state
}

// Expire sets the key to expire immediately, meaning the next call to Get will
// be a miss. If stale keys are enabled, the next Get will trigger the miss
// function but still allow a now-stale nil error to be returned.
//
// Expire only affects a key whose load has finished. Calling Expire while the
// miss function is still running is a no-op; the in-flight load will complete
// with its normal TTL and is not canceled or shortened.
func (s *Set[K]) Expire(k K) {
	s.c.Expire(k)
}

// Range calls fn for every cached key. If fn returns false, iteration stops.
func (s *Set[K]) Range(fn func(K, error) bool) {
	s.c.Range(func(k K, _ struct{}, err error) bool {
		return fn(k, err)
	})
}

// Clean deletes all expired keys from the cache. A key is expired if MaxAge is
// used and the key is older than the max age, or if you manually expired a
// key. If MaxStaleAge is used and not -1, the entry must be older than MaxAge
// + MaxStaleAge. If MaxStaleAge is -1, Clean returns immediately.
func (s *Set[K]) Clean() {
	s.c.Clean()
}

// Set sets a key to exist. If the key is currently loading via Get,
// the load is canceled and Get returns nil.
func (s *Set[K]) Set(k K) {
	s.c.Set(k, struct{}{})
}

// Clear deletes all keys from the set, resetting it to an empty state.
func (s *Set[K]) Clear() {
	s.c.Clear()
}

// StopAutoClean stops the auto clean goroutine that is began with the
// AutoClean option.
func (s *Set[K]) StopAutoClean() {
	s.c.StopAutoClean()
}
