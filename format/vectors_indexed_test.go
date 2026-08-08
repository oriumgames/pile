package format

// The positive indexed-mode conformance vector.
//
// format/vectors.md records that indexed mode had negative vectors and torn
// write coverage only, on the grounds that a byte-pinned positive vector
// "would assert facts about the order this implementation happens to append in
// rather than about the format". That reasoning is sound and it is kept: this
// file's bytes are never compared against anything, and no test here regenerates
// it. What it does instead is make the weaker claim that is still worth making.
//
// A conformance vector is a file plus a statement of what a conforming reader
// must conclude from it. For every other vector the statement includes "and the
// writer produces exactly these bytes"; for an indexed file it cannot, because
// §5 makes the bytes history-dependent. Dropping that one clause leaves a claim
// that is still a real arbiter for a second implementation's *reader*: here is
// an indexed file this implementation wrote, with a history it did not choose,
// and here is what it means. A reader that disagrees is wrong about the format,
// not merely about an append order.
//
// The claims below are therefore all order-free. Each is a fact about the
// content the file holds or about the shape a reader must handle to reach it —
// a dictionary frame it must load before any record decodes, more than one
// block palette segment it must concatenate, dead frames it must not reach by
// scanning. None of them names an offset, a length, a frame's position in the
// file, or the order records were stored in. If a future writer appends in a
// different order, or batches segments differently, or trains a different
// dictionary, this file keeps passing, because none of that is being claimed.
//
// The artefact is written once and checked in. There is deliberately no
// regeneration path in the ordinary run: regenerating it would move bytes that
// nothing asserts, which is a change with no meaning, and the -mkindexedvector
// flag exists only so that a v3 can produce a v3 file. It is not the freeze
// lock's `-update` and does not lift it: no byte comparison anywhere depends on
// this file, so nothing it writes could bless a wire format change.

import (
	"bytes"
	"flag"
	"os"
	"slices"
	"testing"

	"github.com/df-mc/dragonfly/server/block"
	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
)

// mkIndexedVector regenerates indexed_full.pile. See the file comment: this is
// not -update and is not gated by the freeze lock, because the file it writes
// is never byte-compared.
var mkIndexedVector = flag.Bool("mkindexedvector", false,
	"rewrite testdata/vectors/indexed_full.pile (for a format revision only)")

const indexedFullVector = vectorDir + "/indexed_full.pile"

// vecIndexedRange is one section at the bottom of the nether's vertical range.
// Every column in the vector uses it, so a record costs one presence bit and
// the file stays small enough to read.
var vecIndexedRange = cube.Range{0, 15}

// indexedFullFacts is what a conforming reader must conclude from
// indexed_full.pile. Everything here is either content or a shape a reader has
// to handle; nothing here is a byte offset or an append order.
var indexedFullFacts = struct {
	generation uint64
	// content is format.ContentHash of the world the file recovers to,
	// re-encoded as an uncompressed solid file. This is the load-bearing
	// claim: two implementations that agree on it agree on what the file
	// means, whatever their readers did to get there.
	content uint64
	// positions is the exact set of columns the directory names, sorted. The
	// order the reader hands them back in is not claimed; the set is.
	positions [][2]int32
	// probe is a block coordinate inside column (0,0) and the state a reader
	// must find there, so an implementer has one concrete fact to check by
	// hand before trusting a hash.
	probeX, probeY, probeZ int
	probeBlock             string
}{
	generation: 3,
	content:    0x11eb2fd39e143b79,
	positions: [][2]int32{
		{0, 0}, {0, 1}, {0, 2}, {0, 3}, {0, 4},
		{1, 0}, {1, 1}, {1, 2}, {1, 3}, {1, 4},
		{2, 0}, {2, 1}, {2, 2}, {2, 3},
		{3, 0}, {3, 1}, {3, 2}, {3, 3},
		{4, 0}, {4, 1}, {4, 2}, {4, 3},
	},
	probeX: 0, probeY: 0, probeZ: 0,
	probeBlock: "minecraft:bedrock",
}

// ---------------------------------------------------------------------------
// The check
// ---------------------------------------------------------------------------

// TestConformanceVectorIndexed verifies the positive indexed vector. It reads
// the checked-in bytes through an in-memory indexedFile rather than from the
// path, so the artefact cannot be modified by running the suite.
func TestConformanceVectorIndexed(t *testing.T) {
	reg := testRegistry(t)
	file, err := os.ReadFile(indexedFullVector)
	if err != nil {
		t.Fatalf("%v (the vector is checked in; regenerate only for a format revision, with -mkindexedvector)", err)
	}
	w, err := openIndexedOn(&memFile{b: slices.Clone(file)}, "indexed_full.pile", reg, true)
	if err != nil {
		t.Fatalf("the positive indexed vector must open: %v", err)
	}
	defer w.Close()

	// The newest checkpoint is adoptable: unlike indexed_torn, nothing here
	// forces §5.6 recovery, so a reader that fell back would be reporting an
	// older generation's content and this must not be mistaken for success.
	if w.Recovered() {
		t.Error("the file opened by falling back to an older checkpoint; its newest one is intact")
	}
	if got := w.Generation(); got != indexedFullFacts.generation {
		t.Errorf("Generation = %d, want %d", got, indexedFullFacts.generation)
	}

	// The directory names exactly these columns. Compared as a set: which
	// order Positions returns is not a claim this vector makes.
	got := slices.Clone(w.Positions())
	slices.SortFunc(got, func(a, b [2]int32) int {
		if a[0] != b[0] {
			return int(a[0] - b[0])
		}
		return int(a[1] - b[1])
	})
	if !slices.Equal(got, indexedFullFacts.positions) {
		t.Errorf("positions =\n%v\nwant\n%v", got, indexedFullFacts.positions)
	}
	if n := w.ChunkCount(); n != len(indexedFullFacts.positions) {
		t.Errorf("ChunkCount = %d, want %d", n, len(indexedFullFacts.positions))
	}

	// A dictionary frame. The records were written against it and do not
	// decode without it, so a reader that ignores the directory's dictionary
	// reference reads nothing at all: this is the one §5 frame kind no other
	// vector reaches.
	if !w.HasDict() {
		t.Error("the vector carries a shared dictionary and the reader did not find one")
	}

	// More than one block palette segment. Segment frames are cumulative and a
	// reader has to concatenate them in directory order; a file with one
	// segment cannot tell a reader that got that wrong from one that got it
	// right. The count itself is not claimed, only that it is plural.
	if n := len(w.blockSegs); n < 2 {
		t.Errorf("the vector holds %d block palette segment(s); it is built to hold more than one", n)
	}

	// Dead frames the directory does not name. A reader that located content
	// by scanning frames rather than by reading the directory would find
	// superseded records here and could not tell which one wins. The ratio is
	// history, so only its sign is claimed.
	if r := w.GarbageRatio(); r <= 0 {
		t.Errorf("GarbageRatio = %v; the vector is built with superseded records in it", r)
	}

	// Every palette entry resolves, so a reader reporting unresolved states is
	// failing to apply the §3.1 version overrides the segments carry.
	unresolved, err := w.UnresolvedStates()
	if err != nil {
		t.Fatalf("UnresolvedStates: %v", err)
	}
	if len(unresolved) != 0 {
		t.Errorf("unresolved states in a vector whose palette is all vanilla: %v", unresolved)
	}

	// The meta frame. Indexed mode carries the §7 blobs in a frame rather than
	// in the header's meta block, which is why ReadMeta on an indexed file
	// tells a caller nothing about them.
	settings, userData, markers := w.Meta()
	wantSettings, wantUserData, wantMarkers := vecIndexedMeta(t)
	for _, c := range []struct {
		name      string
		got, want []byte
	}{
		{"settings", settings, wantSettings},
		{"userData", userData, wantUserData},
		{"markers", markers, wantMarkers},
	} {
		if !bytes.Equal(c.got, c.want) {
			t.Errorf("meta %s = %x, want %x", c.name, c.got, c.want)
		}
	}

	// One concrete, hand-checkable fact before the hash.
	col, err := w.Column(0, 0)
	if err != nil {
		t.Fatalf("Column(0,0): %v", err)
	}
	f := indexedFullFacts
	runtimeID := col.Col.Chunk.Block(uint8(f.probeX), int16(f.probeY), uint8(f.probeZ), 0)
	b, ok := reg.BlockByRuntimeID(runtimeID)
	if !ok {
		t.Fatalf("block at (%d,%d,%d) has unregistered runtime id %d", f.probeX, f.probeY, f.probeZ, runtimeID)
	}
	if name, _ := b.EncodeBlock(); name != f.probeBlock {
		t.Errorf("block at (%d,%d,%d) is %s, want %s", f.probeX, f.probeY, f.probeZ, name, f.probeBlock)
	}

	// The identity of the content, which is what a second implementation
	// checks its whole reader against.
	if h := vecIndexedContentHash(t, w, reg); h != f.content {
		t.Errorf("content identity = %#016x, want %#016x", h, f.content)
	}
}

// vecIndexedContentHash re-encodes everything an opened indexed world yields as
// an uncompressed solid file and hashes it, which is how §5 files are given an
// identity at all. It is vectorIndexedIdentity over an already-open handle.
func vecIndexedContentHash(t *testing.T, w *IndexedWorld, reg world.BlockRegistry) uint64 {
	t.Helper()
	settings, userData, markers := w.Meta()
	d := &WorldData{
		Settings: settings, UserData: userData, Markers: markers,
	}
	for _, k := range w.Positions() {
		c, err := w.Column(k[0], k[1])
		if err != nil {
			t.Fatal(err)
		}
		d.Columns = append(d.Columns, c)
	}
	var buf bytes.Buffer
	if err := WriteWorld(&buf, d, reg, vecPlain); err != nil {
		t.Fatal(err)
	}
	h, err := ContentHash(buf.Bytes(), reg)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// ---------------------------------------------------------------------------
// The generator
// ---------------------------------------------------------------------------

// vecIndexedMeta is the §7 metadata the vector carries in its meta frame.
func vecIndexedMeta(t *testing.T) (settings, userData, markers []byte) {
	t.Helper()
	var err error
	if settings, err = marshalNBT(map[string]any{
		"name": "indexed vector", "time": int64(6000), "difficulty": int32(1),
	}); err != nil {
		t.Fatal(err)
	}
	if markers, err = marshalNBT(map[string]any{"markers": []map[string]any{
		{"name": "hub", "kind": "spawn", "pos": []any{8.0, 4.0, 8.0}},
	}}); err != nil {
		t.Fatal(err)
	}
	return settings, []byte("indexed-world-user-data"), markers
}

// vecIndexedColumn builds column i. Every column shares one pseudo-random
// 16-cube of stone and dirt, with a bedrock stamp whose position depends on i,
// so the record bodies are nearly identical: that is what gives dictionary
// training something to find, and it is why the file is a few tens of
// kilobytes rather than a megabyte. Three palette entries put the section blob
// at width 1, so a body is a little over four kilobytes and twenty of them
// clear the 64 KiB the trainer refuses to work below.
func vecIndexedColumn(t *testing.T, reg world.BlockRegistry, i int32, extra ...uint32) Column {
	t.Helper()
	ch := chunk.New(reg, vecIndexedRange)
	pal := append([]uint32{rid(t, reg, block.Stone{}), rid(t, reg, block.Dirt{})}, extra...)
	seed := uint32(0x9e3779b9)
	for x := range uint8(16) {
		for z := range uint8(16) {
			for y := int16(0); y <= 15; y++ {
				seed = seed*1664525 + 1013904223
				ch.SetBlock(x, y, z, 0, pal[(seed>>16)%uint32(len(pal))])
			}
		}
	}
	// The per-column stamp, and the block the probe fact names for i == 0.
	ch.SetBlock(uint8(i%16), int16(i%16), uint8((i/16)%16), 0, rid(t, reg, block.Bedrock{}))
	return Column{X: i % 5, Z: i / 5, Col: &chunk.Column{Chunk: ch}}
}

// TestMakeIndexedVector writes indexed_full.pile. It does nothing without
// -mkindexedvector; see the file comment for why this is not -update.
func TestMakeIndexedVector(t *testing.T) {
	if !*mkIndexedVector {
		t.Skip("pass -mkindexedvector to rewrite the indexed vector (format revisions only)")
	}
	reg := testRegistry(t)
	path := indexedFullVector
	_ = os.Remove(path)

	w, err := CreateIndexed(path, reg, Options{Compression: CompressionDefault})
	if err != nil {
		t.Fatal(err)
	}
	settings, userData, markers := vecIndexedMeta(t)
	if err := w.SetMeta(settings, userData, markers); err != nil {
		t.Fatal(err)
	}
	// Twenty columns, then a checkpoint.
	for i := range int32(20) {
		if err := w.Store(vecIndexedColumn(t, reg, i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	// Five of them rewritten, so the first generation's frames stop being
	// reachable through the directory.
	still := rid(t, reg, block.Water{Depth: 8, Still: true})
	for i := range int32(5) {
		if err := w.Store(vecIndexedColumn(t, reg, i, still)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	// Compaction trains a shared dictionary over the live set and rewrites the
	// file around it, folding every palette entry so far into one segment.
	if err := w.Compact(); err != nil {
		t.Fatal(err)
	}
	// Two columns at fresh positions and one rewrite, carrying a block state
	// the compacted segment does not hold, so the file ends with a *second*
	// block palette segment and with a dead frame the newest directory does
	// not name.
	flowing := rid(t, reg, block.Water{Depth: 4, Still: false})
	for _, i := range []int32{20, 21, 3} {
		if err := w.Store(vecIndexedColumn(t, reg, i, still, flowing)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// Report what the checked-in facts have to say, so the constants above can
	// be filled in from a run rather than guessed.
	file, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	r, err := openIndexedOn(&memFile{b: slices.Clone(file)}, path, reg, true)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	pos := slices.Clone(r.Positions())
	slices.SortFunc(pos, func(a, b [2]int32) int {
		if a[0] != b[0] {
			return int(a[0] - b[0])
		}
		return int(a[1] - b[1])
	})
	t.Logf("%s: %d bytes, generation %d, %d columns, dict=%v, segments=%d, garbage=%.3f",
		path, len(file), r.Generation(), r.ChunkCount(), r.HasDict(), len(r.blockSegs), r.GarbageRatio())
	t.Logf("content identity %#016x", vecIndexedContentHash(t, r, reg))
	t.Logf("positions %v", pos)
}
