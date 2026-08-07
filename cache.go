package pile

import (
	"container/list"

	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
)

// lru is a bounded least-recently-used map. A nil *lru is a disabled cache:
// every store is dropped and every lookup misses, so callers need no nil check.
//
// Entries are bounded by count, and optionally by weight as well when a size
// function is supplied. Weight matters where one entry can be enormous on its
// own: a chunk's user data blob may be 16 MiB and a preserved-state sidecar
// holds up to one entry per block position, so a count alone would bound the
// number of entries without bounding the memory.
type lru[K comparable, V any] struct {
	cap     int
	maxSize int         // 0: no weight budget
	size    func(V) int // nil: entries are not weighed
	weight  int
	ll      *list.List // front = most recent; values are *lruEntry[K, V]
	items   map[K]*list.Element
}

type lruEntry[K comparable, V any] struct {
	key    K
	val    V
	weight int
}

func newLRU[K comparable, V any](capacity, maxSize int, size func(V) int) *lru[K, V] {
	if capacity <= 0 {
		return nil
	}
	return &lru[K, V]{
		cap: capacity, maxSize: maxSize, size: size,
		ll: list.New(), items: map[K]*list.Element{},
	}
}

// get returns the cached value (not a copy) and marks it recently used.
func (c *lru[K, V]) get(key K) (V, bool) {
	var zero V
	if c == nil {
		return zero, false
	}
	el, ok := c.items[key]
	if !ok {
		return zero, false
	}
	c.ll.MoveToFront(el)
	return el.Value.(*lruEntry[K, V]).val, true
}

// put stores a value the cache takes ownership of.
func (c *lru[K, V]) put(key K, val V) {
	if c == nil {
		return
	}
	w := 0
	if c.size != nil {
		w = c.size(val)
	}
	if el, ok := c.items[key]; ok {
		c.ll.MoveToFront(el)
		e := el.Value.(*lruEntry[K, V])
		c.weight += w - e.weight
		e.val, e.weight = val, w
		c.evict()
		return
	}
	c.items[key] = c.ll.PushFront(&lruEntry[K, V]{key: key, val: val, weight: w})
	c.weight += w
	c.evict()
}

// evict drops the least recently used entries until both bounds hold. The
// entry just stored is never the one dropped, so a single oversized value is
// held rather than refused: callers ask for what they put in.
func (c *lru[K, V]) evict() {
	for c.ll.Len() > c.cap || (c.maxSize > 0 && c.weight > c.maxSize && c.ll.Len() > 1) {
		oldest := c.ll.Back()
		c.ll.Remove(oldest)
		e := oldest.Value.(*lruEntry[K, V])
		c.weight -= e.weight
		delete(c.items, e.key)
	}
}

// drop invalidates a key.
func (c *lru[K, V]) drop(key K) {
	if c == nil {
		return
	}
	if el, ok := c.items[key]; ok {
		c.ll.Remove(el)
		c.weight -= el.Value.(*lruEntry[K, V]).weight
		delete(c.items, key)
	}
}

// has reports whether a key is cached, without touching recency.
func (c *lru[K, V]) has(key K) bool {
	if c == nil {
		return false
	}
	_, ok := c.items[key]
	return ok
}

// cacheEntry is a decoded column together with the per-chunk data stored
// alongside it in the same record.
type cacheEntry struct {
	col  *chunk.Column
	ud   []byte
	side sidecar
}

// columnCache is a bounded LRU of decoded columns for append-mode
// dimensions, where every load otherwise pays a frame decode.
type columnCache = lru[[2]int32, cacheEntry]

// chunkMeta is the per-chunk user data and preserved unknown-state sidecar a
// store needs in order to carry them across an overwrite without re-reading
// the previous record.
type chunkMeta struct {
	ud   []byte
	side sidecar
}

// metaCache bounds what the provider remembers about positions whose columns
// it is not holding.
//
// This was two plain maps with no delete anywhere in the package, so a chunk
// touched once was remembered for the life of the provider: the user data
// blob, which may be 16 MiB, and the sidecar, which carries one 12-byte entry
// per block position whose state the registry could not resolve and reaches
// 1.15 MiB for a single column. A world that streams chunks in and out grew
// both without limit, which is exactly the promise append mode makes and
// breaks. A column-cache hit re-inserted into both, so the bounded column
// cache could never relieve them either.
type metaCache = lru[[2]int32, chunkMeta]

// Bounds for metaCache. The count is a working set: overwriting a column
// costs one extra record decode when its metadata has been evicted, which is
// what happens anyway the first time a position is touched. The byte budget is
// what keeps a handful of pathological entries from making the count
// meaningless.
const (
	metaCacheColumns = 128
	metaCacheBytes   = 64 << 20
)

func newMetaCache(cacheColumns int) *metaCache {
	return newLRU[[2]int32](max(metaCacheColumns, cacheColumns), metaCacheBytes, chunkMetaSize)
}

// newColumnCache builds the decoded-column LRU, which is bounded by entry
// count alone: its entries are whole columns and all of a size.
func newColumnCache(capacity int) *columnCache {
	return newLRU[[2]int32, cacheEntry](capacity, 0, nil)
}

// chunkMetaSize approximates an entry's retained bytes: the blobs it holds,
// which are the parts that can be large, plus a fixed allowance for the entry
// itself.
func chunkMetaSize(m chunkMeta) int {
	n := len(m.ud) + 356
	n += len(m.side.unknown) * 12
	n += len(m.side.ticks) * 24
	n += len(m.side.bioUnknown) * 12
	for _, s := range m.side.states {
		n += len(s.Name) + 32
	}
	for _, s := range m.side.bioNames {
		n += len(s) + 16
	}
	return n
}

// readahead prefetches the four axis neighbours of pos into the cache in the
// background. Best effort: errors and races with concurrent stores are
// harmless (stores invalidate).
func (p *Provider) readahead(dim world.Dimension, pos world.ChunkPos) {
	neighbours := [4]world.ChunkPos{
		{pos[0] + 1, pos[1]}, {pos[0] - 1, pos[1]},
		{pos[0], pos[1] + 1}, {pos[0], pos[1] - 1},
	}
	go func() {
		for _, n := range neighbours {
			key := [2]int32{n[0], n[1]}
			p.mu.Lock()
			ds := p.dim(dim)
			iw, cache := ds.iw, ds.cache
			cached := cache.has(key)
			closed := p.closed
			p.mu.Unlock()
			if iw == nil || cache == nil || cached || closed {
				continue
			}
			// Capture the record's identity before decoding: a concurrent
			// StoreColumn during the decode must not be overwritten by the
			// stale result published afterwards.
			id, ok := iw.RecordID(n[0], n[1])
			if !ok {
				continue
			}
			fc, err := iw.Column(n[0], n[1])
			if err != nil {
				continue
			}
			p.mu.Lock()
			// StoreColumn holds p.mu for its whole store, so re-checking the
			// identity here is sufficient to serialise against it.
			if now, still := iw.RecordID(n[0], n[1]); still && now == id && !p.closed && ds.cache != nil {
				// The same sidecar a direct load caches: building a partial one
				// here would let cache timing decide whether an unresolved
				// biome survives the next store.
				ds.cache.put(key, cacheEntry{col: fc.Col, ud: fc.UserData, side: sidecarOf(fc)})
			}
			p.mu.Unlock()
		}
	}()
}
