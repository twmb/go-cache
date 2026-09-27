// Package cache provides a concurrency safe, mostly lock-free, singleflight
// request collapsing generic cache with support for stale values.
//
// The guts of this package are a concurrent hash-trie similar to the one
// backing `sync.Map`, with major differences to support singleflight request
// collapsing.
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
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"time"
	"weak"
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
		expires atomic.Int64 // see expiredClaimed for what values mean
		state   atomic.Uint32
		wg      sync.WaitGroup

		// x is nil unless the load errored or the loading has a stale,
		// which keeps the common loading small.
		x atomic.Pointer[loadingExtra[V]]
	}

	loadingWithExtra[V any] struct {
		l loading[V]
		x loadingExtra[V]
	}

	loadingExtra[V any] struct {
		err error // written before the loading is finalized

		// stale is the previous value, captured when a Get replaces an
		// expired or errored loading. We return it while this load is in
		// flight or if this load errors. It is set before the loading is
		// published and never changes.
		stale    stale[V]
		hasStale bool
	}

	// Cache caches comparable keys to arbitrary values. By default, the
	// cache grows without bounds and all keys persist forever. These
	// limits can be changed with options that are passed to New.
	Cache[K comparable, V any] struct {
		t trie[K, loading[V]]

		cfg cfg

		quitOnce  sync.Once
		quitClean chan struct{}
		cleanDone chan struct{}
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
	stateFinalizing
	stateFinalized
)

// expiredClaimed is the loading.expires value Clean stores to claim an
// entry for deletion. The full set of values is:
//
//	0     loading, or loaded and never expires
//	> 0   expires at this now() nano
//	-2    claimed by Clean: expired, and its stale window is closed
//	< -2  born expired (MaxAge(0), MaxErrorAge(0)): the negated now() at
//	      load, which is where the stale window starts
//
// Every negative value is expired, because now() is always positive. The
// epoch is padded by a second so that a negated now() is never -2.
const expiredClaimed = -2

// ent is what the cache stores in the trie.
type ent[K comparable, V any] = trieEntry[K, loading[V]]

// We use the monotonic clock so that expiries do not move if the wall clock
// is stepped. Note that if the monotonic clock pauses while the system is
// suspended, entries only age while the system is running.
var epoch = time.Now().Add(-time.Second)

// testNow, if non-zero, is what now returns, so that tests can freeze the
// clock. Never set in production code.
var testNow atomic.Int64

func now() int64 {
	if n := testNow.Load(); n != 0 {
		return n
	}
	return int64(time.Since(epoch))
}

// satAdd returns a+b capped at math.MaxInt64, so that a huge age does not
// wrap an expiry negative. b must be non-negative.
func satAdd(a, b int64) int64 {
	if s := a + b; s >= a {
		return s
	}
	return math.MaxInt64
}

// newExpires returns the expiry for a value or err loaded at n, sampling
// the clock only if it is needed and n is 0.
func (cfg *cfg) newExpires(err error, n int64) int64 {
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
	if !del0 && ttl == 0 {
		return 0
	}
	if n == 0 {
		n = now()
	}
	if del0 {
		return -n
	}
	return satAdd(n, int64(ttl))
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
//
// The goroutine exits once the cache is garbage collected.
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

// initCache initializes c in place so that NewItem and NewSet can initialize
// their embedded Cache; the autoclean goroutine references c.
func initCache[K comparable, V any](c *Cache[K, V], opts ...Opt) {
	for _, opt := range opts {
		opt.apply(&c.cfg)
	}

	if c.cfg.autoCleanInterval > 0 && c.cfg.maxStaleAge >= 0 {
		c.quitClean = make(chan struct{})
		c.cleanDone = make(chan struct{})
		go autoClean(weak.Make(c), c.cfg.autoCleanInterval, c.quitClean, c.cleanDone)
	}
}

// autoClean cleans the cache every interval. It holds the cache only
// weakly, so that a cache nobody stopped can still be garbage collected.
func autoClean[K comparable, V any](wc weak.Pointer[Cache[K, V]], interval time.Duration, quit, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
		case <-quit:
			return
		}
		c := wc.Value()
		if c == nil {
			return
		}
		c.Clean()
	}
}

// Get returns the cache value for k, running the miss function if the key is
// not yet cached. If stale values are enabled, the currently cached value has
// an error, and there is an unexpired stale value, this returns the stale
// value and no error.
//
// The miss function runs in the calling goroutine, unless Get returns a stale
// value: then it runs in a new goroutine and may still be running when Get
// returns.
func (c *Cache[K, V]) Get(k K, miss func() (V, error)) (v V, err error, s KeyState) {
	e := c.t.loadEntry(k)
	if e != nil {
		if v, err, s = entGet(e, c.cfg.maxIdleAge, c.cfg.maxStaleAge); s == Hit {
			return v, err, s
		}
	}
	return c.getSlow(k, e, miss, func() func() (V, error) { return miss })
}

// getSlow is Get after the hit check on e, which is k's entry or nil. It
// takes the miss function in two forms: load runs the load in this
// goroutine, and detach returns a function that runs the load after
// getSlow returns. We only call detach to refresh in the background, so
// load never escapes and callers only allocate for detach when we refresh.
func (c *Cache[K, V]) getSlow(k K, e *ent[K, V], load func() (V, error), detach func() func() (V, error)) (v V, err error, s KeyState) {

	// We need to load. We either create the entry with our loading, or
	// replace the entry's finalized expired or errored loading with ours.
	// A nil value is a deleted entry that is never written again (see
	// entDel), so we unlink it and create a new entry.
	var l *loading[V]
outer:
	for {
		if e == nil {
			l = &loading[V]{}
			l.wg.Add(1)
			var loaded bool
			if e, loaded = c.t.loadOrStoreEntry(k, l); !loaded {
				break
			}
		}
		for {
			prev := e.p.Load()
			if prev == nil {
				c.t.deleteEntryIf(k, entDead[K, V])
				e = nil
				continue outer
			}
			if v, err, s = entGet(e, c.cfg.maxIdleAge, c.cfg.maxStaleAge); s == Hit {
				return v, err, s
			}
			// entGet loads the value itself, so what it looked at may not
			// be prev. We only return a stale if it came from prev while
			// prev is still loading, and we only replace prev if prev
			// itself is finalized and expired or errored.
			if s == Stale && !prev.finalized() && e.p.Load() == prev {
				return v, err, s
			}
			if !prev.finalized() || (prev.err() == nil && !prev.expired(now())) {
				continue
			}
			if st, ok := maybeNewStale(prev, c.cfg.maxStaleAge); ok {
				// One allocation for both: they have the same lifetime.
				lx := &loadingWithExtra[V]{x: loadingExtra[V]{stale: st, hasStale: true}}
				l = &lx.l
				l.x.Store(&lx.x)
			} else {
				l = new(loading[V])
			}
			l.wg.Add(1)
			if e.p.CompareAndSwap(prev, l) {
				break outer
			}
		}
	}

	if st := l.stale(); st != nil && !st.expired(now()) {
		go c.load(l, detach())
		return st.v, nil, Stale
	}

	// We return what our load produced, even if it is already expired: we
	// do not re-derive a stale from it, and we do not extend its idle age,
	// because we are a Miss. If a Swap replaced our load, the Swap may
	// still be finalizing l, so we wait.
	c.load(l, load)
	l.wg.Wait()
	return l.v, l.err(), Miss
}

// errMissPanicked is what a load is finalized with if the miss function
// panics or exits its goroutine. It is born expired and never observable:
// every read of it is a Miss (or the previous value's stale), so waiting
// Gets run their own load.
var errMissPanicked = errors.New("cache: miss function panicked")

func (c *Cache[K, V]) load(l *loading[V], miss func() (V, error)) {
	var done bool
	defer func() {
		if !done {
			var zero V
			l.setve(zero, errMissPanicked, -now())
		}
	}()
	v, err := miss()
	done = true
	l.setve(v, err, c.cfg.newExpires(err, 0))
}

// TryGet returns the value for the given key if it is cached. This returns
// either the currently stored value, or the current stale if the load errored
// or the value expired, or the load error if there is no stale. If nothing is
// cached, or what is cached is expired, this returns Miss.
func (c *Cache[K, V]) TryGet(k K) (V, error, KeyState) {
	e := c.t.loadEntry(k)
	if e == nil {
		var v V
		return v, nil, Miss
	}
	return entTryGet(e, 0, c.cfg.maxIdleAge, c.cfg.maxStaleAge)
}

// Delete deletes the value for a key and returns the prior value, if stored
// (i.e. the return from TryGet).
func (c *Cache[K, V]) Delete(k K) (V, error, KeyState) {
	var v V
	e := c.t.loadEntry(k)
	if e == nil {
		return v, nil, Miss
	}
	// We sample the clock before deleting so that we report the value as
	// it was when we deleted it.
	n := now()
	was := entDel(e)
	c.t.deleteEntryIf(k, entDead[K, V])
	if was == nil {
		return v, nil, Miss
	}
	return loadingTryGet(was, n, 0, c.cfg.maxStaleAge)
}

// Expire sets a stored value to expire immediately, meaning the next Get will
// be a miss. If stale values are enabled, the next Get will trigger the miss
// function but still allow the now-stale value to be returned.
func (c *Cache[K, V]) Expire(k K) {
	e := c.t.loadEntry(k)
	if e == nil {
		return
	}
	// We replace the loading with an expired copy rather than writing its
	// expiry: a Set or Swap can replace the loading between our load and
	// our write, and a write to the replaced loading would be lost. With
	// the CAS, either we win and the Swap sees an expired value, or the
	// Swap wins and we expire its value.
	for {
		l := e.p.Load()
		if l == nil || !l.finalized() {
			return
		}
		cur := l.expires.Load()
		n := now()
		if cur != 0 && cur <= n {
			return // never move an expiry later
		}
		l2 := &loading[V]{v: l.v}
		l2.x.Store(l.x.Load())
		l2.expires.Store(n - 1)
		l2.state.Store(stateFinalized)
		if e.p.CompareAndSwap(l, l2) {
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
// expired a key. If MaxStaleAge is used and not -1, the entry must be older
// than MaxAge + MaxStaleAge. If MaxStaleAge is -1, Clean returns immediately.
func (c *Cache[K, V]) Clean() {
	if c.cfg.maxStaleAge < 0 {
		return
	}
	tn := now()

	// We delete exactly what TryGet would return Miss for: an expired value
	// whose stale (if any) has also expired. Unlinking while we walk is
	// safe: the walk holds its own references, and unlinking never moves
	// an entry.
	c.t.walk(func(e *ent[K, V]) bool {
		l := e.p.Load()
		if l == nil {
			c.t.deleteEntryIf(e.key, entDead[K, V])
			return true
		}
		if !l.finalized() {
			return true
		}
		expires := l.expires.Load()
		expired := expires != 0 && expires <= tn
		if st := l.stale(); expired && (st == nil || st.expired(tn)) &&
			(l.err() != nil || !staleOpen(expires, tn, c.cfg.maxStaleAge)) {
			// We claim the expiry before deleting so that a racing idle
			// extension, which CASes the expiry it read, fails. If the
			// extension wins, the entry is in use and we leave it.
			if l.expires.CompareAndSwap(expires, expiredClaimed) && e.p.CompareAndSwap(l, nil) {
				c.t.deleteEntryIf(e.key, entDead[K, V])
			}
		}
		return true
	})
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
			<-c.cleanDone
		}
	})
}

// Swap sets a value for a key. If the key is currently loading via Get, the
// load is canceled and Get returns the value from Swap. This returns the
// previously stored value, or the previous stale if the load errored, or the
// previous error if there is no stale. If nothing was cached, this returns
// Miss.
func (c *Cache[K, V]) Swap(k K, v V) (old V, oldErr error, oldState KeyState) {
	// We use one clock sample for our value's expiry and for reporting
	// what we replace. It predates whichever CAS succeeds.
	n := now()
	l := c.finalizedLoading(v, n)

	// was is what we replaced, and wasN is the clock just before we did.
	var was *loading[V]
	var wasN int64
	defer func() {
		if was == nil {
			return
		}
		if !was.finalized() {
			// If was is still loading, we finalize it with our value so
			// that Gets waiting on it return our value.
			if was.setve(v, nil, l.expires.Load()) {
				if st := was.stale(); st != nil && !st.expired(wasN) {
					old, oldState = st.v, Stale
				}
				return
			}
			was.wg.Wait() // the load finalized was first
		}
		old, oldErr, oldState = classifyFinalized(was, was.expires.Load(), wasN, c.cfg.maxStaleAge)
	}()

	if e := c.t.loadEntry(k); e != nil {
		if rm := e.p.Load(); rm != nil {
			was, wasN = rm, n
			if e.p.CompareAndSwap(rm, l) {
				return old, oldErr, oldState
			}
		}
	}
	for {
		e, loaded := c.t.loadOrStoreEntry(k, l)
		if !loaded {
			was = nil
			return old, oldErr, oldState
		}
		for {
			rm := e.p.Load()
			if rm == nil {
				c.t.deleteEntryIf(k, entDead[K, V])
				break
			}
			was, wasN = rm, n
			if e.p.CompareAndSwap(rm, l) {
				return old, oldErr, oldState
			}
		}
	}
}

// setve finalizes l if it is still loading, returning whether we did.
func (l *loading[V]) setve(v V, err error, expires int64) bool {
	if !l.state.CompareAndSwap(stateLoading, stateFinalizing) {
		return false
	}
	l.v = v
	if err != nil {
		if x := l.x.Load(); x != nil {
			x.err = err
		} else {
			l.x.Store(&loadingExtra[V]{err: err})
		}
	}
	l.expires.Store(expires)
	l.state.Store(stateFinalized)
	l.wg.Done()
	return true
}

// Set sets a value for a key. If the key is currently loading via Get, the
// load is canceled and Get returns the value from Set.
func (c *Cache[K, V]) Set(k K, v V) {
	c.Swap(k, v)
}

// CompareAndSwap swaps the old and new values for k if the value has finished
// loading, is not expired or errored, and is equal to old. The type V must be
// comparable.
func (c *Cache[K, V]) CompareAndSwap(k K, old, new V) bool {
	e := c.t.loadEntry(k)
	if e == nil {
		return false
	}
	return c.tryCAS(e, old, new, true)
}

// CompareAndDelete deletes the entry for k if the value has finished loading,
// is not expired or errored, and is equal to old. The type V must be
// comparable.
func (c *Cache[K, V]) CompareAndDelete(k K, old V) (deleted bool) {
	e := c.t.loadEntry(k)
	if e == nil {
		return false
	}
	if !c.tryCAS(e, old, old, false) {
		return false
	}
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
		l2 = c.finalizedLoading(new, 0)
	}
	if casPublishHook != nil {
		casPublishHook()
	}
	for {
		// We check again after allocating: if we were descheduled, the
		// value may have expired.
		if !casMatches(l, old) {
			return false
		}
		if e.p.CompareAndSwap(l, l2) {
			return true
		}
		l = e.p.Load()
	}
}

// casMatches returns whether l is a loaded, unerrored, unexpired value equal
// to old, i.e. whether TryGet would return old as a Hit.
func casMatches[V any](l *loading[V], old V) bool {
	if l == nil || !l.finalized() || l.err() != nil || any(l.v) != any(old) {
		return false
	}
	expires := l.expires.Load()
	if expires == 0 {
		return true
	}
	expires, n := l.expiresNow(expires)
	return expires == 0 || expires > n
}

///////////////////
// ENTRY HELPERS //
///////////////////

func (c *Cache[K, V]) finalizedLoading(v V, n int64) *loading[V] {
	l := &loading[V]{v: v}
	if expires := c.cfg.newExpires(nil, n); expires != 0 {
		l.expires.Store(expires)
	}
	l.state.Store(stateFinalized)
	return l
}

func (l *loading[V]) finalized() bool { return l.state.Load() == stateFinalized }

// err returns the load's error. l must be finalized.
func (l *loading[V]) err() error {
	if x := l.x.Load(); x != nil {
		return x.err
	}
	return nil
}

// stale returns the loading's stale, or nil if it has none.
func (l *loading[V]) stale() *stale[V] {
	if x := l.x.Load(); x != nil && x.hasStale {
		return &x.stale
	}
	return nil
}

// entDel deletes the entry's value, returning what it was.
//
// A nil value is permanent: nothing stores a value into an entry after it
// is nil. Anything that finds a nil value unlinks the entry and creates a
// new one. This is what makes entDead a safe deleteEntryIf predicate: a
// live entry can never be unlinked.
func entDel[K comparable, V any](e *ent[K, V]) *loading[V] {
	return e.p.Swap(nil)
}

func entDead[K comparable, V any](e *ent[K, V]) bool { return e.p.Load() == nil }

func (l *loading[V]) expired(n int64) bool {
	expires := l.expires.Load()
	return expires != 0 && expires <= n // 0 means either miss not resolved, or no max age
}

func (s *stale[V]) expired(n int64) bool {
	expires := s.expires
	return expires != 0 && expires <= n
}

// maybeNewStale returns the stale to carry in a loading that replaces the
// finalized l: l's own stale if l errored, otherwise l's value with the
// stale window l's value has (see staleOpen).
func maybeNewStale[V any](l *loading[V], age time.Duration) (stale[V], bool) {
	if age == 0 {
		return stale[V]{}, false
	}
	if l.err() != nil {
		if st := l.stale(); st != nil {
			return *st, true
		}
		return stale[V]{}, false
	}
	expires := l.expires.Load()
	if expires == expiredClaimed {
		return stale[V]{}, false
	}
	return newStale(l.v, expires, age), true
}

// staleOpen returns whether an expired, unerrored value can still be
// returned as stale at n. expires must be non-zero. This must agree with
// the window newStale creates.
func staleOpen(expires, n int64, age time.Duration) bool {
	if age == 0 || expires == expiredClaimed {
		return false
	}
	if age < 0 {
		return true
	}
	if expires < 0 {
		expires = -expires // born expired
	}
	return n < satAdd(expires, int64(age))
}

// newStale returns a stale for v, whose loading's expires is expires. age
// must be non-zero, and expires must be an expired value's: non-zero and
// not expiredClaimed.
func newStale[V any](v V, expires int64, age time.Duration) stale[V] {
	if age < 0 {
		return stale[V]{v: v}
	}
	if expires < 0 {
		expires = -expires // born expired
	}
	return stale[V]{v, satAdd(expires, int64(age))}
}

// expiresPairingHook, if non-nil, runs in expiresNow between the expiry load
// and the clock sample, so that tests can pause a reader there.
var expiresPairingHook func()

// casPublishHook, if non-nil, runs in tryCAS before the final liveness
// check, so that tests can pause a CAS there.
var casPublishHook func()

// expiresNow returns l's expiry and a clock sample such that the expiry was
// current when the clock was sampled. expires is the caller's load of the
// expiry.
//
// We load the expiry before sampling the clock: Expire stores its own
// now()-1, which may be later than a clock sampled before our load. We then
// load the expiry again and retry if it changed: otherwise, an idle
// extension landing between the load and the sample would pair a dead
// expiry with a clock at which the value is live, and an Expire landing
// there would let CompareAndSwap match a value that was already expired.
func (l *loading[V]) expiresNow(expires int64) (int64, int64) {
	if expiresPairingHook != nil {
		expiresPairingHook()
	}
	for {
		n := now()
		again := l.expires.Load()
		if again == expires {
			return expires, n
		}
		expires = again
	}
}

// classifyFinalized returns what a read of l at n returns. A valid stale
// takes precedence over an error; an unexpired error is returned as a Hit;
// an expired value is returned as Stale while its stale window is open.
func classifyFinalized[V any](l *loading[V], expires, n int64, staleAge time.Duration) (v V, err error, state KeyState) {
	expired := expires != 0 && expires <= n
	if lerr := l.err(); lerr != nil {
		if st := l.stale(); st != nil && !st.expired(n) {
			return st.v, nil, Stale
		}
		if !expired {
			return l.v, lerr, Hit
		}
		return v, err, state
	}
	if expired {
		if staleOpen(expires, n, staleAge) {
			return l.v, nil, Stale
		}
		return v, err, state
	}
	return l.v, nil, Hit
}

// maxIdleSlack bounds how far we let an idle extension drift from now+age
// before we write it; see MaxIdleAge.
const maxIdleSlack = time.Second

// extend resets l's expiry to n+age if the expiry is still what we read.
// We skip the write if it would move the expiry by less than the slack, so
// that a hot key does not write its expiry on every hit. We sample the
// clock again right before the CAS so that we do not revive a value that
// expired while we were descheduled.
func (l *loading[V]) extend(expires, n int64, age time.Duration) {
	target := satAdd(n, int64(age))
	slack := min(int64(age)>>6, int64(maxIdleSlack))
	if d := target - expires; d < slack && d > -slack {
		return
	}
	if now() < expires {
		l.expires.CompareAndSwap(expires, target)
	}
}

// entGet is TryGet, but if the entry is loading with no valid stale, we
// wait for the load. If we waited and the load's result is already expired
// (MaxAge(0), MaxErrorAge(0), or a TTL shorter than the load), we return
// it anyway as a Hit: every Get that collapsed onto a load gets its result.
// The one exception is a panicked load, which has no result to share.
func entGet[K comparable, V any](e *ent[K, V], extend, staleAge time.Duration) (v V, err error, state KeyState) {
	l := e.p.Load()
	if l == nil {
		return v, err, state
	}
	var waited bool
	if !l.finalized() {
		if st := l.stale(); st != nil && !st.expired(now()) {
			return st.v, nil, Stale
		}
		l.wg.Wait()
		waited = true
	}
	expires := l.expires.Load()
	if expires == 0 && l.x.Load() == nil {
		return l.v, nil, Hit
	}
	expires, n := l.expiresNow(expires)
	if waited && expires != 0 && expires <= n && l.err() != errMissPanicked {
		return l.v, l.err(), Hit
	}
	v, err, state = classifyFinalized(l, expires, n, staleAge)
	if extend > 0 && state == Hit && err == nil {
		l.extend(expires, n, extend)
	}
	return v, err, state
}

func entTryGet[K comparable, V any](e *ent[K, V], n int64, extend, staleAge time.Duration) (v V, err error, state KeyState) {
	l := e.p.Load()
	if l == nil {
		return v, err, state
	}
	return loadingTryGet(l, n, extend, staleAge)
}

// loadingTryGet is TryGet for a loading. If n is non-zero, it is the clock
// to use, and extend must be zero.
func loadingTryGet[V any](l *loading[V], n int64, extend, staleAge time.Duration) (v V, err error, state KeyState) {
	if !l.finalized() {
		if n == 0 {
			n = now()
		}
		if st := l.stale(); st != nil && !st.expired(n) {
			return st.v, nil, Stale
		}
		return v, err, state
	}
	// If the caller passed n, it sampled n before this load, which is the
	// order expiresNow uses.
	expires := l.expires.Load()
	if expires == 0 && l.x.Load() == nil {
		return l.v, nil, Hit
	}
	if n == 0 {
		expires, n = l.expiresNow(expires)
	}
	v, err, state = classifyFinalized(l, expires, n, staleAge)
	if extend > 0 && state == Hit && err == nil {
		l.extend(expires, n, extend)
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

// Get returns the currently cached value, running the miss function if the
// item is not yet cached. If stale values are enabled, the currently cached
// value has an error, and there is an unexpired stale value, this returns the
// stale value and no error.
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
// loading, is not expired or errored, and is equal to old. The type V must be
// comparable.
func (i *Item[V]) CompareAndSwap(old, new V) (swapped bool) {
	return i.c.CompareAndSwap(struct{}{}, old, new)
}

// CompareAndDelete deletes the item if the value has finished loading, is not
// expired or errored, and is equal to old. The type V must be comparable.
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
	s := new(Set[K])
	initCache(&s.c, opts...)
	return s
}

// Get ensures the key is cached, running the miss function if the key is not
// yet cached. If stale keys are enabled, the currently cached key has an
// error, and there is a stale key, this returns with no error and a Stale key
// state.
func (s *Set[K]) Get(k K, miss func() error) (err error, state KeyState) {
	c := &s.c
	e := c.t.loadEntry(k)
	if e != nil {
		if _, err, state = entGet(e, c.cfg.maxIdleAge, c.cfg.maxStaleAge); state == Hit {
			return err, state
		}
	}
	load := func() (struct{}, error) { return struct{}{}, miss() }
	_, err, state = c.getSlow(k, e, load, func() func() (struct{}, error) {
		return func() (struct{}, error) { return struct{}{}, miss() }
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
