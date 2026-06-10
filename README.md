cache
=====

Package cache provides a concurrency safe, mostly lock-free, singleflight
request collapsing generic cache with support for stale values.

The internal storage is a concurrent hash-trie, modeled on Go's
`internal/sync.HashTrieMap` (the backing of the current `sync.Map`) but
implemented entirely in userspace via `hash/maphash` for the runtime's typed
hasher. Loads are lock-free; inserts and deletes take only a per-bucket
mutex, never a global one. On top of that storage the cache layers
singleflight request collapsing, stale values, TTLs, idle-age extension, and
error caching.

Functions in this API that return a `KeyState` return the state last, rather
than the error last: the value and error are cached as a single internal unit
and can be thought of as a single value.

The following logical mapping can be done from `sync.Map` to `cache.Cache`
functions:

```go
  sync.Map.Clear            == cache.Cache.Clear
  sync.Map.CompareAndDelete == cache.Cache.CompareAndDelete
  sync.Map.CompareAndSwap   == cache.Cache.CompareAndSwap
  sync.Map.Delete           == (no equivalent -- we just provide LoadAndDelete)
  sync.Map.Load             == cache.Cache.TryGet
  sync.Map.LoadAndDelete    == cache.Cache.Delete
  sync.Map.LoadOrStore      == cache.Cache.Get
  sync.Map.Range            == cache.Cache.Range
  sync.Map.Store            == cache.Cache.Set
  sync.Map.Swap             == cache.Cache.Swap
```


The cache has the concept of expiring values with max ages. As well, a stale
value can be kept and returned from `Get` during a refresh / if a refresh
fails. Keys can be manually expired with `Expire`. Internally expired values or
errors can be occasionally cleaned with `Clean`.

Out of an abundance of paranoia that this code is correct, the test suite
layers several kinds of verification: unit tests at ~98% statement coverage
of the cache and trie (the remainder being defensive panics at
invariant-violation points and race-retry branches that require
interleavings that cannot be forced from userspace); all of the stdlib
`sync.Map` tests, copied into this library and run against `cache.Cache`; a
property test comparing the cache to a `sync.RWMutex`-guarded `map` oracle
over random operation sequences; a linearizability checker that runs batches
of concurrent operations against single keys and verifies that a
real-time-consistent sequential ordering explains every observed return;
structural validation of the trie (hash placement, overflow chains, parent
pointers, pruning) after each concurrent stress test; and regression hammers
for specific interleaving bugs found by audit. CI runs the suite under the
race detector on both amd64 and arm64 (whose weaker memory ordering
surfaces reordering bugs that x86 hides), and without it on 32-bit
linux/386.

This package also provides a cached `Item` and a `Set`. `Item` can be used to
populate an expensive value once and expire or replace it when needed, similar
to a singleton. `Set` can be used as a standard set for when key values are
expensive. Functions that do not make sense on either of these types are not
provided (e.g., `CompareAndSwap` for a `Set`).

Documentation
-------------

[![godev](https://img.shields.io/static/v1?label=godev&message=reference&color=00add8)][godev]

[godev]: https://pkg.go.dev/github.com/twmb/go-cache/cache

Benchmarking
------------

Microbenchmarks on an Apple M1, Go 1.26, against the `CacheMap[int]` wrapper
(`cache.Cache[int, any]` adapted to the `sync.Map` `mapInterface` so the
stdlib benchmarks apply directly):

```
name                            time/op       alloc/op  allocs/op
LoadMostlyHits                   10.3 ns/op     0 B/op   0 allocs/op
LoadMostlyMisses                  5.2 ns/op     0 B/op   0 allocs/op
LoadOrStoreBalanced               448 ns/op   151 B/op   3 allocs/op
LoadOrStoreUnique                 567 ns/op   262 B/op   5 allocs/op
LoadOrStoreCollision             45.6 ns/op    32 B/op   1 allocs/op
LoadAndDeleteBalanced            4.88 ns/op     0 B/op   0 allocs/op
LoadAndDeleteUnique              2.00 ns/op     0 B/op   0 allocs/op
LoadAndDeleteCollision           20.2 ns/op     2 B/op   0 allocs/op
Range                            8.64 µs/op     0 B/op   0 allocs/op
AdversarialAlloc                 11.7 ns/op     0 B/op   0 allocs/op
AdversarialDelete                7.17 ns/op     0 B/op   0 allocs/op
DeleteCollision                  3.69 ns/op     0 B/op   0 allocs/op
SwapCollision                     243 ns/op    80 B/op   1 allocs/op
SwapMostlyHits                    119 ns/op    86 B/op   1 allocs/op
SwapMostlyMisses                  160 ns/op   120 B/op   2 allocs/op
CompareAndSwapCollision          13.1 ns/op     3 B/op   0 allocs/op
CompareAndSwapNoExistingKey      2.57 ns/op     0 B/op   0 allocs/op
CompareAndSwapValueNotEqual      10.6 ns/op     0 B/op   0 allocs/op
CompareAndSwapMostlyHits          128 ns/op    91 B/op   2 allocs/op
CompareAndSwapMostlyMisses       30.1 ns/op    16 B/op   1 allocs/op
CompareAndDeleteCollision        13.0 ns/op     0 B/op   0 allocs/op
CompareAndDeleteMostlyHits        112 ns/op    91 B/op   2 allocs/op
CompareAndDeleteMostlyMisses     13.3 ns/op     8 B/op   0 allocs/op
```

The per-op allocations on the `LoadOrStore*`, `Swap*`, and `CompareAndSwap*Hits`
rows are intrinsic: each of these operations may install a fresh `loading[V]`
in the trie's value slot, and that struct carries the singleflight machinery
(`sync.Mutex`, `sync.WaitGroup`, atomic counters) that a plain `sync.Map` does
not need. The lock-free-read paths (`Load*`, `Delete*`, `Range`, the
`Adversarial*` patterns, and the `CompareAnd*Misses` paths) are alloc-free.
