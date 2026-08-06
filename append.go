package pile

import (
	"fmt"
	"os"

	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
	"github.com/oriumgames/pile/format"
)

// storeAppend appends a column to the dimension's indexed file, creating the
// file on first store and preserving per-chunk user data across overwrites.
func (p *Provider) storeAppend(pos world.ChunkPos, dim world.Dimension, c *chunk.Column, side *sidecar) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	ds := p.dim(dim)
	if ds.iw == nil {
		if err := os.MkdirAll(p.dir, 0o755); err != nil {
			return fmt.Errorf("pile: create world directory: %w", err)
		}
		iw, err := format.CreateIndexed(p.dimPath(dim), p.conf.registry,
			format.Options{
				Compression: p.conf.compression,
				SkipBiomes:  p.conf.skip&SkipBiomes != 0,
				StoreLight:  p.conf.storeLight,
				Dimension:   format.DimensionOf(dim),
			})
		if err != nil {
			return err
		}
		ds.iw = iw
		ds.onDisk = true
	}
	key := [2]int32{pos[0], pos[1]}
	userData, cached := ds.udCache[key]
	cur := ds.unkCache[key]
	if side != nil {
		cur = *side
	} else if !cached {
		// First touch of this position this session: recover user data and
		// the preserved-state sidecar from the previous record, if any.
		if old, err := ds.iw.Column(pos[0], pos[1]); err == nil {
			userData = old.UserData
			cur = sidecarOf(old)
		}
	}
	if p.conf.skip&SkipChunkUserData != 0 {
		userData = nil
	}
	ds.udCache[key] = userData
	ds.unkCache[key] = cur
	ds.cache.drop(key)
	ds.dirty = true
	return ds.iw.Store(format.Column{
		X: pos[0], Z: pos[1], Col: c, UserData: userData,
		Unknown: cur.unknown, UnknownTicks: cur.ticks, UnknownStates: cur.states,
		UnknownBiomes: cur.bioUnknown, UnknownBiomeNames: cur.bioNames,
	})
}
