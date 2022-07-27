// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cache

import (
	"sync"
)

// This file contains reference map implementations for unit-tests.

// mapInterface is the interface Map implements.
type mapInterface[K comparable] interface {
	Load(K) (any, bool)
	Store(key K, value any)
	LoadOrStore(key K, value any) (actual any, loaded bool)
	LoadAndDelete(key K) (value any, loaded bool)
	Delete(K)
	Range(func(key K, value any) (shouldContinue bool))
}

// RWMutexMap is an implementation of mapInterface using a sync.RWMutex.
type RWMutexMap[K comparable] struct {
	mu    sync.RWMutex
	dirty map[K]any
}

func (m *RWMutexMap[K]) Load(key K) (value any, ok bool) {
	m.mu.RLock()
	value, ok = m.dirty[key]
	m.mu.RUnlock()
	return
}

func (m *RWMutexMap[K]) Store(key K, value any) {
	m.mu.Lock()
	if m.dirty == nil {
		m.dirty = make(map[K]any)
	}
	m.dirty[key] = value
	m.mu.Unlock()
}

func (m *RWMutexMap[K]) LoadOrStore(key K, value any) (actual any, loaded bool) {
	m.mu.Lock()
	actual, loaded = m.dirty[key]
	if !loaded {
		actual = value
		if m.dirty == nil {
			m.dirty = make(map[K]any)
		}
		m.dirty[key] = value
	}
	m.mu.Unlock()
	return actual, loaded
}

func (m *RWMutexMap[K]) LoadAndDelete(key K) (value any, loaded bool) {
	m.mu.Lock()
	value, loaded = m.dirty[key]
	if !loaded {
		m.mu.Unlock()
		return nil, false
	}
	delete(m.dirty, key)
	m.mu.Unlock()
	return value, loaded
}

func (m *RWMutexMap[K]) Delete(key K) {
	m.mu.Lock()
	delete(m.dirty, key)
	m.mu.Unlock()
}

func (m *RWMutexMap[K]) Range(f func(key K, value any) (shouldContinue bool)) {
	m.mu.RLock()
	keys := make([]K, 0, len(m.dirty))
	for k := range m.dirty {
		keys = append(keys, k)
	}
	m.mu.RUnlock()

	for _, k := range keys {
		v, ok := m.Load(k)
		if !ok {
			continue
		}
		if !f(k, v) {
			break
		}
	}
}

// CacheMap is an implementation of mapInterface using a cache.Cache.
type CacheMap[K comparable] struct {
	c Cache[K, any]
}

func (c *CacheMap[K]) Load(key K) (value any, ok bool) {
	v, _, s := c.c.TryGet(key)
	return v, s.IsHit()
}

func (c *CacheMap[K]) Store(key K, value any) {
	c.c.Set(key, value)
}

func (c *CacheMap[K]) LoadOrStore(key K, value any) (actual any, loaded bool) {
	actual, _, s := c.c.Get(key, func() (any, error) {
		return value, nil
	})
	return actual, s.IsHit()
}

func (c *CacheMap[K]) LoadAndDelete(key K) (value any, loaded bool) {
	value, _, s := c.c.Delete(key)
	return value, s.IsHit()
}

func (c *CacheMap[K]) Delete(key K) {
	c.c.Delete(key)
}

func (c *CacheMap[K]) Range(f func(key K, value any) (shouldContinue bool)) {
	c.c.Range(func(k K, v any, _ error) bool {
		return f(k, v)
	})
}
