package pile

import (
	"errors"

	"github.com/df-mc/dragonfly/server/world"
	"github.com/oriumgames/pile/format"
)

// UserData returns a copy of the world's application metadata blob.
func (p *Provider) UserData() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return cloneBytes(p.userData)
}

// SetUserData stores the world's application metadata blob.
func (p *Provider) SetUserData(b []byte) {
	if p.conf.readOnly {
		return
	}
	p.mu.Lock()
	p.userData = cloneBytes(b)
	p.metaDirty = true
	p.mu.Unlock()
}

// ChunkUserData returns a copy of the per-chunk metadata blob, or nil.
func (p *Provider) ChunkUserData(pos world.ChunkPos, dim world.Dimension) []byte {
	if p.conf.appendMode {
		p.mu.Lock()
		ds := p.dim(dim)
		key := [2]int32{pos[0], pos[1]}
		if m, ok := ds.meta.get(key); ok {
			out := cloneBytes(m.ud)
			p.mu.Unlock()
			return out
		}
		iw := ds.iw
		p.mu.Unlock()
		if iw == nil {
			return nil
		}
		// Capture the record identity: a StoreColumn landing during the
		// decode must not be undone by caching this stale result.
		id, _ := iw.RecordID(pos[0], pos[1])
		fc, err := iw.Column(pos[0], pos[1])
		if err != nil {
			return nil
		}
		p.mu.Lock()
		if now, still := iw.RecordID(pos[0], pos[1]); still && now == id {
			ds.meta.put(key, chunkMeta{ud: fc.UserData, side: sidecarOf(fc)})
		}
		p.mu.Unlock()
		return cloneBytes(fc.UserData)
	}
	p.mu.Lock()
	c, ok := p.dim(dim).cols[[2]int32{pos[0], pos[1]}]
	p.mu.Unlock()
	if ok {
		return cloneBytes(c.UserData)
	}
	if p.base != nil {
		return p.base.ChunkUserData(pos, dim)
	}
	return nil
}

// SetChunkUserData stores a per-chunk metadata blob. It is a no-op if no
// column is stored at pos.
func (p *Provider) SetChunkUserData(pos world.ChunkPos, dim world.Dimension, b []byte) error {
	if p.conf.readOnly {
		return ErrReadOnly
	}
	if p.conf.appendMode {
		// Hold the provider lock across the whole read-modify-write: a
		// concurrent StoreColumn between reading and re-appending the record
		// would otherwise be reverted to older block data.
		p.mu.Lock()
		defer p.mu.Unlock()
		ds := p.dim(dim)
		if ds.iw == nil {
			return nil
		}
		fc, err := ds.iw.Column(pos[0], pos[1])
		if err != nil {
			if errors.Is(err, format.ErrNoColumn) {
				return nil
			}
			return err
		}
		key := [2]int32{pos[0], pos[1]}
		fc.UserData = cloneBytes(b)
		ds.meta.put(key, chunkMeta{ud: fc.UserData, side: sidecarOf(fc)})
		ds.cache.drop(key)
		ds.dirty = true
		return ds.iw.Store(fc)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	ds := p.dim(dim)
	key := [2]int32{pos[0], pos[1]}
	if c, ok := ds.cols[key]; ok {
		c.UserData = cloneBytes(b)
		ds.cols[key] = c
		ds.dirty = true
	}
	return nil
}

// Markers returns a copy of the world's markers.
func (p *Provider) Markers() []Marker {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Marker, len(p.markers))
	for i, m := range p.markers {
		m.Extra = deepCopyCompound(m.Extra)
		out[i] = m
	}
	return out
}

// SetMarker inserts or replaces (by name) a marker.
func (p *Provider) SetMarker(m Marker) {
	if p.conf.readOnly {
		return
	}
	m.Extra = deepCopyCompound(m.Extra)
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.markers {
		if p.markers[i].Name == m.Name {
			p.markers[i] = m
			p.metaDirty = true
			return
		}
	}
	p.markers = append(p.markers, m)
	p.metaDirty = true
}

// RemoveMarker deletes a marker by name, reporting whether it existed.
func (p *Provider) RemoveMarker(name string) bool {
	if p.conf.readOnly {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.markers {
		if p.markers[i].Name == name {
			p.markers = append(p.markers[:i], p.markers[i+1:]...)
			p.metaDirty = true
			return true
		}
	}
	return false
}
