package format

import (
	"cmp"
	"fmt"
	"io"
	"slices"

	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
	"github.com/klauspost/compress/zstd"
)

// StructureData is the decoded content of a structure file: a block grid of
// fixed dimensions stored as 16³ cells, plus block entities and entities with
// structure-local positions.
type StructureData struct {
	// Size is the dimensions in blocks (width, height, length).
	Size [3]int32
	// Origin is the paste anchor offset applied when the structure is placed.
	Origin [3]int32
	// Cells holds the 16³ cells covering the bounding box, nil for all-air
	// cells. Index order: x-major, then z, then y (see CellIndex).
	Cells []*chunk.SubChunk
	// BlockEntities hold structure-local positions and NBT data.
	BlockEntities []StructureBlockEntity
	// Entities hold NBT data with structure-local Pos.
	Entities []map[string]any
	// UserData is an application-defined metadata blob.
	UserData []byte

	// Unknown and UnknownStates preserve block states that did not resolve
	// against the registry at decode time, so a load/save round trip does not
	// destroy them. Section indexes Cells.
	Unknown       []UnknownBlock
	UnknownStates []BlockState
}

// StructureBlockEntity is a block entity at a structure-local position.
type StructureBlockEntity struct {
	Pos  [3]int32
	Data map[string]any
}

// CellDims returns the cell grid dimensions for a structure size.
func CellDims(size [3]int32) (cx, cy, cz int32) {
	return (size[0] + 15) >> 4, (size[1] + 15) >> 4, (size[2] + 15) >> 4
}

// CellIndex returns the index of cell (x, z, y) in StructureData.Cells:
// x-major, then z, then y.
func CellIndex(size [3]int32, cx, cy, cz int32) int {
	_, ny, nz := CellDims(size)
	return int((cx*nz+cz)*ny + cy)
}

// NewStructureData allocates an empty structure of the given size.
func NewStructureData(size [3]int32) (*StructureData, error) {
	if size[0] <= 0 || size[1] <= 0 || size[2] <= 0 {
		return nil, fmt.Errorf("pile: invalid structure size %v", size)
	}
	// Round in 64-bit: (size+15) overflows int32 near its maximum, which
	// would otherwise yield a negative cell count and panic in make.
	nx := (int64(size[0]) + 15) >> 4
	ny := (int64(size[1]) + 15) >> 4
	nz := (int64(size[2]) + 15) >> 4
	cells := nx * ny * nz
	if cells > maxStructureCells {
		return nil, fmt.Errorf("pile: structure size %v too large (%d cells, limit %d)", size, cells, maxStructureCells)
	}
	return &StructureData{Size: size, Cells: make([]*chunk.SubChunk, cells)}, nil
}

// clearPadding zeroes every position in an edge cell that lies outside the
// structure's declared size.
func clearPadding(s *StructureData, air uint32) {
	nx, ny, nz := CellDims(s.Size)
	for cx := int32(0); cx < nx; cx++ {
		for cy := int32(0); cy < ny; cy++ {
			for cz := int32(0); cz < nz; cz++ {
				cell := s.Cells[CellIndex(s.Size, cx, cy, cz)]
				if cell == nil {
					continue
				}
				for lx := range uint8(16) {
					for ly := range uint8(16) {
						for lz := range uint8(16) {
							x, y, z := cx*16+int32(lx), cy*16+int32(ly), cz*16+int32(lz)
							if x < s.Size[0] && y < s.Size[1] && z < s.Size[2] {
								continue
							}
							for layer := range uint8(len(cell.Layers())) {
								cell.SetBlock(lx, ly, lz, layer, air)
							}
						}
					}
				}
			}
		}
	}
}

// validateStructureData rejects content that would encode into a structure
// file the decoder refuses to read back.
func validateStructureData(s *StructureData) error {
	if err := checkBlob(s.UserData, "structure user data"); err != nil {
		return err
	}
	if n := len(s.Entities); n > maxPerChunk {
		return fmt.Errorf("pile: structure has %d entities, limit %d", n, maxPerChunk)
	}
	if n := len(s.BlockEntities); n > maxPerChunk {
		return fmt.Errorf("pile: structure has %d block entities, limit %d", n, maxPerChunk)
	}
	for i := range 3 {
		if s.Size[i] <= 0 || int64(s.Size[i]) > maxStructureSize {
			// The decoder rejects any component above this ceiling, so the
			// writer must too.
			return fmt.Errorf("pile: invalid structure size %v", s.Size)
		}
	}
	for _, be := range s.BlockEntities {
		for i := range 3 {
			if be.Pos[i] < 0 || be.Pos[i] >= s.Size[i] {
				return fmt.Errorf("pile: block entity at %v is outside the structure %v", be.Pos, s.Size)
			}
		}
	}
	return nil
}

// WriteStructure encodes a structure file. Output is deterministic.
func WriteStructure(out io.Writer, s *StructureData, reg world.BlockRegistry, opts Options) error {
	if err := validateStructureData(s); err != nil {
		return err
	}
	nx, ny, nz := CellDims(s.Size)
	if int64(len(s.Cells)) != int64(nx)*int64(ny)*int64(nz) {
		return fmt.Errorf("pile: cell count %d does not match size %v", len(s.Cells), s.Size)
	}

	// Positions outside the declared box live in edge cells but are not part
	// of the structure: clear them so identical structures encode identically
	// and hidden data cannot ride along.
	clearPadding(s, reg.AirRuntimeID())

	blockPal := newBlockPaletteBuilder(reg)
	placeholder := placeholderRid(reg)
	type secLayer struct {
		sec   int32
		layer uint8
	}
	unknownBySec := make(map[secLayer][]UnknownBlock, len(s.Unknown))
	for _, u := range s.Unknown {
		k := secLayer{sec: u.Section, layer: u.Layer}
		unknownBySec[k] = append(unknownBySec[k], u)
	}

	// Pass 1: extract cells.
	type cellData struct{ layers []secData }
	cells := make([]cellData, len(s.Cells))
	addBlock := func(rid uint32) uint32 { return blockPal.add(rid) }
	for i, sub := range s.Cells {
		// SubChunk compaction is not exported by dragonfly; canonicalBlob
		// sorts palettes by global reference, so blob bytes stay canonical
		// regardless of local palette order. Uniform cells encode as index
		// arrays of one repeated byte, which zstd flattens.
		if sub == nil || sub.Empty() {
			continue
		}
		layers := sub.Layers()
		if len(layers) > maxLayers {
			return fmt.Errorf("pile: cell %d has %d layers (limit %d)", i, len(layers), maxLayers)
		}
		cd := cellData{layers: make([]secData, len(layers))}
		for l, storage := range layers {
			// Extract into raw form first so preserved unknown states can be
			// re-injected before the palette is resolved.
			var raw rawBlockSec
			sd := extractStorage(storage, func(v uint32) uint32 {
				raw.rids = append(raw.rids, v)
				return uint32(len(raw.rids) - 1)
			})
			raw.idx = sd.idx
			if entries := unknownBySec[secLayer{sec: int32(i), layer: uint8(l)}]; len(entries) > 0 {
				injectUnknown(&raw, entries, placeholder, len(s.UnknownStates))
			}
			pal := make([]uint32, len(raw.rids))
			for j, rid := range raw.rids {
				if raw.states != nil && raw.states[j] >= 0 {
					pal[j] = blockPal.addState(s.UnknownStates[raw.states[j]])
					continue
				}
				pal[j] = addBlock(rid)
			}
			cd.layers[l] = secData{pal: pal, idx: raw.idx}
		}
		cells[i] = cd
	}
	blockPalBytes, blockRemap, err := blockPal.finalize()
	if err != nil {
		return err
	}

	// Pass 2: blob table + structure record.
	table := newBlobTable()
	rec := &writer{}
	for i := range 3 {
		rec.uvarint(uint64(s.Size[i]))
	}
	for i := range 3 {
		rec.svarint(int64(s.Origin[i]))
	}
	presence := make([]byte, (len(cells)+7)/8)
	for i, cd := range cells {
		if cd.layers != nil {
			presence[i/8] |= 1 << (i % 8)
		}
	}
	rec.raw(presence)
	for _, cd := range cells {
		if cd.layers == nil {
			continue
		}
		rec.uvarint(uint64(len(cd.layers)))
		for _, sd := range cd.layers {
			rec.uvarint(uint64(table.add(canonicalBlob(sd, blockRemap))))
		}
	}
	// Canonical order: the caller's slice order must not reach the file.
	bes := slices.Clone(s.BlockEntities)
	slices.SortStableFunc(bes, func(a, b StructureBlockEntity) int {
		if v := cmp.Compare(a.Pos[1], b.Pos[1]); v != 0 {
			return v
		}
		if v := cmp.Compare(a.Pos[2], b.Pos[2]); v != 0 {
			return v
		}
		if v := cmp.Compare(a.Pos[0], b.Pos[0]); v != 0 {
			return v
		}
		return compareNBT(a.Data, b.Data)
	})
	ents := slices.Clone(s.Entities)
	slices.SortStableFunc(ents, compareNBT)

	rec.uvarint(uint64(len(bes)))
	for _, be := range bes {
		data := make(map[string]any, len(be.Data))
		for k, v := range be.Data {
			if k == "x" || k == "y" || k == "z" {
				continue
			}
			data[k] = v
		}
		blob, err := marshalNBT(data)
		if err != nil {
			return fmt.Errorf("pile: block entity at %v: %w", be.Pos, err)
		}
		if err := checkBlob(blob, fmt.Sprintf("block entity NBT at %v", be.Pos)); err != nil {
			return err
		}
		for i := range 3 {
			rec.uvarint(uint64(be.Pos[i]))
		}
		rec.blob(blob)
	}
	rec.uvarint(uint64(len(ents)))
	for i, e := range ents {
		blob, err := marshalNBT(e)
		if err != nil {
			return fmt.Errorf("pile: entity %d: %w", i, err)
		}
		if err := checkBlob(blob, fmt.Sprintf("entity %d NBT", i)); err != nil {
			return err
		}
		rec.blob(blob)
	}

	// Assemble body: meta (user data only), block palette, empty biome
	// palette, blob table, record.
	body := &writer{}
	body.blob(nil) // settings
	body.blob(s.UserData)
	body.blob(nil) // markers
	body.blob(nil) // border
	body.raw(blockPalBytes)
	body.uvarint(0) // biome palette: structures store no biomes
	table.encode(body)
	body.raw(rec.bytes())

	flags := uint32(0)
	stored := body.bytes()
	if opts.Compression == CompressionNone {
		flags |= FlagUncompressed
	} else {
		enc, err := zstd.NewWriter(nil,
			zstd.WithEncoderLevel(zstdLevel(opts.Compression)),
			zstd.WithEncoderConcurrency(1))
		if err != nil {
			return fmt.Errorf("pile: create zstd encoder: %w", err)
		}
		stored = enc.EncodeAll(stored, make([]byte, 0, len(stored)/4))
		_ = enc.Close()
	}

	hdr := &writer{}
	hdr.raw(headerMagic[:])
	hdr.u16(Version)
	hdr.u8(KindStructure)
	hdr.u8(ModeSolid)
	hdr.u32(flags)
	hdr.i32(chunk.CurrentBlockVersion)

	tail := &writer{}
	tail.u64(0) // directory offset
	tail.u64(0) // directory length
	tail.u64(0) // generation
	tail.u64(0) // previous footer offset
	tail.raw(footerMagic[:])
	ftr := &writer{}
	ftr.u64(checkpointHash(hdr.bytes(), stored, tail.bytes()))
	ftr.raw(tail.bytes())

	for _, part := range [][]byte{hdr.bytes(), stored, ftr.bytes()} {
		if _, err := out.Write(part); err != nil {
			return fmt.Errorf("pile: write: %w", err)
		}
	}
	return nil
}

// ReadStructure decodes a structure file.
func ReadStructure(file []byte, reg world.BlockRegistry) (*StructureData, error) {
	h, stored, err := parseFrame(file)
	if err != nil {
		return nil, err
	}
	if h.kind != KindStructure {
		return nil, corruptf("file kind %d is not a structure", h.kind)
	}
	// A structure carries no light, stats, biomes or world metadata, so the
	// corresponding flags would be meaningless: reject them rather than
	// ignore them, or the same structure would have several valid encodings.
	if h.flags&^FlagUncompressed != 0 {
		return nil, corruptf("flags 0x%08X are not valid for a structure", h.flags)
	}
	body, err := decompressBody(h, stored)
	if err != nil {
		return nil, err
	}
	r := &reader{b: body}
	settings, userData, markers, border, _, err := readMetaBlobs(r, h.flags)
	if err != nil {
		return nil, err
	}
	if len(settings) != 0 || len(markers) != 0 || len(border) != 0 {
		return nil, corruptf("structure metadata must contain only user data")
	}
	rids, unknown, unkStates, err := decodeBlockPalette(r, reg, h.blockVersion)
	if err != nil {
		return nil, err
	}
	if _, _, _, err := decodeBiomePalette(r); err != nil {
		return nil, err
	}
	blobs, err := decodeBlobTable(r)
	if err != nil {
		return nil, err
	}

	var size [3]int32
	for i := range 3 {
		v, err := r.uvarint()
		if err != nil {
			return nil, err
		}
		if v == 0 || v > maxStructureSize {
			return nil, corruptf("invalid structure size component %d", v)
		}
		size[i] = int32(v)
	}
	s, err := NewStructureData(size)
	if err != nil {
		return nil, corruptf("%v", err)
	}
	s.UserData = cloneBytes(userData)
	for i := range 3 {
		v, err := r.svarint()
		if err != nil {
			return nil, err
		}
		s.Origin[i] = int32(v)
	}

	air := reg.AirRuntimeID()
	presence, err := r.take((len(s.Cells) + 7) / 8)
	if err != nil {
		return nil, err
	}
	for i := range s.Cells {
		if presence[i/8]&(1<<(i%8)) == 0 {
			continue
		}
		layerN, err := r.count(maxLayers, "layer")
		if err != nil {
			return nil, err
		}
		sub := chunk.NewSubChunk(air)
		for l := range layerN {
			ref, err := r.uvarint()
			if err != nil {
				return nil, err
			}
			if int(ref) >= len(blobs) {
				return nil, corruptf("cell blob reference %d out of range", ref)
			}
			report := func(idx uint16, state uint32) {
				s.Unknown = append(s.Unknown, UnknownBlock{
					Section: int32(i), Layer: uint8(l), Index: idx, State: state,
				})
			}
			if err := applyBlockBlob(sub, uint8(l), blobs[ref], rids, air, unknown, report); err != nil {
				return nil, err
			}
		}
		s.Cells[i] = sub
	}

	beN, err := r.count(maxPerChunk, "block entity")
	if err != nil {
		return nil, err
	}
	for range beN {
		var pos [3]int32
		for i := range 3 {
			v, err := r.uvarint()
			if err != nil {
				return nil, err
			}
			// Positions are structure-local: reject anything outside the
			// declared box (which also rules out the int32 conversion
			// wrapping).
			if v >= uint64(size[i]) {
				return nil, corruptf("block entity coordinate %d outside structure size %v", v, size)
			}
			pos[i] = int32(v)
		}
		blob, err := r.blob()
		if err != nil {
			return nil, err
		}
		data, err := unmarshalNBT(blob)
		if err != nil {
			return nil, err
		}
		s.BlockEntities = append(s.BlockEntities, StructureBlockEntity{Pos: pos, Data: data})
	}
	entN, err := r.count(maxPerChunk, "entity")
	if err != nil {
		return nil, err
	}
	for range entN {
		blob, err := r.blob()
		if err != nil {
			return nil, err
		}
		data, err := unmarshalNBT(blob)
		if err != nil {
			return nil, err
		}
		s.Entities = append(s.Entities, data)
	}
	if r.remaining() != 0 {
		return nil, corruptf("%d trailing bytes after structure record", r.remaining())
	}
	if len(s.Unknown) > 0 {
		s.UnknownStates = unkStates
	}
	return s, nil
}
