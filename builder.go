package pile

import (
	"math"

	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
	"github.com/oriumgames/pile/format"
)

// Builder creates worlds programmatically without a running server: void or
// flat arenas, generated lobbies, island templates. Build the content, then
// hand it over with Provider (in-memory world) or Save (write to disk).
//
// A Builder is single-goroutine; after Provider or Save it hands ownership of
// its chunks over and resets to empty.
type Builder struct {
	reg world.BlockRegistry
	rng cube.Range

	cols     map[[2]int32]*builderCol
	settings *world.Settings
	markers  []Marker
	userData []byte
	nextID   int64
	// usedIDs tracks every entity ID handed out or supplied explicitly, so
	// an automatic ID never collides with one the caller chose.
	usedIDs map[int64]bool
}

type builderCol struct {
	ch       *chunk.Chunk
	entities []chunk.Entity
	bes      []chunk.BlockEntity
}

// NewBuilder creates a Builder for worlds with the given vertical range. A
// nil registry uses world.DefaultBlockRegistry.
func NewBuilder(reg world.BlockRegistry, rng cube.Range) *Builder {
	if reg == nil {
		reg = world.DefaultBlockRegistry
	}
	reg.Finalize()
	return &Builder{
		reg:      reg,
		rng:      rng,
		cols:     map[[2]int32]*builderCol{},
		settings: defaultSettings(),
		nextID:   1,
		usedIDs:  map[int64]bool{},
	}
}

// col returns the column for chunk coordinates, creating it when needed.
func (b *Builder) col(cx, cz int32) *builderCol {
	k := [2]int32{cx, cz}
	c, ok := b.cols[k]
	if !ok {
		c = &builderCol{ch: chunk.New(b.reg, b.rng)}
		b.cols[k] = c
	}
	return c
}

// SetBlock places a block. Positions outside the vertical range are ignored.
func (b *Builder) SetBlock(pos cube.Pos, bl world.Block) {
	if pos.Y() < b.rng[0] || pos.Y() > b.rng[1] {
		return
	}
	rid := b.reg.BlockRuntimeID(bl)
	c := b.col(int32(pos.X()>>4), int32(pos.Z()>>4))
	c.ch.SetBlock(uint8(pos.X()&15), int16(pos.Y()), uint8(pos.Z()&15), 0, rid)
	if n, ok := bl.(world.NBTer); ok {
		data := n.EncodeNBT()
		data["x"], data["y"], data["z"] = int32(pos.X()), int32(pos.Y()), int32(pos.Z())
		c.bes = append(c.bes, chunk.BlockEntity{Pos: pos, Data: data})
	}
}

// Fill places a block in every position of the box [lo, hi] (inclusive).
// The chunk lookup is hoisted per column, so large fills stay cheap.
func (b *Builder) Fill(lo, hi cube.Pos, bl world.Block) {
	rid := b.reg.BlockRuntimeID(bl)
	y0, y1 := max(lo.Y(), b.rng[0]), min(hi.Y(), b.rng[1])
	if y0 > y1 {
		return
	}
	for cx := int32(lo.X() >> 4); cx <= int32(hi.X()>>4); cx++ {
		for cz := int32(lo.Z() >> 4); cz <= int32(hi.Z()>>4); cz++ {
			c := b.col(cx, cz)
			x0, x1 := max(int(cx)*16, lo.X()), min(int(cx)*16+15, hi.X())
			z0, z1 := max(int(cz)*16, lo.Z()), min(int(cz)*16+15, hi.Z())
			for x := x0; x <= x1; x++ {
				for z := z0; z <= z1; z++ {
					for y := y0; y <= y1; y++ {
						c.ch.SetBlock(uint8(x&15), int16(y), uint8(z&15), 0, rid)
					}
				}
			}
		}
	}
}

// FillBiome sets the biome in every position of the box [lo, hi].
func (b *Builder) FillBiome(lo, hi cube.Pos, bio world.Biome) {
	id := uint32(bio.EncodeBiome())
	y0, y1 := max(lo.Y(), b.rng[0]), min(hi.Y(), b.rng[1])
	if y0 > y1 {
		return
	}
	for cx := int32(lo.X() >> 4); cx <= int32(hi.X()>>4); cx++ {
		for cz := int32(lo.Z() >> 4); cz <= int32(hi.Z()>>4); cz++ {
			c := b.col(cx, cz)
			x0, x1 := max(int(cx)*16, lo.X()), min(int(cx)*16+15, hi.X())
			z0, z1 := max(int(cz)*16, lo.Z()), min(int(cz)*16+15, hi.Z())
			for x := x0; x <= x1; x++ {
				for z := z0; z <= z1; z++ {
					for y := y0; y <= y1; y++ {
						c.ch.SetBiome(uint8(x&15), int16(y), uint8(z&15), id)
					}
				}
			}
		}
	}
}

// AddEntity adds an entity from NBT data. The data must contain a "Pos" list;
// a "UniqueID" is assigned sequentially when absent.
func (b *Builder) AddEntity(data map[string]any) {
	pos, ok := entityPos(data)
	if !ok {
		return
	}
	data = deepCopyCompound(data)
	id, ok := data["UniqueID"].(int64)
	if !ok {
		for b.usedIDs[b.nextID] {
			b.nextID++
		}
		id = b.nextID
		b.nextID++
		data["UniqueID"] = id
	}
	if b.usedIDs == nil {
		b.usedIDs = map[int64]bool{}
	}
	b.usedIDs[id] = true
	c := b.col(int32(int(math.Floor(pos[0]))>>4), int32(int(math.Floor(pos[2]))>>4))
	c.entities = append(c.entities, chunk.Entity{ID: id, Data: data})
}

// AddBlockEntity attaches block entity NBT to a position.
func (b *Builder) AddBlockEntity(pos cube.Pos, data map[string]any) {
	data = deepCopyCompound(data)
	data["x"], data["y"], data["z"] = int32(pos.X()), int32(pos.Y()), int32(pos.Z())
	c := b.col(int32(pos.X()>>4), int32(pos.Z()>>4))
	c.bes = append(c.bes, chunk.BlockEntity{Pos: pos, Data: data})
}

// Settings sets the world settings.
func (b *Builder) Settings(s *world.Settings) { b.settings = s }

// SetMarker adds or replaces a marker by name.
func (b *Builder) SetMarker(m Marker) {
	for i := range b.markers {
		if b.markers[i].Name == m.Name {
			b.markers[i] = m
			return
		}
	}
	b.markers = append(b.markers, m)
}

// SetUserData sets the world's application metadata blob.
func (b *Builder) SetUserData(data []byte) { b.userData = cloneBytes(data) }

// Provider hands the built world over as an in-memory provider. The Builder
// resets to empty; the chunks are transferred without copying.
func (b *Builder) Provider(opts ...Option) *Provider {
	p := NewMemory(opts...)
	p.settings = b.settings
	p.markers = b.markers
	p.userData = b.userData
	for k, c := range b.cols {
		p.insertColumn(world.Overworld, format.Column{
			X: k[0], Z: k[1],
			Col: &chunk.Column{Chunk: c.ch, Entities: c.entities, BlockEntities: c.bes},
		})
	}
	b.cols = map[[2]int32]*builderCol{}
	b.markers = nil
	b.userData = nil
	b.settings = defaultSettings()
	b.usedIDs = map[int64]bool{}
	b.nextID = 1
	return p
}

// Save builds and writes the world to a directory in one step.
func (b *Builder) Save(dir string, opts ...Option) error {
	p := b.Provider(opts...)
	defer p.Close()
	return p.SaveAs(dir)
}
