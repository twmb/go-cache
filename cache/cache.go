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
	"math"
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
	//
	// Keys behave like the builtin map's: a key containing a NaN is
	// never equal to itself, so it can be stored but never found,
	// deleted, or cleaned again — every Get with such a key runs the
	// miss function and adds an entry that only Clear removes. Likewise,
	// an interface key holding a non-comparable dynamic value panics,
	// as with a map.
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

// epoch anchors now. Expiry math uses the monotonic clock so that two
// causally ordered samples can never run backwards: entGet's "load the
// expiry, then sample the clock" argument — and Expire reliably winning
// races against idle extension — relies on that, and a wall-clock step
// (NTP, manual adjustment) would otherwise move every TTL. The one-second
// pad keeps now strictly positive so it can never collide with the 0
// sentinels ("no expiry" / "caller samples the clock"). Trade-off: on
// platforms whose monotonic clock pauses during system suspend, entries
// age only while the system runs.
var epoch = time.Now().Add(-time.Second)

func now() int64 { return int64(time.Since(epoch)) }

// satAdd returns a+b saturated at math.MaxInt64, for expiry arithmetic on
// nanosecond quantities; b must be non-negative (a may be the -1 expired
// sentinel). Without saturation, a huge MaxAge / MaxStaleAge / MaxIdleAge
// would wrap negative and make entries born (or instantly) expired.
func satAdd(a, b int64) int64 {
	if s := a + b; s >= a {
		return s
	}
	return math.MaxInt64
}

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
	return satAdd(now(), int64(ttl))
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
// value is returned while the refreshed value is erroring. Without MaxAge,
// stales still apply to manually Expired keys and to erroring refreshes.
//
// A special value of -1 allows stale values to be returned indefinitely.
func MaxStaleAge(age time.Duration) Opt { return opt{fn: func(c *cfg) { c.maxStaleAge = age }} }

// MaxErrorAge sets the age to persist load errors. If not specified, the
// default is MaxAge. Using this option with 0 disables caching errors
// entirely.
//
// A cached error suppresses repeat queries only when there is no stale
// value to return: if stale values are enabled and a valid stale exists,
// Get returns the stale and always drives a refresh, regardless of the
// error's remaining age.
func MaxErrorAge(age time.Duration) Opt {
	return opt{fn: func(c *cfg) { c.maxErrAge, c.errAgeSet = age, true }}
}

// MaxIdleAge opts in to extending an entry's expiry on each successful
// access. Each time Get or TryGet returns a live Hit without an error, the
// entry's expiry is reset to now + age. If MaxAge is not set, the idle
// age is also used as the initial TTL.
//
// The reset is best effort under races: a concurrent Expire, or a Clean
// that has already deemed the entry expired, wins over an in-flight
// extension.
func MaxIdleAge(age time.Duration) Opt {
	return opt{fn: func(c *cfg) { c.maxIdleAge = age }}
}

// AutoCleanInterval begins a goroutine that calls Clean every interval. The
// goroutine can be quit with StopAutoClean. It is recommended to use an
// interval that is less than your MaxAge + MaxStaleAge. Values are only
// candidates to be cleaned after the max age has elapsed. At worst, a value
// may persist for a total of MaxAge + MaxStaleAge + AutoCleanInterval.
//
// The goroutine holds a reference to the cache, so a cache that started
// autocleaning is not garbage collected until StopAutoClean is called.
func AutoCleanInterval(interval time.Duration) Opt {
	return opt{fn: func(c *cfg) { c.autoCleanInterval = interval }}
}

// New returns a new cache, with the optional overrides configuring cache
// semantics. If you do not need to configure a cache at all, the zero value
// cache is valid and usable.
func New[K comparable, V any](opts ...Opt) *Cache[K, V] {
	c := new(Cache[K, V])
	initCache(c, opts...)
	return c
}

// initCache applies options and starts the autoclean goroutine if
// configured. It initializes in place (rather than returning a new cache)
// so that NewItem and NewSet can initialize their embedded Cache: the
// autoclean goroutine closes over c, so the configured cache cannot be
// copied after this returns.
func initCache[K comparable, V any](c *Cache[K, V], opts ...Opt) {
	for _, opt := range opts {
		opt.apply(&c.cfg)
	}

	if c.cfg.autoCleanInterval > 0 && c.cfg.maxStaleAge >= 0 {
		c.quitClean = make(chan struct{})
		go func() {
			ticker := time.NewTicker(c.cfg.autoCleanInterval)
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
}

// Get returns the cache value for k, running the miss function in a goroutine
// if the key is not yet cached. If stale values are enabled, the currently
// cached value has an error, and there is an unexpired stale value, this
// returns the stale value and no error.
//
// The miss function runs on an internal goroutine: if it panics, the process
// crashes (the panic cannot be recovered by the Get caller); recover inside
// miss if you need to survive panics. A miss function must not call Get for
// the same key, in this goroutine or another: the inner Get would wait on
// the load the outer Get is driving and deadlock.
func (c *Cache[K, V]) Get(k K, miss func() (V, error)) (v V, err error, s KeyState) {
	// Fast path: if the entry already exists and holds a live value, use
	// it without any locking.
	e := c.t.loadEntry(k)
	if e != nil {
		if v, err, s = entGet(e, c.cfg.maxIdleAge); s == Hit {
			return v, err, s
		}
	}

	// Slow path: acquire or create the entry, installing a fresh loading
	// to drive a miss if there is no live value. A loading is always
	// created before the entry that holds it is published, so a linked
	// entry never holds a nil value slot; nil means the entry was
	// tombstoned by Delete or Clean and is permanently dead (see entDel).
	var l *loading[V]
outer:
	for {
		if e == nil {
			l = &loading[V]{}
			l.wg.Add(1)
			var loaded bool
			if e, loaded = c.t.loadOrStoreEntry(k, l); !loaded {
				break // we created the entry, holding our loading
			}
		}
		for {
			prev := e.p.Load()
			if prev == nil {
				// Tombstoned. Unlink the dead entry (idempotent: the
				// predicate re-checks under the bucket lock and leaves
				// a re-created live entry alone) and start over with a
				// fresh entry.
				c.t.deleteEntryIf(k, entDead[K, V])
				e = nil
				continue outer
			}
			// There is an existing value. Use entGet to decide whether
			// to wait for it (finalized hit), return a stale (load in
			// flight with a valid stale), or replace it (finalized but
			// expired or errored).
			if v, err, s = entGet(e, c.cfg.maxIdleAge); s == Hit {
				return v, err, s
			}
			if s == Stale && !prev.finalized() && e.p.Load() == prev {
				// A load is already in flight and its caller will
				// finalize it; piggyback and return the stale now. The
				// slot re-check pins the stale to prev: entGet loads
				// the slot itself, so without it the stale could come
				// from a newer loading while the in-flight test
				// inspects prev.
				return v, err, s
			}
			// Replace prev only if it is finalized and expired or
			// errored. entGet's non-Hit result usually implies that,
			// but entGet re-loads the slot itself: a racing writer can
			// finalize prev live (or replace it) between our load of
			// prev and entGet's. Re-deriving from prev keeps us from
			// demoting a freshly finalized live value to a stale and
			// driving a redundant load; the retry re-reads the slot.
			if !prev.finalized() || (prev.err == nil && !prev.expired(now())) {
				continue
			}
			// prev is finalized and expired or errored, possibly with a
			// valid stale that nothing is refreshing (if it was in
			// flight with no stale, entGet waited for it to finalize).
			// Install a fresh loading carrying a stale snapshot of prev
			// and drive a new load. The CAS fails if another goroutine
			// got here first; re-evaluate what it installed.
			l = &loading[V]{
				stale: entMaybeNewStale(e, c.cfg.maxStaleAge),
			}
			l.wg.Add(1)
			if e.p.CompareAndSwap(prev, l) {
				break outer
			}
		}
	}

	go func() { v, err := miss(); l.setve(v, err, c.cfg.newExpires(err)) }()

	// Wait for our loading to finalize, then return the value. Waiting via
	// l.wg (our own) synchronizes with whoever called wg.Done — the miss
	// goroutine's setve or a racing Swap's defer — even if e.p has since
	// been replaced by another writer. extend=0: this Get drove the load
	// and publicly returns Miss, so it is not an idle-extending access
	// (MaxIdleAge documents extension on Hits); the value keeps the
	// initial TTL it was born with.
	v, err, s = entGet(e, 0)
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
	// Tombstone the value slot first; the captured loading is exactly what
	// this Delete removed, so the returned value is the value removed.
	// Then physically unlink the entry from the trie so the key (and any
	// memory it refers to) is eligible for GC. The nil slot is terminal
	// (see entDel), so entDead observed under the bucket lock is stable; a
	// concurrent Swap or Get that finds the tombstone re-creates the key
	// as a fresh entry, which the predicate leaves alone.
	//
	// The clock is sampled before the tombstone CAS so the removed value
	// is classified (live, stale, or expired) as of the removal itself;
	// sampling after would let a value that expires in between be
	// misreported as already gone even though this Delete removed it live.
	n := now()
	was := entDel(e)
	c.t.deleteEntryIf(k, entDead[K, V])
	if was == nil {
		return v, err, Miss
	}
	return loadingTryGet(was, n, 0)
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
// than MaxAge + MaxStaleAge, and an entry whose stale value is still within
// its window is never removed. If MaxStaleAge is -1, Clean returns
// immediately.
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
		// The stale check matters for born-expired entries (MaxAge(0) /
		// MaxErrorAge(0) store the -1 sentinel): their grace window would
		// otherwise anchor at the epoch rather than at the entry, evicting
		// stales that TryGet would still serve. For normally expired
		// entries the stale dies at expires+maxStaleAge anyway, so the
		// check changes nothing.
		if expires != 0 && tn > satAdd(expires, int64(c.cfg.maxStaleAge)) &&
			(l.stale == nil || l.stale.expired(tn)) {
			// Win the expiry word before tombstoning: idle extension
			// CASes against the expiry it observed, so claiming the
			// word here (writing the expired sentinel) makes a racing
			// extension fail — the same way it loses to Expire — rather
			// than be silently thrown away by our eviction. If instead
			// the extension wins, the entry is in use; leave it for a
			// later pass.
			if l.expires.CompareAndSwap(expires, -1) && e.p.CompareAndSwap(l, nil) {
				toPrune = append(toPrune, e.key)
			}
		}
		return true
	})

	// Second pass: physically unlink the tombstoned entries. The nil value
	// slot is terminal (see entDel), so the predicate observing nil under
	// the bucket lock cannot be invalidated by a concurrent writer; if the
	// key was deleted and re-created in the meantime, the lookup finds the
	// fresh live entry and the predicate leaves it alone.
	for _, k := range toPrune {
		c.t.deleteEntryIf(k, entDead[K, V])
	}
}

// Clear deletes all keys from the cache, resetting it to an empty state.
func (c *Cache[K, V]) Clear() {
	c.t.clear()
}

// StopAutoClean stops the auto clean goroutine that is begun with the
// AutoCleanInterval option.
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
	var wasN int64
	defer func() {
		if was == nil {
			return
		}

		// The following block is similar to setve, but expanded to
		// return our prior value (or stale, or err) if it exists. wasN
		// was sampled just before the CAS that displaced was, so the
		// displaced value is classified (live, stale, or expired) as of
		// the swap itself; a fresh sample here could misreport a value
		// that expired between the CAS and this defer.
		n := wasN
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

	// Fast path: if the entry already exists with a live value, CAS the
	// value slot directly without going through the trie's bucket lock. A
	// nil slot means the entry was tombstoned by Delete or Clean; it is
	// never written again (see entDel), so we fall to the slow path to
	// re-create the key rather than writing into the dead entry.
	if e := c.t.loadEntry(k); e != nil {
		if rm := e.p.Load(); rm != nil {
			was, wasN = rm, now()
			if e.p.CompareAndSwap(rm, l) {
				return old, oldErr, oldState
			}
			// Lost the CAS; fall into the slow path.
		}
	}

	// Slow path: get or create an entry and swap our value in. The trie's
	// bucket lock makes creation exclusive; tombstoned entries are
	// unlinked and the key re-created as a fresh entry, never resurrected
	// in place.
	for {
		e, loaded := c.t.loadOrStoreEntry(k, l)
		if !loaded {
			// We are the entry's installer; nothing was there before.
			was = nil
			return old, oldErr, oldState
		}
		for {
			rm := e.p.Load()
			if rm == nil {
				// Tombstoned by Delete or Clean. Unlink the dead entry
				// and retry with a fresh one.
				c.t.deleteEntryIf(k, entDead[K, V])
				break
			}
			was, wasN = rm, now()
			if e.p.CompareAndSwap(rm, l) {
				return old, oldErr, oldState
			}
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
// loading without an error, is not expired, and is equal to old. The type V
// must be comparable. Stale values are not considered: only the live value
// is compared against.
func (c *Cache[K, V]) CompareAndSwap(k K, old, new V) bool {
	e := c.t.loadEntry(k)
	if e == nil {
		return false
	}
	return c.tryCAS(e, old, new, true)
}

// CompareAndDelete deletes the entry for k if the value has finished loading
// without an error, is not expired, and is equal to old. The type V must be
// comparable. Stale values are not considered: only the live value is
// compared against.
func (c *Cache[K, V]) CompareAndDelete(k K, old V) bool {
	e := c.t.loadEntry(k)
	if e == nil {
		return false
	}
	if !c.tryCAS(e, old, old, false) {
		return false
	}
	// The successful CAS tombstoned the value slot; physically unlink the
	// entry like Delete does, so delete churn does not leak entries.
	c.t.deleteEntryIf(k, entDead[K, V])
	return true
}

func (c *Cache[K, V]) tryCAS(e *ent[K, V], old, new V, useNew bool) bool {
	l := e.p.Load()
	if !casMatches(l, old) {
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
		if !casMatches(l, old) {
			return false
		}
	}
}

// casMatches reports whether l holds a live cached value equal to old:
// finalized, not errored, not expired. CompareAndSwap and CompareAndDelete
// must agree with TryGet about whether a value exists — an expired or
// errored entry is not a value that can be compared against (an errored
// loading's v is whatever the miss function returned beside the error,
// which was never cached as a value).
func casMatches[V any](l *loading[V], old V) bool {
	return l != nil && l.finalized() && l.err == nil &&
		any(l.v) == any(old) && !l.expired(now())
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

func (l *loading[V]) finalized() bool { return l.state.Load() != 0 }

// entDel tombstones the entry by atomically setting its value slot to nil,
// returning the loading it displaced (nil if the entry was already
// tombstoned).
//
// A nil value slot is terminal: no code path ever stores a non-nil loading
// over it. Writers that encounter a tombstone unlink the dead entry
// (deleteEntryIf with entDead) and re-create the key as a fresh entry. This
// is what makes physical unlinking sound: entDead evaluated under the
// trie's bucket lock cannot be invalidated by a concurrent lock-free CAS,
// so an unlinked entry is always dead and a live entry is never unlinked.
func entDel[K comparable, V any](e *ent[K, V]) *loading[V] {
	for {
		p := e.p.Load()
		if p == nil {
			return nil
		}
		if e.p.CompareAndSwap(p, nil) {
			return p
		}
	}
}

// entDead reports whether the entry is tombstoned. Used as a deleteEntryIf
// predicate: nil value slots are terminal (see entDel), so a nil observed
// under the bucket lock is stable.
func entDead[K comparable, V any](e *ent[K, V]) bool { return e.p.Load() == nil }

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
	return &stale[V]{v, satAdd(expires, int64(age))}
}

// entGet returns the value, the stale value, or — after waiting on an
// in-flight load — whatever the load produced. When we waited, expiry alone
// does not force a miss: Get must hand its caller something even if the
// value expired the instant it loaded (e.g. request collapsing with
// MaxAge(0)).
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
	//
	// The expiry must be loaded before the clock is sampled: Expire stores
	// now()-1 from its own clock, which can be ahead of a clock sampled
	// before our load; sampling after the load guarantees a concurrent
	// Expire is classified as expired here rather than extended over.
	expires := l.expires.Load()
	n := now()
	expired := expires != 0 && expires <= n
	if (!waited && expired) || l.err != nil {
		if l.stale != nil && !l.stale.expired(n) {
			return l.stale.v, nil, Stale
		}
		// The stale value is expired: if our entry is not expired,
		// this must be an error we waited on.
		if !expired {
			return l.v, l.err, Hit
		}
		return v, err, state
	}
	if extend > 0 && l.err == nil && !expired {
		// Extend the idle expiry via CAS against the expiry we evaluated
		// above so that a concurrent Expire (or Swap finalization, or a
		// Clean eviction) is not silently overwritten; if expires
		// changed, the other writer wins. Never extend an expired entry:
		// a waited-on load that finalized already expired (MaxAge(0)
		// collapsing, or a TTL shorter than the load itself) is returned
		// as a courtesy, not resurrected.
		l.expires.CompareAndSwap(expires, satAdd(n, int64(extend)))
	}
	return l.v, l.err, Hit
}

func entTryGet[K comparable, V any](e *ent[K, V], n64 int64, extend time.Duration) (v V, err error, state KeyState) {
	l := entLoad(e)
	if l == nil {
		return v, err, state
	}
	return loadingTryGet(l, n64, extend)
}

// loadingTryGet is entTryGet for a loading already plucked from an entry;
// Delete uses it directly on the loading it captured while tombstoning, so
// the value it returns is exactly the value it removed.
func loadingTryGet[V any](l *loading[V], n64 int64, extend time.Duration) (v V, err error, state KeyState) {
	// Fast path: finalized, no expiry, no error — skip time checks.
	if l.finalized() && l.expires.Load() == 0 && l.err == nil {
		return l.v, nil, Hit
	}
	// If we are loading but there is a valid stale, return it, otherwise
	// return immediately: no get.
	if !l.finalized() {
		if n64 == 0 {
			n64 = now()
		}
		if l.stale != nil && !l.stale.expired(n64) {
			return l.stale.v, nil, Stale
		}
		return v, err, state
	}

	// If we have an error or we are expired, we maybe return the stale.
	// The expiry must be loaded before the clock is sampled (when we are
	// the one sampling it); see the matching comment in entGet. A caller-
	// provided n64 (Range's batch timestamp, Delete's pre-removal clock)
	// never extends, so the ordering does not matter there.
	expires := l.expires.Load()
	if n64 == 0 {
		n64 = now()
	}
	n := n64
	expired := expires != 0 && expires <= n
	if l.err != nil || expired {
		if l.stale != nil && !l.stale.expired(n) {
			return l.stale.v, nil, Stale
		}
		if expired {
			return v, err, state
		}
	}
	if extend > 0 && l.err == nil {
		// CAS so a concurrent Expire is not silently overwritten; see the
		// matching comment in entGet. This path is unreachable when
		// expired (the branch above returns), so unlike entGet no
		// explicit !expired gate is needed.
		l.expires.CompareAndSwap(expires, satAdd(n, int64(extend)))
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
	i := new(Item[V])
	initCache(&i.c, opts...)
	return i
}

// Get returns the currently cached value, running the miss function in a
// goroutine if the item is not yet cached. If stale values are enabled, the
// currently cached value has an error, and there is an unexpired stale value,
// this returns the stale value and no error. See Cache.Get for the miss
// function's panic and re-entrancy caveats.
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
// loading without an error, is not expired, and is equal to old. The type V
// must be comparable.
func (i *Item[V]) CompareAndSwap(old, new V) (swapped bool) {
	return i.c.CompareAndSwap(struct{}{}, old, new)
}

// CompareAndDelete deletes the item if the value has finished loading
// without an error, is not expired, and is equal to old. The type V must be
// comparable.
func (i *Item[V]) CompareAndDelete(old V) (deleted bool) {
	return i.c.CompareAndDelete(struct{}{}, old)
}

// Clear deletes the cached item, resetting the item to an empty state.
func (i *Item[V]) Clear() {
	i.c.Clear()
}

// Clean deletes the item if it is expired. The item is expired if MaxAge is
// used and the item is older than the max age, or if you manually expired
// it. If MaxStaleAge is used and not -1, the item must be older than MaxAge
// + MaxStaleAge. If MaxStaleAge is -1, Clean returns immediately.
func (i *Item[V]) Clean() {
	i.c.Clean()
}

// StopAutoClean stops the auto clean goroutine that is begun with the
// AutoCleanInterval option.
func (i *Item[V]) StopAutoClean() {
	i.c.StopAutoClean()
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
// semantics. If you do not need to configure a set at all, the zero value set
// is valid and usable.
func NewSet[K comparable](opts ...Opt) *Set[K] {
	s := new(Set[K])
	initCache(&s.c, opts...)
	return s
}

// Get ensures the key is cached, running the miss function in a goroutine if
// the key is not yet cached. If stale keys are enabled, the currently cached
// key has an error, and there is a stale key, this returns with no error and a
// Stale key state. See Cache.Get for the miss function's panic and
// re-entrancy caveats.
func (s *Set[K]) Get(k K, miss func() error) (err error, state KeyState) {
	_, err, state = s.c.Get(k, func() (struct{}, error) {
		return struct{}{}, miss()
	})
	return err, state
}

// TryGet returns the cached state for the given key: nil if the key loaded
// successfully, a stale nil if the latest load errored but a valid stale
// exists, or the load error itself. If nothing is cached, or what is cached
// is expired, this returns Miss.
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

// StopAutoClean stops the auto clean goroutine that is begun with the
// AutoCleanInterval option.
func (s *Set[K]) StopAutoClean() {
	s.c.StopAutoClean()
}
