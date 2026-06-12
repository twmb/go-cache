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
		v   V
		err error
		// expires is the entry's expiry word; see the grammar documented
		// at expiredClaimed below.
		expires atomic.Int64
		// stale is the refresh companion: the previous generation's value,
		// snapshotted when this loading was installed to replace an
		// expired or errored predecessor (entMaybeNewStale), and served
		// while this loading is in flight or while its result is an
		// error. It is immutable once the loading is published. An
		// expired err-free value's own post-expiry stale window is not
		// stored anywhere; it is derived from the expires word at read
		// time (see staleOpen).
		stale *stale[V]
		state atomic.Uint32

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

// The grammar of the loading.expires word. Positive values are
// monotonic-clock nanos (see epoch); all negative values are expired at
// every clock sample, since now is always strictly positive:
//
//	0:    load in flight, or — once finalized — "never expires"
//	> 0:  expires at that nano
//	-2:   expiredClaimed, Clean's claim: the value is expired and its
//	      stale window is certified closed
//	< -2: born expired (MaxAge(0) / MaxErrorAge(0)), carrying the negated
//	      birth nano so the value's stale window can anchor at the birth
//	      (see newExpires, staleOpen, newStale)
//
// The epoch's one-second pad keeps now() at or above a second of nanos, so
// a negated birth can never collide with expiredClaimed. The two negative
// shapes are deliberately distinct: a born-expired value's stale window is
// derived from its birth, while a claimed word means the window was
// certified closed and must never be re-derived (see staleOpen and
// entMaybeNewStale).
const expiredClaimed = -2

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
// sentinels ("no expiry" / "caller samples the clock"), and keeps negated
// births (see the expires-word grammar) below expiredClaimed. Trade-off: on
// platforms whose monotonic clock pauses during system suspend, entries
// age only while the system runs.
var epoch = time.Now().Add(-time.Second)

func now() int64 { return int64(time.Since(epoch)) }

// satAdd returns a+b saturated at math.MaxInt64, for expiry arithmetic on
// nanosecond quantities; b must be non-negative, and callers pass a > 0
// (sentinel expiries are filtered before the math). Without saturation, a
// huge MaxAge / MaxStaleAge / MaxIdleAge would wrap negative and make
// entries born (or instantly) expired.
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
		// Born expired: negative, so the value is expired at every clock
		// sample, carrying the negated birth so its stale window anchors
		// at the birth rather than at whatever read happens to derive it
		// (see staleOpen, newStale). The epoch pad keeps this below
		// expiredClaimed.
		return -now()
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
// The stale window is anchored at the moment the value expired — its TTL
// (or idle extension) lapsing, its birth for values born expired
// (MaxAge(0)), or the Expire call that killed it — and every read agrees
// on it: TryGet serves the stale for exactly as long as a Get-driven
// refresh would, and a value whose window has closed is never served
// again.
//
// A negative value (canonically -1) allows stale values to be returned
// indefinitely.
func MaxStaleAge(age time.Duration) Opt { return opt{fn: func(c *cfg) { c.maxStaleAge = age }} }

// MaxErrorAge sets the age to persist load errors. If not specified, the
// default is MaxAge — so with neither option set, a load error is cached
// forever and never retried (until Set, Swap, Delete, or Expire). Using
// this option with 0 disables caching errors entirely.
//
// A cached error suppresses repeat queries only when there is no stale
// value to return: if stale values are enabled and a valid stale exists,
// Get returns the stale and always drives a refresh, regardless of the
// error's remaining age.
//
// With errors uncached (age 0), concurrent Gets that collapsed onto a load
// that then errors do not share the error: only the Get that drove the
// load returns it, and each collapsed waiter re-drives its own load in
// turn, so one erroring wave of N collapsed Gets can issue up to N
// sequential loads.
func MaxErrorAge(age time.Duration) Opt {
	return opt{fn: func(c *cfg) { c.maxErrAge, c.errAgeSet = age, true }}
}

// MaxIdleAge opts in to extending an entry's expiry on each successful
// access. Each time Get or TryGet returns a live Hit without an error, the
// entry's expiry is reset to now + age. If MaxAge is not set, the idle
// age is also used as the initial TTL. A non-positive age is ignored
// entirely: no extension occurs, and no initial TTL is derived from it.
// The reset applies in both directions: with MaxIdleAge smaller than
// MaxAge, an access shortens the entry's remaining life from the
// MaxAge-given expiry to now + age.
//
// The reset is best effort under races: a concurrent Expire, or a Clean
// that has already deemed the entry expired, wins over an in-flight
// extension. In the other direction, an extension computed just before the
// entry's expiry passed can land just after it and briefly revive the entry
// for another idle window. The clock is re-sampled immediately before an
// extension is published, so the revival window is a few instructions wide
// (stretched only by the extending goroutine being descheduled exactly
// there), but it is not fully closed.
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
//
// If MaxStaleAge is negative (stales kept forever), Clean is a no-op and
// the goroutine is not started at all.
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
		if v, err, s = entGet(e, c.cfg.maxIdleAge, c.cfg.maxStaleAge); s == Hit {
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
			if v, err, s = entGet(e, c.cfg.maxIdleAge, c.cfg.maxStaleAge); s == Hit {
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
	// initial TTL it was born with. staleAge=0: this Get returns the
	// load's result, never a re-derivation of that result as a Stale (a
	// short-TTL value can expire the instant it loads; its derived window
	// serves later readers, not the load that produced it) — the only
	// stale surfaced here is a refresh companion carried by a loading
	// still in flight.
	v, err, s = entGet(e, 0, 0)
	switch s {
	case Miss, Hit:
		l.wg.Wait()
		return l.v, l.err, Miss
	}
	return v, err, Stale
}

// TryGet returns the value for the given key if it is cached. This returns
// either the currently stored value, or the current stale if the latest load
// errored or the value has expired while its stale is still valid, or the
// load error if there is no stale. If nothing is cached, or what is cached is
// expired with no valid stale, this returns Miss.
func (c *Cache[K, V]) TryGet(k K) (v V, err error, _ KeyState) {
	e := c.t.loadEntry(k)
	if e == nil {
		return v, err, Miss
	}
	return entTryGet(e, 0, c.cfg.maxIdleAge, c.cfg.maxStaleAge)
}

// Delete deletes the value for a key and returns the prior value, if stored
// (i.e. the return from TryGet).
//
// Delete physically removes the entry from the trie. If a concurrent Get is
// holding a reference to the deleted entry via its in-flight loading, that
// Get still completes with the miss function's result; a new Get arriving
// after Delete creates a fresh entry and may drive an independent miss. A
// load that finalizes concurrently with the removal may be reported as the
// removed value even though no read ever observed it cached.
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
	return loadingTryGet(was, n, 0, c.cfg.maxStaleAge)
}

// Expire sets a stored value to expire immediately, meaning the next Get will
// be a miss. If stale values are enabled, the next Get will trigger the miss
// function but still allow the now-stale value to be returned.
//
// Expire only affects entries that have finished loading. Calling Expire for a
// key whose miss function is still running is a no-op; the in-flight load will
// complete with its normal TTL and is not canceled or shortened. Expiring an
// already-expired entry is also a no-op: the expiry is never moved forward,
// so a dead value's stale window cannot be re-anchored or revived, and a
// concurrent Clean's claim is never overwritten. (An Expire that races the
// entry's natural death can land just after it and trim the dead value's
// remaining stale window; the window only ever shrinks, never grows.)
func (c *Cache[K, V]) Expire(k K) {
	e := c.t.loadEntry(k)
	if e == nil {
		return
	}
	l := entLoad(e)
	if l == nil || !l.finalized() {
		return
	}
	// CAS rather than a blind store, and only ever backward in time. A
	// blind store would move an already-expired word forward — re-anchoring
	// the dead value's stale window at the Expire (resurrecting data the
	// caller explicitly invalidated) and clobbering Clean's claim sentinel
	// inside its claim window. The loop still beats a racing idle
	// extension: if the extension's CAS lands first, ours fails, reloads
	// the extended (live) expiry, and expires it.
	for {
		cur := l.expires.Load()
		n := now()
		if cur != 0 && cur <= n {
			return // already expired (including born-expired and claimed)
		}
		if l.expires.CompareAndSwap(cur, n-1) {
			return
		}
	}
}

// Range calls fn for every cached value. If fn returns false, iteration stops.
func (c *Cache[K, V]) Range(fn func(K, V, error) bool) {
	// When ranging, repeated time.Now() calls add up, so we get the
	// current time when we enter range and avoid it in all tryGet calls.
	tn := now()
	c.t.walk(func(e *ent[K, V]) bool {
		v, err, s := entTryGet(e, tn, 0, c.cfg.maxStaleAge)
		if s.IsMiss() {
			return true
		}
		return fn(e.key, v, err)
	})
}

// Clean deletes all expired values from the cache. A value is expired if
// MaxAge is used and the entry is older than the max age, or if you manually
// expired a key. An entry is removed only once it is invisible to every
// read: with MaxStaleAge used and not negative, a value's stale window runs
// until MaxAge + MaxStaleAge — anchored at the birth for entries born
// expired (MaxAge(0) or MaxErrorAge(0)) — and an entry whose window is
// still open is never removed. Expired errors are removed as soon as no
// valid stale remains. If MaxStaleAge is negative, Clean returns
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
		l := entLoad(e)
		if l == nil {
			toPrune = append(toPrune, e.key)
			return true
		}
		if !l.finalized() {
			return true
		}
		expires := l.expires.Load()
		// An entry is reclaimable only once it is invisible to every
		// read, i.e. exactly when TryGet would return Miss: the value
		// must be expired, any carried refresh-companion stale (servable
		// while the result is an error) must be out of its window, and an
		// err-free value's derived self-stale window must be closed.
		// staleOpen mirrors the read paths, anchoring born-expired
		// entries at their birth — so dead stale-less entries (error
		// churn under MaxErrorAge(0)) are reclaimed immediately, and a
		// huge MaxStaleAge cannot block their reclaim forever. Errored
		// values have no self-stale: they are reclaimable as soon as
		// they are expired with no valid companion.
		expired := expires != 0 && expires <= tn
		if expired && (l.stale == nil || l.stale.expired(tn)) &&
			(l.err != nil || !staleOpen(expires, tn, c.cfg.maxStaleAge)) {
			// Win the expiry word before tombstoning: idle extension
			// CASes against the expiry it observed, so claiming the
			// word here makes a racing extension fail — the same way it
			// loses to Expire — rather than be silently thrown away by
			// our eviction. If instead the extension wins, the entry is
			// in use; leave it for a later pass.
			//
			// The claimed word also reads as "stale window certified
			// closed" everywhere (staleOpen, entMaybeNewStale), so a
			// racing Get that snapshots this loading between our two
			// CASes can never re-derive a window for the dead value.
			if l.expires.CompareAndSwap(expires, expiredClaimed) && e.p.CompareAndSwap(l, nil) {
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
// load's result is discarded (the miss function itself is not interrupted)
// and Get returns the value from Swap. This returns the previously stored
// value, or the previous stale if the load errored, or the previous error if
// there is no stale. If nothing was cached, this returns Miss.
//
// A load that finalizes concurrently with the swap may be reported as the
// prior value even though no read ever observed it cached.
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
		// The displaced value is reported exactly as a TryGet at wasN
		// would have reported it: classifyFinalized is the one
		// classification every read shares.
		old, oldErr, oldState = classifyFinalized(was, was.expires.Load(), n, c.cfg.maxStaleAge)
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
// load's result is discarded (the miss function itself is not interrupted)
// and Get returns the value from Set.
func (c *Cache[K, V]) Set(k K, v V) {
	c.Swap(k, v)
}

// CompareAndSwap swaps the old and new values for k if the value has finished
// loading without an error, is not expired, and is equal to old. The type V
// must be comparable. Stale values are not considered: only the live value
// is compared against.
//
// The expiry check is best effort: a value that expires while the swap is
// in flight — its TTL lapsing or a concurrent Expire landing — may still
// be swapped.
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
//
// The expiry check is best effort: a value that expires while the delete
// is in flight — its TTL lapsing or a concurrent Expire landing — may
// still be deleted.
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
	if casPublishHook != nil {
		casPublishHook()
	}
	for {
		// Re-validate liveness immediately before publishing: the
		// allocation above is the widest deschedule risk in this call, and
		// a value whose TTL lapsed while we were parked there must not be
		// matched — TryGet already reports it as Miss. The handful of
		// instructions between this check and the CAS remain best effort
		// (see CompareAndSwap).
		if !casMatches(l, old) {
			return false
		}
		if e.p.CompareAndSwap(l, l2) {
			return true
		}
		l = e.p.Load()
	}
}

// casMatches reports whether l holds a live cached value equal to old:
// finalized, not errored, not expired. CompareAndSwap and CompareAndDelete
// must agree with TryGet about whether a value exists — an expired or
// errored entry is not a value that can be compared against (an errored
// loading's v is whatever the miss function returned beside the error,
// which was never cached as a value).
//
// Liveness is judged on a coherent (expiry word, clock) pair from
// expiresNow, exactly as the read paths judge it: the clock postdates the
// word it is paired with, so a kill that completed before the pair was
// taken — a lapsed TTL, an Expire, a Clean claim — is always seen, no
// matter how long this call was descheduled mid-check, while an idle
// extension racing the pair re-derives rather than failing a
// continuously-live match. The pair still cannot be made atomic with the
// publishing CAS: a successful swap takes effect at the pointer CAS, and
// the clock cannot be read at that exact instant. tryCAS re-validates
// immediately before each CAS attempt; the instructions-wide residue
// between that check and the CAS is documented on CompareAndSwap and
// CompareAndDelete.
func casMatches[V any](l *loading[V], old V) bool {
	if l == nil || !l.finalized() || l.err != nil || any(l.v) != any(old) {
		return false
	}
	expires, n := l.expiresNow()
	return expires == 0 || expires > n
}

///////////////////
// ENTRY HELPERS //
///////////////////

func (c *Cache[K, V]) finalizedLoading(v V) *loading[V] {
	// No stale companion: the companion is only ever read while a loading
	// is in flight or errored, and a Set/Swap-created loading is born
	// finalized with no error. The value's own post-expiry stale window is
	// derived from the expires word at read time (see staleOpen), so it
	// needs no snapshot here either.
	l := &loading[V]{
		v: v,
	}
	if expires := c.cfg.newExpires(nil); expires != 0 {
		l.expires.Store(expires)
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

// entMaybeNewStale derives a stale snapshot from the entry's current
// loading, for use as the refresh companion of a replacement loading.
//
//   - if entry has no live loading, no stale, return nil
//   - if no stale age, we are not using stales, return nil
//   - if the loading is still pending, return nil (no value to snapshot)
//   - if loading has an error, return the prior stale (companion passthrough)
//   - if Clean claimed the loading (window certified closed), return nil
//   - if age is < 0, return new unexpiring stale
//   - else, return a stale spanning the value's derived window: its expiry
//     (or birth, for born-expired values) plus the stale age — the same
//     window staleOpen computes on the read paths
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
	expires := l.expires.Load()
	if expires == expiredClaimed {
		// Clean claimed this loading: the value is past its full window,
		// which Clean certified closed. Never re-derive a window for it.
		return nil
	}
	return newStale(l.v, expires, age)
}

// staleOpen reports whether the derived self-stale window of an expired,
// err-free loading is still open at n: the same window newStale builds
// when the value is snapshotted as a refresh companion, so every read
// agrees with every refresh about how long an expired value remains
// servable. expires must be the loading's non-zero expiry word: positive
// is the expiry itself, a negated birth anchors born-expired values at
// their birth, and a Clean-claimed word means the window was certified
// closed.
func staleOpen(expires, n int64, age time.Duration) bool {
	if age == 0 || expires == expiredClaimed {
		return false
	}
	if age < 0 {
		return true
	}
	anchor := expires
	if anchor < 0 {
		anchor = -anchor // born expired: the negated birth (see newExpires)
	}
	return n < satAdd(anchor, int64(age))
}

// newStale returns a fresh stale; age must be non-zero.
//
// expires is the main entry's expiry word. A negative word carries the
// value's negated birth (see newExpires) and anchors the window at the
// birth, matching staleOpen. A 0 word (the value never expires) anchors at
// now; it is reachable only when a racing writer replaced the slot between
// the caller's expiry check and our snapshot, and the caller's install CAS
// then fails and discards the snapshot — but handle it sanely regardless.
// Callers must filter expiredClaimed before calling: a claimed value's
// window is certified closed and must not be re-derived (see
// entMaybeNewStale, staleOpen).
func newStale[V any](v V, expires int64, age time.Duration) *stale[V] {
	if age < 0 {
		return &stale[V]{v: v}
	}
	switch {
	case expires == 0:
		expires = now()
	case expires < 0:
		expires = -expires
	}
	return &stale[V]{v, satAdd(expires, int64(age))}
}

// expiresPairingHook, if non-nil, runs in expiresNow between the expiry
// load and the clock sample, so tests can deterministically hold a reader
// descheduled in that window. Never set in production code.
var expiresPairingHook func()

// casPublishHook, if non-nil, runs in tryCAS between the replacement
// loading's allocation and the pre-publish liveness re-validation, so tests
// can deterministically hold a CAS caller descheduled in that window. Never
// set in production code.
var casPublishHook func()

// expiresNow returns the loading's expiry word and a clock sample forming a
// coherent pair. The word is loaded before the clock is sampled: Expire
// CASes in now()-1 from its own clock, which can be ahead of a clock
// sampled before our load, so sampling after the load guarantees a
// concurrent Expire is classified as expired rather than extended over. The
// word is then confirmed after the sample: an idle extension landing
// between the two would otherwise pair the dead pre-extension word with a
// fresh clock, misreporting an entry that is live for this entire call as
// Stale or Miss. Each retry re-loads before re-sampling, preserving the
// Expire ordering.
func (l *loading[V]) expiresNow() (expires, n int64) {
	expires = l.expires.Load()
	if expiresPairingHook != nil {
		expiresPairingHook()
	}
	for {
		n = now()
		if again := l.expires.Load(); again != expires {
			expires = again
			continue
		}
		return expires, n
	}
}

// classifyFinalized reports a finalized loading exactly as a read at clock
// n does: a valid companion stale (the previous generation) outranks an
// error; with no companion, an unexpired error is itself the cached result
// and a dead error is a miss; an expired err-free value is served as Stale
// while its derived window is open (see staleOpen) and is a miss after.
// entGet, loadingTryGet, and Swap's displaced-value report all share this
// classification, so every read and every report agree by construction.
func classifyFinalized[V any](l *loading[V], expires, n int64, staleAge time.Duration) (v V, err error, state KeyState) {
	expired := expires != 0 && expires <= n
	if l.err != nil {
		if l.stale != nil && !l.stale.expired(n) {
			return l.stale.v, nil, Stale
		}
		if !expired {
			return l.v, l.err, Hit
		}
		return v, err, state
	}
	if expired {
		if staleOpen(expires, n, staleAge) {
			return l.v, nil, Stale
		}
		return v, err, state
	}
	return l.v, l.err, Hit
}

// entGet returns the value, the stale value, or — after waiting on an
// in-flight load — whatever the load produced. When we waited, expiry alone
// does not force a miss: Get must hand its caller something even if the
// value expired the instant it loaded (e.g. request collapsing with
// MaxAge(0)).
func entGet[K comparable, V any](e *ent[K, V], extend, staleAge time.Duration) (v V, err error, state KeyState) {
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

	expires, n := l.expiresNow()
	expired := expires != 0 && expires <= n
	if waited && l.err == nil && expired {
		// We waited and the load finalized already expired (MaxAge(0)
		// collapsing, or a TTL shorter than the load itself): hand back
		// the now-expired value as a courtesy rather than re-drive. The
		// courtesy is not an idle-extending access; the value is
		// returned, not resurrected.
		return l.v, nil, Hit
	}
	v, err, state = classifyFinalized(l, expires, n, staleAge)
	if extend > 0 && state == Hit && l.err == nil {
		// Extend the idle expiry via CAS against the expiry we evaluated
		// above so that a concurrent Expire (or Swap finalization, or a
		// Clean eviction) is not silently overwritten; if expires
		// changed, the other writer wins. An err-free Hit is structurally
		// unexpired at n (a waited-on load that finalized already expired
		// was returned above as a courtesy, never reaching here), but n
		// may have gone stale if we were descheduled since it was
		// sampled: a CAS landing after the expiry passed would revive an
		// entry that concurrent readers may have already reported as
		// Miss. Re-sample the clock immediately before publishing; the
		// instructions between this sample and the CAS remain best
		// effort (see MaxIdleAge).
		if now() < expires {
			l.expires.CompareAndSwap(expires, satAdd(n, int64(extend)))
		}
	}
	return v, err, state
}

func entTryGet[K comparable, V any](e *ent[K, V], n64 int64, extend, staleAge time.Duration) (v V, err error, state KeyState) {
	l := entLoad(e)
	if l == nil {
		return v, err, state
	}
	return loadingTryGet(l, n64, extend, staleAge)
}

// loadingTryGet is entTryGet for a loading already plucked from an entry;
// Delete uses it directly on the loading it captured while tombstoning, so
// the value it returns is exactly the value it removed.
func loadingTryGet[V any](l *loading[V], n64 int64, extend, staleAge time.Duration) (v V, err error, state KeyState) {
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

	var expires int64
	if n64 == 0 {
		expires, n64 = l.expiresNow()
	} else {
		// A caller-provided n64 (Range's batch timestamp, Delete's
		// pre-removal clock) predates this load of the word, so the
		// stale-word hazard expiresNow guards against cannot arise; the
		// caller pins its snapshot to n64 and never extends.
		expires = l.expires.Load()
	}
	n := n64
	v, err, state = classifyFinalized(l, expires, n, staleAge)
	if extend > 0 && state == Hit && l.err == nil {
		// CAS so a concurrent Expire is not silently overwritten; see the
		// matching comment in entGet, including the pre-publish clock
		// re-sample.
		if now() < expires {
			l.expires.CompareAndSwap(expires, satAdd(n, int64(extend)))
		}
	}
	return v, err, state
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

// TryGet returns the item's value if it is cached. This returns either the
// currently loaded value, or the current stale if the latest load errored or
// the value has expired while its stale is still valid, or the load error if
// there is no stale. If nothing is cached, or what is cached is expired with
// no valid stale, this returns Miss.
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
// complete with its normal TTL and is not canceled or shortened. Expiring an
// already-expired item is also a no-op: the expiry is never moved forward.
func (i *Item[V]) Expire() {
	i.c.Expire(struct{}{})
}

// Set sets the value for the item. If the item is currently loading via Get,
// the load's result is discarded (the miss function itself is not
// interrupted) and Get returns the value from Set.
func (i *Item[V]) Set(v V) {
	i.c.Set(struct{}{}, v)
}

// Swap sets the value for the item. If the item is currently loading via
// Get, the load's result is discarded (the miss function itself is not
// interrupted) and Get returns the value from Swap. This returns the
// previously stored value, or the previous stale if the load errored, or the
// previous error if there is no stale. If nothing is cached, this returns
// Miss.
func (i *Item[V]) Swap(v V) (old V, oldErr error, oldState KeyState) {
	return i.c.Swap(struct{}{}, v)
}

// CompareAndSwap swaps the old and new values if the value has finished
// loading without an error, is not expired, and is equal to old. The type V
// must be comparable. See Cache.CompareAndSwap for the stale-handling and
// best-effort expiry caveats.
func (i *Item[V]) CompareAndSwap(old, new V) (swapped bool) {
	return i.c.CompareAndSwap(struct{}{}, old, new)
}

// CompareAndDelete deletes the item if the value has finished loading
// without an error, is not expired, and is equal to old. The type V must be
// comparable. See Cache.CompareAndDelete for the stale-handling and
// best-effort expiry caveats.
func (i *Item[V]) CompareAndDelete(old V) (deleted bool) {
	return i.c.CompareAndDelete(struct{}{}, old)
}

// Clear deletes the cached item, resetting the item to an empty state.
func (i *Item[V]) Clear() {
	i.c.Clear()
}

// Clean deletes the item if it is expired. The item is expired if MaxAge is
// used and the item is older than the max age, or if you manually expired
// it. The item is removed only once it is invisible to every read: with
// MaxStaleAge used and not negative, its stale window runs until
// MaxAge + MaxStaleAge — anchored at the birth if the item was born expired
// (MaxAge(0) or MaxErrorAge(0)) — and it is never removed while the window
// is still open. An expired error is removed as soon as no valid stale
// remains. If MaxStaleAge is negative, Clean returns immediately.
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
// successfully, a stale nil if a valid stale exists while the latest load
// errored or the key expired, or the load error itself. If nothing is
// cached, or what is cached is expired with no valid stale, this returns
// Miss.
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
// with its normal TTL and is not canceled or shortened. Expiring an
// already-expired key is also a no-op: the expiry is never moved forward.
func (s *Set[K]) Expire(k K) {
	s.c.Expire(k)
}

// Range calls fn for every cached key. If fn returns false, iteration stops.
func (s *Set[K]) Range(fn func(K, error) bool) {
	s.c.Range(func(k K, _ struct{}, err error) bool {
		return fn(k, err)
	})
}

// Clean deletes all expired keys from the cache. A key is expired if MaxAge
// is used and the key is older than the max age, or if you manually expired
// a key. An entry is removed only once it is invisible to every read: with
// MaxStaleAge used and not negative, its stale window runs until
// MaxAge + MaxStaleAge — anchored at the birth for entries born expired
// (MaxAge(0) or MaxErrorAge(0)) — and an entry whose window is still open
// is never removed. Expired errors are removed as soon as no valid stale
// remains. If MaxStaleAge is negative, Clean returns immediately.
func (s *Set[K]) Clean() {
	s.c.Clean()
}

// Set sets a key to exist. If the key is currently loading via Get, the
// load's result is discarded (the miss function itself is not interrupted)
// and Get returns nil.
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
