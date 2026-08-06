package format

import (
	"fmt"
	"math"
	"reflect"
	"unsafe"

	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
)

// This file contains unsafe mirrors of dragonfly's chunk-internal types,
// giving the codec direct access to packed palette indices, biome storages
// and light arrays. Every mirror is verified against the real struct layout
// at package init: a dragonfly upgrade that changes a layout panics
// immediately at startup instead of corrupting worlds.

// paletteMirror mirrors chunk.Palette.
type paletteMirror struct {
	last      uint32
	lastIndex int16
	size      uint8
	values    []uint32
}

// storageMirror mirrors chunk.PalettedStorage.
type storageMirror struct {
	bitsPerIndex       uint16
	filledBitsPerIndex uint16
	indexMask          uint32
	indicesStart       unsafe.Pointer
	palette            *chunk.Palette
	indices            []uint32
}

// subChunkMirror mirrors chunk.SubChunk.
type subChunkMirror struct {
	air        uint32
	storages   []*chunk.PalettedStorage
	blockLight []uint8
	skyLight   []uint8
}

// chunkMirror mirrors chunk.Chunk.
type chunkMirror struct {
	r                    cube.Range
	br                   chunk.BlockRegistry
	air                  uint32
	recalculateHeightMap bool
	heightMap            []int16
	sub                  []*chunk.SubChunk
	biomes               []*chunk.PalettedStorage
}

func init() {
	assertLayout(reflect.TypeFor[chunk.Palette](), reflect.TypeFor[paletteMirror]())
	assertLayout(reflect.TypeFor[chunk.PalettedStorage](), reflect.TypeFor[storageMirror]())
	assertLayout(reflect.TypeFor[chunk.SubChunk](), reflect.TypeFor[subChunkMirror]())
	assertLayout(reflect.TypeFor[chunk.Chunk](), reflect.TypeFor[chunkMirror]())
}

// assertLayout panics unless the mirror matches the real type field for
// field: same count, offsets, sizes and kinds.
func assertLayout(real, mirror reflect.Type) {
	if real.Size() != mirror.Size() {
		panic(fmt.Sprintf("pile: unsafe mirror of %v: size %d != %d; dragonfly layout changed, update format/unsafe.go",
			real, mirror.Size(), real.Size()))
	}
	if real.NumField() != mirror.NumField() {
		panic(fmt.Sprintf("pile: unsafe mirror of %v: %d fields != %d; dragonfly layout changed, update format/unsafe.go",
			real, mirror.NumField(), real.NumField()))
	}
	for i := range real.NumField() {
		rf, mf := real.Field(i), mirror.Field(i)
		if rf.Offset != mf.Offset || rf.Type.Size() != mf.Type.Size() || rf.Type.Kind() != mf.Type.Kind() {
			panic(fmt.Sprintf("pile: unsafe mirror of %v: field %d (%s) offset/size/kind mismatch; dragonfly layout changed, update format/unsafe.go",
				real, i, rf.Name))
		}
	}
}

func mirrorStorage(s *chunk.PalettedStorage) *storageMirror {
	return (*storageMirror)(unsafe.Pointer(s))
}

func mirrorPalette(p *chunk.Palette) *paletteMirror {
	return (*paletteMirror)(unsafe.Pointer(p))
}

func mirrorSub(s *chunk.SubChunk) *subChunkMirror {
	return (*subChunkMirror)(unsafe.Pointer(s))
}

func mirrorChunk(c *chunk.Chunk) *chunkMirror {
	return (*chunkMirror)(unsafe.Pointer(c))
}

// storageRaw returns a storage's palette values, bits per index and packed
// index words without copying.
func storageRaw(s *chunk.PalettedStorage) (palette []uint32, bits uint16, words []uint32) {
	m := mirrorStorage(s)
	return mirrorPalette(m.palette).values, m.bitsPerIndex, m.indices
}

// unpackStorage decodes a storage's 4096 palette indices into out.
// A zero-bit storage leaves out all-zero.
func unpackStorage(bits uint16, words []uint32, out *[4096]uint16) {
	if bits == 0 {
		clear(out[:])
		return
	}
	filled := 32 / bits * bits
	mask := uint32(1)<<bits - 1
	for i := range uint16(4096) {
		offset := i * bits
		w := words[offset/filled]
		out[i] = uint16(w >> (offset % filled) & mask)
	}
}

// paletteSizes is dragonfly's palette size ladder (bits per index).
var paletteSizes = [...]uint16{0, 1, 2, 3, 4, 5, 6, 8, 16}

// storageSizeFor returns the bits per index for a palette of n values.
func storageSizeFor(n int) uint16 {
	for _, s := range paletteSizes {
		if n <= 1<<s {
			return s
		}
	}
	return 16
}

// storageWordCount returns the uint32 word count for a bits-per-index size,
// including dragonfly's padding for sizes 3, 5 and 6.
func storageWordCount(bits uint16) int {
	if bits == 0 {
		return 0
	}
	count := 4096 / int(32/bits)
	if bits == 3 || bits == 5 || bits == 6 {
		count++
	}
	return count
}

// makeStorage constructs a PalettedStorage equivalent to what dragonfly's own
// Set-based construction would compact to: the given palette values with the
// given 4096 indices. idx may be nil for a uniform (single-value) storage.
func makeStorage(palette []uint32, idx *[4096]uint16) *chunk.PalettedStorage {
	bits := storageSizeFor(len(palette))
	words := make([]uint32, storageWordCount(bits))
	if bits > 0 && idx != nil {
		filled := 32 / bits * bits
		for i := range uint16(4096) {
			offset := i * bits
			words[offset/filled] |= uint32(idx[i]) << (offset % filled)
		}
	}

	p := new(chunk.Palette)
	pm := mirrorPalette(p)
	pm.last = math.MaxUint32
	pm.size = uint8(bits)
	pm.values = palette

	s := new(chunk.PalettedStorage)
	sm := mirrorStorage(s)
	sm.bitsPerIndex = bits
	if bits != 0 {
		sm.filledBitsPerIndex = 32 / bits * bits
	}
	sm.indexMask = uint32(1)<<bits - 1
	sm.indices = words
	sm.indicesStart = unsafe.Pointer(unsafe.SliceData(words))
	sm.palette = p
	return s
}

// subSetLayer places a storage at the given layer of a sub chunk, padding
// missing lower layers with empty (all-air) storages.
func subSetLayer(sub *chunk.SubChunk, layer int, s *chunk.PalettedStorage) {
	m := mirrorSub(sub)
	for len(m.storages) < layer {
		m.storages = append(m.storages, makeStorage([]uint32{m.air}, nil))
	}
	if len(m.storages) == layer {
		m.storages = append(m.storages, s)
	} else {
		m.storages[layer] = s
	}
}

// chunkSetBiomes replaces the biome storage of one section.
func chunkSetBiomes(c *chunk.Chunk, section int, s *chunk.PalettedStorage) {
	mirrorChunk(c).biomes[section] = s
}

// chunkBiomeStorage returns the biome storage of one section.
func chunkBiomeStorage(c *chunk.Chunk, section int) *chunk.PalettedStorage {
	return mirrorChunk(c).biomes[section]
}

// chunkAir returns the chunk's air runtime ID.
func chunkAir(c *chunk.Chunk) uint32 {
	return mirrorChunk(c).air
}

// subLight returns a sub chunk's block light and sky light nibble arrays
// (2048 bytes each), or nil when unset.
func subLight(sub *chunk.SubChunk) (blockLight, skyLight []uint8) {
	m := mirrorSub(sub)
	return m.blockLight, m.skyLight
}

// subSetLight sets a sub chunk's light nibble arrays. Slices must be 2048
// bytes (or nil to leave unset).
func subSetLight(sub *chunk.SubChunk, blockLight, skyLight []uint8) {
	m := mirrorSub(sub)
	if blockLight != nil {
		m.blockLight = blockLight
	}
	if skyLight != nil {
		m.skyLight = skyLight
	}
}

// AdaptRange returns a chunk equivalent to ch re-based onto rng: overlapping
// sections (blocks, biomes, light) are carried over by reference and sections
// outside the new range are dropped. Returns ch unchanged when the range
// already matches. Needed because dragonfly indexes sub chunks by the
// dimension's range without bounds checks: a chunk decoded with a mismatched
// range would panic the server on first block access outside it.
func AdaptRange(ch *chunk.Chunk, reg world.BlockRegistry, rng cube.Range) *chunk.Chunk {
	if ch.Range() == rng {
		return ch
	}
	out := chunk.New(reg, rng)
	om, nm := mirrorChunk(ch), mirrorChunk(out)
	oldMin, newMin := ch.Range()[0]>>4, rng[0]>>4
	for i := range om.sub {
		j := i + oldMin - newMin
		if j >= 0 && j < len(nm.sub) {
			nm.sub[j] = om.sub[i]
			nm.biomes[j] = om.biomes[i]
		}
	}
	return out
}
