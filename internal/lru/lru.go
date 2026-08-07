// Package lru holds the bounded least-recently-used map used by both the
// provider's column and metadata caches and the format package's shared zstd
// dictionary codecs.
//
// It lives under internal/ rather than in either package because format is
// imported by pile, so a type defined in pile cannot be reached from format,
// and a second implementation is exactly what one bounded map wants least.
package lru

import "container/list"

// Cache is a bounded least-recently-used map. A nil *Cache is a disabled
// cache: every store is dropped and every lookup misses, so callers need no
// nil check.
//
// Entries are bounded by count, and optionally by weight as well when a size
// function is supplied. Weight matters where one entry can be enormous on its
// own: a chunk's user data blob may be 16 MiB, a preserved-state sidecar holds
// up to one entry per block position, and a zstd dictionary carried by a file
// may be 1 MiB, so a count alone would bound the number of entries without
// bounding the memory.
//
// Cache is not safe for concurrent use; callers hold their own lock.
type Cache[K comparable, V any] struct {
	cap     int
	maxSize int         // 0: no weight budget
	size    func(V) int // nil: entries are not weighed
	weight  int
	ll      *list.List // front = most recent; values are *entry[K, V]
	items   map[K]*list.Element
}

type entry[K comparable, V any] struct {
	key    K
	val    V
	weight int
}

// New builds a cache bounded at capacity entries and, when size is non-nil, at
// maxSize total weight. A capacity of zero or less returns nil: a disabled
// cache.
func New[K comparable, V any](capacity, maxSize int, size func(V) int) *Cache[K, V] {
	if capacity <= 0 {
		return nil
	}
	return &Cache[K, V]{
		cap: capacity, maxSize: maxSize, size: size,
		ll: list.New(), items: map[K]*list.Element{},
	}
}

// Get returns the cached value (not a copy) and marks it recently used.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	var zero V
	if c == nil {
		return zero, false
	}
	el, ok := c.items[key]
	if !ok {
		return zero, false
	}
	c.ll.MoveToFront(el)
	return el.Value.(*entry[K, V]).val, true
}

// Put stores a value the cache takes ownership of.
func (c *Cache[K, V]) Put(key K, val V) {
	if c == nil {
		return
	}
	w := 0
	if c.size != nil {
		w = c.size(val)
	}
	if el, ok := c.items[key]; ok {
		c.ll.MoveToFront(el)
		e := el.Value.(*entry[K, V])
		c.weight += w - e.weight
		e.val, e.weight = val, w
		c.evict()
		return
	}
	c.items[key] = c.ll.PushFront(&entry[K, V]{key: key, val: val, weight: w})
	c.weight += w
	c.evict()
}

// evict drops the least recently used entries until both bounds hold. The
// entry just stored is never the one dropped, so a single oversized value is
// held rather than refused: callers ask for what they put in.
//
// Eviction unlinks; it never closes or frees. A value handed out by Get stays
// valid for as long as its holder keeps the reference, which is what makes it
// safe to evict a codec another goroutine is decompressing through.
func (c *Cache[K, V]) evict() {
	for c.ll.Len() > c.cap || (c.maxSize > 0 && c.weight > c.maxSize && c.ll.Len() > 1) {
		oldest := c.ll.Back()
		c.ll.Remove(oldest)
		e := oldest.Value.(*entry[K, V])
		c.weight -= e.weight
		delete(c.items, e.key)
	}
}

// Drop invalidates a key.
func (c *Cache[K, V]) Drop(key K) {
	if c == nil {
		return
	}
	if el, ok := c.items[key]; ok {
		c.ll.Remove(el)
		c.weight -= el.Value.(*entry[K, V]).weight
		delete(c.items, key)
	}
}

// Has reports whether a key is cached, without touching recency.
func (c *Cache[K, V]) Has(key K) bool {
	if c == nil {
		return false
	}
	_, ok := c.items[key]
	return ok
}

// Len reports how many entries the cache holds.
func (c *Cache[K, V]) Len() int {
	if c == nil {
		return 0
	}
	return c.ll.Len()
}

// Weight reports the total weight of the entries held, or 0 when entries are
// not weighed.
func (c *Cache[K, V]) Weight() int {
	if c == nil {
		return 0
	}
	return c.weight
}

// Values returns the cached values, most recently used first. It exists for
// tests that need to ask whether a particular entry is still held.
func (c *Cache[K, V]) Values() []V {
	if c == nil {
		return nil
	}
	out := make([]V, 0, c.ll.Len())
	for el := c.ll.Front(); el != nil; el = el.Next() {
		out = append(out, el.Value.(*entry[K, V]).val)
	}
	return out
}
