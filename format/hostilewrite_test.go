package format

// The hostile-input matrix for the *writers*.
//
// `format/hostile_test.go` is the reader half: bytes cut at every boundary and
// counts at 0, 1, max and max+1, driven through the four decoders. This file is
// the other half, and it is a different problem. The inputs here are Go values,
// not bytes — a WorldData or a StructureData a caller can construct and a
// decoder would never produce — so there is no truncation axis and no count
// field to walk. What there is instead is §8's requirement that a writer refuse
// content its own reader would reject, which was found unimplemented for the
// NBT container budget one pass ago and had no systematic check anywhere else.
//
// The finding class that matters most, and that TestHostileWriteShapes exists
// to catch generically, is therefore: **a writer emitting a file its own reader
// refuses.** Its close relatives are a writer that silently changes the content
// it was handed and reports success, and a write that costs far more than the
// value it was given — the latter reachable from ContentHash, which decodes and
// re-encodes and is therefore an ordinary-tooling entry point for
// attacker-controlled bytes.
//
// Five of those were live when this file was written. Each has a named test
// below with its measured before:
//
//  1. A block entity or scheduled update outside the column it is stored in.
//     X and Z are folded into one packed nibble pair, so (100,0,200) in chunk
//     (0,0) was written and read back as (4,0,8); two positions 16 apart folded
//     onto one and the reader then refused the file. Y was not folded but not
//     bounded either, and the reader requires it inside the record's span.
//  2. A structure whose cell grid passes §8's ceiling while every axis is legal.
//  3. A block runtime ID the registry does not know, stored as air.
//  4. The sidecar-layer scan, quadratic in cells x preserved states: 125 s of
//     CPU inside ContentHash on a 155-byte file.
//  5. A nil *WorldData or *StructureData, which panicked.
//
// Nothing here may change which files a *reader* accepts, and nothing here may
// move a byte any writer produces for content it already accepts: the goldens
// and both vector suites are the check on both, and they were green throughout.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
)

// ---------------------------------------------------------------------------
// Shape construction
// ---------------------------------------------------------------------------

// hwColumn returns a column at (x,z) holding one non-air block, so that
// something is present for a shape to be attached to.
func hwColumn(t testing.TB, reg world.BlockRegistry, x, z int32, r cube.Range) Column {
	t.Helper()
	ch := chunk.New(reg, r)
	stone, ok := reg.StateToRuntimeID("minecraft:stone", map[string]any{})
	if !ok {
		t.Fatal("no stone in the registry")
	}
	ch.SetBlock(0, int16(r[0]), 0, 0, stone)
	return Column{X: x, Z: z, Col: &chunk.Column{Chunk: ch}}
}

// hwFillPlains sets every biome in the chunk to the fallback the decoder
// substitutes for a name it cannot resolve, which is the only place a preserved
// biome name is re-emitted.
func hwFillPlains(ch *chunk.Chunk) {
	plains := fallbackBiomeID()
	r := ch.Range()
	for x := range uint8(16) {
		for z := range uint8(16) {
			for y := int16(r[0]); y <= int16(r[1]); y++ {
				ch.SetBiome(x, y, z, plains)
			}
		}
	}
}

// hwStructure returns a structure of the given size with one non-air cell.
func hwStructure(t testing.TB, reg world.BlockRegistry, size [3]int32) *StructureData {
	t.Helper()
	s, err := NewStructureData(size)
	if err != nil {
		t.Fatal(err)
	}
	stone, _ := reg.StateToRuntimeID("minecraft:stone", map[string]any{})
	cell := chunk.NewSubChunk(reg.AirRuntimeID())
	cell.SetBlock(0, 0, 0, 0, stone)
	s.Cells[0] = cell
	return s
}

// ---------------------------------------------------------------------------
// The generic engine
// ---------------------------------------------------------------------------

// hwWorldShape is one hostile world a caller can construct. build returns a
// fresh value on every call, because a writer is allowed to consume it (and
// WriteStructure does).
type hwWorldShape struct {
	name  string
	build func(t *testing.T, reg world.BlockRegistry) *WorldData
	// mustRefuse is set where the shape is one the writer is now required to
	// reject rather than merely required not to mis-encode.
	mustRefuse bool
}

type hwStructShape struct {
	name       string
	build      func(t *testing.T, reg world.BlockRegistry) *StructureData
	mustRefuse bool
}

// TestHostileWriteShapes drives WriteWorld over values a decoder would never
// produce. It is the generic net: any shape that reaches the wire and comes
// back wrong fails here whether or not anyone thought to name the rule.
func TestHostileWriteShapes(t *testing.T) {
	reg := testRegistry(t)
	small := cube.Range{0, 15}
	shapes := []hwWorldShape{
		{name: "empty world", build: func(*testing.T, world.BlockRegistry) *WorldData {
			return &WorldData{}
		}},
		{name: "column with no chunk", mustRefuse: true, build: func(*testing.T, world.BlockRegistry) *WorldData {
			return &WorldData{Columns: []Column{{X: 1, Z: 2}}}
		}},
		{name: "nil chunk inside a column", mustRefuse: true, build: func(*testing.T, world.BlockRegistry) *WorldData {
			return &WorldData{Columns: []Column{{X: 0, Z: 0, Col: &chunk.Column{}}}}
		}},
		{name: "two columns at one position", mustRefuse: true, build: func(t *testing.T, reg world.BlockRegistry) *WorldData {
			return &WorldData{Columns: []Column{
				hwColumn(t, reg, 4, 4, small), hwColumn(t, reg, 4, 4, small),
			}}
		}},
		{name: "unaligned vertical range", mustRefuse: true, build: func(t *testing.T, reg world.BlockRegistry) *WorldData {
			ch := chunk.New(reg, cube.Range{0, 20})
			return &WorldData{Columns: []Column{{X: 0, Z: 0, Col: &chunk.Column{Chunk: ch}}}}
		}},
		{name: "chunk positions at both int32 extremes", build: func(t *testing.T, reg world.BlockRegistry) *WorldData {
			var d WorldData
			for _, p := range [][2]int32{
				{-1 << 31, -1 << 31}, {-1 << 31, 1<<31 - 1},
				{1<<31 - 1, -1 << 31}, {1<<31 - 1, 1<<31 - 1},
			} {
				d.Columns = append(d.Columns, hwColumn(t, reg, p[0], p[1], small))
			}
			return &d
		}},
		{name: "the full int16 block-Y domain, 4096 sections", build: func(t *testing.T, reg world.BlockRegistry) *WorldData {
			return &WorldData{Columns: []Column{hwColumn(t, reg, 0, 0, cube.Range{-32768, 32767})}}
		}},
		{name: "one section past the block-Y domain", mustRefuse: true, build: func(t *testing.T, reg world.BlockRegistry) *WorldData {
			ch := chunk.New(reg, cube.Range{-32768 - 16, 32767})
			return &WorldData{Columns: []Column{{X: 0, Z: 0, Col: &chunk.Column{Chunk: ch}}}}
		}},
		{name: "255 layers, the limit", build: func(t *testing.T, reg world.BlockRegistry) *WorldData {
			c := hwColumn(t, reg, 0, 0, small)
			stone, _ := reg.StateToRuntimeID("minecraft:stone", map[string]any{})
			for l := range 255 {
				c.Col.Chunk.Sub()[0].SetBlock(0, 0, 0, uint8(l), stone)
			}
			return &WorldData{Columns: []Column{c}}
		}},
		{name: "a trailing all-air layer above a populated one", build: func(t *testing.T, reg world.BlockRegistry) *WorldData {
			// Not a rule the caller can violate directly: the writer drops the
			// layer. It is here because it is the shape that goes wrong when it
			// stops doing so, and the engine's "its own reader accepts it" half
			// is what catches that.
			c := hwColumn(t, reg, 0, 0, small)
			c.Col.Chunk.Sub()[0].SetBlock(0, 0, 0, 1, reg.AirRuntimeID())
			c.Col.Chunk.Sub()[0].SetBlock(0, 0, 0, 2, reg.AirRuntimeID())
			return &WorldData{Columns: []Column{c}}
		}},
		{name: "an all-air layer under a populated one", build: func(t *testing.T, reg world.BlockRegistry) *WorldData {
			c := hwColumn(t, reg, 0, 0, small)
			water, _ := reg.StateToRuntimeID("minecraft:water", map[string]any{"liquid_depth": int32(0)})
			c.Col.Chunk.Sub()[0].SetBlock(0, 0, 0, 0, reg.AirRuntimeID())
			c.Col.Chunk.Sub()[0].SetBlock(0, 0, 0, 1, water)
			return &WorldData{Columns: []Column{c}}
		}},
		{name: "block entity 16 blocks outside the column", mustRefuse: true, build: func(t *testing.T, reg world.BlockRegistry) *WorldData {
			c := hwColumn(t, reg, 0, 0, small)
			c.Col.BlockEntities = []chunk.BlockEntity{
				{Pos: cube.Pos{0, 0, 0}, Data: map[string]any{"id": "minecraft:chest"}},
				{Pos: cube.Pos{16, 0, 0}, Data: map[string]any{"id": "minecraft:furnace"}},
			}
			return &WorldData{Columns: []Column{c}}
		}},
		{name: "block entity above the chunk's range", mustRefuse: true, build: func(t *testing.T, reg world.BlockRegistry) *WorldData {
			c := hwColumn(t, reg, 0, 0, small)
			c.Col.BlockEntities = []chunk.BlockEntity{
				{Pos: cube.Pos{0, 1000, 0}, Data: map[string]any{"id": "minecraft:chest"}},
			}
			return &WorldData{Columns: []Column{c}}
		}},
		{name: "scheduled update below the chunk's range", mustRefuse: true, build: func(t *testing.T, reg world.BlockRegistry) *WorldData {
			c := hwColumn(t, reg, 0, 0, small)
			stone, _ := reg.StateToRuntimeID("minecraft:stone", map[string]any{})
			c.Col.ScheduledBlocks = []chunk.ScheduledBlockUpdate{
				{Pos: cube.Pos{0, -500, 0}, Block: stone, Tick: 1},
			}
			return &WorldData{Columns: []Column{c}}
		}},
		{name: "two block entities at one position", mustRefuse: true, build: func(t *testing.T, reg world.BlockRegistry) *WorldData {
			c := hwColumn(t, reg, 0, 0, small)
			c.Col.BlockEntities = []chunk.BlockEntity{
				{Pos: cube.Pos{1, 1, 1}, Data: map[string]any{"id": "a"}},
				{Pos: cube.Pos{1, 1, 1}, Data: map[string]any{"id": "b"}},
			}
			return &WorldData{Columns: []Column{c}}
		}},
		{name: "unregistered block runtime ID", mustRefuse: true, build: func(t *testing.T, reg world.BlockRegistry) *WorldData {
			c := hwColumn(t, reg, 0, 0, small)
			c.Col.Chunk.SetBlock(1, 0, 0, 0, 1<<24)
			return &WorldData{Columns: []Column{c}}
		}},
		{name: "unregistered runtime ID on a scheduled update", mustRefuse: true, build: func(t *testing.T, reg world.BlockRegistry) *WorldData {
			c := hwColumn(t, reg, 0, 0, small)
			c.Col.ScheduledBlocks = []chunk.ScheduledBlockUpdate{
				{Pos: cube.Pos{0, 0, 0}, Block: reg.AirRuntimeID(), Tick: 5},
				{Pos: cube.Pos{0, 0, 0}, Block: 1 << 24, Tick: 5},
			}
			return &WorldData{Columns: []Column{c}}
		}},
		{name: "one block entity past the per-chunk limit", mustRefuse: true, build: func(t *testing.T, reg world.BlockRegistry) *WorldData {
			c := hwColumn(t, reg, 0, 0, small)
			c.Col.BlockEntities = make([]chunk.BlockEntity, maxPerChunk+1)
			return &WorldData{Columns: []Column{c}}
		}},
		{name: "one entity past the per-chunk limit", mustRefuse: true, build: func(t *testing.T, reg world.BlockRegistry) *WorldData {
			c := hwColumn(t, reg, 0, 0, small)
			c.Col.Entities = make([]chunk.Entity, maxPerChunk+1)
			return &WorldData{Columns: []Column{c}}
		}},
		{name: "one scheduled update past the per-chunk limit", mustRefuse: true, build: func(t *testing.T, reg world.BlockRegistry) *WorldData {
			c := hwColumn(t, reg, 0, 0, small)
			c.Col.ScheduledBlocks = make([]chunk.ScheduledBlockUpdate, maxPerChunk+1)
			return &WorldData{Columns: []Column{c}}
		}},
		{name: "two preserved states that collide after version normalisation", build: func(t *testing.T, reg world.BlockRegistry) *WorldData {
			// Version 0 means "the palette's own version" and an explicit
			// current version means the same thing, so these are one entry.
			// A writer that kept both would put two indistinguishable entries in
			// the palette, which the reader refuses as a duplicate.
			c := hwColumn(t, reg, 0, 0, small)
			c.Col.Chunk.Sub()[0].SetBlock(0, 0, 0, 0, placeholderRid(reg))
			c.Col.Chunk.Sub()[0].SetBlock(1, 0, 0, 0, placeholderRid(reg))
			c.Unknown = []UnknownBlock{
				{Section: 0, Layer: 0, Index: 0, State: 0},
				{Section: 0, Layer: 0, Index: 1 << 8, State: 1},
			}
			c.UnknownStates = []BlockState{
				{Name: "pile:twinned", Version: 0},
				{Name: "pile:twinned", Version: chunk.CurrentBlockVersion},
			}
			return &WorldData{Columns: []Column{c}}
		}},
		{name: "entities that tie on ID and on bytes", build: func(t *testing.T, reg world.BlockRegistry) *WorldData {
			c := hwColumn(t, reg, 0, 0, small)
			for range 4 {
				c.Col.Entities = append(c.Col.Entities, chunk.Entity{
					ID: 7, Data: map[string]any{"identifier": "minecraft:pig"},
				})
			}
			return &WorldData{Columns: []Column{c}}
		}},
		{name: "sidecar naming a state the column does not carry", build: func(t *testing.T, reg world.BlockRegistry) *WorldData {
			c := hwColumn(t, reg, 0, 0, small)
			c.Unknown = []UnknownBlock{{Section: 0, Layer: 0, Index: 0, State: 99}}
			return &WorldData{Columns: []Column{c}}
		}},
		{name: "sidecar naming a section the column does not have", build: func(t *testing.T, reg world.BlockRegistry) *WorldData {
			c := hwColumn(t, reg, 0, 0, small)
			c.Unknown = []UnknownBlock{{Section: 1000, Layer: 0, Index: 0, State: 0}}
			c.UnknownStates = []BlockState{{Name: "pile:absent"}}
			return &WorldData{Columns: []Column{c}}
		}},
		{name: "sidecar reaching layer 254 of a section with one storage", build: func(t *testing.T, reg world.BlockRegistry) *WorldData {
			c := hwColumn(t, reg, 0, 0, small)
			c.Col.Chunk.Sub()[0].SetBlock(0, 0, 0, 0, placeholderRid(reg))
			c.Unknown = []UnknownBlock{{Section: 0, Layer: 254, Index: 0, State: 0}}
			c.UnknownStates = []BlockState{{Name: "pile:deep_layer"}}
			return &WorldData{Columns: []Column{c}}
		}},
		{name: "sidecar reaching layer 255, one past the limit", mustRefuse: true, build: func(t *testing.T, reg world.BlockRegistry) *WorldData {
			c := hwColumn(t, reg, 0, 0, small)
			c.Col.Chunk.Sub()[0].SetBlock(0, 0, 0, 0, placeholderRid(reg))
			c.Unknown = []UnknownBlock{{Section: 0, Layer: 255, Index: 0, State: 0}}
			c.UnknownStates = []BlockState{{Name: "pile:too_deep"}}
			return &WorldData{Columns: []Column{c}}
		}},
		{name: "unknown biome name without a namespace", mustRefuse: true, build: func(t *testing.T, reg world.BlockRegistry) *WorldData {
			c := hwColumn(t, reg, 0, 0, small)
			// The sidecar re-emits a preserved name only where the fallback
			// biome is still in place, so the section has to be plains for the
			// entry to reach the palette at all.
			hwFillPlains(c.Col.Chunk)
			c.UnknownBiomes = []UnknownBlock{{Section: 0, Index: WholeStorage, State: 0}}
			c.UnknownBiomeNames = []string{"bare"}
			return &WorldData{Columns: []Column{c}}
		}},
		{name: "unknown biome sidecar naming a name it does not carry", mustRefuse: true, build: func(t *testing.T, reg world.BlockRegistry) *WorldData {
			c := hwColumn(t, reg, 0, 0, small)
			hwFillPlains(c.Col.Chunk)
			c.UnknownBiomes = []UnknownBlock{{Section: 0, Index: WholeStorage, State: 7}}
			return &WorldData{Columns: []Column{c}}
		}},
		{name: "preserved biome name carried through", build: func(t *testing.T, reg world.BlockRegistry) *WorldData {
			c := hwColumn(t, reg, 0, 0, small)
			hwFillPlains(c.Col.Chunk)
			c.UnknownBiomes = []UnknownBlock{{Section: 0, Index: WholeStorage, State: 0}}
			c.UnknownBiomeNames = []string{"pile:unheard_of"}
			return &WorldData{Columns: []Column{c}}
		}},
		{name: "block state name that is not valid UTF-8", mustRefuse: true, build: func(t *testing.T, reg world.BlockRegistry) *WorldData {
			c := hwColumn(t, reg, 0, 0, small)
			c.Col.Chunk.Sub()[0].SetBlock(1, 0, 0, 0, placeholderRid(reg))
			c.Unknown = []UnknownBlock{{Section: 0, Layer: 0, Index: 1 << 8, State: 0}}
			c.UnknownStates = []BlockState{{Name: "pile:\xff\xfe"}}
			return &WorldData{Columns: []Column{c}}
		}},
		{name: "block state with 65 properties, one past the limit", mustRefuse: true, build: func(t *testing.T, reg world.BlockRegistry) *WorldData {
			c := hwColumn(t, reg, 0, 0, small)
			c.Col.Chunk.Sub()[0].SetBlock(1, 0, 0, 0, placeholderRid(reg))
			props := map[string]any{}
			for i := range 65 {
				props[fmt.Sprintf("p%03d", i)] = int32(i)
			}
			c.Unknown = []UnknownBlock{{Section: 0, Layer: 0, Index: 1 << 8, State: 0}}
			c.UnknownStates = []BlockState{{Name: "pile:wide", Properties: props}}
			return &WorldData{Columns: []Column{c}}
		}},
		{name: "settings blob that is not NBT", mustRefuse: true, build: func(*testing.T, world.BlockRegistry) *WorldData {
			return &WorldData{Settings: []byte{0xFF, 0xFF, 0xFF}}
		}},
		{name: "border blob with a list where an int array belongs", mustRefuse: true, build: func(t *testing.T, reg world.BlockRegistry) *WorldData {
			return &WorldData{Border: mustNBT(t, map[string]any{"min": []int32{-8, -8}})}
		}},
		{name: "markers out of order", mustRefuse: true, build: func(t *testing.T, reg world.BlockRegistry) *WorldData {
			return &WorldData{Markers: mustNBT(t, map[string]any{"markers": []map[string]any{
				{"name": "b", "kind": "spawn", "pos": []float64{0, 0, 0}},
				{"name": "a", "kind": "spawn", "pos": []float64{0, 0, 0}},
			}})}
		}},
		{name: "user data one byte past the blob limit", mustRefuse: true, build: func(*testing.T, world.BlockRegistry) *WorldData {
			return &WorldData{UserData: make([]byte, maxBlobLen+1)}
		}},
	}
	for _, sh := range shapes {
		t.Run(sh.name, func(t *testing.T) {
			d := sh.build(t, reg)
			var first bytes.Buffer
			err := hwRecover(func() error {
				return WriteWorld(&first, d, reg, Options{Compression: CompressionNone})
			})
			if err != nil {
				if strings.HasPrefix(err.Error(), "runtime error") {
					t.Fatalf("refused by the runtime, not by the writer: %v", err)
				}
				return
			}
			if sh.mustRefuse {
				t.Fatalf("the writer accepted a shape it is required to refuse (%d bytes)", first.Len())
			}
			hwFixedPoint(t, reg, first.Bytes())
		})
	}
}

// hwFixedPoint is the property every accepted shape is held to: the file the
// writer emitted must be one its own reader accepts, and re-encoding what that
// reader produced must not move a byte. A writer that emits a readable file
// which then re-encodes differently has two encodings of one content, which is
// what the canonical form forbids, and it is also what would make ContentHash
// report two different worlds depending on how many times a file had been
// rewritten.
func hwFixedPoint(t *testing.T, reg world.BlockRegistry, first []byte) {
	t.Helper()
	d, err := ReadWorld(first, reg)
	if err != nil {
		t.Fatalf("the writer emitted a file its own reader refuses: %v", err)
	}
	var second bytes.Buffer
	if err := WriteWorld(&second, d, reg, Options{Compression: CompressionNone}); err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	if !bytes.Equal(first, second.Bytes()) {
		t.Fatalf("write -> read -> write is not a fixed point: %d then %d bytes", len(first), second.Len())
	}
	again, err := ReadWorld(second.Bytes(), reg)
	if err != nil {
		t.Fatalf("re-encoded file does not decode: %v", err)
	}
	var third bytes.Buffer
	if err := WriteWorld(&third, again, reg, Options{Compression: CompressionNone}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(second.Bytes(), third.Bytes()) {
		t.Fatal("encoding oscillates rather than converging")
	}
}

// hwRecover turns a panic into an error so the engine can report which shape
// produced it rather than losing the table position in a stack trace.
func hwRecover(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("runtime error: %v", r)
		}
	}()
	return fn()
}

// TestHostileWriteStructureShapes is the same engine over WriteStructure.
func TestHostileWriteStructureShapes(t *testing.T) {
	reg := testRegistry(t)
	shapes := []hwStructShape{
		{name: "zero size", mustRefuse: true, build: func(*testing.T, world.BlockRegistry) *StructureData {
			return &StructureData{}
		}},
		{name: "negative size", mustRefuse: true, build: func(*testing.T, world.BlockRegistry) *StructureData {
			return &StructureData{Size: [3]int32{-1, 8, 8}, Cells: make([]*chunk.SubChunk, 1)}
		}},
		{name: "size at the axis ceiling", build: func(t *testing.T, reg world.BlockRegistry) *StructureData {
			return hwStructure(t, reg, [3]int32{maxStructureSize, 16, 16})
		}},
		{name: "one axis past the ceiling", mustRefuse: true, build: func(*testing.T, world.BlockRegistry) *StructureData {
			return &StructureData{Size: [3]int32{maxStructureSize + 1, 16, 16}}
		}},
		{name: "cell grid one row past the ceiling", mustRefuse: true, build: func(*testing.T, world.BlockRegistry) *StructureData {
			size := [3]int32{maxStructureSize, 272, 16}
			nx, ny, nz := CellDims(size)
			return &StructureData{Size: size, Cells: make([]*chunk.SubChunk, int64(nx)*int64(ny)*int64(nz))}
		}},
		{name: "cell count disagreeing with the size", mustRefuse: true, build: func(*testing.T, world.BlockRegistry) *StructureData {
			return &StructureData{Size: [3]int32{16, 16, 16}, Cells: make([]*chunk.SubChunk, 5)}
		}},
		{name: "no cells at all", mustRefuse: true, build: func(*testing.T, world.BlockRegistry) *StructureData {
			return &StructureData{Size: [3]int32{16, 16, 16}}
		}},
		{name: "a 1x1x1 box whose cell is full of padding", build: func(t *testing.T, reg world.BlockRegistry) *StructureData {
			s, err := NewStructureData([3]int32{1, 1, 1})
			if err != nil {
				t.Fatal(err)
			}
			stone, _ := reg.StateToRuntimeID("minecraft:stone", map[string]any{})
			cell := chunk.NewSubChunk(reg.AirRuntimeID())
			for x := range uint8(16) {
				for y := range uint8(16) {
					for z := range uint8(16) {
						cell.SetBlock(x, y, z, 0, stone)
					}
				}
			}
			s.Cells[0] = cell
			return s
		}},
		{name: "block entity outside the declared box", mustRefuse: true, build: func(t *testing.T, reg world.BlockRegistry) *StructureData {
			s := hwStructure(t, reg, [3]int32{16, 16, 16})
			s.BlockEntities = []StructureBlockEntity{{Pos: [3]int32{16, 0, 0}, Data: map[string]any{"id": "a"}}}
			return s
		}},
		{name: "block entity at a negative position", mustRefuse: true, build: func(t *testing.T, reg world.BlockRegistry) *StructureData {
			s := hwStructure(t, reg, [3]int32{16, 16, 16})
			s.BlockEntities = []StructureBlockEntity{{Pos: [3]int32{-1, 0, 0}, Data: map[string]any{"id": "a"}}}
			return s
		}},
		{name: "two block entities at one position", mustRefuse: true, build: func(t *testing.T, reg world.BlockRegistry) *StructureData {
			s := hwStructure(t, reg, [3]int32{16, 16, 16})
			s.BlockEntities = []StructureBlockEntity{
				{Pos: [3]int32{1, 1, 1}, Data: map[string]any{"id": "a"}},
				{Pos: [3]int32{1, 1, 1}, Data: map[string]any{"id": "b"}},
			}
			return s
		}},
		{name: "entities in the caller's order", build: func(t *testing.T, reg world.BlockRegistry) *StructureData {
			s := hwStructure(t, reg, [3]int32{16, 16, 16})
			s.Entities = []map[string]any{{"b": int32(2)}, {"a": int32(1)}}
			return s
		}},
		{name: "unregistered block runtime ID in a cell", mustRefuse: true, build: func(t *testing.T, reg world.BlockRegistry) *StructureData {
			s := hwStructure(t, reg, [3]int32{16, 16, 16})
			s.Cells[0].SetBlock(1, 0, 0, 0, 1<<24)
			return s
		}},
		{name: "whole-storage sidecar on an edge cell", build: func(t *testing.T, reg world.BlockRegistry) *StructureData {
			s, err := NewStructureData([3]int32{1, 1, 1})
			if err != nil {
				t.Fatal(err)
			}
			cell := chunk.NewSubChunk(reg.AirRuntimeID())
			cell.SetBlock(0, 0, 0, 0, placeholderRid(reg))
			s.Cells[0] = cell
			s.Unknown = []UnknownBlock{{Section: 0, Layer: 0, Index: WholeStorage, State: 0}}
			s.UnknownStates = []BlockState{{Name: "pile:edge"}}
			return s
		}},
		{name: "sidecar naming a cell that does not exist", build: func(t *testing.T, reg world.BlockRegistry) *StructureData {
			s := hwStructure(t, reg, [3]int32{16, 16, 16})
			s.Unknown = []UnknownBlock{{Section: 5000, Layer: 0, Index: 0, State: 0}}
			s.UnknownStates = []BlockState{{Name: "pile:absent"}}
			return s
		}},
		{name: "user data one byte past the blob limit", mustRefuse: true, build: func(t *testing.T, reg world.BlockRegistry) *StructureData {
			s := hwStructure(t, reg, [3]int32{16, 16, 16})
			s.UserData = make([]byte, maxBlobLen+1)
			return s
		}},
	}
	for _, sh := range shapes {
		t.Run(sh.name, func(t *testing.T) {
			s := sh.build(t, reg)
			var first bytes.Buffer
			err := hwRecover(func() error {
				return WriteStructure(&first, s, reg, Options{Compression: CompressionNone})
			})
			if err != nil {
				if strings.HasPrefix(err.Error(), "runtime error") {
					t.Fatalf("refused by the runtime, not by the writer: %v", err)
				}
				return
			}
			if sh.mustRefuse {
				t.Fatalf("the writer accepted a shape it is required to refuse (%d bytes)", first.Len())
			}
			back, err := ReadStructure(first.Bytes(), reg)
			if err != nil {
				t.Fatalf("the writer emitted %d bytes its own reader refuses: %v", first.Len(), err)
			}
			var second bytes.Buffer
			if err := WriteStructure(&second, back, reg, Options{Compression: CompressionNone}); err != nil {
				t.Fatalf("re-encode: %v", err)
			}
			if !bytes.Equal(first.Bytes(), second.Bytes()) {
				t.Fatalf("write -> read -> write is not a fixed point: %d then %d bytes",
					first.Len(), second.Len())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The named findings
// ---------------------------------------------------------------------------

// TestHostileWriteRejectsPositionsOutsideTheColumn is finding 1. A record packs
// a block entity's x and z into one byte of nibbles, so the writer had no way
// to represent a position outside the column and folded it in instead. The
// three cases below are the three distinguishable consequences.
func TestHostileWriteRejectsPositionsOutsideTheColumn(t *testing.T) {
	reg := testRegistry(t)
	stone, _ := reg.StateToRuntimeID("minecraft:stone", map[string]any{})
	cases := []struct {
		name   string
		mutate func(c *Column)
		before string
	}{
		{
			name:   "X outside the footprint is silently relocated",
			before: "(100,0,200) in chunk (0,0) was written, and read back, as (4,0,8)",
			mutate: func(c *Column) {
				c.Col.BlockEntities = []chunk.BlockEntity{
					{Pos: cube.Pos{100, 0, 200}, Data: map[string]any{"id": "minecraft:chest"}},
				}
			},
		},
		{
			name:   "two positions 16 apart collide on the wire",
			before: "accepted, and ReadWorld then refused the file for repeating a position",
			mutate: func(c *Column) {
				c.Col.BlockEntities = []chunk.BlockEntity{
					{Pos: cube.Pos{0, 0, 0}, Data: map[string]any{"id": "a"}},
					{Pos: cube.Pos{16, 0, 0}, Data: map[string]any{"id": "b"}},
				}
			},
		},
		{
			name:   "a scheduled update outside the column",
			before: "accepted, and folded (0,0,32) into (0,0,0)",
			mutate: func(c *Column) {
				c.Col.ScheduledBlocks = []chunk.ScheduledBlockUpdate{
					{Pos: cube.Pos{0, 0, 32}, Block: stone, Tick: 1},
				}
			},
		},
		{
			name:   "a block entity above the chunk's range",
			before: "accepted, and ReadWorld refused: block entity at Y 1000 is outside the chunk's span 0..15",
			mutate: func(c *Column) {
				c.Col.BlockEntities = []chunk.BlockEntity{
					{Pos: cube.Pos{0, 1000, 0}, Data: map[string]any{"id": "a"}},
				}
			},
		},
		{
			name:   "a scheduled update below the chunk's range",
			before: "accepted, and ReadWorld refused: scheduled update at Y -500 is outside the chunk's span 0..15",
			mutate: func(c *Column) {
				c.Col.ScheduledBlocks = []chunk.ScheduledBlockUpdate{
					{Pos: cube.Pos{0, -500, 0}, Block: stone, Tick: 1},
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			col := hwColumn(t, reg, 0, 0, cube.Range{0, 15})
			c.mutate(&col)
			var buf bytes.Buffer
			err := WriteWorld(&buf, &WorldData{Columns: []Column{col}}, reg, Options{Compression: CompressionNone})
			if err == nil {
				t.Fatalf("accepted (%d bytes); before the fix: %s", buf.Len(), c.before)
			}
			// The same input must be refused by every writer, not only by the
			// solid one: Store shares validateColumn and Compact goes through it.
			path := filepath.Join(t.TempDir(), "w.pile")
			w, cerr := CreateIndexed(path, reg, Options{Compression: CompressionNone})
			if cerr != nil {
				t.Fatal(cerr)
			}
			if serr := w.Store(col); serr == nil {
				t.Fatal("IndexedWorld.Store accepted what WriteWorld refused")
			}
			_ = w.Close()
		})
	}
}

// TestHostileWriteRejectsUnknownRuntimeID is finding 3. A runtime ID the
// registry does not know used to be stored as minecraft:air, which lost the
// block without a word and, where the section held nothing else, produced a
// record whose only layer resolves to uniform air — which §4.3 says an absent
// section is, so ReadWorld refused it.
func TestHostileWriteRejectsUnknownRuntimeID(t *testing.T) {
	reg := testRegistry(t)
	const bogus = 1 << 24

	t.Run("world", func(t *testing.T) {
		c := hwColumn(t, reg, 0, 0, cube.Range{0, 15})
		c.Col.Chunk.SetBlock(1, 0, 0, 0, bogus)
		var buf bytes.Buffer
		if err := WriteWorld(&buf, &WorldData{Columns: []Column{c}}, reg, Options{Compression: CompressionNone}); err == nil {
			t.Fatalf("accepted (%d bytes); before the fix the block became air", buf.Len())
		}
	})
	t.Run("world, section holding only the unknown ID", func(t *testing.T) {
		ch := chunk.New(reg, cube.Range{0, 15})
		ch.SetBlock(0, 0, 0, 0, bogus)
		c := Column{X: 0, Z: 0, Col: &chunk.Column{Chunk: ch}}
		var buf bytes.Buffer
		if err := WriteWorld(&buf, &WorldData{Columns: []Column{c}}, reg, Options{Compression: CompressionNone}); err == nil {
			t.Fatalf("accepted (%d bytes); before the fix ReadWorld refused it: "+
				"section 0 ends in an all-air layer, so it is either absent or shorter", buf.Len())
		}
	})
	t.Run("structure", func(t *testing.T) {
		s := hwStructure(t, reg, [3]int32{16, 16, 16})
		s.Cells[0].SetBlock(1, 0, 0, 0, bogus)
		var buf bytes.Buffer
		if err := WriteStructure(&buf, s, reg, Options{Compression: CompressionNone}); err == nil {
			t.Fatalf("accepted (%d bytes)", buf.Len())
		}
	})
	t.Run("indexed Store", func(t *testing.T) {
		c := hwColumn(t, reg, 0, 0, cube.Range{0, 15})
		c.Col.Chunk.SetBlock(1, 0, 0, 0, bogus)
		path := filepath.Join(t.TempDir(), "w.pile")
		w, err := CreateIndexed(path, reg, Options{Compression: CompressionNone})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Store(c); err == nil {
			t.Fatal("Store accepted an unregistered runtime ID")
		}
		// A refused Store must leave nothing behind: the world still has to
		// close, reopen and hold nothing.
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		r, err := OpenIndexed(path, reg, true)
		if err != nil {
			t.Fatal(err)
		}
		if n := r.ChunkCount(); n != 0 {
			t.Fatalf("a refused Store left %d chunks behind", n)
		}
		_ = r.Close()
	})
}

// TestHostileWriteStructureCellCeiling is finding 2. Every axis of
// [1048576, 272, 16] is legal and its grid is 1,114,112 cells, one row past
// §8's ceiling of 1,048,576 — 8.9 MB of pointers, which a caller can hold.
func TestHostileWriteStructureCellCeiling(t *testing.T) {
	reg := testRegistry(t)
	at := func(size [3]int32) error {
		nx, ny, nz := CellDims(size)
		s := &StructureData{Size: size, Cells: make([]*chunk.SubChunk, int64(nx)*int64(ny)*int64(nz))}
		stone, _ := reg.StateToRuntimeID("minecraft:stone", map[string]any{})
		cell := chunk.NewSubChunk(reg.AirRuntimeID())
		cell.SetBlock(0, 0, 0, 0, stone)
		s.Cells[0] = cell
		var buf bytes.Buffer
		if err := WriteStructure(&buf, s, reg, Options{Compression: CompressionNone}); err != nil {
			return err
		}
		if _, err := ReadStructure(buf.Bytes(), reg); err != nil {
			return fmt.Errorf("the writer emitted %d bytes its own reader refuses: %w", buf.Len(), err)
		}
		return nil
	}
	// At the ceiling exactly: 65,536 x 16 x 1 = 1,048,576 cells, and it must
	// still round trip, or the fix has moved the accept boundary.
	if err := at([3]int32{maxStructureSize, 256, 16}); err != nil {
		t.Fatalf("a structure at the cell ceiling must still be writable: %v", err)
	}
	// One row past it.
	err := at([3]int32{maxStructureSize, 272, 16})
	if err == nil {
		t.Fatal("a structure one row past the cell ceiling was written and read back, " +
			"which cannot happen: before the fix WriteStructure produced 143,477 bytes and " +
			"ReadStructure refused them")
	}
	if strings.Contains(err.Error(), "its own reader refuses") {
		t.Fatal(err)
	}
}

// TestHostileWriteSidecarScanIsLinear is finding 4, and it is the one reachable
// from ordinary tooling: ContentHash decodes and re-encodes, so a legal file is
// enough to reach it.
//
// The writers folded a column's or structure's preserved-state sidecar into a
// map keyed by (section, layer), then asked that map, once per section or cell,
// how many layers that one section reached — by scanning every key. Cost was
// cells x keys, and both factors are bounded only by §8's own ceilings.
//
// The measurement: a **155-byte** structure file declaring 1,048,576 cells, of
// which 4,096 hold one unresolvable state each, took **2 m 4 s** inside
// ContentHash. 1,024 present cells took 45 s and 256 took 17 s — linear in the
// key count, on a file whose size barely moves, which is the shape of the bug.
func TestHostileWriteSidecarScanIsLinear(t *testing.T) {
	if testing.Short() {
		t.Skip("decodes a million-cell structure")
	}
	reg := testRegistry(t)
	f := hwStructureSidecarFile(maxStructureSize, 256, 16, 4096)
	s, err := ReadStructure(f, reg)
	if err != nil {
		t.Fatalf("the fixture must be a legal file: %v", err)
	}
	if len(s.Cells) != maxStructureCells || len(s.Unknown) != 4096 {
		t.Fatalf("fixture is %d cells and %d preserved states, want %d and 4096",
			len(s.Cells), len(s.Unknown), maxStructureCells)
	}
	start := time.Now()
	if _, err := ContentHash(f, reg); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	t.Logf("ContentHash over a %d-byte file of %d cells and %d preserved states: %v (before: 2m4s)",
		len(f), len(s.Cells), len(s.Unknown), elapsed)
	// The ceiling is three orders of magnitude under the measured before, not a
	// tight bound on the fixed code, because what is being guarded against is
	// quadratic growth and not ten per cent.
	if elapsed > 10*time.Second {
		t.Fatalf("ContentHash over %d bytes took %v: the per-section sidecar scan is quadratic again",
			len(f), elapsed)
	}
}

// hwStructureSidecarFile builds a legal structure file whose grid is the size
// implied by the three axes and whose first `present` cells each hold one
// uniform layer of a state no registry resolves. Assembled from bytes rather
// than through WriteStructure, because building it through the writer would run
// the very loop it is measuring.
func hwStructureSidecarFile(sizeX, sizeY, sizeZ uint64, present int) []byte {
	w := &writer{}
	hostileMetaPrefix(w)
	w.uvarint(1) // one block palette entry
	w.str("pile:definitely_not_a_block")
	w.uvarint(0) // no properties
	w.uvarint(0) // no version overrides
	w.uvarint(0) // structures carry no biomes
	blob := &writer{}
	blob.uvarint(1) // one local palette entry
	blob.uvarint(0) // global reference 0
	blob.u8(widthUniform)
	w.uvarint(1) // one blob in the table
	w.raw(blob.bytes())
	w.uvarint(sizeX)
	w.uvarint(sizeY)
	w.uvarint(sizeZ)
	for range 3 {
		w.svarint(0) // origin
	}
	cells := int(((sizeX + 15) >> 4) * ((sizeY + 15) >> 4) * ((sizeZ + 15) >> 4))
	presence := make([]byte, (cells+7)/8)
	for i := range present {
		presence[i/8] |= 1 << (i % 8)
	}
	w.raw(presence)
	for range present {
		w.uvarint(1) // one layer
		w.uvarint(0) // blob 0
	}
	w.uvarint(0) // block entities
	w.uvarint(0) // entities
	return hostileSeal(KindStructure, w.bytes(), true)
}

// TestHostileWriteNilInputs is finding 5. Both entry points dereferenced their
// argument before looking at it.
func TestHostileWriteNilInputs(t *testing.T) {
	reg := testRegistry(t)
	var buf bytes.Buffer
	if err := hwRecover(func() error { return WriteWorld(&buf, nil, reg, Options{}) }); err == nil {
		t.Fatal("WriteWorld(nil) reported success")
	} else if strings.HasPrefix(err.Error(), "runtime error") {
		t.Fatalf("WriteWorld(nil) panicked: %v", err)
	}
	if err := hwRecover(func() error { return WriteStructure(&buf, nil, reg, Options{}) }); err == nil {
		t.Fatal("WriteStructure(nil) reported success")
	} else if strings.HasPrefix(err.Error(), "runtime error") {
		t.Fatalf("WriteStructure(nil) panicked: %v", err)
	}
}

// ---------------------------------------------------------------------------
// ContentHash: the entry point that re-encodes attacker-controlled bytes
// ---------------------------------------------------------------------------

// TestHostileContentHashOverNegativeVectors runs ContentHash over every file
// the conformance appendix says a reader must refuse. ContentHash reaches the
// writers, so a vector that gets past the reader on one of its two paths and
// into an encoder is exactly the shape this file exists to catch; what is
// required is a clean return, never a panic and never a hang.
func TestHostileContentHashOverNegativeVectors(t *testing.T) {
	reg := testRegistry(t)
	entries, err := os.ReadDir(vectorDir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "neg_") || !strings.HasSuffix(e.Name(), ".pile") {
			continue
		}
		n++
		name := e.Name()
		t.Run(name, func(t *testing.T) {
			file, err := os.ReadFile(filepath.Join(vectorDir, name))
			if err != nil {
				t.Fatal(err)
			}
			var herr error
			if perr := hwRecover(func() error {
				_, herr = ContentHash(file, reg)
				return nil
			}); perr != nil {
				t.Fatalf("ContentHash panicked: %v", perr)
			}
			if herr == nil {
				// An indexed negative vector is not a solid file, so ContentHash
				// may legitimately fail to reach the rule the vector holds; what
				// it may never do is succeed on a file both readers refuse.
				if _, rw := ReadWorld(file, reg); rw != nil {
					t.Fatalf("ContentHash succeeded on a file ReadWorld refuses: %v", rw)
				}
				return
			}
			requireCleanRejection(t, name, herr)
		})
	}
	if n < 50 {
		t.Fatalf("only %d negative vectors found in %s; the appendix has 59", n, vectorDir)
	}
}

// TestHostileContentHashIsAFixedPointOverFixtures: ContentHash is defined as
// the hash of the canonical re-encoding, so hashing a file and hashing the file
// that re-encoding produces must agree. If they did not, the identity of a
// world would depend on how many times it had been rewritten.
func TestHostileContentHashIsAFixedPointOverFixtures(t *testing.T) {
	reg := testRegistry(t)
	var files []string
	for _, dir := range []string{goldenDir, vectorDir} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".pile") || strings.HasPrefix(e.Name(), "neg_") {
				continue
			}
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	checked := 0
	for _, path := range files {
		file, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		h, err := ContentHash(file, reg)
		if err != nil {
			continue // indexed fixtures are not solid files
		}
		checked++
		// Re-encode and hash again.
		var out bytes.Buffer
		if k, _, perr := parseFrame(file); perr == nil && k.kind == KindStructure {
			s, err := ReadStructure(file, reg)
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			if err := WriteStructure(&out, s, reg, Options{Compression: CompressionNone}); err != nil {
				t.Fatalf("%s: %v", path, err)
			}
		} else {
			d, err := ReadWorld(file, reg)
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			if err := WriteWorld(&out, d, reg, Options{Compression: CompressionNone}); err != nil {
				t.Fatalf("%s: %v", path, err)
			}
		}
		h2, err := ContentHash(out.Bytes(), reg)
		if err != nil {
			t.Fatalf("%s: the re-encoding does not decode: %v", path, err)
		}
		if h != h2 {
			t.Fatalf("%s: ContentHash is not a fixed point: %016x then %016x", path, h, h2)
		}
	}
	if checked < 20 {
		t.Fatalf("only %d solid fixtures were hashed; the sweep is not covering the fixtures", checked)
	}
}

// ---------------------------------------------------------------------------
// NBT: the writer must refuse what the reader refuses
// ---------------------------------------------------------------------------

// TestHostileWriteNBTCeilings drives the marshaller at and past each of the
// three ceilings §8 puts on an NBT blob, through the surface a caller actually
// reaches it by — a block entity's data. The rule is an equivalence and not an
// implication: at the ceiling the writer must accept and the reader must
// accept; past it the writer must refuse.
func TestHostileWriteNBTCeilings(t *testing.T) {
	reg := testRegistry(t)
	nest := func(depth int) map[string]any {
		v := any(map[string]any{"leaf": int32(1)})
		for range depth {
			v = map[string]any{"n": v}
		}
		return v.(map[string]any)
	}
	listOf := func(n int) map[string]any {
		l := make([]map[string]any, n)
		for i := range l {
			l[i] = map[string]any{}
		}
		return map[string]any{"l": l}
	}
	siblings := func(n int) map[string]any {
		m := make(map[string]any, n)
		for i := range n {
			m[fmt.Sprintf("k%07d", i)] = map[string]any{}
		}
		return m
	}
	cases := []struct {
		name  string
		data  map[string]any
		valid bool // whether the reader accepts a blob of this shape
	}{
		// nest(n) wraps a leaf compound n times, so the root plus n levels is
		// n+1 deep and the last accepted value is one below the ceiling.
		{"depth at the ceiling", nest(maxNBTDepth - 1), true},
		{"depth one past the ceiling", nest(maxNBTDepth), false},
		// The field holding the list is itself a container and is charged one,
		// so a list of exactly the ceiling is already one over.
		{"list elements at the ceiling", listOf(maxNBTElements - 1), true},
		{"list elements one past", listOf(maxNBTElements), false},
		{"sibling compounds at the ceiling", siblings(maxNBTElements), true},
		{"sibling compounds one past", siblings(maxNBTElements + 1), false},
		// Finding 6: §1 and §8 put an NBT string at 65,535 and the decoder
		// takes the length as a signed int16, so 32,768 is where it really
		// stops. The writer now stops there too.
		{"nbt string at the length the decoder accepts", map[string]any{"s": strings.Repeat("a", maxNBTStringWrite)}, true},
		{"nbt string one past it", map[string]any{"s": strings.Repeat("a", maxNBTStringWrite+1)}, false},
		{"nbt string at the length §8 states", map[string]any{"s": strings.Repeat("a", maxStringLen)}, false},
		{"nbt compound key one past it", map[string]any{strings.Repeat("k", maxNBTStringWrite+1): int32(1)}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			blob, err := marshalNBT(c.data)
			if !c.valid {
				if err == nil {
					t.Fatalf("the writer produced %d bytes the reader refuses: %v",
						len(blob), validateNBT(blob))
				}
				return
			}
			if err != nil {
				t.Fatalf("the writer refused a blob the reader accepts: %v", err)
			}
			if err := validateNBT(blob); err != nil {
				t.Fatalf("the writer emitted a blob its own validator refuses: %v", err)
			}
			// validateNBT is a structural walk; unmarshalNBT is what a record's
			// block entity and entity blobs are actually read back through, and
			// the two do not have the same accept boundary. Finding 6 lived in
			// exactly that gap.
			if _, err := unmarshalNBT(blob); err != nil {
				t.Fatalf("the writer emitted a blob unmarshalNBT refuses: %v", err)
			}
			// And through the real surface, so the check is on a path a caller
			// reaches rather than on the marshaller alone.
			if len(blob) > maxBlobLen {
				return
			}
			col := hwColumn(t, reg, 0, 0, cube.Range{0, 15})
			col.Col.BlockEntities = []chunk.BlockEntity{{Pos: cube.Pos{0, 0, 0}, Data: c.data}}
			var buf bytes.Buffer
			if err := WriteWorld(&buf, &WorldData{Columns: []Column{col}}, reg, Options{Compression: CompressionNone}); err != nil {
				t.Fatalf("WriteWorld refused a block entity the reader would accept: %v", err)
			}
			if _, err := ReadWorld(buf.Bytes(), reg); err != nil {
				t.Fatalf("the writer emitted %d bytes its own reader refuses: %v", buf.Len(), err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The indexed write surface
// ---------------------------------------------------------------------------

// TestHostileIndexedWriteSurface drives Store, Checkpoint, Compact and Close
// with the same shapes, and requires the file to be reopenable and every stored
// column readable after each. A refused Store must also leave nothing behind:
// palette entries added before the rejection would still reach the next
// checkpoint, and an empty world that had refused a column would then not
// encode like an empty world that never saw one.
func TestHostileIndexedWriteSurface(t *testing.T) {
	reg := testRegistry(t)
	stone, _ := reg.StateToRuntimeID("minecraft:stone", map[string]any{})
	good := func(x, z int32) Column {
		c := hwColumn(t, reg, x, z, cube.Range{0, 15})
		c.Col.BlockEntities = []chunk.BlockEntity{{
			Pos:  cube.Pos{int(x)*16 + 1, 1, int(z)*16 + 2},
			Data: map[string]any{"id": "minecraft:chest"},
		}}
		return c
	}
	bad := []struct {
		name string
		col  func() Column
	}{
		{"block entity outside the column", func() Column {
			c := hwColumn(t, reg, 3, 3, cube.Range{0, 15})
			c.Col.BlockEntities = []chunk.BlockEntity{{Pos: cube.Pos{0, 0, 0}, Data: map[string]any{"id": "a"}}}
			return c
		}},
		{"scheduled update below the range", func() Column {
			c := hwColumn(t, reg, 4, 4, cube.Range{0, 15})
			c.Col.ScheduledBlocks = []chunk.ScheduledBlockUpdate{{Pos: cube.Pos{64, -1, 64}, Block: stone}}
			return c
		}},
		{"unregistered runtime ID", func() Column {
			c := hwColumn(t, reg, 5, 5, cube.Range{0, 15})
			c.Col.Chunk.SetBlock(1, 0, 0, 0, 1<<24)
			return c
		}},
		{"no chunk data", func() Column { return Column{X: 6, Z: 6} }},
		{"unaligned range", func() Column {
			return Column{X: 7, Z: 7, Col: &chunk.Column{Chunk: chunk.New(reg, cube.Range{0, 20})}}
		}},
	}

	// A column that resolves part of its palette and then fails. Everything in
	// `bad` above is refused by validateColumn, which runs before a single
	// palette entry is touched, so none of it can prove the wind-back exists.
	// This one gets one preserved state admitted and fails on the second, which
	// is the only shape that leaves anything behind.
	halfResolved := func() Column {
		c := hwColumn(t, reg, 8, 8, cube.Range{0, 15})
		c.Col.Chunk.Sub()[0].SetBlock(0, 0, 0, 0, placeholderRid(reg))
		c.Col.Chunk.Sub()[0].SetBlock(1, 0, 0, 0, placeholderRid(reg))
		c.Unknown = []UnknownBlock{
			{Section: 0, Layer: 0, Index: 0, State: 0},
			{Section: 0, Layer: 0, Index: 1 << 8, State: 1},
		}
		c.UnknownStates = []BlockState{
			{Name: "pile:admitted_first"},
			{Name: "pile:\xff\xfe"}, // refused: not valid UTF-8
		}
		return c
	}

	path := filepath.Join(t.TempDir(), "hostile.pile")
	w, err := CreateIndexed(path, reg, Options{Compression: CompressionDefault})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Store(good(0, 0)); err != nil {
		t.Fatal(err)
	}
	if err := w.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	beforeHash := hwIndexedHash(t, path, reg, w)
	for _, b := range bad {
		t.Run(b.name, func(t *testing.T) {
			if err := hwRecover(func() error { return w.Store(b.col()) }); err == nil {
				t.Fatal("Store accepted it")
			} else if strings.HasPrefix(err.Error(), "runtime error") {
				t.Fatalf("Store panicked: %v", err)
			}
		})
	}
	if err := hwRecover(func() error { return w.Store(halfResolved()) }); err == nil {
		t.Fatal("Store accepted a column whose palette it could not finish")
	}
	// Every rejection wound the palettes back: a checkpoint taken now must hold
	// exactly what the one before the rejections held, and — the part the
	// projected content hash cannot see — must not carry the palette entry the
	// refused column got as far as admitting.
	if err := w.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if got := hwIndexedHash(t, path, reg, w); got != beforeHash {
		t.Fatalf("refused stores changed the world's content: %016x, want %016x", got, beforeHash)
	}
	leftover, err := w.UnresolvedStates()
	if err != nil {
		t.Fatal(err)
	}
	if len(leftover) != 0 {
		t.Fatalf("a refused Store left %d unreferenced palette entries behind: %v", len(leftover), leftover)
	}
	if err := w.Store(good(1, 0)); err != nil {
		t.Fatal(err)
	}
	if err := w.SetMeta(mustNBT(t, map[string]any{"name": "hostile"}), []byte("u"), nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := w.Compact(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := OpenIndexed(path, reg, true)
	if err != nil {
		t.Fatalf("the file the hostile sequence produced does not reopen: %v", err)
	}
	defer r.Close()
	if n := r.ChunkCount(); n != 2 {
		t.Fatalf("chunk count = %d, want 2", n)
	}
	for _, k := range r.Positions() {
		if _, err := r.Column(k[0], k[1]); err != nil {
			t.Fatalf("column (%d,%d) does not read back: %v", k[0], k[1], err)
		}
	}
}

// hwIndexedHash returns a content identity for the live set of an indexed
// world, by projecting it into a solid file. w must be the open handle: the
// point is to compare the world across operations, not the file's bytes, which
// indexed mode is explicitly not required to keep stable.
func hwIndexedHash(t *testing.T, path string, reg world.BlockRegistry, w *IndexedWorld) uint64 {
	t.Helper()
	d := &WorldData{}
	for _, k := range w.Positions() {
		c, err := w.Column(k[0], k[1])
		if err != nil {
			t.Fatal(err)
		}
		d.Columns = append(d.Columns, c)
	}
	var buf bytes.Buffer
	if err := WriteWorld(&buf, d, reg, Options{Compression: CompressionNone}); err != nil {
		t.Fatal(err)
	}
	h, err := ContentHash(buf.Bytes(), reg)
	if err != nil {
		t.Fatal(err)
	}
	return h
}
