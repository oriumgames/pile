package pile

import (
	"container/list"

	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
)

// columnCache is a bounded LRU of decoded columns for append-mode
// dimensions, where every load otherwise pays a frame decode.
type columnCache struct {
	cap   int
	ll    *list.List // front = most recent; values are *cacheEntry
	items map[[2]int32]*list.Element
}

type cacheEntry struct {
	key  [2]int32
	col  *chunk.Column
	ud   []byte
	side sidecar
}

func newColumnCache(capacity int) *columnCache {
	if capacity <= 0 {
		return nil
	}
	return &columnCache{cap: capacity, ll: list.New(), items: map[[2]int32]*list.Element{}}
}

// get returns the cached column (not a copy) and marks it recently used.
func (c *columnCache) get(key [2]int32) (*chunk.Column, []byte, sidecar, bool) {
	if c == nil {
		return nil, nil, sidecar{}, false
	}
	el, ok := c.items[key]
	if !ok {
		return nil, nil, sidecar{}, false
	}
	c.ll.MoveToFront(el)
	e := el.Value.(*cacheEntry)
	return e.col, e.ud, e.side, true
}

// put stores a column the cache takes ownership of.
func (c *columnCache) put(key [2]int32, col *chunk.Column, ud []byte, side sidecar) {
	if c == nil {
		return
	}
	if el, ok := c.items[key]; ok {
		c.ll.MoveToFront(el)
		e := el.Value.(*cacheEntry)
		e.col, e.ud, e.side = col, ud, side
		return
	}
	c.items[key] = c.ll.PushFront(&cacheEntry{key: key, col: col, ud: ud, side: side})
	for c.ll.Len() > c.cap {
		oldest := c.ll.Back()
		c.ll.Remove(oldest)
		delete(c.items, oldest.Value.(*cacheEntry).key)
	}
}

// drop invalidates a key.
func (c *columnCache) drop(key [2]int32) {
	if c == nil {
		return
	}
	if el, ok := c.items[key]; ok {
		c.ll.Remove(el)
		delete(c.items, key)
	}
}

// has reports whether a key is cached, without touching recency.
func (c *columnCache) has(key [2]int32) bool {
	if c == nil {
		return false
	}
	_, ok := c.items[key]
	return ok
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
				ds.cache.put(key, fc.Col, fc.UserData, sidecar{unknown: fc.Unknown, ticks: fc.UnknownTicks, states: fc.UnknownStates})
			}
			p.mu.Unlock()
		}
	}()
}
