package pile

import (
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
	"github.com/oriumgames/pile/internal/lru"
)

// cacheEntry is a decoded column together with the per-chunk data stored
// alongside it in the same record.
type cacheEntry struct {
	col  *chunk.Column
	ud   []byte
	side sidecar
}

// columnCache is a bounded LRU of decoded columns for append-mode
// dimensions, where every load otherwise pays a frame decode.
type columnCache = lru.Cache[[2]int32, cacheEntry]

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
type metaCache = lru.Cache[[2]int32, chunkMeta]

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
	return lru.New[[2]int32](max(metaCacheColumns, cacheColumns), metaCacheBytes, chunkMetaSize)
}

// newColumnCache builds the decoded-column LRU, which is bounded by entry
// count alone: its entries are whole columns and all of a size.
func newColumnCache(capacity int) *columnCache {
	return lru.New[[2]int32, cacheEntry](capacity, 0, nil)
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
			cached := cache.Has(key)
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
				ds.cache.Put(key, cacheEntry{col: fc.Col, ud: fc.UserData, side: sidecarOf(fc)})
			}
			p.mu.Unlock()
		}
	}()
}
