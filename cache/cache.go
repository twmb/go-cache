// Package cache provides a concurrency safe, mostly lock-free, singleflight
// request collapsing generic cache with support for stale values.
//
// The guts of this package are similar to `sync.Map`, with minor differences
// in when the internal locked write-map is promoted an atomic read-only map,
// and major differences to support singleflight request collapsing.
//
// Functions in this API that return a KeyState return the state last, rather
// than the error last: the value and error are cached as a single internal
// unit and can be thought of as a single value.
package cache

import (
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
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
		expires int64 // nano at which this ent is unusable, if non-zero
		stale   *stale[V]
		state   uint32 // set once; if not set, waitgroup is used

		wg sync.WaitGroup
		mu sync.Mutex
	}
	ent[V any] struct {
		p unsafe.Pointer // [nil | promotingDelete | *loading]
	}
	read[K comparable, V any] struct {
		m          map[K]*ent[V]
		incomplete bool
	}

	// Cache caches comparable keys to arbitrary values. By default the
	// cache grows without bounds and all keys persist forever. These
	// limits can be changed with options that are passed to New.
	Cache[K comparable, V any] struct {
		cfg cfg

		r     unsafe.Pointer // *read
		mu    sync.Mutex
		dirty map[K]*ent[V]

		misses int
	}

	cfg struct {
		maxAge      time.Duration
		maxStaleAge time.Duration
		maxErrAge   time.Duration
		ageSet      bool
	}

	opt struct{ fn func(*cfg) }

	// Opt configures a cache.
	Opt interface {
		apply(*cfg)
	}
)

func now() int64 { return time.Now().UnixNano() }

func (cfg *cfg) newExpires(err error) int64 {
	ttl := cfg.maxAge
	if err != nil {
		ttl = cfg.maxErrAge
	}
	if ttl > 0 {
		return time.Now().Add(ttl).UnixNano()
	}
	return 0
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
// default is MaxAge.
func MaxErrorAge(age time.Duration) Opt { return opt{fn: func(c *cfg) { c.maxErrAge = age }} }

// New returns a new cache, with the optional overrides configuring cache
// semantics. If you do not need to configure a cache at all, the zero value
// cache is valid and usable.
func New[K comparable, V any](opts ...Opt) *Cache[K, V] {
	var c cfg
	for _, opt := range opts {
		opt.apply(&c)
	}
	if c.maxErrAge == 0 {
		c.maxErrAge = c.maxAge
	}
	return &Cache[K, V]{
		cfg: c,
	}
}

// Get returns the cache value for k, running the miss function in a goroutine
// if the key is not yet cached. If stale values are enabled, the currently
// cached value has an error, and there is an unexpired stale value, this
// returns the stale value and no error.
func (c *Cache[K, V]) Get(k K, miss func() (V, error)) (v V, err error, s KeyState) {
	r := c.read()
	e := r.m[k]
	if v, err, s = e.get(); s == Hit {
		return
	}

	// We missed in the read map. We lock and check again to guard against
	// something concurrent. Odds are we are falling into the logic below.
	c.mu.Lock()
	r = c.read()
	e = r.m[k]
	if v, err, s = e.get(); s == Hit {
		c.mu.Unlock()
		return
	}

	// We could have an entry in our read map that was deleted and has not
	// yet gone through the promote&clear process. We only check the dirty
	// map if the entry is nil.
	if e == nil && r.incomplete {
		e = c.dirty[k]
		c.missed(r)
		r = c.read()
		if v, err, s = e.get(); s == Hit {
			c.mu.Unlock()
			return
		}
	}

	l := &loading[V]{
		stale: e.maybeNewStale(c.cfg.maxStaleAge),
	}
	l.wg.Add(1)

	// If we have no entry, this is completely new. If we had an entry, it
	// was in the read map and not yet deleted through promotion. We know
	// the pointer is not promotingDelete, since that is only set while
	// promoting which requires the cache lock which we have right now.
	//
	// In the worst case we race with a concurrent Set and we override its
	// results.
	if e != nil {
		atomic.StorePointer(&e.p, unsafe.Pointer(l))
	} else {
		e = &ent[V]{p: unsafe.Pointer(l)}
		c.storeDirty(r, k, e)
	}
	c.mu.Unlock()

	// We have created a new entry: if we are configured to not cache, we
	// clear this entry upon return.
	if c.cfg.ageSet && c.cfg.maxAge <= 0 {
		defer c.Delete(k)
	}
	go func() { v, err := miss(); l.setve(v, err, c.cfg.newExpires(err)) }()

	// We could have set our own stale value which can be returned
	// immediately rather than waiting for the get. If there is a valid
	// stale to be returned now, `get` returns it. If `get` returns a Hit,
	// we ended up waiting for ourself and we return a miss. There should
	// be no Miss returns from `get`.
	v, err, s = e.get()
	switch s {
	case Miss, Hit:
		return l.v, l.err, Miss
	}
	return v, err, Stale
}

func (c *Cache[K, V]) tryLoadEnt(k K, dirty func()) *ent[V] {
	r := c.read()
	e := r.m[k]
	if e == nil && r.incomplete {
		c.mu.Lock()
		r = c.read()
		e = r.m[k]
		if e == nil && r.incomplete {
			e = c.dirty[k]
			if dirty != nil {
				dirty()
			}
			c.missed(r)
		}
		c.mu.Unlock()
	}
	return e
}

// TryGet returns the value for the given key if it is cached. This returns
// either the currently loaded value, or if the current load has an error, the
// stale value if present, or the currently stored error. If nothing is cached,
// or what is cached is expired, this returns Miss.
func (c *Cache[K, V]) TryGet(k K) (V, error, KeyState) {
	e := c.tryLoadEnt(k, nil)
	return e.tryGet(0)
}

// Delete deletes the value for a key and returns the prior value, if loaded
// (i.e. the return from TryGet).
func (c *Cache[K, V]) Delete(k K) (V, error, KeyState) {
	e := c.tryLoadEnt(k, func() { delete(c.dirty, k) })
	defer e.del()
	return e.tryGet(0)
}

// Expire sets a loaded value to expire immediately, meaning the next Get will
// be a miss. If stale values are enabled, the next Get will trigger the miss
// function but still allow the now-stale value to be returned.
func (c *Cache[K, V]) Expire(k K) {
	e := c.tryLoadEnt(k, nil)
	if l := e.load(); l != nil && l.finalized() {
		atomic.SwapInt64(&l.expires, now())
	}
}

// Each calls fn for every cached value. If fn returns false, iteration stops.
func (c *Cache[K, V]) Range(fn func(K, V, error) bool) {
	// When ranging, repeated time.Now() calls add up, so we get the
	// current time when we enter range and avoid it in all tryGet calls.
	now := now()
	c.each(func(k K, e *ent[V]) bool {
		v, err, s := e.tryGet(now)
		if s.IsMiss() {
			return true
		}
		return fn(k, v, err)
	})
}

// Similar to sync.Map, we promote on range because range is O(N) (usually) and
// this amortizes out.
func (c *Cache[K, V]) each(fn func(K, *ent[V]) bool) {
	r := c.read()
	if r.incomplete {
		c.mu.Lock()
		r = c.read()
		if r.incomplete {
			c.promote()
			r = c.read()
		}
		c.mu.Unlock()
	}
	for k, e := range r.m {
		if !fn(k, e) {
			return
		}
	}
}

// Clean deletes all expired values from the cache. A value is expired if
// MaxAge is used and the entry is older than the max age, or if you manually
// expired a key. If MaxStaleAge is used and not -1, the entry must be older
// than MaxAge + MaxStaleAge. If MaxStaleAge is -1, Clean returns immediately.
func (c *Cache[K, V]) Clean() {
	// If MaxStaleAge is -1, the user opted into persisting stales forever
	// and we do not clean.
	if c.cfg.maxStaleAge < 0 {
		return
	}
	now := now()
	c.each(func(k K, e *ent[V]) bool {
		if l := e.load(); l != nil && l.finalized() {
			expires := atomic.LoadInt64(&l.expires)
			if expires != 0 && now > expires+int64(c.cfg.maxStaleAge) {
				c.Delete(k)
			}
		}
		return true
	})
}

// Set sets a value for a key. If the key is currently loading via Get, the
// load is canceled and Get returns the value from Set.
func (c *Cache[K, V]) Set(k K, v V) {
	l := &loading[V]{
		state:   1,
		v:       v,
		expires: c.cfg.newExpires(nil),
	}
	if c.cfg.maxStaleAge != 0 {
		l.stale = newStale(v, now(), c.cfg.maxStaleAge)
	}
	p := unsafe.Pointer(l)

	var was unsafe.Pointer

	r := c.read()
	e, ok := r.m[k]

	defer func() {
		if was == nil {
			return
		}
		((*loading[V])(was)).setve(v, nil, c.cfg.newExpires(nil))
	}()

	if ok {
		for {
			rm := atomic.LoadPointer(&e.p)
			if rm == promotingDelete { // deleted & currently being ignored in a promote
				break
			}
			was = rm
			if atomic.CompareAndSwapPointer(&e.p, rm, p) {
				return
			}
		}
	}

	c.mu.Lock()
	r = c.read()
	if e = r.m[k]; e != nil {
		was = atomic.SwapPointer(&e.p, p) // was not in read, but promoted by the time we entered the lock and is now in read
	} else if e = c.dirty[k]; e != nil {
		was = atomic.SwapPointer(&e.p, p)
	} else {
		c.storeDirty(r, k, &ent[V]{p: p})
	}
	c.mu.Unlock()
}

func (l *loading[V]) setve(v V, err error, expires int64) {
	if atomic.LoadUint32(&l.state) == 1 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state != 0 {
		return
	}
	l.v, l.err, l.expires = v, err, expires
	atomic.StoreUint32(&l.state, 1)
	l.wg.Done()
}

//////////////////////
// CACHE READ/DIRTY //
//////////////////////

var promotingDelete = unsafe.Pointer(new(any))

func (c *Cache[K, V]) read() read[K, V] {
	p := atomic.LoadPointer(&c.r)
	if p == nil {
		return read[K, V]{}
	}
	return *(*read[K, V])(p)
}
func (c *Cache[K, V]) storeRead(r read[K, V]) { atomic.StorePointer(&c.r, unsafe.Pointer(&r)) }

func (c *Cache[K, V]) storeDirty(r read[K, V], k K, e *ent[V]) {
	if !r.incomplete {
		if c.dirty == nil {
			c.dirty = make(map[K]*ent[V])
		}
		c.storeRead(read[K, V]{m: r.m, incomplete: true})
	}
	c.dirty[k] = e
}

func (c *Cache[K, V]) missed(r read[K, V]) {
	c.misses++
	if len(c.dirty) == 0 || c.misses > len(r.m)>>1 {
		c.promote()
	}
}

func (c *Cache[K, V]) promote() {
	r := c.read()
	keep := r.m

	defer func() {
		c.storeRead(read[K, V]{m: keep})
		c.misses = 0
	}()

	if len(c.dirty) == 0 {
		return
	}

	keep = make(map[K]*ent[V], len(keep)+len(c.dirty))
	for k, e := range c.dirty {
		keep[k] = e
	}
	c.dirty = nil

outer:
	for k, e := range r.m {
		p := atomic.LoadPointer(&e.p)
		for p == nil {
			if atomic.CompareAndSwapPointer(&e.p, nil, promotingDelete) {
				continue outer
			}
			p = atomic.LoadPointer(&e.p) // concurrently deleted while promoting
		}
		if p != promotingDelete {
			keep[k] = e
		}
	}
}

/////////
// ENT //
/////////

func (l *loading[V]) finalized() bool { return atomic.LoadUint32(&l.state) == 1 }

func (e *ent[V]) del() {
	if e == nil {
		return
	}
	for {
		p := atomic.LoadPointer(&e.p)
		if p == nil || p == promotingDelete {
			return
		}
		if atomic.CompareAndSwapPointer(&e.p, p, nil) {
			return
		}
	}
}

func (e *ent[V]) load() *loading[V] {
	if e == nil {
		return nil
	}
	p := atomic.LoadPointer(&e.p)
	if p == nil || p == promotingDelete {
		return nil
	}
	return (*loading[V])(p)
}

func (l *loading[V]) expired(now int64) bool {
	expires := atomic.LoadInt64(&l.expires)
	return expires != 0 && expires <= now // 0 means either miss not resolved, or no max age
}

func (s *stale[V]) expired(now int64) bool {
	expires := atomic.LoadInt64(&s.expires)
	return expires != 0 && expires <= now
}

// If an entry is expiring, we create a new entry with this previous entry as
// a stale.
//
//   * if entry is nil, no stale, return nil
//   * if no stale age, we are not using stales, return nil
//   * if entry has an error, return prior stale
//   * if age is < 0, return new unexpiring stale
//   * else, return new stale with prior expiry + stale age
func (e *ent[V]) maybeNewStale(age time.Duration) *stale[V] {
	if e == nil || age == 0 {
		return nil
	}
	l := e.load()
	if l == nil || !l.finalized() {
		return nil
	}
	if l.err != nil {
		return l.stale
	}
	return newStale(l.v, l.expires, age)
}

// Actually returns the stale; age must be non-zero.
func newStale[V any](v V, expires int64, age time.Duration) *stale[V] {
	if age < 0 {
		return &stale[V]{v: v}
	}
	return &stale[V]{v, expires + int64(age)}
}

// get always returns the value or the stale value. We do not check if our
// value is expired: we call this at the end of Get, we must always return
// something even if it is to be immediately expired.
func (e *ent[V]) get() (v V, err error, s KeyState) {
	l := e.load()
	var waited bool
	if l == nil {
		// Could be nil if deleted while in the read map, or
		// promotingDelete.
		return
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
	now := now()
	if !waited && l.expired(now) || l.err != nil {
		if l.stale != nil && !l.stale.expired(now) {
			return l.stale.v, nil, Stale
		}
		// The stale value is expired: if our entry is not expired,
		// this must be an error we waited on.
		if !l.expired(now) {
			return l.v, l.err, Hit
		}
		return
	}
	return l.v, l.err, Hit
}

func (e *ent[V]) tryGet(n64 int64) (v V, err error, s KeyState) {
	if e == nil {
		return
	}
	l := e.load()
	if l == nil { // deleting or promotedDelete
		return
	}

	if n64 == 0 {
		n64 = now()
	}
	now := n64

	// If we are loading but there is a valid stale, return it, otherwise
	// return immediately: no get.
	if !l.finalized() {
		if l.stale != nil && !l.stale.expired(now) {
			return l.stale.v, nil, Stale
		}
		return
	}

	// If we have an error or we are expired, we maybe return the stale.
	if l.err != nil || l.expired(now) {
		if l.stale != nil && !l.stale.expired(now) {
			return l.stale.v, nil, Stale
		}
	}
	if l.expired(now) {
		return
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
	if c.maxErrAge == 0 {
		c.maxErrAge = c.maxAge
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
func (i *Item[V]) Get(miss func() (V, error)) (v V, err error, s KeyState) {
	return i.c.Get(struct{}{}, miss)
}

// TryGet returns the value the item if it is cached. This returns either the
// currently loaded value, or if the current load has an error, the stale value
// if present, or the currently stored error. If nothing is cached, or what is
// cached is expired, this returns Miss.
func (i *Item[V]) TryGet() (v V, err error, s KeyState) {
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
