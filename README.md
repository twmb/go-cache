cache
=====

Package cache provides a concurrency safe, mostly lock-free, singleflight
request collapsing generic cache with support for stale values.

The storage is a concurrent hash-trie similar to Go's internal
`HashTrieMap`, which backs `sync.Map`: loads are lock-free, and inserts and
deletes lock only the trie node being changed.

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

Out of an abundance of paranoia that this code is correct, the tests include
all stdlib `sync.Map` tests run against `cache.Cache`, a property test
against a mutex guarded map, a linearizability checker for concurrent
operations on a single key, structural checks of the trie after concurrent
stress, and regression tests for specific races. CI runs the tests with the
race detector on amd64 and arm64, and without it on 386.

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

These are the stdlib `sync.Map` benchmarks, run against `sync.Map` and against
a `cache.Cache[int, any]` adapted to the same interface. Both are hash-tries
underneath. The numbers are medians of 8 runs on an Apple M2 with Go 1.27;
the last column is allocs per op, `sync.Map` / cache.

```
name                            sync.Map     cache    allocs
LoadMostlyHits                    3.10ns    3.28ns     0 / 0
LoadMostlyMisses                  1.66ns    1.81ns     0 / 0
LoadOrStoreBalanced               47.2ns    80.0ns     2 / 3
LoadOrStoreUnique                 95.1ns     121ns     3 / 4
LoadOrStoreCollision              2.53ns    10.3ns     0 / 1
LoadAndDeleteBalanced             2.05ns    1.86ns     0 / 0
LoadAndDeleteUnique               2.13ns    0.65ns     0 / 0
LoadAndDeleteCollision            3.43ns    2.15ns     0 / 0
Range                             1.45µs    2.25µs     0 / 0
AdversarialAlloc                  5.61ns    4.59ns     0 / 0
AdversarialDelete                 2.98ns    2.91ns     0 / 0
DeleteCollision                   1.90ns    1.66ns     0 / 0
SwapCollision                      122ns     258ns     1 / 1
SwapMostlyHits                    35.9ns    30.2ns     2 / 1
SwapMostlyMisses                   208ns     245ns     3 / 3
CompareAndSwapCollision           6.02ns    9.11ns     0 / 0
CompareAndSwapNoExistingKey       4.81ns    0.80ns     0 / 0
CompareAndSwapValueNotEqual       3.69ns    3.07ns     0 / 0
CompareAndSwapMostlyHits          38.3ns    27.7ns     3 / 2
CompareAndSwapMostlyMisses        11.6ns    7.93ns     2 / 1
CompareAndDeleteCollision         3.08ns    4.36ns     0 / 0
CompareAndDeleteMostlyHits        43.5ns    62.9ns     3 / 3
CompareAndDeleteMostlyMisses      5.25ns    5.31ns     0 / 1
Clear                             84.8ns     128ns     2 / 3
```

Loads are within 10% of `sync.Map`, and deletes are faster. Stores cost more:
each stored value is a `loading` that also carries singleflight and expiry
state, and the adapter's `LoadOrStore` allocates a closure for its miss
function on every call.
