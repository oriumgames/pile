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
