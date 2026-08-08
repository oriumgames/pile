package pile

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
	"github.com/df-mc/goleveldb/leveldb"
	"github.com/oriumgames/pile/format"
)

// Structure is a block region stored in the pile structure format. It
// implements world.Structure, so it can be built with tx.BuildStructure; the
// PasteInto fast path additionally carries entities and block entity NBT,
// which the world.Structure interface cannot express.
type Structure struct {
	data    *format.StructureData
	reg     world.BlockRegistry
	air     uint32
	skipAir bool
	bes     map[[3]int32]map[string]any

	// maxDecoded is the caller's decode ceiling for LoadStructure, in bytes.
	// Zero is the format's own.
	maxDecoded int64
}

var _ world.Structure = (*Structure)(nil)

// StructureOption configures a Structure.
type StructureOption func(*Structure)

// SkipAir makes At return nil for air cells, so building the structure onto a
// world leaves existing blocks in air positions untouched. PasteInto honours
// it the same way.
func SkipAir() StructureOption { return func(s *Structure) { s.skipAir = true } }

// StructureRegistry sets the block registry used to resolve blocks. Default:
// world.DefaultBlockRegistry.
func StructureRegistry(reg world.BlockRegistry) StructureOption {
	return func(s *Structure) {
		if reg != nil {
			s.reg = reg
		}
	}
}

// StructureMaxDecodedBytes bounds the live decoded state LoadStructure may
// produce, in bytes. It is MaxDecodedBytes for structure files: 0 (the default)
// is the format's own ceiling, a value above it is clamped down, and a refusal
// reports format.ErrDecodeBudget rather than claiming the file is corrupt.
//
// It has the same blind spot the provider's does, and it matters more here
// because a structure is a single object: the ceiling charges decoded cells and
// section storages, and charges nothing for the block entities and entities a
// structure carries.
func StructureMaxDecodedBytes(n int64) StructureOption {
	return func(s *Structure) { s.maxDecoded = n }
}

// structureReadOpts translates a structure's decode policy into format
// ReadOptions, returning nil when there is nothing to state.
func (s *Structure) structureReadOpts() []format.ReadOption {
	if s.maxDecoded <= 0 {
		return nil
	}
	return []format.ReadOption{format.MaxDecodedBytes(s.maxDecoded)}
}

// newStructure wraps decoded structure data.
func newStructure(data *format.StructureData, opts ...StructureOption) *Structure {
	s := &Structure{data: data, reg: world.DefaultBlockRegistry}
	for _, o := range opts {
		o(s)
	}
	s.reg.Finalize()
	s.air = s.reg.AirRuntimeID()
	s.bes = make(map[[3]int32]map[string]any, len(data.BlockEntities))
	for _, be := range data.BlockEntities {
		s.bes[be.Pos] = be.Data
	}
	return s
}

// LoadStructure reads a structure file.
func LoadStructure(path string, opts ...StructureOption) (*Structure, error) {
	file, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s := &Structure{reg: world.DefaultBlockRegistry}
	for _, o := range opts {
		o(s)
	}
	s.reg.Finalize()
	data, err := format.ReadStructure(file, s.reg, s.structureReadOpts()...)
	if err != nil {
		return nil, fmt.Errorf("pile: read structure %s: %w", path, err)
	}
	return newStructure(data, opts...), nil
}

// Save writes the structure to a file atomically.
func (s *Structure) Save(path string) error {
	tmp := path + ".tmp"
	f, err := createExclusive(tmp)
	if err != nil {
		return err
	}
	err = format.WriteStructure(f, s.data, s.reg, format.Options{Compression: format.CompressionBest})
	if err2 := f.Sync(); err == nil {
		err = err2
	}
	if err2 := f.Close(); err == nil {
		err = err2
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := preserveMode(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

// Data exposes the underlying structure data. It is the Structure's own, not a
// copy: mutating it mutates the Structure, and the encoder requires Cells to
// stay exactly CellDims(Size) long, so a caller that resizes one without the
// other has built a value that will not save.
func (s *Structure) Data() *format.StructureData { return s.data }

// Dimensions returns the structure's size, implementing world.Structure.
func (s *Structure) Dimensions() [3]int {
	return [3]int{int(s.data.Size[0]), int(s.data.Size[1]), int(s.data.Size[2])}
}

// At returns the block at a structure-local position, implementing
// world.Structure. Blocks carrying NBT (chests, signs, ...) are returned with
// their block entity data decoded into the block.
func (s *Structure) At(x, y, z int, _ func(x, y, z int) world.Block) (world.Block, world.Liquid) {
	rid, liquidRid := s.ridAt(x, y, z)
	var liq world.Liquid
	if liquidRid != s.air {
		if b, ok := s.reg.BlockByRuntimeID(liquidRid); ok {
			liq, _ = b.(world.Liquid)
		}
	}
	if rid == s.air {
		if s.skipAir {
			return nil, liq
		}
		return s.reg.Air(), liq
	}
	b, ok := s.reg.BlockByRuntimeID(rid)
	if !ok {
		return s.reg.Air(), liq
	}
	if data, hasBE := s.bes[[3]int32{int32(x), int32(y), int32(z)}]; hasBE {
		if n, isNBT := b.(world.NBTer); isNBT {
			if nb, isBlock := n.DecodeNBT(data).(world.Block); isBlock {
				b = nb
			}
		}
	}
	return b, liq
}

// layerCount returns how many storage layers the structure actually uses,
// bounded by what the format accepts.
func (s *Structure) layerCount() uint8 {
	n := uint8(1)
	for _, c := range s.data.Cells {
		if c == nil {
			continue
		}
		if l := len(c.Layers()); l > int(n) {
			n = uint8(min(l, format.MaxLayers))
		}
	}
	// A preserved state can name a layer no cell allocated, the same way it
	// can in a world section. Pasting only the allocated layers would leave
	// those states behind, which is the extraction defect in the other
	// direction.
	for _, u := range s.data.Unknown {
		if int(u.Layer)+1 > int(n) {
			n = uint8(min(int(u.Layer)+1, format.MaxLayers))
		}
	}
	return n
}

// ridAt returns the layer-0 and layer-1 runtime IDs at a local position, air
// for anything outside the structure or in absent cells.
func (s *Structure) ridAt(x, y, z int) (uint32, uint32) {
	sz := s.data.Size
	if x < 0 || y < 0 || z < 0 || x >= int(sz[0]) || y >= int(sz[1]) || z >= int(sz[2]) {
		return s.air, s.air
	}
	cell := s.data.Cells[format.CellIndex(sz, int32(x>>4), int32(y>>4), int32(z>>4))]
	if cell == nil {
		return s.air, s.air
	}
	return cell.Block(uint8(x&15), uint8(y&15), uint8(z&15), 0),
		cell.Block(uint8(x&15), uint8(y&15), uint8(z&15), 1)
}

// maxPasteCoord is the last block coordinate that maps onto a distinct int32
// chunk key: chunk keys are int32 and a chunk spans 16 blocks.
const maxPasteCoord = 1<<35 - 1

// checkPasteCoords refuses a position no chunk key can name.
func checkPasteCoords(what string, p cube.Pos) error {
	for i, v := range p {
		if v < -maxPasteCoord || v > maxPasteCoord {
			return fmt.Errorf("pile: %s axis %d is %d, outside the addressable range ±%d", what, i, v, int64(maxPasteCoord))
		}
	}
	return nil
}

// regionSize turns an inclusive block box into a structure size, in exact
// arithmetic.
//
// The narrowing this replaces was int32(hi.X() - lo.X() + 1), computed in int
// and then truncated, so a span of 2^32+1 became a size of 1: NewStructureData
// accepted the 1x1x1 structure and the chunk loop below then walked the real
// span, 268,435,457 chunk positions on one axis alone. Nothing about that is
// slow-but-finite in practice — a caller passing --max 4294967296,0,0 on the
// command line waits forever — so the box is validated before anything is
// allocated, and every later bound (the cell ceiling, the chunk loop) is
// derived from a size that is now the box it came from.
//
// The chunk loop is bounded once this holds: NewStructureData refuses a size
// whose cell count passes §8's ceiling, and the loop runs one iteration per
// cell column.
func regionSize(lo, hi cube.Pos) ([3]int32, error) {
	var size [3]int32
	for i := range 3 {
		if hi[i] < lo[i] {
			return size, fmt.Errorf("pile: region axis %d is inverted: %d..%d", i, lo[i], hi[i])
		}
		// Exact: hi >= lo, so the unsigned difference is the true span even
		// when the signed one would overflow.
		span := uint64(hi[i]) - uint64(lo[i]) + 1
		if span > math.MaxInt32 {
			return size, fmt.Errorf("pile: region axis %d spans %d blocks (%d..%d), which no structure can hold", i, span, lo[i], hi[i])
		}
		size[i] = int32(span)
	}
	// The chunk keys the loop builds are int32 on the wire, so a box outside
	// the addressable chunk range cannot name the columns it would need.
	for _, i := range [2]int{0, 2} {
		if lo[i]>>4 < math.MinInt32 || hi[i]>>4 > math.MaxInt32 {
			return size, fmt.Errorf("pile: region axis %d (%d..%d) lies outside the addressable chunk range", i, lo[i], hi[i])
		}
	}
	return size, nil
}

// ExtractStructure copies the block region [lo, hi] (inclusive) of a
// dimension from a provider into a new Structure, including block entities
// and entities inside the region. Options apply to the resulting structure.
func ExtractStructure(p *Provider, dim world.Dimension, lo, hi cube.Pos, opts ...StructureOption) (*Structure, error) {
	size, err := regionSize(lo, hi)
	if err != nil {
		return nil, err
	}
	data, err := format.NewStructureData(size)
	if err != nil {
		return nil, err
	}
	out := newStructure(data, opts...)

	for cx := int32(lo.X() >> 4); cx <= int32(hi.X()>>4); cx++ {
		for cz := int32(lo.Z() >> 4); cz <= int32(hi.Z()>>4); cz++ {
			col, err := p.LoadColumn(world.ChunkPos{cx, cz}, dim)
			if errors.Is(err, leveldb.ErrNotFound) {
				continue
			} else if err != nil {
				return nil, err
			}
			side := p.columnSidecar(world.ChunkPos{cx, cz}, dim)
			extractChunkRegion(out, col, cx, cz, lo, hi, side.unknown, side.states)
		}
	}
	return out, nil
}

// extractChunkRegion copies the intersection of a column with the region,
// carrying preserved unknown block states across into structure-local
// sidecar entries.
func extractChunkRegion(s *Structure, col *chunk.Column, cx, cz int32, lo, hi cube.Pos, unknown []format.UnknownBlock, states []format.BlockState) {
	ch := col.Chunk
	r := ch.Range()

	type srcKey struct {
		sec   int32
		layer uint8
		idx   uint16
	}
	srcUnknown := make(map[srcKey]uint32, len(unknown))
	uniform := make(map[srcKey]uint32, len(unknown))
	for _, u := range unknown {
		if u.Index == format.WholeStorage {
			uniform[srcKey{sec: u.Section, layer: u.Layer}] = u.State
			continue
		}
		srcUnknown[srcKey{sec: u.Section, layer: u.Layer, idx: u.Index}] = u.State
	}
	stateBase, based := uint32(0), false
	base := func() uint32 {
		if !based {
			stateBase = uint32(len(s.data.UnknownStates))
			s.data.UnknownStates = append(s.data.UnknownStates, states...)
			based = true
		}
		return stateBase
	}
	x0 := max(int(cx)*16, lo.X())
	x1 := min(int(cx)*16+15, hi.X())
	z0 := max(int(cz)*16, lo.Z())
	z1 := min(int(cz)*16+15, hi.Z())
	y0 := max(r[0], lo.Y())
	y1 := min(r[1], hi.Y())

	// A preserved state can name a layer with no storage of its own, so the
	// traversal ceiling covers whatever the sidecar reaches too.
	layerN := max(chunkLayers(ch), sidecarLayers(unknown))
	for wx := x0; wx <= x1; wx++ {
		for wz := z0; wz <= z1; wz++ {
			for wy := y0; wy <= y1; wy++ {
				for layer := range layerN {
					rid := ch.Block(uint8(wx&15), int16(wy), uint8(wz&15), layer)
					srcSec := int32(wy >> 4)
					srcIdx := uint16(wx&15)<<8 | uint16(wz&15)<<4 | uint16(wy&15)
					var state uint32
					var ok bool
					if len(unknown) > 0 {
						state, ok = srcUnknown[srcKey{sec: srcSec, layer: layer, idx: srcIdx}]
						if !ok {
							state, ok = uniform[srcKey{sec: srcSec, layer: layer}]
						}
					}
					// The sidecar is consulted before the air test: on a
					// registry that resolves neither the state nor the
					// placeholder, an unresolved block reads as air, and
					// skipping air first would drop the very positions the
					// sidecar exists to preserve.
					if !ok && rid == s.air {
						continue
					}
					lx, ly, lz := wx-lo.X(), wy-lo.Y(), wz-lo.Z()
					if rid != s.air {
						s.setLocal(lx, ly, lz, layer, rid)
					}
					if ok {
						s.data.Unknown = append(s.data.Unknown, format.UnknownBlock{
							Section: int32(format.CellIndex(s.data.Size, int32(lx>>4), int32(ly>>4), int32(lz>>4))),
							Layer:   layer,
							Index:   uint16(lx&15)<<8 | uint16(lz&15)<<4 | uint16(ly&15),
							State:   base() + state,
						})
					}
				}
			}
		}
	}
	for _, be := range col.BlockEntities {
		pos := be.Pos
		if pos.X() < lo.X() || pos.X() > hi.X() || pos.Y() < lo.Y() || pos.Y() > hi.Y() ||
			pos.Z() < lo.Z() || pos.Z() > hi.Z() {
			continue
		}
		local := [3]int32{int32(pos.X() - lo.X()), int32(pos.Y() - lo.Y()), int32(pos.Z() - lo.Z())}
		data := deepCopyCompound(be.Data)
		delete(data, "x")
		delete(data, "y")
		delete(data, "z")
		s.data.BlockEntities = append(s.data.BlockEntities, format.StructureBlockEntity{Pos: local, Data: data})
		s.bes[local] = data
	}
	for _, e := range col.Entities {
		pos, ok := entityPos(e.Data)
		if !ok {
			continue
		}
		if pos[0] < float64(lo.X()) || pos[0] >= float64(hi.X()+1) ||
			pos[1] < float64(lo.Y()) || pos[1] >= float64(hi.Y()+1) ||
			pos[2] < float64(lo.Z()) || pos[2] >= float64(hi.Z()+1) {
			continue
		}
		data := deepCopyCompound(e.Data)
		data["Pos"] = []any{
			float32(pos[0] - float64(lo.X())),
			float32(pos[1] - float64(lo.Y())),
			float32(pos[2] - float64(lo.Z())),
		}
		// The stable ID lives beside the NBT in memory and is only written
		// into it on encode; carry it explicitly so an extraction taken
		// before any save does not lose it (which would paste every such
		// entity as ID 0).
		if _, ok := data["UniqueID"].(int64); !ok {
			data["UniqueID"] = e.ID
		}
		s.data.Entities = append(s.data.Entities, data)
	}
}

// chunkLayers returns the highest storage layer count in a chunk, bounded by
// what the format accepts.
func chunkLayers(ch *chunk.Chunk) uint8 {
	n := 1
	for _, sub := range ch.Sub() {
		if l := len(sub.Layers()); l > n {
			n = l
		}
	}
	return uint8(min(n, format.MaxLayers))
}

// setLocal writes a runtime ID at a structure-local position, allocating the
// cell when needed.
func (s *Structure) setLocal(x, y, z int, layer uint8, rid uint32) {
	idx := format.CellIndex(s.data.Size, int32(x>>4), int32(y>>4), int32(z>>4))
	cell := s.data.Cells[idx]
	if cell == nil {
		cell = chunk.NewSubChunk(s.air)
		s.data.Cells[idx] = cell
	}
	cell.SetBlock(uint8(x&15), uint8(y&15), uint8(z&15), layer, rid)
}

// PasteInto writes the structure into a provider's stored world at position
// `at` (the structure's origin offset is applied). Blocks, block entities and
// entities are carried; with SkipAir, air positions leave existing blocks
// untouched. Positions outside the dimension's range are dropped.
func (s *Structure) PasteInto(p *Provider, dim world.Dimension, at cube.Pos) error {
	// Every world position this paste touches has to be addressable. A chunk
	// key is int32 on the wire and the code below builds one with
	// int32(wx >> 4), so a position far outside that range was narrowed
	// silently: the paste landed at coordinates nobody asked for, and two
	// columns of one structure could collide onto a single key, which is
	// content lost with no error anywhere. maxPasteCoord is the last block
	// coordinate that maps onto a distinct int32 chunk key; checking `at`
	// before adding the origin is also what keeps that addition from
	// overflowing.
	if err := checkPasteCoords("paste position", at); err != nil {
		return err
	}
	base := cube.Pos{
		at.X() + int(s.data.Origin[0]),
		at.Y() + int(s.data.Origin[1]),
		at.Z() + int(s.data.Origin[2]),
	}
	if err := checkPasteCoords("paste position plus the structure origin", base); err != nil {
		return err
	}
	sz := s.data.Size
	r := dim.Range()

	// Preserved unknown states from the structure, keyed by their cell,
	// layer and cell-local index so they can be re-anchored in world space.
	type cellKey struct {
		cell  int32
		layer uint8
		idx   uint16
	}
	srcUnknown := make(map[cellKey]uint32, len(s.data.Unknown))
	uniform := make(map[cellKey]uint32, len(s.data.Unknown))
	for _, u := range s.data.Unknown {
		if u.Index == format.WholeStorage {
			uniform[cellKey{cell: u.Section, layer: u.Layer}] = u.State
			continue
		}
		srcUnknown[cellKey{cell: u.Section, layer: u.Layer, idx: u.Index}] = u.State
	}

	type colKey [2]int32
	touched := map[colKey]*chunk.Column{}
	// Destination sidecars start from what the column already had, so states
	// preserved elsewhere in the chunk are not dropped by the paste; the
	// structure's own states are appended after them.
	sides := map[colKey]*sidecar{}
	stateBase := map[colKey]uint32{}
	sideFor := func(k colKey) (*sidecar, uint32) {
		sd, ok := sides[k]
		if !ok {
			cur := p.columnSidecar(world.ChunkPos{k[0], k[1]}, dim)
			sd = &sidecar{
				unknown:    append([]format.UnknownBlock(nil), cur.unknown...),
				ticks:      append([]format.UnknownTick(nil), cur.ticks...),
				states:     append(append([]format.BlockState(nil), cur.states...), s.data.UnknownStates...),
				bioUnknown: cur.bioUnknown,
				bioNames:   cur.bioNames,
			}
			sides[k] = sd
			stateBase[k] = uint32(len(cur.states))
		}
		return sd, stateBase[k]
	}
	columnFor := func(wx, wz int) (*chunk.Column, error) {
		k := colKey{int32(wx >> 4), int32(wz >> 4)}
		if c, ok := touched[k]; ok {
			return c, nil
		}
		c, err := p.LoadColumn(world.ChunkPos{k[0], k[1]}, dim)
		if errors.Is(err, leveldb.ErrNotFound) {
			c = &chunk.Column{Chunk: chunk.New(s.reg, r)}
		} else if err != nil {
			return nil, err
		}
		touched[k] = c
		return c, nil
	}

	layerN := s.layerCount()

	// wrote reports whether the paste owns a world position (and so must
	// also clear stale content there).
	// preserved reports whether a local position carries a preserved state in
	// any layer. On a registry whose placeholder resolves to air the position
	// reads as empty, so an air test alone would skip it and drop the state.
	preserved := func(lx, ly, lz int) bool {
		if len(s.data.Unknown) == 0 {
			return false
		}
		cell := int32(format.CellIndex(sz, int32(lx>>4), int32(ly>>4), int32(lz>>4)))
		idx := uint16(lx&15)<<8 | uint16(lz&15)<<4 | uint16(ly&15)
		for layer := range layerN {
			if _, ok := srcUnknown[cellKey{cell: cell, layer: layer, idx: idx}]; ok {
				return true
			}
			if _, ok := uniform[cellKey{cell: cell, layer: layer}]; ok {
				return true
			}
		}
		return false
	}
	wrote := func(lx, ly, lz int) bool {
		if !s.skipAir {
			return true
		}
		for layer := range layerN {
			if s.cellRid(int32(lx), int32(ly), int32(lz), layer) != s.air {
				return true
			}
		}
		return preserved(lx, ly, lz)
	}

	for x := 0; x < int(sz[0]); x++ {
		for z := 0; z < int(sz[2]); z++ {
			wx, wz := base.X()+x, base.Z()+z
			var col *chunk.Column
			for y := 0; y < int(sz[1]); y++ {
				wy := base.Y() + y
				if wy < r[0] || wy > r[1] {
					continue
				}
				if s.skipAir && !wrote(x, y, z) {
					continue
				}
				// Only materialise the destination column once the paste
				// actually owns a position in it: an all-air SkipAir paste
				// must not create (and thereby "generate") empty chunks.
				if col == nil {
					c, err := columnFor(wx, wz)
					if err != nil {
						return err
					}
					col = c
				}
				for layer := range layerN {
					rid := s.cellRid(int32(x), int32(y), int32(z), layer)
					if layer == 0 && rid == s.air && s.skipAir {
						continue
					}
					col.Chunk.SetBlock(uint8(wx&15), int16(wy), uint8(wz&15), layer, rid)
				}

				if len(s.data.Unknown) > 0 {
					cell := int32(format.CellIndex(sz, int32(x>>4), int32(y>>4), int32(z>>4)))
					cellIdx := uint16(x&15)<<8 | uint16(z&15)<<4 | uint16(y&15)
					dstSec := int32(wy >> 4)
					dstIdx := uint16(wx&15)<<8 | uint16(wz&15)<<4 | uint16(wy&15)
					for layer := range layerN {
						state, ok := srcUnknown[cellKey{cell: cell, layer: layer, idx: cellIdx}]
						if !ok {
							state, ok = uniform[cellKey{cell: cell, layer: layer}]
						}
						if ok {
							k := colKey{int32(wx >> 4), int32(wz >> 4)}
							sd, sbase := sideFor(k)
							sd.unknown = append(sd.unknown, format.UnknownBlock{
								Section: dstSec, Layer: layer, Index: dstIdx, State: sbase + state,
							})
						}
					}
				}
			}
		}
	}

	// Drop preserved states at positions the paste overwrote: the pasted
	// content owns them now.
	for k, sd := range sides {
		kept := sd.unknown[:0]
		for _, u := range sd.unknown {
			lx := int(u.Index>>8&15) + int(k[0])*16 - base.X()
			ly := int(u.Section)*16 + int(u.Index&15) - base.Y()
			lz := int(u.Index>>4&15) + int(k[1])*16 - base.Z()
			inside := lx >= 0 && ly >= 0 && lz >= 0 &&
				lx < int(sz[0]) && ly < int(sz[1]) && lz < int(sz[2])
			if inside && wrote(lx, ly, lz) && u.State < stateBase[k] {
				continue // an inherited entry the paste replaced
			}
			kept = append(kept, u)
		}
		sd.unknown = kept
	}

	// Remove block entities the paste overwrote; the structure's own follow.
	for _, col := range touched {
		kept := col.BlockEntities[:0]
		for _, be := range col.BlockEntities {
			lx := be.Pos.X() - base.X()
			ly := be.Pos.Y() - base.Y()
			lz := be.Pos.Z() - base.Z()
			inside := lx >= 0 && ly >= 0 && lz >= 0 &&
				lx < int(sz[0]) && ly < int(sz[1]) && lz < int(sz[2])
			if inside && wrote(lx, ly, lz) {
				continue
			}
			kept = append(kept, be)
		}
		col.BlockEntities = kept
	}
	for _, be := range s.data.BlockEntities {
		wx, wy, wz := base.X()+int(be.Pos[0]), base.Y()+int(be.Pos[1]), base.Z()+int(be.Pos[2])
		if wy < r[0] || wy > r[1] {
			continue
		}
		col, err := columnFor(wx, wz)
		if err != nil {
			return err
		}
		data := deepCopyCompound(be.Data)
		data["x"], data["y"], data["z"] = int32(wx), int32(wy), int32(wz)
		col.BlockEntities = append(col.BlockEntities, chunk.BlockEntity{
			Pos: cube.Pos{wx, wy, wz}, Data: data,
		})
	}
	for _, e := range s.data.Entities {
		pos, ok := entityPos(e)
		if !ok {
			continue
		}
		wx, wy, wz := pos[0]+float64(base.X()), pos[1]+float64(base.Y()), pos[2]+float64(base.Z())
		if wy < float64(r[0]) || wy > float64(r[1])+1 {
			continue // outside the dimension, like out-of-range blocks
		}
		col, err := columnFor(int(math.Floor(wx)), int(math.Floor(wz)))
		if err != nil {
			return err
		}
		data := deepCopyCompound(e)
		data["Pos"] = []any{float32(wx), float32(wy), float32(wz)}
		id, _ := data["UniqueID"].(int64)
		col.Entities = append(col.Entities, chunk.Entity{ID: id, Data: data})
	}

	for k, col := range touched {
		// A column the paste added preserved states to carries them; others
		// keep whatever the provider already had.
		if err := p.storeColumn(world.ChunkPos{k[0], k[1]}, dim, col, sides[k]); err != nil {
			return err
		}
	}
	return nil
}

// entityPos extracts an entity's position from its NBT data.
func entityPos(data map[string]any) ([3]float64, bool) {
	switch pos := data["Pos"].(type) {
	case []any:
		if len(pos) == 3 {
			var out [3]float64
			for i := range 3 {
				switch v := pos[i].(type) {
				case float32:
					out[i] = float64(v)
				case float64:
					out[i] = v
				default:
					return out, false
				}
			}
			return out, true
		}
	case []float32:
		if len(pos) == 3 {
			return [3]float64{float64(pos[0]), float64(pos[1]), float64(pos[2])}, true
		}
	}
	return [3]float64{}, false
}
