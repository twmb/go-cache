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
  sync.Map.Delete         ==  cache.Cache.Delete
  sync.Map.Get            ==  cache.Cache.TryGet
  sync.Map.LoadAndDelete  ==  (no equivalent)
  sync.Map.LoadOrStore    ==  cache.Cache.Get
  sync.Map.Range          ==  cache.Cache.Range
  sync.Map.Store          ==  cache.Cache.Set
```

The cache has the concept of expiring values with max ages. As well, a stale
value can be kept and returned from `Get` during a refresh / if a refresh
fails. Keys can be manually expired with `Expire`. Internally expired values or
errors can be occasionally cleaned with `Clean`.

Out of an abundance of paranoia that this code is correct, there are unit tests
to hit 97% coverage of the `cache.Cache` type. As well, all tests against
`sync.Map` are copied into this library and used against `cache.Cache`.

Documentation
-------------

[![godev](https://img.shields.io/static/v1?label=godev&message=reference&color=00add8)][godev]
[godev]: https://pkg.go.dev/github.com/twmb/cache
