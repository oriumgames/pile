package format

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
)

// Tests for the normative validity rules the freeze review added: a decoder
// must accept exactly one encoding of any given content.

func TestRejectsNonMinimalVarints(t *testing.T) {
	r := &reader{b: []byte{0x80, 0x00}} // overlong zero
	if _, err := r.uvarint(); err == nil {
		t.Error("overlong uvarint accepted")
	}
	r = &reader{b: []byte{0x80, 0x00}}
	if _, err := r.svarint(); err == nil {
		t.Error("overlong svarint accepted")
	}
	r = &reader{b: []byte{0x00}}
	if v, err := r.uvarint(); err != nil || v != 0 {
		t.Errorf("minimal zero rejected: %v %v", v, err)
	}
}

func TestRejectsNonCanonicalBlob(t *testing.T) {
	// Descending references are not canonical.
	w := &writer{}
	w.uvarint(2)
	w.uvarint(5)
	w.uvarint(3)
	w.u8(widthU8)
	w.raw(make([]byte, 4096))
	if _, err := decodeOneBlob(&reader{b: w.bytes()}); err == nil {
		t.Error("descending palette references accepted")
	}

	// A two-entry palette must use u8 indices, not u16.
	w = &writer{}
	w.uvarint(2)
	w.uvarint(0)
	w.uvarint(1)
	w.u8(widthU16)
	w.raw(make([]byte, 8192))
	if _, err := decodeOneBlob(&reader{b: w.bytes()}); err == nil {
		t.Error("non-minimal index width accepted")
	}
}

func TestRejectsReservedFlags(t *testing.T) {
	reg := testRegistry(t)
	var buf bytes.Buffer
	if err := WriteWorld(&buf, &WorldData{}, reg, Options{Compression: CompressionNone}); err != nil {
		t.Fatal(err)
	}
	file := buf.Bytes()
	for _, bit := range []uint32{1 << 2, 1 << 5, 1 << 15} {
		bad := bytes.Clone(file)
		flags := uint32(bad[8]) | uint32(bad[9])<<8 | uint32(bad[10])<<16 | uint32(bad[11])<<24
		flags |= bit
		bad[8], bad[9], bad[10], bad[11] = byte(flags), byte(flags>>8), byte(flags>>16), byte(flags>>24)
		if _, err := ReadWorld(bad, reg); err == nil {
			t.Errorf("reserved flag bit 0x%X accepted", bit)
		}
	}
}

func TestRejectsUnalignedRange(t *testing.T) {
	reg := testRegistry(t)
	if AlignedRange(cube.Range{1, 16}) {
		t.Error("unaligned range reported as aligned")
	}
	if !AlignedRange(cube.Range{-64, 319}) {
		t.Error("the overworld range reported as unaligned")
	}
	ch := chunk.New(reg, cube.Range{1, 16})
	d := &WorldData{Columns: []Column{{X: 0, Z: 0, Col: &chunk.Column{Chunk: ch}}}}
	if err := WriteWorld(&bytes.Buffer{}, d, reg, Options{}); err == nil {
		t.Error("a column with an unaligned range was encoded")
	}
}

// TestIndexedDetectsFrameCorruption: a shared frame must be authenticated, so
// bit rot in a palette segment or the metadata cannot silently alter content.
func TestIndexedDetectsFrameCorruption(t *testing.T) {
	reg := testRegistry(t)
	path := filepath.Join(t.TempDir(), "auth.pile")
	w, err := CreateIndexed(path, reg, Options{Compression: CompressionNone})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Store(buildTestColumn(t, reg, 0, 0)); err != nil {
		t.Fatal(err)
	}
	settings, _ := marshalNBT(map[string]any{"name": "abc"})
	if err := w.SetMeta(settings, []byte("abc"), nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	idx := bytes.Index(raw, []byte("abc"))
	if idx < 0 {
		t.Fatal("metadata not found in the uncompressed file")
	}
	raw[idx] = 'z'
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	// The newest checkpoint references a frame that no longer matches its
	// hash. Serving the altered value would be the failure; falling back to
	// an older complete checkpoint (or refusing to open) is correct, and the
	// fallback must be visible to the caller.
	r, err := OpenIndexed(path, reg, true)
	if err != nil {
		return // refused outright: also acceptable
	}
	defer r.Close()
	_, ud, _, _ := r.Meta()
	if string(ud) == "zbc" {
		t.Fatal("a corrupted metadata frame was served as valid")
	}
	if !r.Recovered() {
		t.Fatal("fell back to an older checkpoint without reporting it")
	}
}

// TestSettingsExtraKeysPreserved is the metadata-extensibility rule.
func TestSettingsExtraKeysPreserved(t *testing.T) {
	reg := testRegistry(t)
	settings, err := marshalNBT(map[string]any{
		"name": "x", "futureSetting": int32(7),
	})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := WriteWorld(&buf, &WorldData{Settings: settings}, reg, Options{Compression: CompressionNone}); err != nil {
		t.Fatal(err)
	}
	got, err := ReadMeta(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	m, err := UnmarshalNBT(got.Settings)
	if err != nil {
		t.Fatal(err)
	}
	if m["futureSetting"] != int32(7) {
		t.Fatalf("format dropped an unknown settings key: %v", m)
	}
	_ = world.Overworld
}

// Round 11 rules. These are the validity and canonicalisation rules that
// closed the last spec/implementation divergences before freeze.

// TestPaletteOrderFollowsEncodedBytes: the palette's canonical order is
// defined on the bytes an entry encodes to, so an implementation in any
// language reproduces it without first agreeing on a host-language string
// form. Preserved states carry a version and registry states do not, so any
// order defined on a per-kind key would compare two different key spaces
// against each other.
func TestPaletteOrderFollowsEncodedBytes(t *testing.T) {
	b := newBlockPaletteBuilder(testRegistry(t))
	b.addState(BlockState{Name: "audit:zzz", Version: 1})
	b.addState(BlockState{Name: "audit:a", Version: 2})
	enc, remap, err := b.finalize()
	if err != nil {
		t.Fatal(err)
	}
	// Equal counts, so the encoded bytes decide. The length prefix leads, so
	// the shorter name comes first.
	short, long := bytes.Index(enc, []byte("audit:a")), bytes.Index(enc, []byte("audit:zzz"))
	if short < 0 || long < 0 {
		t.Fatalf("palette lost an entry: %x", enc)
	}
	if short > long {
		t.Fatalf("palette order ignores the encoded bytes: audit:a at %d, audit:zzz at %d", short, long)
	}
	if remap[0] == remap[1] {
		t.Fatal("two distinct states collapsed to one palette index")
	}

	// Same state at two versions: distinct entries, ordered by version.
	b2 := newBlockPaletteBuilder(testRegistry(t))
	i1 := b2.addState(BlockState{Name: "audit:x", Version: 7})
	i2 := b2.addState(BlockState{Name: "audit:x", Version: 3})
	_, remap2, err := b2.finalize()
	if err != nil {
		t.Fatal(err)
	}
	if remap2[i1] < remap2[i2] {
		t.Fatal("equal states are not ordered by ascending version")
	}
}

// TestPaletteMergesIdenticalEntries: two entries that encode identically at
// the same version are the same state. Keeping both would put two
// indistinguishable entries in the palette with nothing to order them by.
func TestPaletteMergesIdenticalEntries(t *testing.T) {
	b := newBlockPaletteBuilder(testRegistry(t))
	i1 := b.addState(BlockState{Name: "audit:same", Properties: map[string]any{"k": int32(1)}})
	b.ent = append(b.ent, blockPaletteEntry{
		name: "audit:same", props: map[string]any{"k": int32(1)}, count: 1,
	})
	i2 := uint32(len(b.ent) - 1)
	_, remap, err := b.finalize()
	if err != nil {
		t.Fatal(err)
	}
	if remap[i1] != remap[i2] {
		t.Fatalf("identical states kept separate palette indices %d and %d", remap[i1], remap[i2])
	}
}

// TestRejectsDuplicateChunks: two columns at one position have equal Morton
// keys, so nothing but the caller's slice order would decide which record
// comes first, and a reader has no defined way to choose between them.
func TestRejectsDuplicateChunks(t *testing.T) {
	reg := testRegistry(t)
	d := testWorld(t, reg)
	d.Columns = append(d.Columns, d.Columns[0])
	var buf bytes.Buffer
	if err := WriteWorld(&buf, d, reg, Options{Compression: CompressionNone}); err == nil {
		t.Fatal("a world with two columns at one position was accepted")
	}
}

// TestOpaqueNBTArraysSurvive: array tags round trip. Normalising them to lists
// would be lossy in a way vanilla content notices, since Bedrock stores UUIDs
// and similar fields as int arrays.
func TestOpaqueNBTArraysSurvive(t *testing.T) {
	in := map[string]any{
		"ba": [2]byte{1, 2}, "ia": [2]int32{3, 4}, "la": [2]int64{5, 6},
		"list": []int32{7, 8},
	}
	first, err := marshalNBT(in)
	if err != nil {
		t.Fatal(err)
	}
	back, err := unmarshalNBT(first)
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"ba": "[2]uint8", "ia": "[2]int32", "la": "[2]int64", "list": "[]int32"} {
		if got := fmt.Sprintf("%T", back[k]); got != want {
			t.Fatalf("%s decoded as %s, want %s: the array/list distinction is lost", k, got, want)
		}
	}
	second, err := marshalNBT(back)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("opaque NBT arrays do not survive a round trip")
	}
}

// TestRejectsBorderSchema: §7.3 fixes the tag of every border field. A
// dynamically typed decoder cannot tell a list from an array after the fact,
// so the tags have to be right on the way in.
func TestRejectsBorderSchema(t *testing.T) {
	reg := testRegistry(t)
	d := testWorld(t, reg)
	bad, err := marshalNBT(map[string]any{"min": []int32{-1, -1}, "max": [2]int32{1, 1}})
	if err != nil {
		t.Fatal(err)
	}
	d.Border = bad
	var buf bytes.Buffer
	if err := WriteWorld(&buf, d, reg, Options{Compression: CompressionNone}); err == nil {
		t.Fatal("a border whose min is a list was accepted")
	}
}

// TestCollectionTiesUseWrittenBytes: block entity coordinates are stripped and
// an entity's UniqueID is overwritten with its authoritative ID, so neither
// may decide the order. Two worlds differing only in those discarded values
// must encode identically.
func TestCollectionTiesUseWrittenBytes(t *testing.T) {
	reg := testRegistry(t)
	build := func(x1, x2 int32, u1, u2 int64) []byte {
		ch := chunk.New(reg, cube.Range{-64, 319})
		stone, _ := reg.StateToRuntimeID("minecraft:stone", map[string]any{})
		ch.SetBlock(0, -64, 0, 0, stone)
		d := &WorldData{Columns: []Column{{X: 0, Z: 0, Col: &chunk.Column{
			Chunk: ch,
			// Same position, same effective content: only the stripped
			// coordinates differ.
			BlockEntities: []chunk.BlockEntity{
				{Pos: cube.Pos{1, -60, 1}, Data: map[string]any{"id": "minecraft:chest", "x": x1}},
				{Pos: cube.Pos{1, -60, 1}, Data: map[string]any{"id": "minecraft:chest", "x": x2}},
			},
			// Same ID, same effective content: only the overwritten UniqueID
			// differs.
			Entities: []chunk.Entity{
				{ID: 5, Data: map[string]any{"identifier": "minecraft:cow", "UniqueID": u1}},
				{ID: 5, Data: map[string]any{"identifier": "minecraft:cow", "UniqueID": u2}},
			},
		}}}}
		var buf bytes.Buffer
		if err := WriteWorld(&buf, d, reg, Options{Compression: CompressionNone}); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	if a, b := build(1, 2, 11, 22), build(2, 1, 22, 11); !bytes.Equal(a, b) {
		t.Fatalf("discarded NBT fields decided the record order: %d vs %d bytes", len(a), len(b))
	}
}

// TestStructureOriginExtremes: the paste anchor is an int32 and must be
// representable across its whole domain, since the decoder now refuses to
// narrow anything wider.
func TestStructureOriginExtremes(t *testing.T) {
	reg := testRegistry(t)
	data, err := NewStructureData([3]int32{16, 16, 16})
	if err != nil {
		t.Fatal(err)
	}
	data.Origin = [3]int32{math.MinInt32, 0, math.MaxInt32}
	var buf bytes.Buffer
	if err := WriteStructure(&buf, data, reg, Options{Compression: CompressionNone}); err != nil {
		t.Fatal(err)
	}
	got, err := ReadStructure(buf.Bytes(), reg)
	if err != nil {
		t.Fatal(err)
	}
	if got.Origin != data.Origin {
		t.Fatalf("origin = %v, want %v", got.Origin, data.Origin)
	}
}

// TestPaletteIndexWidthBoundary: 256 palette entries still address with one
// byte, 257 need two. The dense golden sits well past the boundary, so the
// transition itself needs its own check.
func TestPaletteIndexWidthBoundary(t *testing.T) {
	reg := testRegistry(t)
	size := func(states int) int {
		ch := chunk.New(reg, cube.Range{-64, 319})
		placeholder := placeholderRid(reg)
		c := Column{X: 0, Z: 0, Col: &chunk.Column{Chunk: ch}}
		for i := range states {
			x, z := uint8(i%16), uint8((i/16)%16)
			ch.SetBlock(x, -64, z, 0, placeholder)
			c.UnknownStates = append(c.UnknownStates, BlockState{
				Name: fmt.Sprintf("audit:w%04d", i), Version: 1,
			})
			c.Unknown = append(c.Unknown, UnknownBlock{
				Section: -4, Layer: 0,
				Index: uint16(x)<<8 | uint16(z)<<4, State: uint32(i),
			})
		}
		var buf bytes.Buffer
		if err := WriteWorld(&buf, &WorldData{Columns: []Column{c}}, reg, Options{Compression: CompressionNone}); err != nil {
			t.Fatal(err)
		}
		return buf.Len()
	}
	// 255 preserved states plus air is 256 entries: one byte per index.
	// 256 plus air is 257: two bytes, so the section's index array doubles.
	small, large := size(255), size(256)
	if grew := large - small; grew < 4000 {
		t.Fatalf("crossing 256 palette entries grew the file by only %d bytes: "+
			"the two-byte index path may not have engaged", grew)
	}
}

// TestIndexedRejectsInvalidState: solid mode validates states when it
// finalizes its palette. Indexed mode appends as it goes and has no such
// moment, so the same rules apply at admission: without them a save succeeds
// and the reopen rejects the segment, rolling the world back.
func TestIndexedRejectsInvalidState(t *testing.T) {
	reg := testRegistry(t)
	path := filepath.Join(t.TempDir(), "w.pile")
	w, err := CreateIndexed(path, reg, Options{Compression: CompressionNone})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	props := make(map[string]any, 65)
	for i := range 65 {
		props[fmt.Sprintf("p%02d", i)] = int32(i)
	}
	c := buildTestColumn(t, reg, 0, 0)
	c.Col.Chunk.SetBlock(0, -64, 0, 0, placeholderRid(reg))
	c.UnknownStates = []BlockState{{Name: "audit:toowide", Properties: props, Version: 1}}
	c.Unknown = []UnknownBlock{{Section: -4, Layer: 0, Index: 0, State: 0}}
	if err := w.Store(c); err == nil {
		t.Fatal("a state with 65 properties was admitted to an indexed palette")
	}
}

// TestIndexedSetMetaRejectsNonCanonical: a checkpoint that fails to reopen
// rolls the world back to an older one, losing every chunk stored since, so
// metadata has to be refused at the point it is handed over.
func TestIndexedSetMetaRejectsNonCanonical(t *testing.T) {
	reg := testRegistry(t)
	path := filepath.Join(t.TempDir(), "w.pile")
	w, err := CreateIndexed(path, reg, Options{Compression: CompressionNone})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	// Keys out of order: structurally sound, but not canonical.
	bad := []byte{0x0a, 0, 0,
		0x01, 1, 0, 'b', 1,
		0x01, 1, 0, 'a', 1,
		0x00}
	if err := w.SetMeta(bad, nil, nil, nil); err == nil {
		t.Fatal("SetMeta accepted a settings blob the reader rejects")
	}
	if err := w.SetMeta(nil, nil, nil, []byte{0x0a, 0, 0, 0x00}); err == nil {
		t.Fatal("SetMeta accepted a border with no min or max")
	}
}

// TestRecoveredHeaderCheckpointVerifies: the directory prologue is the
// authoritative header image, so a checkpoint written after recovering from a
// damaged physical header must verify against that image. Hashing the damaged
// bytes instead would tie the world to its own corruption: repairing the
// header, or opening it with a reader that trusts the prologue, would
// invalidate the newest checkpoint and roll back everything stored since.
func TestRecoveredHeaderCheckpointVerifies(t *testing.T) {
	reg := testRegistry(t)
	path := filepath.Join(t.TempDir(), "w.pile")
	w, err := CreateIndexed(path, reg, Options{Compression: CompressionNone})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Store(buildTestColumn(t, reg, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// Damage the header's block version, which the directory also records.
	file, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	file[12] ^= 0xff
	if err := os.WriteFile(path, file, 0o644); err != nil {
		t.Fatal(err)
	}

	w, err = OpenIndexed(path, reg, false)
	if err != nil {
		t.Fatalf("a world with a damaged header must still open: %v", err)
	}
	if err := w.Store(buildTestColumn(t, reg, 1, 0)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// Verify the newest checkpoint the way an independent reader would: from
	// the directory prologue, not from the bytes on disk at offset 0.
	file, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	footer := file[len(file)-footerSize:]
	want := binary.LittleEndian.Uint64(footer[0:8])
	dirOff := binary.LittleEndian.Uint64(footer[8:16])
	dirLen := binary.LittleEndian.Uint64(footer[16:24])
	frame := file[dirOff : dirOff+dirLen]
	img, ok := prologueHeaderImage(frame)
	if !ok {
		t.Fatal("the directory prologue is unreadable")
	}
	if got := checkpointHash(img, frame, footer[8:]); got != want {
		t.Fatalf("checkpoint written after recovery does not verify against the "+
			"directory prologue: got %016x, want %016x", got, want)
	}
}

// Round 12 rules.

// aliasRegistry gives one block state two runtime IDs, which a custom registry
// is free to do. The palette holds one entry per unique state, so both IDs must
// collapse to a single entry and any section using both must come out uniform.
type aliasRegistry struct {
	world.BlockRegistry
	alias uint32 // the extra ID, a duplicate of alias-1's state
}

func newAliasRegistry(base world.BlockRegistry) *aliasRegistry {
	return &aliasRegistry{BlockRegistry: base, alias: uint32(base.BlockCount())}
}

func (r *aliasRegistry) BlockCount() int { return r.BlockRegistry.BlockCount() + 1 }

func (r *aliasRegistry) RuntimeIDToState(rid uint32) (string, map[string]any, bool) {
	if rid == r.alias {
		rid = r.alias - 1
	}
	return r.BlockRegistry.RuntimeIDToState(rid)
}

// TestAliasedRuntimeIDsCollapse: two runtime IDs describing one state must
// produce one palette entry and, where a section holds only those two, a
// uniform section. Emitting the reference twice would break the strictly
// ascending rule the reader enforces, and would hold the section at a wider
// index than its true entry count needs.
func TestAliasedRuntimeIDsCollapse(t *testing.T) {
	reg := newAliasRegistry(testRegistry(t))
	twin := reg.alias - 1

	// Fill a whole section, so a correct merge leaves it genuinely uniform.
	ch := chunk.New(reg, cube.Range{-64, 319})
	for x := range uint8(16) {
		for z := range uint8(16) {
			for y := int16(-64); y < -48; y++ {
				rid := twin
				if (x+z+uint8(y&15))%2 == 0 {
					rid = reg.alias
				}
				ch.SetBlock(x, y, z, 0, rid)
			}
		}
	}
	d := &WorldData{Columns: []Column{{X: 0, Z: 0, Col: &chunk.Column{Chunk: ch}}}}
	var buf bytes.Buffer
	if err := WriteWorld(&buf, d, reg, Options{Compression: CompressionNone}); err != nil {
		t.Fatal(err)
	}
	// The reader rejects a section palette whose references repeat, so a
	// failure to merge shows up here first.
	got, err := ReadWorld(buf.Bytes(), reg)
	if err != nil {
		t.Fatalf("aliased runtime IDs produced a file the reader rejects: %v", err)
	}
	sub := got.Columns[0].Col.Chunk.Sub()[0]
	if n := sub.Layers()[0].Palette().Len(); n != 1 {
		t.Fatalf("section palette has %d entries, want 1: aliased states did not merge", n)
	}

	// The same content addressed through a single runtime ID must give the
	// same bytes: which ID a registry happened to hand out is not content.
	plain := chunk.New(reg, cube.Range{-64, 319})
	for x := range uint8(16) {
		for z := range uint8(16) {
			for y := int16(-64); y < -48; y++ {
				plain.SetBlock(x, y, z, 0, twin)
			}
		}
	}
	var single bytes.Buffer
	if err := WriteWorld(&single, &WorldData{Columns: []Column{{X: 0, Z: 0,
		Col: &chunk.Column{Chunk: plain}}}}, reg, Options{Compression: CompressionNone}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), single.Bytes()) {
		t.Fatalf("aliased and single-ID encodings differ: %d vs %d bytes", buf.Len(), single.Len())
	}
}

// TestIndexedAliasedRuntimeIDsCollapse: indexed palettes are first-seen order,
// but "one entry per unique state" is not waived by that.
func TestIndexedAliasedRuntimeIDsCollapse(t *testing.T) {
	reg := newAliasRegistry(testRegistry(t))
	twin := reg.alias - 1
	path := filepath.Join(t.TempDir(), "w.pile")
	w, err := CreateIndexed(path, reg, Options{Compression: CompressionNone})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	ch := chunk.New(reg, cube.Range{-64, 319})
	ch.SetBlock(0, -64, 0, 0, twin)
	ch.SetBlock(1, -64, 0, 0, reg.alias)
	if err := w.Store(Column{X: 0, Z: 0, Col: &chunk.Column{Chunk: ch}}); err != nil {
		t.Fatal(err)
	}
	got, err := w.Column(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if a, b := got.Col.Chunk.Block(0, -64, 0, 0), got.Col.Chunk.Block(1, -64, 0, 0); a != b {
		t.Fatalf("aliased states resolved to different blocks %d and %d", a, b)
	}
	if n := got.Col.Chunk.Sub()[0].Layers()[0].Palette().Len(); n != 2 {
		// air plus the one merged state
		t.Fatalf("section palette has %d entries, want 2: the aliases did not merge", n)
	}
}

// TestEntityIDZeroRoundTrips: zero is a legal UniqueID. Replacing it on read
// would make encode, decode and encode again produce different bytes.
func TestEntityIDZeroRoundTrips(t *testing.T) {
	reg := testRegistry(t)
	ch := chunk.New(reg, cube.Range{-64, 319})
	stone, _ := reg.StateToRuntimeID("minecraft:stone", map[string]any{})
	ch.SetBlock(0, -64, 0, 0, stone)
	d := &WorldData{Columns: []Column{{X: 0, Z: 0, Col: &chunk.Column{
		Chunk:    ch,
		Entities: []chunk.Entity{{ID: 0, Data: map[string]any{"identifier": "minecraft:cow"}}},
	}}}}
	first := encode(t, d, reg, CompressionNone)
	back, err := ReadWorld(first, reg)
	if err != nil {
		t.Fatal(err)
	}
	if id := back.Columns[0].Col.Entities[0].ID; id != 0 {
		t.Fatalf("UniqueID 0 came back as %d: the reader rewrote a legal value", id)
	}
	if second := encode(t, back, reg, CompressionNone); !bytes.Equal(first, second) {
		t.Fatal("a zero UniqueID does not survive encode, decode and encode")
	}
}

// TestRejectsNonUTF8Strings: the string primitive is UTF-8 by rule, not by
// description. Palettes are ordered bytewise, so arbitrary bytes would order
// differently under an implementation that decodes before comparing.
func TestRejectsNonUTF8Strings(t *testing.T) {
	reg := testRegistry(t)
	d := testWorld(t, reg)
	d.Columns[0].UnknownStates = []BlockState{{Name: string([]byte{0xff}), Version: 1}}
	d.Columns[0].Unknown = []UnknownBlock{{Section: -4, Layer: 0, Index: 0, State: 0}}
	d.Columns[0].Col.Chunk.SetBlock(0, -64, 0, 0, placeholderRid(reg))
	var buf bytes.Buffer
	if err := WriteWorld(&buf, d, reg, Options{Compression: CompressionNone}); err == nil {
		t.Fatal("a block state name that is not valid UTF-8 was accepted")
	}
}

// TestUnknownDefaultBiomePreserved: the default biome can itself be a name no
// registry resolves, and its sections are elided from the file. Without a
// sidecar entry for them the name survives in the bytes but not through a read
// and rewrite.
func TestUnknownDefaultBiomePreserved(t *testing.T) {
	reg := testRegistry(t)
	ch := chunk.New(reg, cube.Range{-64, 319})
	stone, _ := reg.StateToRuntimeID("minecraft:stone", map[string]any{})
	ch.SetBlock(0, -64, 0, 0, stone)
	// The sidecar only re-substitutes where the reader's fallback biome is
	// still in place, so every section has to hold that fallback. Marking all
	// of them makes the unresolved name the most common one, which is what
	// puts it in the header as the default and elides every section.
	plains := uint32(1)
	if b, ok := world.BiomeByName(plainsBiomeName()); ok {
		plains = uint32(b.EncodeBiome())
	}
	r := ch.Range()
	for x := range uint8(16) {
		for z := range uint8(16) {
			for y := int16(r[0]); y <= int16(r[1]); y++ {
				ch.SetBiome(x, y, z, plains)
			}
		}
	}
	var unknownBiomes []UnknownBlock
	for sec := int32(r[0] >> 4); sec <= int32(r[1]>>4); sec++ {
		unknownBiomes = append(unknownBiomes, UnknownBlock{
			Section: sec, Index: WholeStorage, State: 0,
		})
	}
	d := &WorldData{Columns: []Column{{
		X: 0, Z: 0, Col: &chunk.Column{Chunk: ch},
		UnknownBiomeNames: []string{"audit:unknown"},
		UnknownBiomes:     unknownBiomes,
	}}}
	first := encode(t, d, reg, CompressionNone)
	back, err := ReadWorld(first, reg)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Columns[0].UnknownBiomes) == 0 {
		t.Fatal("the unresolved default biome was not reported: a rewrite renames it")
	}
	if second := encode(t, back, reg, CompressionNone); !bytes.Equal(first, second) {
		t.Fatal("an unresolved default biome does not survive encode, decode and encode")
	}
}

// TestBiomeNamesAreNamespaced: biome names are fully qualified on the wire.
// Dragonfly names vanilla biomes bare, but a bare name is not a stable
// identifier outside its own registry, and accepting both spellings would give
// one biome two encodings.
func TestBiomeNamesAreNamespaced(t *testing.T) {
	reg := testRegistry(t)
	file := encode(t, testWorld(t, reg), reg, CompressionNone)
	body := file[headerSize : len(file)-footerSize]
	if !bytes.Contains(body, []byte("minecraft:ocean")) {
		t.Fatal("the biome palette does not carry a namespaced name")
	}
	if bytes.Contains(body, []byte("\x05ocean")) {
		t.Fatal("the biome palette still carries a bare name")
	}

	// A bare name is not a second spelling of the same biome; it is invalid.
	// Rewriting the length prefix and name in place keeps every later offset.
	patched := bytes.Clone(file)
	i := bytes.Index(patched, []byte("\x0fminecraft:ocean"))
	if i < 0 {
		t.Fatal("namespaced name not found for patching")
	}
	copy(patched[i:], append([]byte{0x05}, []byte("oceanXXXXXXXXXX")...))
	rehashSolid(patched)
	if _, err := ReadWorld(patched, reg); err == nil {
		t.Fatal("an unnamespaced biome name was accepted")
	}
}

// Round 13 rules.

// TestInternalAirLayerSurvives: layer numbers are semantic. Layer 1 is the
// waterlogging layer, so dropping an all-air layer 0 beneath it renumbers
// everything above and turns waterlogging into a solid liquid.
func TestInternalAirLayerSurvives(t *testing.T) {
	reg := testRegistry(t)
	water, _ := reg.StateToRuntimeID("minecraft:water", map[string]any{"liquid_depth": int32(0)})
	stone, _ := reg.StateToRuntimeID("minecraft:stone", map[string]any{})

	ch := chunk.New(reg, cube.Range{-64, 319})
	ch.SetBlock(0, 100, 0, 0, stone) // a different section, so this one stays air
	ch.SetBlock(5, -60, 5, 1, water) // layer 1 only: layer 0 is uniformly air
	d := &WorldData{Columns: []Column{{X: 0, Z: 0, Col: &chunk.Column{Chunk: ch}}}}

	first := encode(t, d, reg, CompressionNone)
	got, err := ReadWorld(first, reg)
	if err != nil {
		t.Fatal(err)
	}
	gc := got.Columns[0].Col.Chunk
	if rid := gc.Block(5, -60, 5, 1); rid != water {
		t.Fatalf("water left layer 1 (found %d there): an internal air layer was dropped", rid)
	}
	if rid := gc.Block(5, -60, 5, 0); rid == water {
		t.Fatal("water arrived in layer 0: waterlogging became a liquid block")
	}
	if second := encode(t, got, reg, CompressionNone); !bytes.Equal(first, second) {
		t.Fatal("a chunk with an internal air layer does not survive encode, decode and encode")
	}
}

// TestStructureInternalAirLayerSurvives is the same rule for structure cells.
func TestStructureInternalAirLayerSurvives(t *testing.T) {
	reg := testRegistry(t)
	water, _ := reg.StateToRuntimeID("minecraft:water", map[string]any{"liquid_depth": int32(0)})
	air := reg.AirRuntimeID()

	data, err := NewStructureData([3]int32{16, 16, 16})
	if err != nil {
		t.Fatal(err)
	}
	sub := chunk.NewSubChunk(air)
	sub.SetBlock(1, 2, 3, 1, water) // layer 1 only
	data.Cells[0] = sub

	var buf bytes.Buffer
	if err := WriteStructure(&buf, data, reg, Options{Compression: CompressionNone}); err != nil {
		t.Fatal(err)
	}
	got, err := ReadStructure(buf.Bytes(), reg)
	if err != nil {
		t.Fatal(err)
	}
	cell := got.Cells[0]
	if cell == nil {
		t.Fatal("the cell holding only a waterlogging layer was dropped")
	}
	if rid := cell.Block(1, 2, 3, 1); rid != water {
		t.Fatalf("water left layer 1 (found %d there)", rid)
	}
}

// TestTrailingAirLayersDropped: the flip side of the rule. A layer past the
// last stored one already reads as air, so trailing all-air layers must not
// reach the file, or one chunk would have an encoding per spare layer.
func TestTrailingAirLayersDropped(t *testing.T) {
	reg := testRegistry(t)
	stone, _ := reg.StateToRuntimeID("minecraft:stone", map[string]any{})
	air := reg.AirRuntimeID()

	bare := chunk.New(reg, cube.Range{-64, 319})
	bare.SetBlock(0, -64, 0, 0, stone)

	padded := chunk.New(reg, cube.Range{-64, 319})
	padded.SetBlock(0, -64, 0, 0, stone)
	padded.SetBlock(0, -64, 0, 3, air) // forces layers 1..3 into existence

	a := encode(t, &WorldData{Columns: []Column{{X: 0, Z: 0, Col: &chunk.Column{Chunk: bare}}}}, reg, CompressionNone)
	b := encode(t, &WorldData{Columns: []Column{{X: 0, Z: 0, Col: &chunk.Column{Chunk: padded}}}}, reg, CompressionNone)
	if !bytes.Equal(a, b) {
		t.Fatalf("spare trailing air layers reached the file: %d vs %d bytes", len(a), len(b))
	}
}

// TestRejectsMetaSchemaViolations: §7 fixes the tag of every field it names,
// and a dynamically typed decoder cannot tell afterwards which tag a value came
// from, so the blobs have to be right on the way in.
func TestRejectsMetaSchemaViolations(t *testing.T) {
	reg := testRegistry(t)
	write := func(d *WorldData) error {
		var buf bytes.Buffer
		return WriteWorld(&buf, d, reg, Options{Compression: CompressionNone})
	}

	// time is a long, not an int.
	d := testWorld(t, reg)
	bad, err := marshalNBT(map[string]any{"name": "x", "time": int32(5)})
	if err != nil {
		t.Fatal(err)
	}
	d.Settings = bad
	if err := write(d); err == nil {
		t.Fatal("settings with time as an int were accepted")
	}

	// Markers must be sorted by name, strictly.
	d = testWorld(t, reg)
	unsorted, err := marshalNBT(map[string]any{"markers": []map[string]any{
		{"name": "b", "kind": "region", "pos": []any{0.0, 0.0, 0.0}},
		{"name": "a", "kind": "region", "pos": []any{0.0, 0.0, 0.0}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	d.Markers = unsorted
	if err := write(d); err == nil {
		t.Fatal("an unsorted marker list was accepted")
	}

	// A marker position is three doubles.
	d = testWorld(t, reg)
	badPos, err := marshalNBT(map[string]any{"markers": []map[string]any{
		{"name": "a", "kind": "region", "pos": []any{float32(0), float32(0), float32(0)}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	d.Markers = badPos
	if err := write(d); err == nil {
		t.Fatal("a marker position of floats was accepted")
	}
}

// TestIndexedRecoversDamagedKindAndMode: only the magic and version have to
// survive in the physical header. Kind and mode are carried by the directory
// prologue, which the checkpoint hash authenticates, so damage to the physical
// bytes must not defeat a file that is otherwise intact.
func TestIndexedRecoversDamagedKindAndMode(t *testing.T) {
	reg := testRegistry(t)
	path := filepath.Join(t.TempDir(), "w.pile")
	w, err := CreateIndexed(path, reg, Options{Compression: CompressionNone})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Store(buildTestColumn(t, reg, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	file, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	file[6] = KindStructure // kind
	file[7] = ModeSolid     // mode
	if err := os.WriteFile(path, file, 0o644); err != nil {
		t.Fatal(err)
	}

	w, err = OpenIndexed(path, reg, true)
	if err != nil {
		t.Fatalf("a damaged physical kind and mode defeated recovery: %v", err)
	}
	defer w.Close()
	if !w.HeaderDamaged() {
		t.Fatal("the damaged physical header was not reported")
	}
	if w.ChunkCount() != 1 {
		t.Fatalf("recovered chunks = %d, want 1", w.ChunkCount())
	}
}

// TestEmptyChunkKeepsFullSpan: the section span is the chunk's whole vertical
// range, never trimmed. Trimming would give one chunk several encodings and
// leave the content outside the span undefined.
func TestEmptyChunkKeepsFullSpan(t *testing.T) {
	reg := testRegistry(t)
	r := cube.Range{-64, 319}
	ch := chunk.New(reg, r)
	d := &WorldData{Columns: []Column{{X: 0, Z: 0, Col: &chunk.Column{Chunk: ch}}}}
	file := encode(t, d, reg, CompressionNone)
	got, err := ReadWorld(file, reg)
	if err != nil {
		t.Fatal(err)
	}
	if gr := got.Columns[0].Col.Chunk.Range(); gr != r {
		t.Fatalf("empty chunk came back with range %v, want %v: the span was trimmed", gr, r)
	}
	if second := encode(t, got, reg, CompressionNone); !bytes.Equal(file, second) {
		t.Fatal("an empty chunk does not survive encode, decode and encode")
	}
}

// Round 14 rules.

// TestVersionZeroRoundTrips: a decoder gives a preserved state an explicit
// version, so a later save at a different runtime version still says what the
// state was expressed at. When that version is the writer's own, it means the
// same thing as zero and must not become an override, or a round trip that
// changed nothing grows the file.
func TestVersionZeroRoundTrips(t *testing.T) {
	reg := testRegistry(t)
	ch := chunk.New(reg, cube.Range{-64, 319})
	ch.SetBlock(0, -64, 0, 0, placeholderRid(reg))
	d := &WorldData{Columns: []Column{{
		X: 0, Z: 0, Col: &chunk.Column{Chunk: ch},
		UnknownStates: []BlockState{{Name: "audit:missing", Version: 0}},
		Unknown:       []UnknownBlock{{Section: -4, Layer: 0, Index: 0, State: 0}},
	}}}
	first := encode(t, d, reg, CompressionNone)
	back, err := ReadWorld(first, reg)
	if err != nil {
		t.Fatal(err)
	}
	if second := encode(t, back, reg, CompressionNone); !bytes.Equal(first, second) {
		t.Fatalf("a version-zero preserved state does not survive a round trip: %d then %d bytes",
			len(first), len(second))
	}
}

// TestMaxLayerCellPadding: a cell at the layer limit still has its out-of-box
// padding cleared. Counting the layers in a uint8 wrapped the limit to zero and
// skipped the padding, so two structures differing only outside their own
// bounds encoded differently.
func TestMaxLayerCellPadding(t *testing.T) {
	reg := testRegistry(t)
	stone, _ := reg.StateToRuntimeID("minecraft:stone", map[string]any{})
	dirt, _ := reg.StateToRuntimeID("minecraft:dirt", map[string]any{})
	air := reg.AirRuntimeID()

	build := func(pad uint32) []byte {
		data, err := NewStructureData([3]int32{1, 1, 1})
		if err != nil {
			t.Fatal(err)
		}
		sub := chunk.NewSubChunk(air)
		// Force 256 layers into existence, then place blocks in the last one.
		sub.SetBlock(0, 0, 0, maxLayers-1, stone)
		sub.SetBlock(15, 15, 15, maxLayers-1, pad) // outside the 1x1x1 box
		data.Cells[0] = sub
		var buf bytes.Buffer
		if err := WriteStructure(&buf, data, reg, Options{Compression: CompressionNone}); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	if a, b := build(stone), build(dirt); !bytes.Equal(a, b) {
		t.Fatalf("content outside the structure box reached the file: %d vs %d bytes", len(a), len(b))
	}
}

// TestRejectsUnrepresentableRange: block Y is an int16 throughout dragonfly's
// chunk API, so an aligned but astronomically high range is not representable.
// Accepting one narrows the section index into a completely different range.
func TestRejectsUnrepresentableRange(t *testing.T) {
	reg := testRegistry(t)
	ch := chunk.New(reg, cube.Range{-64, 319})
	d := &WorldData{Columns: []Column{{X: 0, Z: 0, Col: &chunk.Column{Chunk: ch}}}}
	if err := validateColumn(d.Columns[0]); err != nil {
		t.Fatalf("an ordinary range was rejected: %v", err)
	}
	// cube.Range is a pair of ints, so it can hold more than the chunk API can
	// address: this one is aligned and a single section high, but its block Y
	// is past int16.
	high := Column{X: 0, Z: 0, Col: &chunk.Column{Chunk: chunk.New(reg, cube.Range{32768, 32783})}}
	if err := validateColumn(high); err == nil {
		t.Fatal("a range outside the int16 block Y domain was accepted")
	}
}

// TestRejectsUnaddressableLayerCount: dragonfly grows a sub chunk's storage
// slice with `for uint8(len(storages)) <= layer`, so a 256th layer makes that
// comparison wrap and append without end. A file claiming one would hang the
// server that read it, which makes this a rule about what a decoder accepts
// rather than a detail of what a writer happens to produce.
func TestRejectsUnaddressableLayerCount(t *testing.T) {
	if maxLayers != 255 {
		t.Fatalf("maxLayers = %d: a layer count dragonfly cannot address is reachable", maxLayers)
	}
	reg := testRegistry(t)
	file := encode(t, testWorld(t, reg), reg, CompressionNone)
	body := file[headerSize : len(file)-footerSize]

	// Find a record's layer count and claim one layer past the limit.
	// layerN is a uvarint immediately after the block presence bitset, and
	// every fixture record stores exactly one layer, so the first 0x01 after
	// the palettes is it. Rather than hunt for the offset, assert the reader's
	// bound directly: it is what stands between a hostile file and the hang.
	if _, err := (&reader{b: []byte{0x80, 0x02}}).count(maxLayers, "layer"); err == nil {
		t.Fatal("a layer count of 256 was accepted")
	}
	if len(body) == 0 {
		t.Fatal("empty body")
	}
}

// Consolidation pass rules.

// TestRejectsReservedDimension: the dimension field has three defined values
// and five reserved ones. Reserved values are rejected rather than ignored, so
// they stay available to a later version.
func TestRejectsReservedDimension(t *testing.T) {
	reg := testRegistry(t)
	d := testWorld(t, reg)
	d.Dimension = Dimension(5)
	var buf bytes.Buffer
	if err := WriteWorld(&buf, d, reg, Options{Compression: CompressionNone}); err == nil {
		t.Fatal("a reserved dimension was written")
	}

	// And on the way in: set the bits directly in a valid file's header.
	file := encode(t, testWorld(t, reg), reg, CompressionNone)
	for _, v := range []uint32{3, 5, 7} {
		bad := bytes.Clone(file)
		flags := binary.LittleEndian.Uint32(bad[8:12])
		binary.LittleEndian.PutUint32(bad[8:12], flags|v<<dimensionShift)
		rehashSolid(bad)
		if _, err := ReadWorld(bad, reg); err == nil {
			t.Fatalf("reserved dimension %d was accepted", v)
		}
	}

	// The defined ones round trip.
	for _, dim := range []Dimension{Overworld, Nether, End} {
		w := testWorld(t, reg)
		w.Dimension = dim
		got, err := ReadWorld(encode(t, w, reg, CompressionNone), reg)
		if err != nil {
			t.Fatal(err)
		}
		if got.Dimension != dim {
			t.Fatalf("dimension %v came back as %v", dim, got.Dimension)
		}
	}
}

// TestStructureLeavesDimensionBitsZero: a structure is not a dimension, so the
// field is meaningless there and must be clear, or one structure would have
// eight encodings.
func TestStructureLeavesDimensionBitsZero(t *testing.T) {
	reg := testRegistry(t)
	data, err := NewStructureData([3]int32{16, 16, 16})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := WriteStructure(&buf, data, reg, Options{Compression: CompressionNone}); err != nil {
		t.Fatal(err)
	}
	file := buf.Bytes()
	if flags := binary.LittleEndian.Uint32(file[8:12]); flags&dimensionMask != 0 {
		t.Fatalf("structure header flags 0x%08X set dimension bits", flags)
	}
	bad := bytes.Clone(file)
	binary.LittleEndian.PutUint32(bad[8:12], binary.LittleEndian.Uint32(bad[8:12])|1<<dimensionShift)
	rehashSolid(bad)
	if _, err := ReadStructure(bad, reg); err == nil {
		t.Fatal("a structure carrying a dimension was accepted")
	}
}

// TestRejectsIndexedStructureKind: a structure is always solid, so the
// kind/mode pair naming an indexed structure has no layout and must be refused
// rather than half-interpreted.
func TestRejectsIndexedStructureKind(t *testing.T) {
	reg := testRegistry(t)
	data, err := NewStructureData([3]int32{16, 16, 16})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := WriteStructure(&buf, data, reg, Options{Compression: CompressionNone}); err != nil {
		t.Fatal(err)
	}
	bad := buf.Bytes()
	bad[7] = ModeIndexed
	rehashSolid(bad)
	if _, err := ReadStructure(bad, reg); err == nil {
		t.Fatal("an indexed structure was accepted")
	}
	if _, err := ReadWorld(bad, reg); err == nil {
		t.Fatal("an indexed structure was accepted as a world")
	}
}

// TestPaletteCountsOccurrencesNotBlobs: the palette's reference count is taken
// before the blob table deduplicates, so a section blob shared by many
// sections counts once per section. Counting distinct blobs instead would
// reorder the palette as soon as deduplication succeeded, which is exactly when
// two writers are most likely to disagree.
func TestPaletteCountsOccurrencesNotBlobs(t *testing.T) {
	reg := testRegistry(t)
	stone, _ := reg.StateToRuntimeID("minecraft:stone", map[string]any{})
	dirt, _ := reg.StateToRuntimeID("minecraft:dirt", map[string]any{})

	// Dirt appears in one section, in two distinct blobs. Stone appears in
	// four sections that all share one blob. Counting blobs would put dirt
	// first; counting occurrences puts stone first.
	ch := chunk.New(reg, cube.Range{-64, 319})
	for sec := range int16(4) {
		y := -64 + sec*16
		for x := range uint8(16) {
			for z := range uint8(16) {
				ch.SetBlock(x, y, z, 0, stone)
			}
		}
	}
	ch.SetBlock(0, 100, 0, 0, dirt)
	ch.SetBlock(1, 116, 1, 0, dirt)

	file := encode(t, &WorldData{Columns: []Column{{X: 0, Z: 0,
		Col: &chunk.Column{Chunk: ch}}}}, reg, CompressionNone)
	body := file[headerSize : len(file)-footerSize]
	si, di := bytes.Index(body, []byte("minecraft:stone")), bytes.Index(body, []byte("minecraft:dirt"))
	if si < 0 || di < 0 {
		t.Fatal("palette lost an entry")
	}
	if si > di {
		t.Fatal("the palette is ordered by distinct blobs rather than by occurrences")
	}
}

// TestIndexedRejectsSolidOnlyFlags: indexed mode has nowhere to put a stats
// compound and no section to elide a default biome from, so a file claiming
// either makes a promise its layout cannot keep.
func TestIndexedRejectsSolidOnlyFlags(t *testing.T) {
	reg := testRegistry(t)
	for _, flag := range []uint32{FlagStats, FlagDefaultBiome} {
		path := filepath.Join(t.TempDir(), "w.pile")
		w, err := CreateIndexed(path, reg, Options{Compression: CompressionNone})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Store(buildTestColumn(t, reg, 0, 0)); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		// The directory prologue is authoritative, so the flag has to be set
		// there for the file to claim it at all.
		if !patchDirectoryFlags(t, path, flag) {
			t.Skip("could not locate the directory prologue")
		}
		// A file always has an earlier checkpoint to fall back to, so the
		// claim is refused either by failing to open or by rejecting that
		// checkpoint and recovering to the one before it.
		v, err := OpenIndexed(path, reg, true)
		if err != nil {
			continue
		}
		recovered := v.Recovered()
		v.Close()
		if !recovered {
			t.Fatalf("an indexed file claiming flag 0x%X was accepted", flag)
		}
	}
}

// TestIndexedRejectsDictionaryWhenUncompressed: a dictionary means nothing to a
// file stored raw, so carrying one is a second way to say the same thing, and
// the second one cannot be read.
func TestIndexedRejectsDictionaryWhenUncompressed(t *testing.T) {
	reg := testRegistry(t)
	path := filepath.Join(t.TempDir(), "w.pile")
	w, err := CreateIndexed(path, reg, Options{Compression: CompressionNone})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Store(buildTestColumn(t, reg, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if !patchDirectoryDict(t, path) {
		t.Skip("could not locate the directory prologue")
	}
	v, err := OpenIndexed(path, reg, true)
	if err != nil {
		return // refused outright
	}
	recovered := v.Recovered()
	v.Close()
	if !recovered {
		t.Fatal("an uncompressed indexed file referencing a dictionary was accepted")
	}
}

// patchDirectoryFlags rewrites the flags word in an uncompressed indexed
// file's directory prologue and refreshes the checkpoint hash, so the file
// makes the claim rather than merely having a damaged header.
func patchDirectoryFlags(t *testing.T, path string, set uint32) bool {
	t.Helper()
	return patchDirectory(t, path, func(dir []byte) bool {
		flags := binary.LittleEndian.Uint32(dir[2:6])
		binary.LittleEndian.PutUint32(dir[2:6], flags|set)
		return true
	})
}

// patchDirectoryDict points the directory's dictionary reference at a frame,
// keeping every varint one byte wide so no offset moves.
func patchDirectoryDict(t *testing.T, path string) bool {
	t.Helper()
	return patchDirectory(t, path, func(dir []byte) bool {
		// prologue: kind, mode, flags, blockVersion = 10 bytes, then the meta
		// reference. Both references are absent in a freshly written file, so
		// each is two zero varints and an eight-byte hash.
		const prologue, refLen = 10, 2 + 8
		dict := prologue + refLen
		if len(dir) < dict+refLen || dir[dict] != 0 || dir[dict+1] != 0 {
			return false
		}
		dir[dict] = headerSize // offset
		dir[dict+1] = 1        // length
		return true
	})
}

func patchDirectory(t *testing.T, path string, edit func(dir []byte) bool) bool {
	t.Helper()
	file, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	footer := file[len(file)-footerSize:]
	off := binary.LittleEndian.Uint64(footer[8:16])
	length := binary.LittleEndian.Uint64(footer[16:24])
	if off == 0 || length == 0 || off+length > uint64(len(file)) {
		return false
	}
	dir := file[off : off+length]
	if !edit(dir) {
		return false
	}
	binary.LittleEndian.PutUint64(footer[0:8], checkpointHash(file[:headerSize], dir, footer[8:]))
	if err := os.WriteFile(path, file, 0o644); err != nil {
		t.Fatal(err)
	}
	return true
}
