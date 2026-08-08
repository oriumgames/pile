package format

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
)

// Property tests.
//
// The golden files pin the bytes of a handful of fixed worlds, which catches a
// format change but only for the shapes someone thought to build. Several
// defects survived many review rounds because no fixture had the shape that
// exposed them: an all-air layer under a populated one, a preserved biome name
// on an elided section, two runtime IDs for one state.
//
// These tests state the properties instead, and run them over generated worlds
// that deliberately reach into the corners. The central one is canonicality:
// encoding is a function of content, so decoding and re-encoding must not move
// a byte. Almost every canonicality defect found so far shows up as a failure
// of exactly that.

// canonical checks encode(decode(encode(x))) == encode(x), the property that
// says the encoder has one output per content. A second decode and encode
// catches anything that oscillates rather than converging.
func canonical(t *testing.T, reg world.BlockRegistry, d *WorldData, opts Options) []byte {
	t.Helper()
	var first bytes.Buffer
	if err := WriteWorld(&first, d, reg, opts); err != nil {
		t.Fatalf("encode: %v", err)
	}
	back, err := ReadWorld(first.Bytes(), reg)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var second bytes.Buffer
	if err := WriteWorld(&second, back, reg, opts); err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatalf("encode(decode(encode(x))) != encode(x): %d then %d bytes",
			first.Len(), second.Len())
	}
	again, err := ReadWorld(second.Bytes(), reg)
	if err != nil {
		t.Fatalf("decode of re-encoded file: %v", err)
	}
	var third bytes.Buffer
	if err := WriteWorld(&third, again, reg, opts); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(second.Bytes(), third.Bytes()) {
		t.Fatal("encoding oscillates rather than converging")
	}
	return first.Bytes()
}

// TestPropertyCanonicalOverBoundaryMatrix runs the canonicality property over
// the generated corner cases. The matrix is the point: each axis is a place
// where a count, a width or an omission rule changes behaviour, and the
// product reaches combinations no hand-written fixture covers.
func TestPropertyCanonicalOverBoundaryMatrix(t *testing.T) {
	reg := testRegistry(t)
	for _, c := range boundaryCases(t, reg) {
		t.Run(c.name, func(t *testing.T) {
			canonical(t, c.registry(reg), c.world, Options{Compression: CompressionNone})
		})
	}
}

// TestPropertyCanonicalCompressed repeats the property with compression on, so
// a difference that only the compressed path can produce still shows up.
func TestPropertyCanonicalCompressed(t *testing.T) {
	reg := testRegistry(t)
	for _, c := range boundaryCases(t, reg) {
		t.Run(c.name, func(t *testing.T) {
			canonical(t, c.registry(reg), c.world, Options{Compression: CompressionBest})
		})
	}
}

// TestPropertyOptionsDoNotChangeContent: an option selects how the file is
// written, never what it says. Stats, light and compression must all decode to
// the same content hash.
func TestPropertyOptionsDoNotChangeContent(t *testing.T) {
	reg := testRegistry(t)
	for _, c := range boundaryCases(t, reg) {
		t.Run(c.name, func(t *testing.T) {
			reg := c.registry(reg)
			var base bytes.Buffer
			if err := WriteWorld(&base, c.world, reg, Options{Compression: CompressionNone}); err != nil {
				t.Fatal(err)
			}
			want, err := ContentHash(base.Bytes(), reg)
			if err != nil {
				t.Fatal(err)
			}
			for _, opts := range []Options{
				{Compression: CompressionNone, StoreLight: true},
				{Compression: CompressionBest},
				{Compression: CompressionFast},
				{Compression: CompressionNone, Stats: true},
				{Compression: CompressionNone, StoreLight: true},
				{Compression: CompressionBest, Stats: true, StoreLight: true},
				{Compression: CompressionNone, Workers: 1},
			} {
				var buf bytes.Buffer
				if err := WriteWorld(&buf, c.world, reg, opts); err != nil {
					t.Fatalf("%+v: %v", opts, err)
				}
				got, err := ContentHash(buf.Bytes(), reg)
				if err != nil {
					t.Fatalf("%+v: %v", opts, err)
				}
				if got != want {
					t.Fatalf("%+v changed the content hash: %016x, want %016x", opts, got, want)
				}
			}
		})
	}
}

// TestPropertyMutationIsRejectedOrVisible is the adversarial direction. A
// canonical format admits one encoding per content, so flipping a byte of a
// valid file must either fail to decode or decode to something that re-encodes
// differently. A mutation that survives and re-encodes to the original bytes is
// a second encoding of one world, which is exactly what a canonical format
// promises cannot exist.
func TestPropertyMutationIsRejectedOrVisible(t *testing.T) {
	reg := testRegistry(t)
	file := encode(t, testWorld(t, reg), reg, CompressionNone)

	// The footer's hash covers the header and body, so a mutation there is
	// caught by the checksum rather than by canonicality; rehash after each
	// change so the test exercises the rules rather than the checksum.
	rng := rand.New(rand.NewPCG(1, 2))
	bodyStart, bodyEnd := headerSize, len(file)-footerSize
	survived := 0
	for range 400 {
		mutated := bytes.Clone(file)
		pos := bodyStart + rng.IntN(bodyEnd-bodyStart)
		mutated[pos] ^= byte(1 << rng.IntN(8))
		rehashSolid(mutated)
		if bytes.Equal(mutated, file) {
			continue
		}
		d, err := ReadWorld(mutated, reg)
		if err != nil {
			continue // rejected, which is the common and correct outcome
		}
		survived++
		var round bytes.Buffer
		if err := WriteWorld(&round, d, reg, Options{Compression: CompressionNone}); err != nil {
			continue // accepted on read but refused on write is also fine
		}
		if bytes.Equal(round.Bytes(), file) {
			t.Fatalf("a mutation at body offset %d decodes to the original content: "+
				"the file has two encodings", pos-bodyStart)
		}
	}
	// If every mutation were rejected outright the test would pass while
	// proving nothing about canonicality, so require that some got past the
	// reader and were caught by re-encoding instead.
	if survived == 0 {
		t.Fatal("no mutation survived decoding: the test exercised only the reader's bounds")
	}
	t.Logf("%d of 400 mutations decoded and were caught by re-encoding", survived)
}

// aliasWidthCase builds a section whose local palette holds one more slot than
// the one-byte index width allows, where exactly one pair of slots are aliases
// of a single state. Folding them before the width is chosen keeps the section
// at the narrower width; choosing first would widen every index in it.
func aliasWidthCase(t *testing.T) boundaryCase {
	t.Helper()
	reg := newAliasRegistry(testRegistry(t))
	ch := chunk.New(reg, cube.Range{-64, 319})
	placeholder := placeholderRid(reg)
	c := Column{X: 0, Z: 0, Col: &chunk.Column{Chunk: ch}}

	// 255 distinct preserved states, plus both aliases of one real state:
	// 257 slots that fold to 256.
	for i := range 255 {
		x, z := uint8(i%16), uint8((i/16)%16)
		ch.SetBlock(x, -64, z, 0, placeholder)
		c.UnknownStates = append(c.UnknownStates, BlockState{
			Name: fmt.Sprintf("audit:w%03d", i), Version: 1,
		})
		c.Unknown = append(c.Unknown, UnknownBlock{
			Section: -4, Layer: 0,
			Index: uint16(x)<<8 | uint16(z)<<4, State: uint32(i),
		})
	}
	ch.SetBlock(0, -63, 0, 0, reg.alias-1)
	ch.SetBlock(1, -63, 0, 0, reg.alias)
	return boundaryCase{name: "alias_at_width_boundary",
		world: &WorldData{Columns: []Column{c}}, reg: reg}
}

// boundaryCase is one generated world with a name describing which corner it
// occupies.
type boundaryCase struct {
	name  string
	world *WorldData
	// reg is the registry the case's chunks were built against, when that is
	// not the default one. It used to be absent, and the alias case was
	// therefore encoded under a registry that did not know its second alias:
	// the folding it exists to exercise was being done by the writer's
	// unknown-runtime-ID fallback instead, which now refuses. A case built
	// against a registry must be written with it.
	reg world.BlockRegistry
}

// registry returns the registry a case must be encoded with.
func (c boundaryCase) registry(dflt world.BlockRegistry) world.BlockRegistry {
	if c.reg != nil {
		return c.reg
	}
	return dflt
}

// boundaryCases generates the corner cases worth running every property over.
// The axes are the ones that have actually produced defects: palette widths at
// their transition, layer counts and which layers hold air, section spans,
// preserved states and their versions, biome uniformity, and empty versus
// populated collections.
func boundaryCases(t *testing.T, reg world.BlockRegistry) []boundaryCase {
	t.Helper()
	stone, _ := reg.StateToRuntimeID("minecraft:stone", map[string]any{})
	water, _ := reg.StateToRuntimeID("minecraft:water", map[string]any{"liquid_depth": int32(0)})
	placeholder := placeholderRid(reg)
	r := cube.Range{-64, 319}

	col := func(build func(ch *chunk.Chunk)) *chunk.Chunk {
		ch := chunk.New(reg, r)
		build(ch)
		return ch
	}
	wrap := func(name string, c Column) boundaryCase {
		return boundaryCase{name: name, world: &WorldData{Columns: []Column{c}}}
	}
	plain := func(name string, ch *chunk.Chunk) boundaryCase {
		return wrap(name, Column{X: 0, Z: 0, Col: &chunk.Column{Chunk: ch}})
	}

	cases := []boundaryCase{
		{name: "no_columns", world: &WorldData{}},
		plain("empty_chunk", col(func(ch *chunk.Chunk) {})),
		plain("one_block", col(func(ch *chunk.Chunk) { ch.SetBlock(0, -64, 0, 0, stone) })),
		plain("uniform_section", col(func(ch *chunk.Chunk) {
			for x := range uint8(16) {
				for z := range uint8(16) {
					for y := int16(-64); y < -48; y++ {
						ch.SetBlock(x, y, z, 0, stone)
					}
				}
			}
		})),
		plain("top_and_bottom", col(func(ch *chunk.Chunk) {
			ch.SetBlock(0, int16(r[0]), 0, 0, stone)
			ch.SetBlock(15, int16(r[1]), 15, 0, stone)
		})),
		plain("air_layer_below_water", col(func(ch *chunk.Chunk) {
			ch.SetBlock(0, 100, 0, 0, stone)
			ch.SetBlock(5, -60, 5, 1, water)
		})),
		plain("spare_trailing_layers", col(func(ch *chunk.Chunk) {
			ch.SetBlock(0, -64, 0, 0, stone)
			ch.SetBlock(0, -64, 0, 3, reg.AirRuntimeID())
		})),
	}

	// Palette widths at and around the one-byte boundary, built from preserved
	// states so the case does not depend on how a registry numbers blocks.
	for _, n := range []int{1, 2, 255, 256, 257} {
		c := Column{X: 0, Z: 0, Col: &chunk.Column{Chunk: chunk.New(reg, r)}}
		for i := range n {
			x, z := uint8(i%16), uint8((i/16)%16)
			y := int16(-64) + int16(i/256)
			c.Col.Chunk.SetBlock(x, y, z, 0, placeholder)
			c.UnknownStates = append(c.UnknownStates, BlockState{
				Name: fmt.Sprintf("audit:p%04d", i), Version: int32(1 + i%3),
			})
			c.Unknown = append(c.Unknown, UnknownBlock{
				Section: -4, Layer: 0,
				Index: uint16(x)<<8 | uint16(z)<<4 | uint16(y&15), State: uint32(i),
			})
		}
		cases = append(cases, wrap(fmt.Sprintf("palette_%03d", n), c))
	}

	// Version zero, the writer's own version, and a foreign one. The first two
	// mean the same thing and must encode the same way.
	for _, v := range []int32{0, chunk.CurrentBlockVersion, 17825806} {
		ch := chunk.New(reg, r)
		ch.SetBlock(1, -64, 1, 0, placeholder)
		cases = append(cases, wrap(fmt.Sprintf("version_%d", v), Column{
			X: 0, Z: 0, Col: &chunk.Column{Chunk: ch},
			UnknownStates: []BlockState{{Name: "audit:v", Version: v}},
			Unknown: []UnknownBlock{{
				Section: -4, Layer: 0, Index: uint16(1)<<8 | uint16(1)<<4, State: 0,
			}},
		}))
	}

	// Collections empty, singular and tied. Ties are where the ordering rules
	// stop being obvious.
	ch := chunk.New(reg, r)
	ch.SetBlock(0, -64, 0, 0, stone)
	cases = append(cases, wrap("tied_collections", Column{X: 0, Z: 0, Col: &chunk.Column{
		Chunk: ch,
		// Ties that remain legal now that the keys themselves are unique:
		// block entities at different positions carrying identical NBT,
		// entities sharing an id, and updates at one position differing only
		// in the block they name.
		BlockEntities: []chunk.BlockEntity{
			{Pos: cube.Pos{2, -60, 1}, Data: map[string]any{"id": "minecraft:chest"}},
			{Pos: cube.Pos{1, -60, 1}, Data: map[string]any{"id": "minecraft:chest"}},
		},
		Entities: []chunk.Entity{
			{ID: 0, Data: map[string]any{"identifier": "minecraft:cow"}},
			{ID: 0, Data: map[string]any{"identifier": "minecraft:pig"}},
		},
		ScheduledBlocks: []chunk.ScheduledBlockUpdate{
			{Pos: cube.Pos{2, -60, 2}, Block: stone, Tick: 7},
			{Pos: cube.Pos{2, -60, 2}, Block: water, Tick: 7},
			{Pos: cube.Pos{2, -60, 2}, Block: stone, Tick: 8},
		},
	}}))

	// Section spans other than the vanilla 24. A 24-section chunk makes every
	// presence bitset a whole number of bytes, so the padding rules are never
	// exercised by the matrix; the smallest and largest legal spans are.
	for _, span := range []cube.Range{{0, 15}, {-64, 111}, {-2048 * 16, -2048*16 + 16*100 - 1}} {
		ch := chunk.New(reg, span)
		ch.SetBlock(0, int16(span[0]), 0, 0, stone)
		cases = append(cases, boundaryCase{
			name: fmt.Sprintf("span_%d_%d", span[0], span[1]),
			world: &WorldData{Columns: []Column{{X: 0, Z: 0,
				Col: &chunk.Column{Chunk: ch}}}},
		})
	}

	// Baked light. The matrix sets StoreLight but never installs any light, so
	// no case could produce a set light-presence bit: the independence of
	// light from block presence, its one-sided flags and its own bitset
	// padding were all unreachable.
	lit := chunk.New(reg, r)
	lit.SetBlock(0, -64, 0, 0, stone)
	chunk.LightArea([]*chunk.Chunk{lit}, 0, 0).Fill()
	cases = append(cases, boundaryCase{name: "baked_light",
		world: &WorldData{Columns: []Column{{X: 0, Z: 0, Col: &chunk.Column{Chunk: lit}}}}})

	// A local palette that crosses the index-width boundary only after alias
	// folding: 257 slots resolving to 256 entries must still choose the
	// one-byte width. Registry-independent generation cannot produce this, so
	// it needs the aliasing registry.
	cases = append(cases, aliasWidthCase(t))

	// Chunk coordinates at both int32 extremes and adjacent, so Morton keys and
	// position deltas are exercised at the ends of their domain.
	extreme := &WorldData{}
	for _, p := range [][2]int32{
		{-2147483648, -2147483648}, {-2147483648, 2147483647},
		{0, 0}, {1, 0}, {2147483647, -2147483648}, {2147483647, 2147483647},
	} {
		extreme.Columns = append(extreme.Columns, Column{X: p[0], Z: p[1], Col: &chunk.Column{
			Chunk: col(func(ch *chunk.Chunk) { ch.SetBlock(0, -64, 0, 0, stone) }),
		}})
	}
	cases = append(cases, boundaryCase{name: "extreme_positions", world: extreme})

	return cases
}

// TestPropertyStructureCanonical runs the canonicality property over structure
// files. Structures share the palette, blob and layer machinery with worlds but
// have their own envelope rules, so they need their own pass.
func TestPropertyStructureCanonical(t *testing.T) {
	reg := testRegistry(t)
	for _, c := range structureBoundaryCases(t, reg) {
		t.Run(c.name, func(t *testing.T) {
			for _, opts := range []Options{
				{Compression: CompressionNone}, {Compression: CompressionBest},
			} {
				var first bytes.Buffer
				if err := WriteStructure(&first, c.data, reg, opts); err != nil {
					t.Fatalf("encode: %v", err)
				}
				back, err := ReadStructure(first.Bytes(), reg)
				if err != nil {
					t.Fatalf("decode: %v", err)
				}
				var second bytes.Buffer
				if err := WriteStructure(&second, back, reg, opts); err != nil {
					t.Fatalf("re-encode: %v", err)
				}
				if !bytes.Equal(first.Bytes(), second.Bytes()) {
					t.Fatalf("structure encoding is not canonical: %d then %d bytes",
						first.Len(), second.Len())
				}
			}
		})
	}
}

type structureCase struct {
	name string
	data *StructureData
}

// structureBoundaryCases covers the corners specific to structures: boxes that
// are not multiples of 16 (so edge cells have padding), origins at the ends of
// their domain, empty and single-cell boxes, and cells whose layers are
// arranged to test the air rules.
func structureBoundaryCases(t *testing.T, reg world.BlockRegistry) []structureCase {
	t.Helper()
	stone, _ := reg.StateToRuntimeID("minecraft:stone", map[string]any{})
	water, _ := reg.StateToRuntimeID("minecraft:water", map[string]any{"liquid_depth": int32(0)})
	air := reg.AirRuntimeID()

	make := func(t *testing.T, size [3]int32) *StructureData {
		d, err := NewStructureData(size)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}

	var out []structureCase
	for _, size := range [][3]int32{{1, 1, 1}, {15, 15, 15}, {16, 16, 16}, {17, 1, 33}, {32, 16, 32}} {
		name := fmt.Sprintf("size_%dx%dx%d", size[0], size[1], size[2])
		empty := make(t, size)
		out = append(out, structureCase{name + "_empty", empty})

		filled := make(t, size)
		for i := range filled.Cells {
			sub := chunk.NewSubChunk(air)
			sub.SetBlock(0, 0, 0, 0, stone)
			filled.Cells[i] = sub
		}
		out = append(out, structureCase{name + "_filled", filled})
	}

	// An air layer 0 under a populated layer 1, the shape that renumbered
	// layers until it was caught.
	logged := make(t, [3]int32{16, 16, 16})
	sub := chunk.NewSubChunk(air)
	sub.SetBlock(1, 2, 3, 1, water)
	logged.Cells[0] = sub
	out = append(out, structureCase{"waterlogged_only", logged})

	// Origins at both int32 extremes.
	for _, o := range [][3]int32{{0, 0, 0}, {-2147483648, 0, 2147483647}} {
		d := make(t, [3]int32{16, 16, 16})
		d.Origin = o
		s := chunk.NewSubChunk(air)
		s.SetBlock(0, 0, 0, 0, stone)
		d.Cells[0] = s
		out = append(out, structureCase{fmt.Sprintf("origin_%d", o[0]), d})
	}
	return out
}

// TestPropertyIndexedCanonicalContent: indexed files are history dependent by
// design, so their bytes are not a function of content. Their *content* still
// is: storing the same columns in any order, with or without checkpoints
// between, must yield the same content hash.
func TestPropertyIndexedCanonicalContent(t *testing.T) {
	reg := testRegistry(t)
	cases := boundaryCases(t, reg)
	for _, c := range cases {
		if len(c.world.Columns) == 0 {
			continue // nothing to store
		}
		t.Run(c.name, func(t *testing.T) {
			reg := c.registry(reg)
			hash := func(order []int, checkpointEvery int) uint64 {
				path := filepath.Join(t.TempDir(), "w.pile")
				w, err := CreateIndexed(path, reg, Options{Compression: CompressionNone})
				if err != nil {
					t.Fatal(err)
				}
				for n, i := range order {
					if err := w.Store(c.world.Columns[i]); err != nil {
						t.Fatal(err)
					}
					if checkpointEvery > 0 && n%checkpointEvery == 0 {
						if err := w.Checkpoint(); err != nil {
							t.Fatal(err)
						}
					}
				}
				if err := w.Close(); err != nil {
					t.Fatal(err)
				}
				return indexedContentHash(t, path, reg)
			}
			forward := make([]int, len(c.world.Columns))
			for i := range forward {
				forward[i] = i
			}
			backward := slices.Clone(forward)
			slices.Reverse(backward)

			want := hash(forward, 0)
			if got := hash(backward, 0); got != want {
				t.Fatalf("store order changed the content: %016x vs %016x", got, want)
			}
			if got := hash(forward, 1); got != want {
				t.Fatalf("checkpointing changed the content: %016x vs %016x", got, want)
			}
		})
	}
}

// indexedContentHash reduces an indexed world to the identity of what it
// holds, by reading it out and re-encoding it as a solid file. Indexed bytes
// depend on the order things were written; content does not.
func indexedContentHash(t *testing.T, path string, reg world.BlockRegistry) uint64 {
	t.Helper()
	w, err := OpenIndexed(path, reg, true)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	settings, userData := w.Meta()
	d := &WorldData{
		Settings: settings, UserData: userData,
	}
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

// TestPropertyTornCheckpointRecovers: truncating an indexed file anywhere past
// its first checkpoint must leave a file that still opens, on the newest
// checkpoint that survived intact. A truncation that opens as something other
// than a prefix of the history would mean the footer chain does not bound what
// a reader can believe.
func TestPropertyTornCheckpointRecovers(t *testing.T) {
	reg := testRegistry(t)
	path := filepath.Join(t.TempDir(), "w.pile")
	w, err := CreateIndexed(path, reg, Options{Compression: CompressionNone})
	if err != nil {
		t.Fatal(err)
	}
	// Three generations, so there is a chain to walk back along.
	seen := 0
	for gen := range 3 {
		for i := range int32(3) {
			if err := w.Store(buildTestColumn(t, reg, i, int32(gen))); err != nil {
				t.Fatal(err)
			}
		}
		if err := w.Checkpoint(); err != nil {
			t.Fatal(err)
		}
		seen += 3
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	full, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	recovered := 0
	for cut := headerSize + footerSize; cut < len(full); cut += 97 {
		torn := filepath.Join(t.TempDir(), "torn.pile")
		if err := os.WriteFile(torn, full[:cut], 0o644); err != nil {
			t.Fatal(err)
		}
		v, err := OpenIndexed(torn, reg, true)
		if err != nil {
			continue // no intact checkpoint left in the prefix
		}
		recovered++
		// Whatever it recovered must be internally consistent: every column
		// the directory names has to read back.
		for _, k := range v.Positions() {
			if _, err := v.Column(k[0], k[1]); err != nil {
				v.Close()
				t.Fatalf("truncation at %d recovered a directory naming an unreadable column (%d,%d): %v",
					cut, k[0], k[1], err)
			}
		}
		if n := v.ChunkCount(); n > seen {
			v.Close()
			t.Fatalf("truncation at %d recovered %d chunks, more than the %d ever stored", cut, n, seen)
		}
		v.Close()
	}
	if recovered == 0 {
		t.Fatal("no truncation recovered: the test exercised nothing")
	}
	t.Logf("%d truncation points recovered a usable world", recovered)
}

// TestPropertyCanonicalWithLight runs the canonicality property with light
// stored, so a difference only the light path can produce still shows up. The
// matrix's other properties run without it, and light is the one option whose
// content the writer collects rather than the caller supplying.
func TestPropertyCanonicalWithLight(t *testing.T) {
	reg := testRegistry(t)
	for _, c := range boundaryCases(t, reg) {
		t.Run(c.name, func(t *testing.T) {
			canonical(t, c.registry(reg), c.world, Options{Compression: CompressionNone, StoreLight: true})
		})
	}
}
