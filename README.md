cache
=====

Package cache provides a concurrency safe, mostly lock-free, singleflight
request collapsing generic cache with support for stale values.

The guts of this package are similar to `sync.Map`, with minor differences in
when the internal locked write-map is promoted an atomic read-only map, and
major differences to support singleflight request collapsing.

Functions in this API that return a `KeyState` return the state last, rather
than the error last: the value and error are cached as a single internal unit
and can be thought of as a single value.

The following logical mapping can be done from `sync.Map` to `cache.Cache`
functions:

```go
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

Out of an abundance of paranoia that this code is correct, there are unit tests
to hit 97% coverage of the `cache.Cache` type. As well, all tests against
`sync.Map` are copied into this library and used against `cache.Cache`.

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

Some of the APIs are actually faster or more memory efficient than `sync.Map`
because of generics. Some are slower and require more allocs because of the
request collapsing aspect. Some benchmarks below show more CPU, but this is due
to a few slight changes in how the write (dirty) map is internally promoted to
the read map.

The benchmarks are from the stdlib using a key and value of type int:

```
name                          old time/op    new time/op    delta
LoadMostlyHits                  12.8ns ±44%    14.0ns ±50%      ~     (p=0.393 n=10+10)
LoadMostlyMisses                10.7ns ±53%     4.6ns ±47%   -57.31%  (p=0.000 n=10+10)
LoadOrStoreBalanced              351ns ± 3%     705ns ±27%  +100.68%  (p=0.000 n=8+10)
LoadOrStoreUnique                663ns ±13%    1296ns ±31%   +95.53%  (p=0.000 n=10+10)
LoadOrStoreCollision            9.71ns ±60%   29.26ns ±46%  +201.44%  (p=0.000 n=10+10)
LoadAndDeleteBalanced           11.2ns ±42%     7.1ns ±48%   -36.06%  (p=0.035 n=10+10)
LoadAndDeleteUnique             9.19ns ±62%    4.17ns ±47%   -54.59%  (p=0.001 n=10+10)
LoadAndDeleteCollision          10.9ns ±41%    14.3ns ±35%   +31.18%  (p=0.023 n=10+10)
Range                           3.73µs ±48%    5.40µs ±45%      ~     (p=0.063 n=10+10)
AdversarialAlloc                 298ns ±22%     259ns ±21%      ~     (p=0.063 n=10+10)
AdversarialDelete               83.7ns ±14%   105.6ns ±20%      ~     (p=0.052 n=10+10)
DeleteCollision                 6.87ns ±71%   4.86ns ±100%      ~     (p=0.424 n=10+10)
SwapCollision                    201ns ±26%     266ns ±31%      ~     (p=0.143 n=10+10)
SwapMostlyHits                  40.3ns ±42%    59.7ns ±51%      ~     (p=0.143 n=10+10)
SwapMostlyMisses                 662ns ±18%     607ns ±24%    -8.19%  (p=0.023 n=10+10)
CompareAndSwapCollision         33.4ns ±41%    24.9ns ±65%   -25.44%  (p=0.023 n=10+10)
CompareAndSwapNoExistingKey     10.3ns ±46%     2.0ns ±53%   -80.15%  (p=0.000 n=10+10)
CompareAndSwapValueNotEqual     9.64ns ±51%    6.02ns ±95%   -37.49%  (p=0.019 n=10+10)
CompareAndSwapMostlyHits        46.9ns ±39%    56.8ns ±47%      ~     (p=0.143 n=10+10)
CompareAndSwapMostlyMisses      22.9ns ±39%    13.4ns ±48%   -41.48%  (p=0.023 n=10+10)
CompareAndDeleteCollision       28.1ns ±47%    18.3ns ±37%   -34.88%  (p=0.019 n=10+10)
CompareAndDeleteMostlyHits      64.0ns ±38%    63.9ns ±52%      ~     (p=0.165 n=10+10)
CompareAndDeleteMostlyMisses    18.3ns ±37%     8.7ns ±53%   -52.65%  (p=0.023 n=10+10)

name                          old alloc/op   new alloc/op   delta
LoadMostlyHits                   6.00B ± 0%     0.00B       -100.00%  (p=0.000 n=10+10)
LoadMostlyMisses                 6.00B ± 0%     0.00B       -100.00%  (p=0.000 n=10+10)
LoadOrStoreBalanced              74.7B ±16%    195.3B ± 1%  +161.45%  (p=0.000 n=10+10)
LoadOrStoreUnique                 159B ± 7%      330B ± 7%  +107.15%  (p=0.000 n=9+10)
LoadOrStoreCollision             0.00B         32.00B ± 0%     +Inf%  (p=0.000 n=10+10)
LoadAndDeleteBalanced            4.00B ± 0%     0.00B       -100.00%  (p=0.000 n=10+10)
LoadAndDeleteUnique              8.00B ± 0%     0.00B       -100.00%  (p=0.000 n=10+10)
LoadAndDeleteCollision           1.00B ± 0%     1.00B ± 0%      ~     (all equal)
Range                            16.0B ± 0%      0.0B       -100.00%  (p=0.000 n=10+10)
AdversarialAlloc                 53.4B ± 3%     64.8B ± 3%   +21.35%  (p=0.000 n=10+10)
AdversarialDelete                22.6B ±12%     39.0B ± 0%   +72.57%  (p=0.000 n=10+10)
DeleteCollision                  0.00B          0.00B           ~     (all equal)
SwapCollision                    16.0B ± 0%     80.0B ± 0%  +400.00%  (p=0.000 n=10+10)
SwapMostlyHits                   28.0B ± 0%     86.0B ± 0%  +207.14%  (p=0.000 n=10+10)
SwapMostlyMisses                  124B ± 0%      136B ± 1%    +9.71%  (p=0.000 n=10+10)
CompareAndSwapCollision          7.60B ± 8%    21.70B ±17%  +185.53%  (p=0.000 n=10+10)
CompareAndSwapNoExistingKey      8.00B ± 0%     0.00B       -100.00%  (p=0.000 n=10+10)
CompareAndSwapValueNotEqual      0.00B          0.00B           ~     (all equal)
CompareAndSwapMostlyHits         33.0B ± 0%     91.0B ± 0%  +175.76%  (p=0.000 n=10+10)
CompareAndSwapMostlyMisses       23.0B ± 0%     16.0B ± 0%   -30.43%  (p=0.000 n=10+10)
CompareAndDeleteCollision        0.00B          2.00B ± 0%     +Inf%  (p=0.000 n=10+8)
CompareAndDeleteMostlyHits       39.0B ± 0%     90.6B ± 1%  +132.31%  (p=0.000 n=10+10)
CompareAndDeleteMostlyMisses     16.0B ± 0%      8.0B ± 0%   -50.00%  (p=0.002 n=8+10)

name                          old allocs/op  new allocs/op  delta
LoadMostlyHits                    0.00           0.00           ~     (all equal)
LoadMostlyMisses                  0.00           0.00           ~     (all equal)
LoadOrStoreBalanced               2.00 ± 0%      3.00 ± 0%   +50.00%  (p=0.000 n=10+10)
LoadOrStoreUnique                 4.00 ± 0%      5.00 ± 0%   +25.00%  (p=0.000 n=10+10)
LoadOrStoreCollision              0.00           1.00 ± 0%     +Inf%  (p=0.000 n=10+10)
LoadAndDeleteBalanced             0.00           0.00           ~     (all equal)
LoadAndDeleteUnique              0.60 ±100%      0.00       -100.00%  (p=0.011 n=10+10)
LoadAndDeleteCollision            0.00           0.00           ~     (all equal)
Range                             1.00 ± 0%      0.00       -100.00%  (p=0.000 n=10+10)
AdversarialAlloc                  1.00 ± 0%      0.00       -100.00%  (p=0.000 n=10+10)
AdversarialDelete                 1.00 ± 0%      0.00       -100.00%  (p=0.000 n=10+10)
DeleteCollision                   0.00           0.00           ~     (all equal)
SwapCollision                     1.00 ± 0%      1.00 ± 0%      ~     (all equal)
SwapMostlyHits                    2.00 ± 0%      1.00 ± 0%   -50.00%  (p=0.000 n=10+10)
SwapMostlyMisses                  6.00 ± 0%      4.00 ± 0%   -33.33%  (p=0.000 n=10+10)
CompareAndSwapCollision           0.00           0.00           ~     (all equal)
CompareAndSwapNoExistingKey      0.50 ±100%      0.00       -100.00%  (p=0.033 n=10+10)
CompareAndSwapValueNotEqual       0.00           0.00           ~     (all equal)
CompareAndSwapMostlyHits          3.00 ± 0%      2.00 ± 0%   -33.33%  (p=0.000 n=10+10)
CompareAndSwapMostlyMisses        2.00 ± 0%      1.00 ± 0%   -50.00%  (p=0.000 n=10+10)
CompareAndDeleteCollision         0.00           0.00           ~     (all equal)
CompareAndDeleteMostlyHits        3.00 ± 0%      2.00 ± 0%   -33.33%  (p=0.000 n=10+10)
CompareAndDeleteMostlyMisses      1.00 ± 0%      0.00       -100.00%  (p=0.000 n=10+10)
```
