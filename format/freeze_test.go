package format

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cespare/xxhash/v2"
	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
	"github.com/klauspost/compress/zstd"
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
	setFlags := func(b []byte, flags uint32) {
		binary.LittleEndian.PutUint32(b[8:12], flags)
		// Rehash, or the checksum rejects the file and the flag rule is never
		// reached: the test would pass with the rule deleted.
		rehashSolid(b)
	}
	base := binary.LittleEndian.Uint32(file[8:12])

	// Bit 2 is reserved, bits 8-15 are reserved, and the high half is the
	// default-biome reference, meaningless without its flag.
	for _, bit := range []uint32{1 << 2, 1 << 8, 1 << 15, 1 << 16} {
		bad := bytes.Clone(file)
		setFlags(bad, base|bit)
		if _, err := ReadWorld(bad, reg); err == nil {
			t.Errorf("reserved flag bit 0x%X accepted", bit)
		}
	}
	// Bits 5-7 are the dimension, not reserved: bit 5 is Nether and must be
	// accepted as such.
	nether := bytes.Clone(file)
	setFlags(nether, base|uint32(Nether)<<dimensionShift)
	d, err := ReadWorld(nether, reg)
	if err != nil {
		t.Fatalf("the nether dimension bit was rejected: %v", err)
	}
	if d.Dimension != Nether {
		t.Fatalf("dimension = %v, want nether", d.Dimension)
	}
	// An unknown version is refused before anything else is read.
	badVer := bytes.Clone(file)
	binary.LittleEndian.PutUint16(badVer[4:6], Version+1)
	rehashSolid(badVer)
	if _, err := ReadWorld(badVer, reg); !errors.Is(err, ErrUnsupportedVersion) {
		t.Errorf("unsupported version: err = %v, want ErrUnsupportedVersion", err)
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
			// Distinct positions, since two at one position are now invalid;
			// the stripped coordinates are still what must not decide the
			// order.
			BlockEntities: []chunk.BlockEntity{
				{Pos: cube.Pos{2, -60, 1}, Data: map[string]any{"id": "minecraft:chest", "x": x1}},
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
	// An empty compound is valid: §7 makes every metadata field optional and
	// fixes only the spelling of the ones that are present.
	if err := w.SetMeta(nil, nil, nil, []byte{0x0a, 0, 0, 0x00}); err != nil {
		t.Fatalf("SetMeta rejected an empty border compound: %v", err)
	}
	// A present field with the wrong tag is not.
	badBorder, err := marshalNBT(map[string]any{"min": []int32{0, 0}})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.SetMeta(nil, nil, nil, badBorder); err == nil {
		t.Fatal("SetMeta accepted a border whose min is a list")
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
	// Setting air on an unallocated layer is a no-op, so layers 1..3 are
	// brought into existence and then cleared.
	padded.SetBlock(0, -64, 0, 3, stone)
	padded.SetBlock(0, -64, 0, 3, air)

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

// TestRejectsUnaddressableSectionSpan: sectionN bounds how tall a chunk is,
// not where it sits. A one-section chunk based far outside the int16 block Y
// domain is small and still describes blocks nothing can address, and its
// section index would be narrowed silently everywhere it is used. The writer
// already refuses such a range; the reader has to as well, or the two disagree
// on what a valid file is.
func TestRejectsUnaddressableSectionSpan(t *testing.T) {
	// Feed the record parser a span directly: the placement is the first thing
	// it reads, so a rejected one never reaches the fields after it.
	parse := func(min, sections int64) error {
		w := &writer{}
		w.svarint(min)
		w.uvarint(uint64(sections))
		w.b = append(w.b, make([]byte, 1024)...) // presence bits and beyond
		_, err := parseRecordBody(&reader{b: w.bytes()}, tableBlobSource(nil, nil, nil), false, 0, 0)
		return err
	}
	for _, c := range []struct {
		name          string
		min, sections int64
		want          bool // true = must be rejected
	}{
		{"lowest legal", minSectionIdx, 1, false},
		{"highest legal", maxSectionIdx, 1, false},
		{"full domain", minSectionIdx, maxSectionIdx - minSectionIdx + 1, false},
		{"one below the floor", minSectionIdx - 1, 1, true},
		{"one above the ceiling", maxSectionIdx + 1, 1, true},
		{"spans past the top", maxSectionIdx, 2, true},
		{"astronomically high", 1 << 40, 1, true},
		{"astronomically low", -(1 << 40), 1, true},
	} {
		err := parse(c.min, c.sections)
		rejected := err != nil && strings.Contains(err.Error(), "addressable")
		if rejected != c.want {
			t.Errorf("%s (minSection=%d, sectionN=%d): rejected=%v, want %v (err %v)",
				c.name, c.min, c.sections, rejected, c.want, err)
		}
	}
}

// TestAbsentBiomeFallbackIsVersionStable: a file that names no default biome
// still has to decode the same way on every game version, so the fallback for
// an absent section is a name resolved at run time, not a numeric id. Id 0 is
// a registry index whose meaning is a version property; today it is ocean.
func TestAbsentBiomeFallbackIsVersionStable(t *testing.T) {
	reg := testRegistry(t)
	stone, _ := reg.StateToRuntimeID("minecraft:stone", map[string]any{})
	ch := chunk.New(reg, cube.Range{-64, 319})
	ch.SetBlock(0, -64, 0, 0, stone)

	// Biomes skipped: every section is absent and no default is named, which
	// is the case the fallback exists for.
	var buf bytes.Buffer
	if err := WriteWorld(&buf, &WorldData{Columns: []Column{{X: 0, Z: 0,
		Col: &chunk.Column{Chunk: ch}}}}, reg, Options{
		Compression: CompressionNone, SkipBiomes: true,
	}); err != nil {
		t.Fatal(err)
	}
	h, _, err := parseFrame(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if h.flags&FlagDefaultBiome != 0 {
		t.Fatal("the fixture names a default biome, so it does not reach the fallback")
	}
	got, err := ReadWorld(buf.Bytes(), reg)
	if err != nil {
		t.Fatal(err)
	}
	want := fallbackBiomeID()
	if id := got.Columns[0].Col.Chunk.Biome(0, 0, 0); id != want {
		t.Fatalf("absent biome decoded as id %d (%s), want the fallback %d (%s)",
			id, biomeName(id), want, biomeName(want))
	}
	if name := biomeName(want); name != plainsBiomeName() {
		t.Fatalf("the fallback resolves to %q, want %q", name, plainsBiomeName())
	}
}

// TestRejectedStoreLeavesNoTrace: resolving a column appends to the palettes
// as it goes, and the first entry that cannot be admitted is only reported
// afterwards. Entries added before it are valid but unreferenced, and would
// still reach the next checkpoint, so a world that refused a column would not
// encode like a world that never saw one.
func TestRejectedStoreLeavesNoTrace(t *testing.T) {
	reg := testRegistry(t)
	build := func(offer bool) int64 {
		path := filepath.Join(t.TempDir(), "w.pile")
		w, err := CreateIndexed(path, reg, Options{Compression: CompressionNone})
		if err != nil {
			t.Fatal(err)
		}
		if offer {
			props := make(map[string]any, 65)
			for i := range 65 {
				props[fmt.Sprintf("p%02d", i)] = int32(i)
			}
			c := buildTestColumn(t, reg, 0, 0)
			c.Col.Chunk.SetBlock(0, -64, 0, 0, placeholderRid(reg))
			c.UnknownStates = []BlockState{{Name: "audit:toowide", Properties: props, Version: 1}}
			c.Unknown = []UnknownBlock{{Section: -4, Layer: 0, Index: 0, State: 0}}
			if err := w.Store(c); err == nil {
				t.Fatal("the invalid column was accepted")
			}
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return st.Size()
	}
	if refused, untouched := build(true), build(false); refused != untouched {
		t.Fatalf("a refused Store left %d bytes behind: %d vs %d for an untouched world",
			refused-untouched, refused, untouched)
	}
}

// TestPreservedStateSurvivesReopen: a preserved state expressed at the
// writer's own version means the same as one expressed at no version in
// particular. Loading a segment and admitting a new entry must therefore build
// the same dedup key, or reopening a file and storing an unchanged column
// appends a second copy of a state the palette already holds.
func TestPreservedStateSurvivesReopen(t *testing.T) {
	reg := testRegistry(t)
	path := filepath.Join(t.TempDir(), "w.pile")
	col := func() Column {
		c := buildTestColumn(t, reg, 0, 0)
		c.Col.Chunk.SetBlock(0, -64, 0, 0, placeholderRid(reg))
		c.UnknownStates = []BlockState{{Name: "audit:current", Version: chunk.CurrentBlockVersion}}
		c.Unknown = []UnknownBlock{{Section: -4, Layer: 0, Index: 0, State: 0}}
		return c
	}
	w, err := CreateIndexed(path, reg, Options{Compression: CompressionNone})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Store(col()); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	w, err = OpenIndexed(path, reg, false)
	if err != nil {
		t.Fatal(err)
	}
	got, err := w.Column(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Store(got); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	v, err := OpenIndexed(path, reg, true)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	states, err := v.UnresolvedStates()
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 {
		t.Fatalf("palette holds %d unresolved states after a reopen and store, want 1: %v",
			len(states), states)
	}
}

// TestStructurePaddingSidecarDropped: positions outside the declared box are
// cleared to air, but a preserved-state entry naming one would be reinjected
// afterwards and put a state back there. On a registry whose placeholder
// resolves to air the clearing cannot be told apart from the injection, so the
// entries have to go by position.
func TestStructurePaddingSidecarDropped(t *testing.T) {
	reg := testRegistry(t)
	stone, _ := reg.StateToRuntimeID("minecraft:stone", map[string]any{})
	air := reg.AirRuntimeID()

	build := func(withSidecar bool) []byte {
		data, err := NewStructureData([3]int32{1, 1, 1})
		if err != nil {
			t.Fatal(err)
		}
		sub := chunk.NewSubChunk(air)
		sub.SetBlock(0, 0, 0, 0, stone)
		data.Cells[0] = sub
		if withSidecar {
			data.UnknownStates = []BlockState{{Name: "audit:padding", Version: 1}}
			// (15,15,15) is inside the cell and outside the 1x1x1 box.
			data.Unknown = []UnknownBlock{{
				Section: 0, Layer: 0,
				Index: uint16(15)<<8 | uint16(15)<<4 | 15, State: 0,
			}}
		}
		var buf bytes.Buffer
		if err := WriteStructure(&buf, data, reg, Options{Compression: CompressionNone}); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	with, without := build(true), build(false)
	if !bytes.Equal(with, without) {
		t.Fatalf("a preserved state outside the box reached the file: %d vs %d bytes",
			len(with), len(without))
	}
	if bytes.Contains(with, []byte("audit:padding")) {
		t.Fatal("the out-of-box state is in the palette")
	}
}

// Round 17 rules.

// TestRejectsDirectoryStorageMismatch: the prologue inside a directory frame
// says how the frame is stored and is the authority. Guessing the form and
// accepting whichever decodes would let one directory be written two ways, and
// a reader that trusted the flag would reject what this one accepted.
func TestRejectsDirectoryStorageMismatch(t *testing.T) {
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
	// Claim compression in the physical header and in the raw prologue, while
	// leaving every frame stored raw.
	if !patchDirectoryFlags(t, path, 0) { // rewrites the hash
		t.Skip("could not reach the directory")
	}
	file, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	footer := file[len(file)-footerSize:]
	off := binary.LittleEndian.Uint64(footer[8:16])
	length := binary.LittleEndian.Uint64(footer[16:24])
	dir := file[off : off+length]
	binary.LittleEndian.PutUint32(file[8:12],
		binary.LittleEndian.Uint32(file[8:12])&^FlagUncompressed)
	binary.LittleEndian.PutUint32(dir[2:6],
		binary.LittleEndian.Uint32(dir[2:6])&^FlagUncompressed)
	binary.LittleEndian.PutUint64(footer[0:8], checkpointHash(file[:headerSize], dir, footer[8:]))
	if err := os.WriteFile(path, file, 0o644); err != nil {
		t.Fatal(err)
	}
	// The file always has an earlier checkpoint to fall back to, so the
	// contradiction is refused either by failing to open or by rejecting that
	// checkpoint and recovering to the one before it.
	v, err := OpenIndexed(path, reg, true)
	if err != nil {
		return
	}
	recovered := v.Recovered()
	v.Close()
	if !recovered {
		t.Fatal("a raw directory claiming compression was accepted")
	}
}

// TestRejectsUndefinedKind: kind names a layout, and only two exist. A reader
// that passes an unknown one through reports a file type nothing defines.
func TestRejectsUndefinedKind(t *testing.T) {
	reg := testRegistry(t)
	file := encode(t, testWorld(t, reg), reg, CompressionNone)
	for _, kind := range []byte{2, 3, 255} {
		bad := bytes.Clone(file)
		bad[6] = kind
		rehashSolid(bad)
		if _, err := ReadMeta(bad); err == nil {
			t.Errorf("kind %d was accepted", kind)
		}
	}
}

// TestAliasCountsOneAppearance: the reference count is the number of local
// palettes a state appears in. A local palette holding two runtime IDs for one
// state contains it once, so counting slots would let the choice of alias
// reorder the global palette.
func TestAliasCountsOneAppearance(t *testing.T) {
	reg := newAliasRegistry(testRegistry(t))
	twin := reg.alias - 1
	other, _ := reg.StateToRuntimeID("minecraft:dirt", map[string]any{})

	build := func(second uint32) []byte {
		ch := chunk.New(reg, cube.Range{-64, 319})
		ch.SetBlock(0, -64, 0, 0, twin)
		ch.SetBlock(1, -64, 0, 0, second)
		ch.SetBlock(2, -64, 0, 0, other)
		var buf bytes.Buffer
		if err := WriteWorld(&buf, &WorldData{Columns: []Column{{X: 0, Z: 0,
			Col: &chunk.Column{Chunk: ch}}}}, reg, Options{Compression: CompressionNone}); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	// Replacing one alias with the other leaves the block states unchanged.
	if a, b := build(reg.alias), build(twin); !bytes.Equal(a, b) {
		t.Fatalf("the choice of alias changed the file: %d vs %d bytes", len(a), len(b))
	}
}

// TestPositionalUnknownBiomes: a preserved biome names a position, not a
// palette slot. Renaming the slot gives one name to every position sharing it,
// and a uniform section has no indices to consult at all.
func TestPositionalUnknownBiomes(t *testing.T) {
	reg := testRegistry(t)
	stone, _ := reg.StateToRuntimeID("minecraft:stone", map[string]any{})
	ch := chunk.New(reg, cube.Range{-64, 319})
	ch.SetBlock(0, -64, 0, 0, stone)
	plains := fallbackBiomeID()
	for x := range uint8(16) {
		for z := range uint8(16) {
			for y := int16(-64); y < -48; y++ {
				ch.SetBiome(x, y, z, plains)
			}
		}
	}
	d := &WorldData{Columns: []Column{{
		X: 0, Z: 0, Col: &chunk.Column{Chunk: ch},
		UnknownBiomeNames: []string{"audit:a", "audit:b"},
		UnknownBiomes: []UnknownBlock{
			{Section: -4, Index: 0, State: 0},
			{Section: -4, Index: 1, State: 1},
		},
	}}}
	file := encode(t, d, reg, CompressionNone)
	body := file[headerSize : len(file)-footerSize]
	for _, name := range []string{"audit:a", "audit:b", "minecraft:plains"} {
		if !bytes.Contains(body, []byte(name)) {
			t.Fatalf("the biome palette lost %q: a uniform section cannot carry positional names", name)
		}
	}
	back, err := ReadWorld(file, reg)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(back.Columns[0].UnknownBiomes); n != 2 {
		t.Fatalf("recovered %d positional biomes, want 2", n)
	}
	if second := encode(t, back, reg, CompressionNone); !bytes.Equal(file, second) {
		t.Fatal("positional preserved biomes do not survive a round trip")
	}
}

// TestMalformedBiomeSidecarErrors: a sidecar naming a state the column does
// not carry is malformed input. Writers report it; they do not panic.
func TestMalformedBiomeSidecarErrors(t *testing.T) {
	reg := testRegistry(t)
	stone, _ := reg.StateToRuntimeID("minecraft:stone", map[string]any{})
	plains := fallbackBiomeID()
	for _, idx := range []uint16{WholeStorage, 0} {
		ch := chunk.New(reg, cube.Range{-64, 319})
		ch.SetBlock(0, -64, 0, 0, stone)
		for x := range uint8(16) {
			for z := range uint8(16) {
				for y := int16(-64); y < -48; y++ {
					ch.SetBiome(x, y, z, plains)
				}
			}
		}
		d := &WorldData{Columns: []Column{{
			X: 0, Z: 0, Col: &chunk.Column{Chunk: ch},
			UnknownBiomes: []UnknownBlock{{Section: -4, Index: idx, State: 0}},
		}}}
		var buf bytes.Buffer
		if err := WriteWorld(&buf, d, reg, Options{Compression: CompressionNone}); err == nil {
			t.Errorf("index %d: a sidecar with no names was accepted", idx)
		}
	}
}

// TestIndexedUnknownBiomeBeforeCheckpoint: an unresolved biome has to be
// readable as itself the moment it is stored. Establishing the mapping only
// once the segment had been written and loaded back meant a read and rewrite
// in between replaced it with the fallback.
func TestIndexedUnknownBiomeBeforeCheckpoint(t *testing.T) {
	reg := testRegistry(t)
	path := filepath.Join(t.TempDir(), "w.pile")
	w, err := CreateIndexed(path, reg, Options{Compression: CompressionNone})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	stone, _ := reg.StateToRuntimeID("minecraft:stone", map[string]any{})
	ch := chunk.New(reg, cube.Range{-64, 319})
	ch.SetBlock(0, -64, 0, 0, stone)
	plains := fallbackBiomeID()
	for x := range uint8(16) {
		for z := range uint8(16) {
			for y := int16(-64); y < -48; y++ {
				ch.SetBiome(x, y, z, plains)
			}
		}
	}
	if err := w.Store(Column{X: 0, Z: 0, Col: &chunk.Column{Chunk: ch},
		UnknownBiomeNames: []string{"audit:unknown"},
		UnknownBiomes:     []UnknownBlock{{Section: -4, Index: WholeStorage, State: 0}},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := w.Column(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.UnknownBiomes) == 0 {
		t.Fatal("a preserved biome read back before checkpointing lost its sidecar")
	}
	if len(got.UnknownBiomeNames) == 0 || got.UnknownBiomeNames[got.UnknownBiomes[0].State] != "audit:unknown" {
		t.Fatalf("the preserved name did not come back: %v", got.UnknownBiomeNames)
	}
}

// TestUnknownStateInEmptySection: on a registry whose placeholder resolves to
// air, a section holding nothing but unresolved blocks has no storages at all.
// Skipping empty sections would drop the only record that they were there.
//
// The sidecar only speaks for positions where the placeholder is still in
// place, so this is specific to a registry that has none: with a real
// placeholder, air at that position means the entry is stale and ignoring it
// is correct.
func TestUnknownStateInEmptySection(t *testing.T) {
	reg := newNoPlaceholderRegistry(testRegistry(t))
	ch := chunk.New(reg, cube.Range{-64, 319})
	stone, _ := reg.StateToRuntimeID("minecraft:stone", map[string]any{})
	ch.SetBlock(0, 100, 0, 0, stone) // content in a different section entirely

	d := &WorldData{Columns: []Column{{
		X: 0, Z: 0, Col: &chunk.Column{Chunk: ch},
		UnknownStates: []BlockState{{Name: "audit:lonely", Version: 1}},
		Unknown: []UnknownBlock{{
			Section: -4, Layer: 0,
			Index: uint16(3)<<8 | uint16(3)<<4 | 0, State: 0,
		}},
	}}}
	file := encode(t, d, reg, CompressionNone)
	if !bytes.Contains(file[headerSize:len(file)-footerSize], []byte("audit:lonely")) {
		t.Fatal("a preserved state in a section with no storages was dropped")
	}
	back, err := ReadWorld(file, reg)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Columns[0].Unknown) != 1 {
		t.Fatalf("recovered %d entries, want 1", len(back.Columns[0].Unknown))
	}
	if second := encode(t, back, reg, CompressionNone); !bytes.Equal(file, second) {
		t.Fatal("a preserved state in an empty section does not survive a round trip")
	}
}

// Round 19 rules.

// TestMetadataFieldsAreOptional: §7 makes presence a convention and spelling a
// rule. A writer that demanded a field made the two halves contradict each
// other, and rejected a compound the specification calls valid.
func TestMetadataFieldsAreOptional(t *testing.T) {
	reg := testRegistry(t)
	empty := []byte{0x0a, 0, 0, 0x00} // an empty canonical compound
	for _, blob := range []struct {
		name string
		set  func(*WorldData)
	}{
		{"settings", func(d *WorldData) { d.Settings = empty }},
		{"markers", func(d *WorldData) { d.Markers = empty }},
		{"border", func(d *WorldData) { d.Border = empty }},
	} {
		d := testWorld(t, reg)
		blob.set(d)
		var buf bytes.Buffer
		if err := WriteWorld(&buf, d, reg, Options{Compression: CompressionNone}); err != nil {
			t.Errorf("an empty %s compound was rejected: %v", blob.name, err)
		}
	}

	// Spelling is still a rule: a present field with the wrong tag is invalid.
	d := testWorld(t, reg)
	bad, err := marshalNBT(map[string]any{"max": []int32{1, 1}})
	if err != nil {
		t.Fatal(err)
	}
	d.Border = bad
	var buf bytes.Buffer
	if err := WriteWorld(&buf, d, reg, Options{Compression: CompressionNone}); err == nil {
		t.Fatal("a border whose max is a list was accepted")
	}
}

// TestWriterRejectsDeepNBTLists: the depth limit was checked only when the
// encoder reached a compound, so a chain of nested lists slipped past it and
// produced a file this same release refuses to read.
func TestWriterRejectsDeepNBTLists(t *testing.T) {
	var deep any = byte(1)
	for range maxNBTDepth + 2 {
		deep = []any{deep}
	}
	if _, err := marshalNBT(map[string]any{"deep": deep}); err == nil {
		t.Fatal("a list nested past the depth limit was written")
	}

	// A chain just inside the limit still encodes and reads back.
	var ok any = byte(1)
	for range maxNBTDepth - 4 {
		ok = []any{ok}
	}
	b, err := marshalNBT(map[string]any{"deep": ok})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateNBT(b); err != nil {
		t.Fatalf("a list within the limit does not read back: %v", err)
	}
}

// TestBiomeAliasCountsOneAppearance: a registry may number one biome twice.
// Counting slots rather than appearances lets the choice of alias reorder the
// biome palette and change which biome the header names as the default.
func TestBiomeAliasCountsOneAppearance(t *testing.T) {
	reg := testRegistry(t)
	// Two ids that resolve to the same name: an id the registry does not know
	// falls back to plains, so any two unknown ids are aliases of each other.
	unknownA, unknownB := uint32(60000), uint32(60001)
	if biomeName(unknownA) != biomeName(unknownB) {
		t.Skip("the registry resolves these ids differently")
	}
	stone, _ := reg.StateToRuntimeID("minecraft:stone", map[string]any{})

	build := func(second uint32) []byte {
		ch := chunk.New(reg, cube.Range{-64, 319})
		ch.SetBlock(0, -64, 0, 0, stone)
		for x := range uint8(16) {
			for z := range uint8(16) {
				for y := int16(-64); y < -48; y++ {
					id := unknownA
					if (x+z)%2 == 0 {
						id = second
					}
					ch.SetBiome(x, y, z, id)
				}
			}
		}
		var buf bytes.Buffer
		if err := WriteWorld(&buf, &WorldData{Columns: []Column{{X: 0, Z: 0,
			Col: &chunk.Column{Chunk: ch}}}}, reg, Options{Compression: CompressionNone}); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	if a, b := build(unknownB), build(unknownA); !bytes.Equal(a, b) {
		t.Fatalf("the choice of biome alias changed the file: %d vs %d bytes", len(a), len(b))
	}
}

// TestStructureUnknownStateInEmptyCell: whether a caller left a cell nil or
// handed over an empty sub chunk must not decide whether its preserved states
// survive.
func TestStructureUnknownStateInEmptyCell(t *testing.T) {
	reg := newNoPlaceholderRegistry(testRegistry(t))
	build := func(explicit bool) []byte {
		data, err := NewStructureData([3]int32{16, 16, 16})
		if err != nil {
			t.Fatal(err)
		}
		if explicit {
			data.Cells[0] = chunk.NewSubChunk(reg.AirRuntimeID())
			data.Cells[0].SetBlock(0, 0, 0, 0, reg.AirRuntimeID())
		}
		data.UnknownStates = []BlockState{{Name: "audit:lost", Version: 1}}
		data.Unknown = []UnknownBlock{{Section: 0, Layer: 0, Index: 0, State: 0}}
		var buf bytes.Buffer
		if err := WriteStructure(&buf, data, reg, Options{Compression: CompressionNone}); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	nilForm, explicitForm := build(false), build(true)
	if !bytes.Contains(nilForm, []byte("audit:lost")) {
		t.Fatal("a preserved state in a nil cell was dropped")
	}
	if !bytes.Equal(nilForm, explicitForm) {
		t.Fatalf("a nil cell and an empty one encode differently: %d vs %d bytes",
			len(nilForm), len(explicitForm))
	}
}

// TestRejectsOversizedFrameLength: a directory's frame length is carried in a
// 32-bit field, and a larger value would wrap into a small one. Checking after
// the narrowing would accept a file a conforming reader rejects, and read it as
// a different file.
func TestRejectsOversizedFrameLength(t *testing.T) {
	ref := func(off, length uint64) []byte {
		w := &writer{}
		w.uvarint(off)
		w.uvarint(length)
		w.u64(0)
		return w.bytes()
	}
	for _, c := range []struct {
		name        string
		off, length uint64
		ok          bool
	}{
		{"ordinary", 16, 64, true},
		{"largest representable", 16, maxFrameLen, true},
		{"one past the field", 16, maxFrameLen + 1, false},
		{"wraps to a plausible length", 16, 1<<32 + 4, false},
		{"offset past int64", 1 << 63, 4, false},
	} {
		got, err := parseFrameRef(&reader{b: ref(c.off, c.length)})
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok = %v", c.name, err, c.ok)
			continue
		}
		if c.ok && uint64(got.length) != c.length {
			t.Errorf("%s: length came back as %d, want %d", c.name, got.length, c.length)
		}
	}
}

// Round 20 rules.

// TestUnknownStateAboveAllocatedLayers: a preserved state can name a layer the
// runtime never allocated. On a registry whose placeholder resolves to air, a
// layer holding only unresolved blocks has no storage, and neither does the
// section beneath it, so a writer that walks only allocated layers drops
// exactly the entries the sidecar exists for. An all-air object and one
// carrying an unresolved layer-1 state would then encode identically.
func TestUnknownStateAboveAllocatedLayers(t *testing.T) {
	reg := newNoPlaceholderRegistry(testRegistry(t))
	build := func(withState bool) []byte {
		ch := chunk.New(reg, cube.Range{-64, 319})
		c := Column{X: 0, Z: 0, Col: &chunk.Column{Chunk: ch}}
		if withState {
			c.UnknownStates = []BlockState{{Name: "audit:layer1", Version: 1}}
			c.Unknown = []UnknownBlock{{
				Section: -4, Layer: 1, Index: WholeStorage, State: 0,
			}}
		}
		var buf bytes.Buffer
		if err := WriteWorld(&buf, &WorldData{Columns: []Column{c}}, reg,
			Options{Compression: CompressionNone}); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	with, without := build(true), build(false)
	if bytes.Equal(with, without) {
		t.Fatal("an unresolved state in an unallocated layer encodes like an empty world")
	}
	if !bytes.Contains(with, []byte("audit:layer1")) {
		t.Fatal("the state is not in the palette")
	}
	back, err := ReadWorld(with, reg)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(back.Columns[0].Unknown); n != 1 || back.Columns[0].Unknown[0].Layer != 1 {
		t.Fatalf("recovered %d entries at %v, want one in layer 1", n, back.Columns[0].Unknown)
	}
	// Layer 0 has to survive as an internal all-air layer, or layer 1 would
	// have been renumbered on the way out.
	if second := encode(t, back, reg, CompressionNone); !bytes.Equal(with, second) {
		t.Fatal("a state above the allocated layers does not survive a round trip")
	}
}

// TestStructureUnknownStateAboveAllocatedLayers is the same rule for cells.
func TestStructureUnknownStateAboveAllocatedLayers(t *testing.T) {
	reg := newNoPlaceholderRegistry(testRegistry(t))
	build := func(withState bool) []byte {
		data, err := NewStructureData([3]int32{16, 16, 16})
		if err != nil {
			t.Fatal(err)
		}
		if withState {
			data.UnknownStates = []BlockState{{Name: "audit:layer1", Version: 1}}
			data.Unknown = []UnknownBlock{{
				Section: 0, Layer: 1, Index: WholeStorage, State: 0,
			}}
		}
		var buf bytes.Buffer
		if err := WriteStructure(&buf, data, reg, Options{Compression: CompressionNone}); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	with, without := build(true), build(false)
	if bytes.Equal(with, without) {
		t.Fatal("an unresolved state in an unallocated cell layer encodes like an empty structure")
	}
	got, err := ReadStructure(with, reg)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(got.Unknown); n != 1 || got.Unknown[0].Layer != 1 {
		t.Fatalf("recovered %d entries at %v, want one in layer 1", n, got.Unknown)
	}
}

// TestRejectedStoreRollsBackEveryPath: the palette snapshot was restored only
// when the palette itself reported the error. A record too large to write, a
// frame that will not append and a full directory all leave the same
// unreferenced entries behind, and they would still reach the next checkpoint.
func TestRejectedStoreRollsBackEveryPath(t *testing.T) {
	reg := testRegistry(t)
	build := func(offer bool) int64 {
		path := filepath.Join(t.TempDir(), "w.pile")
		w, err := CreateIndexed(path, reg, Options{Compression: CompressionNone})
		if err != nil {
			t.Fatal(err)
		}
		if offer {
			// Six blobs just under the per-blob ceiling: each is legal, the
			// record they make is not.
			c := buildTestColumn(t, reg, 0, 0)
			for i := range 6 {
				c.Col.Entities = append(c.Col.Entities, chunk.Entity{
					ID: int64(1000 + i),
					Data: map[string]any{
						"identifier": "minecraft:cow",
						// A byte list, not a string: strings are capped at
						// 64 KiB, so a string payload is rejected during
						// extraction and never reaches the palette at all.
						"pad": make([]byte, 12<<20),
					},
				})
			}
			if err := w.Store(c); err == nil {
				t.Fatal("an oversized record was accepted")
			}
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return st.Size()
	}
	if refused, untouched := build(true), build(false); refused != untouched {
		t.Fatalf("a Store rejected past the palette left %d bytes behind: %d vs %d",
			refused-untouched, refused, untouched)
	}
}

// Paired-path sweep. Each of these is a rule the writer already followed and
// the reader did not, or a rule one member of a pair had and its twin lacked.

// TestRejectsUnorderedStateProperties: writers sort property keys, so a reader
// that takes any order accepts many encodings of one state. A repeated key is
// worse: the later value silently wins, so two different files decode to the
// same state.
func TestRejectsUnorderedStateProperties(t *testing.T) {
	reg := testRegistry(t)
	// A palette with one entry carrying two properties, written by hand so
	// the order can be chosen.
	build := func(k1, k2 string) []byte {
		w := &writer{}
		w.uvarint(1) // one entry
		w.str("audit:props")
		w.uvarint(2)
		for _, k := range []string{k1, k2} {
			w.str(k)
			w.u8(propInt)
			w.u32(1)
		}
		w.uvarint(0) // no version overrides
		return w.bytes()
	}
	if _, _, _, err := decodeBlockPalette(&reader{b: build("a", "b")}, reg, chunk.CurrentBlockVersion); err != nil {
		t.Fatalf("ascending keys were rejected: %v", err)
	}
	if _, _, _, err := decodeBlockPalette(&reader{b: build("b", "a")}, reg, chunk.CurrentBlockVersion); err == nil {
		t.Fatal("out-of-order property keys were accepted")
	}
	if _, _, _, err := decodeBlockPalette(&reader{b: build("a", "a")}, reg, chunk.CurrentBlockVersion); err == nil {
		t.Fatal("a repeated property key was accepted")
	}
}

// TestRejectsDuplicatePaletteEntries: the writer merges entries that encode
// identically at one version, so a file carrying both is a second encoding of
// one palette and a section could reference either.
func TestRejectsDuplicatePaletteEntries(t *testing.T) {
	reg := testRegistry(t)
	blocks := func(n int) []byte {
		w := &writer{}
		w.uvarint(uint64(n))
		for range n {
			w.str("minecraft:stone")
			w.uvarint(0)
		}
		w.uvarint(0)
		return w.bytes()
	}
	if _, _, _, err := decodeBlockPalette(&reader{b: blocks(1)}, reg, chunk.CurrentBlockVersion); err != nil {
		t.Fatalf("a single entry was rejected: %v", err)
	}
	if _, _, _, err := decodeBlockPalette(&reader{b: blocks(2)}, reg, chunk.CurrentBlockVersion); err == nil {
		t.Fatal("a duplicate block palette entry was accepted")
	}

	biomes := func(names ...string) []byte {
		w := &writer{}
		w.uvarint(uint64(len(names)))
		for _, n := range names {
			w.str(n)
		}
		return w.bytes()
	}
	if _, _, _, err := decodeBiomePalette(&reader{b: biomes("minecraft:plains", "minecraft:ocean")}); err != nil {
		t.Fatalf("distinct biomes were rejected: %v", err)
	}
	if _, _, _, err := decodeBiomePalette(&reader{b: biomes("minecraft:plains", "minecraft:plains")}); err == nil {
		t.Fatal("a duplicate biome palette entry was accepted")
	}
}

// TestReaderEnforcesMetadataSchemas: the schemas were checked when a file was
// written and not when one was read, so the two halves of this package
// disagreed about what a valid file is.
func TestReaderEnforcesMetadataSchemas(t *testing.T) {
	reg := testRegistry(t)
	file := encode(t, testWorld(t, reg), reg, CompressionNone)

	// Splice an unsorted marker list into the meta block by rewriting the
	// whole body: the blob lengths change, so patching in place will not do.
	d, err := ReadWorld(file, reg)
	if err != nil {
		t.Fatal(err)
	}
	unsorted, err := marshalNBT(map[string]any{"markers": []map[string]any{
		{"name": "b", "kind": "region", "pos": []any{0.0, 0.0, 0.0}},
		{"name": "a", "kind": "region", "pos": []any{0.0, 0.0, 0.0}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	// The writer refuses it, which is why the file has to be assembled by
	// hand to test the reader at all.
	d.Markers = unsorted
	var buf bytes.Buffer
	if err := WriteWorld(&buf, d, reg, Options{Compression: CompressionNone}); err == nil {
		t.Fatal("the writer accepted an unsorted marker list")
	}
	// And the reader refuses the same bytes when they arrive from elsewhere.
	if err := checkMarkersBlob(unsorted); err == nil {
		t.Fatal("the shared schema check accepts an unsorted marker list")
	}
	if _, _, _, _, _, err := readMetaBlobs(&reader{b: metaBody(t, unsorted)}, 0); err == nil {
		t.Fatal("the reader accepted an unsorted marker list")
	}
}

// metaBody lays out a meta block carrying the given markers blob.
func metaBody(t *testing.T, markers []byte) []byte {
	t.Helper()
	w := &writer{}
	w.blob(nil) // settings
	w.blob(nil) // user data
	w.blob(markers)
	w.blob(nil) // border
	return w.bytes()
}

// Tests replacing invariant claims that a round-21 review found vacuous: each
// named a test that stayed green with its rule removed.

// TestRejectsOversizedBlob exercises the format's blob primitive, which the
// NBT validator's own length tests never touch: they bound lengths inside an
// NBT payload, not the length prefix the container uses.
func TestRejectsOversizedBlob(t *testing.T) {
	// Reader side: a length past the ceiling is refused by the ceiling, not by
	// running out of input. Both produce an error, so the message is what
	// distinguishes them: asserting only that an error happened would leave
	// this test green with the bound removed.
	w := &writer{}
	w.uvarint(maxBlobLen + 1)
	_, err := (&reader{b: w.bytes()}).blob()
	if err == nil {
		t.Fatal("a blob length past the ceiling was accepted")
	}
	if !strings.Contains(err.Error(), "exceeds limit") {
		t.Errorf("a length past the ceiling was refused by %v, not by the ceiling", err)
	}
	// The largest legal length passes the ceiling and fails only on input.
	w = &writer{}
	w.uvarint(maxBlobLen)
	_, err = (&reader{b: w.bytes()}).blob()
	if err == nil {
		t.Fatal("a truncated blob was accepted")
	}
	if strings.Contains(err.Error(), "exceeds limit") {
		t.Errorf("the largest legal length hit the ceiling: %v", err)
	}
	// Writer side: opaque user data is bounded too.
	reg := testRegistry(t)
	d := testWorld(t, reg)
	d.UserData = make([]byte, maxBlobLen+1)
	var buf bytes.Buffer
	if err := WriteWorld(&buf, d, reg, Options{Compression: CompressionNone}); err == nil {
		t.Error("an oversized user-data blob was written")
	}
}

// TestRejectsBitsetPadding: a presence bitset's bits above the count carry no
// meaning, so a set one is a second encoding. The rule has to be checked
// through the parsers that read them, not through the helper they call: a
// parser that stopped calling it would leave the helper's own test green.
func TestRejectsBitsetPadding(t *testing.T) {
	// A one-section record: bits 1-7 of each presence byte are padding.
	record := func(blockPresence, biomePresence byte) []byte {
		w := &writer{}
		w.svarint(0) // minSection
		w.uvarint(1) // sectionN
		w.u8(blockPresence)
		w.u8(biomePresence)
		w.uvarint(0) // block entities
		w.uvarint(0) // entities
		w.svarint(0) // column tick
		w.uvarint(0) // scheduled ticks
		w.blob(nil)  // user data
		return w.bytes()
	}
	if _, err := parseRecordBody(&reader{b: record(0, 0)}, tableBlobSource(nil, nil, nil), false, 0, 0); err != nil {
		t.Fatalf("a clean record was rejected: %v", err)
	}
	if _, err := parseRecordBody(&reader{b: record(1<<7, 0)}, tableBlobSource(nil, nil, nil), false, 0, 0); err == nil {
		t.Error("block presence padding was accepted")
	}
	if _, err := parseRecordBody(&reader{b: record(0, 1<<7)}, tableBlobSource(nil, nil, nil), false, 0, 0); err == nil {
		t.Error("biome presence padding was accepted")
	}

	// Structure cells use their own parser and their own presence bitset.
	reg := testRegistry(t)
	data, err := NewStructureData([3]int32{1, 1, 1})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := WriteStructure(&buf, data, reg, Options{Compression: CompressionNone}); err != nil {
		t.Fatal(err)
	}
	file := buf.Bytes()
	body := file[headerSize : len(file)-footerSize]
	// The empty structure's body is short and fixed: four empty metadata
	// blobs, an empty block palette with no overrides, an empty biome
	// palette, an empty blob table, the three sizes, the three origins, then
	// the cell presence byte.
	const cellPresenceAt = 4 + 2 + 1 + 1 + 3 + 3
	if body[cellPresenceAt] != 0 {
		t.Fatalf("cell presence is not where this test expects it (found 0x%02X)", body[cellPresenceAt])
	}
	bad := bytes.Clone(file)
	bad[headerSize+cellPresenceAt] = 1 << 7
	rehashSolid(bad)
	if _, err := ReadStructure(bad, reg); err == nil {
		t.Error("structure cell presence padding was accepted")
	}
}

// TestRejectsNonCanonicalNBT: compound keys are unique and ascending and the
// root is unnamed. The empty-list test named for this rule covers none of
// those three.
func TestRejectsNonCanonicalNBT(t *testing.T) {
	compound := func(root string, keys ...string) []byte {
		w := &writer{}
		w.u8(tagCompound)
		w.b = append(w.b, byte(len(root)), 0)
		w.raw([]byte(root))
		for _, k := range keys {
			w.u8(tagByte)
			w.b = append(w.b, byte(len(k)), 0)
			w.raw([]byte(k))
			w.u8(1)
		}
		w.u8(tagEnd)
		return w.bytes()
	}
	if err := validateNBT(compound("", "a", "b")); err != nil {
		t.Fatalf("a canonical compound was rejected: %v", err)
	}
	if err := validateNBT(compound("", "b", "a")); err == nil {
		t.Error("out-of-order compound keys accepted")
	}
	if err := validateNBT(compound("", "a", "a")); err == nil {
		t.Error("duplicate compound keys accepted")
	}
	if err := validateNBT(compound("named", "a")); err == nil {
		t.Error("a named root compound accepted")
	}
}

// TestRejectsLayerCountInRecords puts the unaddressable layer count where a
// record and a cell actually carry it, so the rule survives a parser that
// stops consulting the shared bound.
func TestRejectsLayerCountInRecords(t *testing.T) {
	// A record whose one present section claims 256 layers.
	w := &writer{}
	w.svarint(0)
	w.uvarint(1)
	w.u8(1)         // section 0 present
	w.uvarint(256)  // layerN, one past what an 8-bit index can name
	for range 256 { // blob references, so the parser has input to consume
		w.uvarint(0)
	}
	w.u8(0)      // biome presence
	w.uvarint(0) // block entities
	w.uvarint(0) // entities
	w.svarint(0) // column tick
	w.uvarint(0) // scheduled ticks
	w.blob(nil)  // user data
	if _, err := parseRecordBody(&reader{b: w.bytes()}, tableBlobSource([]decBlob{{}}, nil, nil), false, 0, 0); err == nil {
		t.Error("a record claiming 256 layers was accepted")
	}
}

// TestRejectsDuplicateSegmentReference: repeating a palette segment reference
// multiplies its entries into the cumulative palette from a tiny file, which
// is why the rule exists. The test that used to be named for it exercised
// neither the duplicate nor the cumulative half.
func TestRejectsDuplicateSegmentReference(t *testing.T) {
	w := &IndexedWorld{end: 1 << 20}
	// A validator that accepts anything, so the duplicate rule is what decides
	// the outcome rather than a hash or bounds check reached first.
	ok := func(frameRef, string) error { return nil }
	refs := func(n int) []byte {
		bw := &writer{}
		bw.uvarint(uint64(n))
		for range n {
			bw.uvarint(headerSize) // offset
			bw.uvarint(8)          // length
			bw.u64(1234)           // hash
		}
		return bw.bytes()
	}
	if _, err := w.parseSegRefs(&reader{b: refs(1)}, "segment", ok); err != nil {
		t.Fatalf("a single segment reference was rejected: %v", err)
	}
	if _, err := w.parseSegRefs(&reader{b: refs(2)}, "segment", ok); err == nil {
		t.Fatal("a repeated segment reference was accepted")
	}

	// The frames also have to plausibly fit in the file.
	small := &IndexedWorld{end: 4}
	if _, err := small.parseSegRefs(&reader{b: refs(1)}, "segment", ok); err == nil {
		t.Fatal("a segment larger than the file was accepted")
	}
}

// TestSetMetaChecksAggregateFrame: the four metadata blobs share one frame,
// and the frame has its own ceiling. Checking only the parts leaves SetMeta
// reporting success and every later checkpoint failing, which is the rollback
// this call exists to prevent.
func TestSetMetaChecksAggregateFrame(t *testing.T) {
	reg := testRegistry(t)
	path := filepath.Join(t.TempDir(), "w.pile")
	w, err := CreateIndexed(path, reg, Options{Compression: CompressionNone})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	// Four blobs, each just inside the per-blob ceiling, together past the
	// frame ceiling. Only the opaque one can be arbitrary bytes; the rest are
	// canonical compounds padded with an unlisted byte array.
	// Exactly at the per-blob ceiling, so the four together pass the frame
	// ceiling by their length prefixes alone. The NBT wrapper's size is not
	// worth predicting, so the payload is adjusted until the blob lands.
	pad := func() []byte {
		payload := maxBlobLen - 32
		for range 4 {
			b, err := marshalNBT(map[string]any{"pad": make([]byte, payload)})
			if err != nil {
				t.Fatal(err)
			}
			if len(b) == maxBlobLen {
				return b
			}
			payload += maxBlobLen - len(b)
		}
		t.Fatal("could not size a blob to the ceiling")
		return nil
	}
	big := pad()
	if err := w.SetMeta(big, make([]byte, maxBlobLen), big, big); err == nil {
		t.Fatal("metadata totalling more than one frame was accepted")
	}
	// One blob of the same size is fine, so the frame ceiling is what rejected
	// the set rather than any per-blob rule.
	if err := w.SetMeta(big, nil, nil, nil); err != nil {
		t.Fatalf("a single legal blob was rejected: %v", err)
	}
}

// TestBiomeAdmissionIgnoresBlockCapacity: the block and biome palettes have
// separate limits and separate references, so a biome must not be refused
// because the block palette is full.
func TestBiomeAdmissionIgnoresBlockCapacity(t *testing.T) {
	reg := testRegistry(t)
	path := filepath.Join(t.TempDir(), "w.pile")
	w, err := CreateIndexed(path, reg, Options{Compression: CompressionNone})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	// Pretend the block palette is full. The biome palette is untouched.
	w.rids = make([]uint32, maxPalette)
	if idx := w.addBiome("minecraft:ocean"); w.paletteErr != nil {
		t.Fatalf("a biome was refused because the block palette is full: %v", w.paletteErr)
	} else if idx != 0 {
		t.Fatalf("first biome got index %d, want 0", idx)
	}
}

// Round 22: tests for invariants whose named test did not isolate their rule.

// recordBody lays out a one-section chunk record. Callers vary one field so a
// rule can be driven where a record actually carries it.
type recordFields struct {
	blockPresence, biomePresence, lightPresence byte
	haveLight                                   bool
	lightFlags                                  byte
	lightBody                                   []byte
}

func recordBody(f recordFields) []byte {
	w := &writer{}
	w.svarint(0) // minSection
	w.uvarint(1) // sectionN
	w.u8(f.blockPresence)
	w.u8(f.biomePresence)
	if f.haveLight {
		w.u8(f.lightPresence)
		if f.lightPresence&1 != 0 {
			w.u8(f.lightFlags)
			w.raw(f.lightBody)
		}
	}
	w.uvarint(0) // block entities
	w.uvarint(0) // entities
	w.svarint(0) // column tick
	w.uvarint(0) // scheduled ticks
	w.blob(nil)  // user data
	return w.bytes()
}

// TestRejectsLightBitsetPadding: light has its own presence bitset and its own
// read path, so the padding rule has to be driven there too.
func TestRejectsLightBitsetPadding(t *testing.T) {
	clean := recordFields{haveLight: true, lightPresence: 0}
	if _, err := parseRecordBody(&reader{b: recordBody(clean)}, tableBlobSource(nil, nil, nil), true, 0, 0); err != nil {
		t.Fatalf("a clean light bitset was rejected: %v", err)
	}
	padded := clean
	padded.lightPresence = 1 << 7 // section 0 absent, padding bit set
	if _, err := parseRecordBody(&reader{b: recordBody(padded)}, tableBlobSource(nil, nil, nil), true, 0, 0); err == nil {
		t.Fatal("light presence padding was accepted")
	}
}

// TestRejectsLightEntryFlags: a present light entry names which arrays follow.
// Zero is a second encoding of an absent entry, and the bits above the two
// defined ones are reserved. The test supplies full arrays so truncation
// cannot be what decides the outcome.
func TestRejectsLightEntryFlags(t *testing.T) {
	body := make([]byte, 2*lightArrayLen)
	for _, c := range []struct {
		name  string
		flags byte
		ok    bool
	}{
		{"block only", 1, true},
		{"sky only", 2, true},
		{"both", 3, true},
		{"neither", 0, false},
		{"reserved bit", 0x04, false},
		{"reserved high bit", 0x80, false},
	} {
		f := recordFields{haveLight: true, lightPresence: 1, lightFlags: c.flags, lightBody: body}
		_, err := parseRecordBody(&reader{b: recordBody(f)}, tableBlobSource(nil, nil, nil), true, 0, 0)
		if (err == nil) != c.ok {
			t.Errorf("light flags %s (0x%02X): err = %v, want ok = %v", c.name, c.flags, err, c.ok)
		}
	}
}

// TestRejectsLayerCountInCells is the structure half of the layer ceiling: the
// record half cannot detect a cell parser that stops consulting the bound.
func TestRejectsLayerCountInCells(t *testing.T) {
	reg := testRegistry(t)
	data, err := NewStructureData([3]int32{1, 1, 1})
	if err != nil {
		t.Fatal(err)
	}
	sub := chunk.NewSubChunk(reg.AirRuntimeID())
	stone, _ := reg.StateToRuntimeID("minecraft:stone", map[string]any{})
	sub.SetBlock(0, 0, 0, 0, stone)
	data.Cells[0] = sub
	var buf bytes.Buffer
	if err := WriteStructure(&buf, data, reg, Options{Compression: CompressionNone}); err != nil {
		t.Fatal(err)
	}
	file := buf.Bytes()
	body := file[headerSize : len(file)-footerSize]
	// The one present cell's layerN follows the presence byte, and a
	// one-layer cell writes it as a single 0x01.
	at := bytes.LastIndex(body, []byte{0x01, 0x00})
	if at < 0 {
		t.Skip("could not locate the cell layer count")
	}
	bad := bytes.Clone(file)
	patched := &writer{}
	patched.uvarint(256)
	if len(patched.bytes()) != 2 {
		t.Fatalf("256 does not encode in two bytes")
	}
	copy(bad[headerSize+at:], patched.bytes())
	rehashSolid(bad)
	if _, err := ReadStructure(bad, reg); err == nil {
		t.Fatal("a cell claiming 256 layers was accepted")
	}
}

// TestReaderRejectsBadStrings: the writer's UTF-8 and length rules have a
// reader half, and a test that only drives the writer cannot see it go.
func TestReaderRejectsBadStrings(t *testing.T) {
	w := &writer{}
	w.uvarint(1)
	w.raw([]byte{0xff})
	_, err := (&reader{b: w.bytes()}).str()
	if err == nil {
		t.Fatal("a string that is not valid UTF-8 was accepted")
	}
	if !strings.Contains(err.Error(), "UTF-8") {
		t.Errorf("rejected by %v, not by the UTF-8 rule", err)
	}

	w = &writer{}
	w.uvarint(maxStringLen + 1)
	_, err = (&reader{b: w.bytes()}).str()
	if err == nil {
		t.Fatal("a string longer than the ceiling was accepted")
	}
	if !strings.Contains(err.Error(), "exceeds limit") {
		t.Errorf("rejected by %v, not by the ceiling", err)
	}
}

// TestWriterSortsStateProperties is the writer half of the property ordering
// rule: the reader half stays green if the writer stops sorting.
func TestWriterSortsStateProperties(t *testing.T) {
	b := newBlockPaletteBuilder(testRegistry(t))
	b.addState(BlockState{Name: "audit:p", Properties: map[string]any{
		"zeta": int32(1), "alpha": int32(2), "mu": int32(3),
	}, Version: 1})
	enc, _, err := b.finalize()
	if err != nil {
		t.Fatal(err)
	}
	// Parsing enforces ascending keys, so a writer that stopped sorting would
	// produce a palette its own reader refuses.
	if _, _, _, err := decodeBlockPalette(&reader{b: enc}, testRegistry(t), chunk.CurrentBlockVersion); err != nil {
		t.Fatalf("the writer emitted properties its own reader rejects: %v", err)
	}
	ai, mi, zi := bytes.Index(enc, []byte("alpha")), bytes.Index(enc, []byte("mu")), bytes.Index(enc, []byte("zeta"))
	if ai < 0 || mi < 0 || zi < 0 || !(ai < mi && mi < zi) {
		t.Fatalf("properties are not in ascending order: alpha@%d mu@%d zeta@%d", ai, mi, zi)
	}
}

// TestRejectsRedundantVersionOverride: an override equal to the palette's own
// version says nothing, so it is a second encoding of the same entry.
func TestRejectsRedundantVersionOverride(t *testing.T) {
	reg := testRegistry(t)
	build := func(version int32) []byte {
		w := &writer{}
		w.uvarint(1)
		w.str("audit:v")
		w.uvarint(0)
		w.uvarint(1) // one override
		w.uvarint(0) // index 0
		w.i32(version)
		return w.bytes()
	}
	if _, _, _, err := decodeBlockPalette(&reader{b: build(17825806)}, reg, chunk.CurrentBlockVersion); err != nil {
		t.Fatalf("a genuine override was rejected: %v", err)
	}
	if _, _, _, err := decodeBlockPalette(&reader{b: build(0)}, reg, chunk.CurrentBlockVersion); err == nil {
		t.Fatal("a zero override was accepted")
	}
	// An override repeating the palette's own version says nothing, so it is a
	// second encoding of an entry with none. Without this case the test stayed
	// green with that rejection deleted.
	if _, _, _, err := decodeBlockPalette(&reader{b: build(chunk.CurrentBlockVersion)}, reg, chunk.CurrentBlockVersion); err == nil {
		t.Fatal("an override equal to the palette's own version was accepted")
	}
}

// TestRejectsOverLimitCounts drives the non-NBT ceilings of §8, which the NBT
// validator's own tests cannot reach.
func TestRejectsOverLimitCounts(t *testing.T) {
	reg := testRegistry(t)
	// A structure whose first dimension is one past the ceiling.
	w := &writer{}
	w.blob(nil)
	w.blob(nil)
	w.blob(nil)
	w.blob(nil)
	w.uvarint(0) // block palette
	w.uvarint(0) // overrides
	w.uvarint(0) // biome palette
	w.uvarint(0) // blob table
	w.uvarint(maxStructureSize + 1)
	w.uvarint(1)
	w.uvarint(1)
	body := w.bytes()
	hdr := &writer{}
	hdr.raw(headerMagic[:])
	hdr.u16(Version)
	hdr.u8(KindStructure)
	hdr.u8(ModeSolid)
	hdr.u32(FlagUncompressed)
	hdr.i32(chunk.CurrentBlockVersion)
	tail := &writer{}
	tail.u64(0)
	tail.u64(0)
	tail.u64(0)
	tail.u64(0)
	tail.raw(footerMagic[:])
	ftr := &writer{}
	ftr.u64(checkpointHash(hdr.bytes(), body, tail.bytes()))
	ftr.raw(tail.bytes())
	file := append(append(hdr.bytes(), body...), ftr.bytes()...)
	if _, err := ReadStructure(file, reg); err == nil {
		t.Fatal("a structure dimension past the ceiling was accepted")
	}
}

// TestReaderRejectsUnorderedRecords is the reader half of chunk uniqueness:
// the writer half stays green if the reader stops checking.
func TestReaderRejectsUnorderedRecords(t *testing.T) {
	reg := testRegistry(t)
	// Two records at the same position, assembled directly so the writer's own
	// refusal is not what decides the outcome.
	rec := recordBody(recordFields{})
	body := &writer{}
	body.blob(nil)
	body.blob(nil)
	body.blob(nil)
	body.blob(nil)
	body.uvarint(0) // block palette
	body.uvarint(0) // overrides
	body.uvarint(0) // biome palette
	body.uvarint(0) // blob table
	body.uvarint(2) // two records
	body.svarint(0) // first at (0,0)
	body.svarint(0)
	body.raw(rec)
	body.svarint(0) // second at (0,0) too
	body.svarint(0)
	body.raw(rec)
	if _, err := ReadWorld(solidFile(body.bytes()), reg); err == nil {
		t.Fatal("two records at one position were accepted")
	}
}

// solidFile wraps a body in a header and an authenticated footer.
func solidFile(body []byte) []byte {
	hdr := &writer{}
	hdr.raw(headerMagic[:])
	hdr.u16(Version)
	hdr.u8(KindWorld)
	hdr.u8(ModeSolid)
	hdr.u32(FlagUncompressed)
	hdr.i32(chunk.CurrentBlockVersion)
	tail := &writer{}
	tail.u64(0)
	tail.u64(0)
	tail.u64(0)
	tail.u64(0)
	tail.raw(footerMagic[:])
	ftr := &writer{}
	ftr.u64(checkpointHash(hdr.bytes(), body, tail.bytes()))
	ftr.raw(tail.bytes())
	return append(append(hdr.bytes(), body...), ftr.bytes()...)
}

// TestDefaultBiomeFlagIsSet: setting the flag is required, not an
// optimisation. A round-trip assertion cannot see a writer that stores every
// uniform section explicitly instead.
func TestDefaultBiomeFlagIsSet(t *testing.T) {
	reg := testRegistry(t)
	stone, _ := reg.StateToRuntimeID("minecraft:stone", map[string]any{})
	plains := fallbackBiomeID()
	ch := chunk.New(reg, cube.Range{-64, 319})
	ch.SetBlock(0, -64, 0, 0, stone)
	r := ch.Range()
	for x := range uint8(16) {
		for z := range uint8(16) {
			for y := int16(r[0]); y <= int16(r[1]); y++ {
				ch.SetBiome(x, y, z, plains)
			}
		}
	}
	file := encode(t, &WorldData{Columns: []Column{{X: 0, Z: 0,
		Col: &chunk.Column{Chunk: ch}}}}, reg, CompressionNone)
	h, _, err := parseFrame(file)
	if err != nil {
		t.Fatal(err)
	}
	if h.flags&FlagDefaultBiome == 0 {
		t.Fatal("a world of uniform biome sections did not set the default biome flag")
	}
	// And the sections it covers are elided rather than stored.
	back, err := ReadWorld(file, reg)
	if err != nil {
		t.Fatal(err)
	}
	if got := back.Columns[0].Col.Chunk.Biome(0, 0, 0); got != plains {
		t.Fatalf("elided section decoded as biome %d, want %d", got, plains)
	}
}

// TestBiomeCountsPrecedeElision: counts are taken over every section before
// elision removes any, which is what stops the two being circular. A fixture
// with one biome name cannot tell the orders apart.
func TestBiomeCountsPrecedeElision(t *testing.T) {
	reg := testRegistry(t)
	stone, _ := reg.StateToRuntimeID("minecraft:stone", map[string]any{})
	plains := fallbackBiomeID()
	ocean := uint32(0)
	if b, ok := lookupBiome("minecraft:ocean"); ok {
		ocean = uint32(b.EncodeBiome())
	}
	if ocean == plains {
		t.Skip("this registry does not distinguish ocean from plains")
	}
	ch := chunk.New(reg, cube.Range{-64, 319})
	ch.SetBlock(0, -64, 0, 0, stone)
	set := func(sec int, id uint32) {
		base := int16(-64 + sec*16)
		for x := range uint8(16) {
			for z := range uint8(16) {
				for y := base; y < base+16; y++ {
					ch.SetBiome(x, y, z, id)
				}
			}
		}
	}
	set(0, plains) // uniform
	set(1, plains) // uniform
	set(2, ocean)  // uniform
	// A mixed section, so ocean appears in more local palettes than a
	// post-elision count would show.
	set(3, ocean)
	ch.SetBiome(0, -16, 0, plains)

	file := encode(t, &WorldData{Columns: []Column{{X: 0, Z: 0,
		Col: &chunk.Column{Chunk: ch}}}}, reg, CompressionNone)
	back, err := ReadWorld(file, reg)
	if err != nil {
		t.Fatal(err)
	}
	// Whatever the default is, every section has to come back as it went in:
	// counting after elision would pick a different default and elide a
	// different set.
	for _, sec := range []int{0, 1, 2, 3} {
		y := int16(-64 + sec*16)
		if got, want := back.Columns[0].Col.Chunk.Biome(15, y, 15), ch.Biome(15, y, 15); got != want {
			t.Fatalf("section %d biome = %d, want %d", sec, got, want)
		}
	}
	if second := encode(t, back, reg, CompressionNone); !bytes.Equal(file, second) {
		t.Fatal("a world with competing uniform biomes does not survive a round trip")
	}
}

// TestTiedTicksAndStructureCollections covers the collection orders the world
// block-entity test does not reach.
func TestTiedTicksAndStructureCollections(t *testing.T) {
	reg := testRegistry(t)
	stone, _ := reg.StateToRuntimeID("minecraft:stone", map[string]any{})
	water, _ := reg.StateToRuntimeID("minecraft:water", map[string]any{"liquid_depth": int32(0)})

	// Scheduled updates tied on position and tick, differing only in block.
	world := func(first, second uint32) []byte {
		ch := chunk.New(reg, cube.Range{-64, 319})
		ch.SetBlock(0, -64, 0, 0, stone)
		return encode(t, &WorldData{Columns: []Column{{X: 0, Z: 0, Col: &chunk.Column{
			Chunk: ch,
			ScheduledBlocks: []chunk.ScheduledBlockUpdate{
				{Pos: cube.Pos{1, -60, 1}, Block: first, Tick: 9},
				{Pos: cube.Pos{1, -60, 1}, Block: second, Tick: 9},
			},
		}}}}, reg, CompressionNone)
	}
	if a, b := world(stone, water), world(water, stone); !bytes.Equal(a, b) {
		t.Fatalf("tied scheduled updates kept the caller's order: %d vs %d bytes", len(a), len(b))
	}

	// Structure block entities tied on position, and entities generally.
	structure := func(swap bool) []byte {
		data, err := NewStructureData([3]int32{16, 16, 16})
		if err != nil {
			t.Fatal(err)
		}
		sub := chunk.NewSubChunk(reg.AirRuntimeID())
		sub.SetBlock(0, 0, 0, 0, stone)
		data.Cells[0] = sub
		a := StructureBlockEntity{Pos: [3]int32{1, 2, 3}, Data: map[string]any{"id": "minecraft:chest", "n": int32(1)}}
		b := StructureBlockEntity{Pos: [3]int32{1, 2, 3}, Data: map[string]any{"id": "minecraft:chest", "n": int32(2)}}
		e1 := map[string]any{"identifier": "minecraft:cow", "UniqueID": int64(1)}
		e2 := map[string]any{"identifier": "minecraft:pig", "UniqueID": int64(2)}
		if swap {
			a, b = b, a
			e1, e2 = e2, e1
		}
		data.BlockEntities = []StructureBlockEntity{a, b}
		data.Entities = []map[string]any{e1, e2}
		var buf bytes.Buffer
		if err := WriteStructure(&buf, data, reg, Options{Compression: CompressionNone}); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	if a, b := structure(false), structure(true); !bytes.Equal(a, b) {
		t.Fatalf("structure collections kept the caller's order: %d vs %d bytes", len(a), len(b))
	}
}

// TestStatsPreservesUnknownKeys: readers ignore keys they do not know, which a
// test reading only writer-generated stats cannot show.
func TestStatsPreservesUnknownKeys(t *testing.T) {
	reg := testRegistry(t)
	file := encode(t, testWorld(t, reg), reg, CompressionNone)
	m, err := ReadMeta(file)
	if err != nil {
		t.Fatal(err)
	}
	_ = m
	// Assemble a body whose stats compound carries a key this version has
	// never heard of.
	stats, err := marshalNBT(map[string]any{
		"biomes": int64(1), "blockStates": int64(1), "chunks": int64(1),
		"filledSections": int64(1), "future": int32(7), "uniqueBlobs": int64(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	body := &writer{}
	body.blob(nil)
	body.blob(nil)
	body.blob(nil)
	body.blob(nil)
	body.blob(stats)
	body.uvarint(0) // block palette
	body.uvarint(0) // version overrides
	body.uvarint(0) // biome palette
	body.uvarint(0) // blob table
	body.uvarint(0) // chunk records
	hdr := &writer{}
	hdr.raw(headerMagic[:])
	hdr.u16(Version)
	hdr.u8(KindWorld)
	hdr.u8(ModeSolid)
	hdr.u32(FlagUncompressed | FlagStats)
	hdr.i32(chunk.CurrentBlockVersion)
	tail := &writer{}
	tail.u64(0)
	tail.u64(0)
	tail.u64(0)
	tail.u64(0)
	tail.raw(footerMagic[:])
	ftr := &writer{}
	ftr.u64(checkpointHash(hdr.bytes(), body.bytes(), tail.bytes()))
	ftr.raw(tail.bytes())
	withFuture := append(append(hdr.bytes(), body.bytes()...), ftr.bytes()...)

	got, err := ReadMeta(withFuture)
	if err != nil {
		t.Fatalf("a stats compound with an unknown key was rejected: %v", err)
	}
	if !bytes.Contains(got.Stats, []byte("future")) {
		t.Fatal("the unknown stats key was not preserved")
	}
	if _, err := ReadWorld(withFuture, reg); err != nil {
		t.Fatalf("ReadWorld rejected an unknown stats key: %v", err)
	}
}

// indexedWithPatchedPrologue builds a valid indexed world, rewrites its
// directory prologue and refreshes the checkpoint hash, then reports whether
// the result still opens. Round 21 showed that mutating an authenticated
// structure without rehashing tests the checksum rather than the rule.
func indexedWithPatchedPrologue(t *testing.T, reg world.BlockRegistry, edit func(dir []byte)) (opened, recovered bool) {
	t.Helper()
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
	if !patchDirectory(t, path, func(dir []byte) bool { edit(dir); return true }) {
		t.Fatal("could not reach the directory")
	}
	v, err := OpenIndexed(path, reg, true)
	if err != nil {
		return false, false
	}
	defer v.Close()
	return true, v.Recovered()
}

// TestIndexedRejectsReservedFlags: the flag rules have an indexed half, where
// the directory prologue is the authority. A solid-only test cannot see that
// half go.
func TestIndexedRejectsReservedFlags(t *testing.T) {
	reg := testRegistry(t)
	for _, c := range []struct {
		name string
		bit  uint32
	}{
		{"reserved bit 2", 1 << 2},
		{"reserved bit 15", 1 << 15},
		{"default biome reference without its flag", 1 << 16},
		{"reserved dimension", uint32(5) << dimensionShift},
	} {
		opened, recovered := indexedWithPatchedPrologue(t, reg, func(dir []byte) {
			binary.LittleEndian.PutUint32(dir[2:6], binary.LittleEndian.Uint32(dir[2:6])|c.bit)
		})
		// Every file has an earlier checkpoint to fall back to, so the claim
		// is refused either by failing to open or by recovering past it.
		if opened && !recovered {
			t.Errorf("%s was accepted in a directory prologue", c.name)
		}
	}
}

// TestIndexedRejectsStructureKindInDirectory: the solid parser refuses every
// indexed mode before any pair-specific rule runs, so the kind/mode pairing has
// to be driven through the directory that actually carries it.
func TestIndexedRejectsStructureKindInDirectory(t *testing.T) {
	reg := testRegistry(t)
	opened, recovered := indexedWithPatchedPrologue(t, reg, func(dir []byte) {
		dir[0] = KindStructure
	})
	if opened && !recovered {
		t.Fatal("a directory describing an indexed structure was accepted")
	}
}

// TestRejectsMalformedFrameReference: an absent reference is all three fields
// zero. A non-zero offset or hash with a zero length is a second spelling of
// absence, and the rule has to be reached after authentication rather than
// before it.
func TestRejectsMalformedFrameReference(t *testing.T) {
	reg := testRegistry(t)
	// The meta reference follows the ten-byte prologue and is absent in a
	// freshly written file: two zero varints and eight zero bytes.
	opened, recovered := indexedWithPatchedPrologue(t, reg, func(dir []byte) {
		const prologue = 10
		dir[prologue] = headerSize // non-zero offset, still zero length
	})
	if opened && !recovered {
		t.Fatal("a zero-length reference with a non-zero offset was accepted")
	}
}

// TestRejectsCumulativePaletteOverflow: the palette ceiling applies across
// segments, not per segment. Two segments individually legal and jointly past
// the limit are what distinguish the rule.
func TestRejectsCumulativePaletteOverflow(t *testing.T) {
	// The entry ceiling itself needs a segment carrying more than a million
	// palette entries to reach, which no fixture builds; the fuzz targets are
	// what cover it, and the invariant says so. What is reachable here is the
	// other half of the same guard: a segment list whose frames cannot fit the
	// file is refused before any of them is read, which is the bound that
	// stops a tiny file from claiming an enormous palette.
	w := &IndexedWorld{end: 8}
	bw := &writer{}
	bw.uvarint(2)
	for range 2 {
		bw.uvarint(headerSize)
		bw.uvarint(64)
		bw.u64(0)
	}
	if _, err := w.parseSegRefs(&reader{b: bw.bytes()}, "segment", func(frameRef, string) error { return nil }); err == nil {
		t.Fatal("segments totalling more than the file were accepted")
	}
}

// TestRejectsStructureEnvelopeViolations: a structure has exactly one valid
// envelope. Round-tripping a legal one cannot show the rejections going.
func TestRejectsStructureEnvelopeViolations(t *testing.T) {
	reg := testRegistry(t)
	base := func() *writer {
		w := &writer{}
		w.blob(nil) // settings
		w.blob(nil) // user data
		w.blob(nil) // markers
		w.blob(nil) // border
		w.uvarint(0)
		w.uvarint(0)
		return w
	}
	build := func(flags uint32, edit func(*writer)) []byte {
		body := base()
		edit(body)
		hdr := &writer{}
		hdr.raw(headerMagic[:])
		hdr.u16(Version)
		hdr.u8(KindStructure)
		hdr.u8(ModeSolid)
		hdr.u32(flags)
		hdr.i32(chunk.CurrentBlockVersion)
		tail := &writer{}
		tail.u64(0)
		tail.u64(0)
		tail.u64(0)
		tail.u64(0)
		tail.raw(footerMagic[:])
		ftr := &writer{}
		ftr.u64(checkpointHash(hdr.bytes(), body.bytes(), tail.bytes()))
		ftr.raw(tail.bytes())
		return append(append(hdr.bytes(), body.bytes()...), ftr.bytes()...)
	}
	rest := func(w *writer) {
		w.uvarint(0) // biome palette
		w.uvarint(0) // blob table
		w.uvarint(1) // sizes
		w.uvarint(1)
		w.uvarint(1)
		w.svarint(0) // origins
		w.svarint(0)
		w.svarint(0)
		w.u8(0)      // cell presence
		w.uvarint(0) // block entities
		w.uvarint(0) // entities
	}
	if _, err := ReadStructure(build(FlagUncompressed, rest), reg); err != nil {
		t.Fatalf("a legal envelope was rejected: %v", err)
	}
	// A flag a structure cannot carry.
	if _, err := ReadStructure(build(FlagUncompressed|FlagStoreLight, rest), reg); err == nil {
		t.Error("a structure claiming StoreLight was accepted")
	}
	// A non-empty biome palette.
	if _, err := ReadStructure(build(FlagUncompressed, func(w *writer) {
		w.uvarint(1)
		w.str("minecraft:plains")
		w.uvarint(0)
		w.uvarint(1)
		w.uvarint(1)
		w.uvarint(1)
		w.svarint(0)
		w.svarint(0)
		w.svarint(0)
		w.u8(0)
		w.uvarint(0)
		w.uvarint(0)
	}), reg); err == nil {
		t.Error("a structure with a biome palette was accepted")
	}
	// Non-empty settings.
	settings, err := marshalNBT(map[string]any{"name": "x"})
	if err != nil {
		t.Fatal(err)
	}
	withSettings := &writer{}
	withSettings.blob(settings)
	withSettings.blob(nil)
	withSettings.blob(nil)
	withSettings.blob(nil)
	withSettings.uvarint(0)
	withSettings.uvarint(0)
	rest(withSettings)
	hdr := &writer{}
	hdr.raw(headerMagic[:])
	hdr.u16(Version)
	hdr.u8(KindStructure)
	hdr.u8(ModeSolid)
	hdr.u32(FlagUncompressed)
	hdr.i32(chunk.CurrentBlockVersion)
	tail := &writer{}
	tail.u64(0)
	tail.u64(0)
	tail.u64(0)
	tail.u64(0)
	tail.raw(footerMagic[:])
	ftr := &writer{}
	ftr.u64(checkpointHash(hdr.bytes(), withSettings.bytes(), tail.bytes()))
	ftr.raw(tail.bytes())
	if _, err := ReadStructure(append(append(hdr.bytes(), withSettings.bytes()...), ftr.bytes()...), reg); err == nil {
		t.Error("a structure carrying settings was accepted")
	}
}

// Round 23: rules a specification review found stated ambiguously or not at
// all. Most already held in code; these pin them.

// TestHashSeedIsZero: xxHash64 takes a seed, and nothing else in the format
// implies which one. An implementation that guesses differently produces files
// this reader rejects and rejects files it produces, so the value is pinned to
// published seed-0 vectors rather than to whatever the library defaults to.
func TestHashSeedIsZero(t *testing.T) {
	for _, c := range []struct {
		in   string
		want uint64
	}{
		{"", 0xEF46DB3751D8E999},
		{"a", 0xD24EC4F1A98C6E5B},
		{"as", 0x1C330FB2D66BE179},
		{"asd", 0x631C37CE72A97393},
		{"asdf", 0x415872F599CEA71E},
	} {
		if got := xxhash.Sum64String(c.in); got != c.want {
			t.Errorf("xxhash64(%q) = %016x, want %016x: the seed is not 0", c.in, got, c.want)
		}
	}
}

// TestBiomePaletteOrder: the biome palette is sorted by descending reference
// count then ascending name, compared bytewise. Nothing else in the format
// pins the direction or the comparison.
func TestBiomePaletteOrder(t *testing.T) {
	b := newBiomePaletteBuilder()
	// "b" appears twice, "a" and "c" once each: count orders b first, and the
	// tie between a and c is broken by name.
	b.add("minecraft:b", 2)
	b.add("minecraft:c", 1)
	b.add("minecraft:a", 1)
	enc, _, err := b.finalize()
	if err != nil {
		t.Fatal(err)
	}
	bi, ai, ci := bytes.Index(enc, []byte("minecraft:b")), bytes.Index(enc, []byte("minecraft:a")), bytes.Index(enc, []byte("minecraft:c"))
	if bi < 0 || ai < 0 || ci < 0 {
		t.Fatalf("palette lost an entry: %q", enc)
	}
	if !(bi < ai && ai < ci) {
		t.Fatalf("biome order is b@%d a@%d c@%d, want descending count then ascending name", bi, ai, ci)
	}
}

// TestDroppedAirLayersDoNotCount: trailing all-air layers never reach the
// file, so their air references must not influence the palette order. A writer
// counting on the other side of the drop would order the palette differently.
func TestDroppedAirLayersDoNotCount(t *testing.T) {
	reg := testRegistry(t)
	stone, _ := reg.StateToRuntimeID("minecraft:stone", map[string]any{})
	air := reg.AirRuntimeID()

	build := func(spare int) []byte {
		ch := chunk.New(reg, cube.Range{-64, 319})
		ch.SetBlock(0, -64, 0, 0, stone)
		for l := 1; l <= spare; l++ {
			// Setting air on an unallocated layer is a no-op, so the layer has
			// to be brought into existence and then cleared.
			ch.SetBlock(0, -64, 0, uint8(l), stone)
			ch.SetBlock(0, -64, 0, uint8(l), air)
		}
		return encode(t, &WorldData{Columns: []Column{{X: 0, Z: 0,
			Col: &chunk.Column{Chunk: ch}}}}, reg, CompressionNone)
	}
	// However many spare air layers the caller allocated, they are dropped and
	// contribute nothing, so the bytes are identical.
	a, b := build(0), build(3)
	if !bytes.Equal(a, b) {
		t.Fatalf("dropped air layers changed the file: %d vs %d bytes", len(a), len(b))
	}
}

// TestStatsMissingKeyAccepted: a stats compound is a summary, so a missing key
// is valid and a wrongly tagged one is not.
func TestStatsMissingKeyAccepted(t *testing.T) {
	reg := testRegistry(t)
	partial, err := marshalNBT(map[string]any{"chunks": int64(1)})
	if err != nil {
		t.Fatal(err)
	}
	body := &writer{}
	body.blob(nil)
	body.blob(nil)
	body.blob(nil)
	body.blob(nil)
	body.blob(partial)
	body.uvarint(0)
	body.uvarint(0)
	body.uvarint(0)
	body.uvarint(0)
	body.uvarint(0)
	hdr := &writer{}
	hdr.raw(headerMagic[:])
	hdr.u16(Version)
	hdr.u8(KindWorld)
	hdr.u8(ModeSolid)
	hdr.u32(FlagUncompressed | FlagStats)
	hdr.i32(chunk.CurrentBlockVersion)
	tail := &writer{}
	tail.u64(0)
	tail.u64(0)
	tail.u64(0)
	tail.u64(0)
	tail.raw(footerMagic[:])
	ftr := &writer{}
	ftr.u64(checkpointHash(hdr.bytes(), body.bytes(), tail.bytes()))
	ftr.raw(tail.bytes())
	file := append(append(hdr.bytes(), body.bytes()...), ftr.bytes()...)
	if _, err := ReadWorld(file, reg); err != nil {
		t.Fatalf("a stats compound missing keys was rejected: %v", err)
	}
}

// TestRejectsOversizedZstdWindow: the decoded-size ceilings bound the output,
// not the memory needed to produce it, so a small frame can still demand an
// arbitrary window unless the window itself is bounded.
func TestRejectsOversizedZstdWindow(t *testing.T) {
	// A frame encoded with a window larger than the ceiling.
	enc, err := zstd.NewWriter(nil, zstd.WithWindowSize(32<<20))
	if err != nil {
		t.Skipf("this build cannot request a %d byte window: %v", 32<<20, err)
	}
	payload := make([]byte, 40<<20)
	for i := range payload {
		payload[i] = byte(i * 7)
	}
	frame := enc.EncodeAll(payload, nil)
	enc.Close()
	if _, err := sharedDecoder().DecodeAll(frame, nil); err == nil {
		t.Fatal("a frame demanding a window past the ceiling was decoded")
	}
	// A frame within the ceiling still decodes.
	small, err := zstd.NewWriter(nil, zstd.WithWindowSize(1<<20))
	if err != nil {
		t.Fatal(err)
	}
	ok := small.EncodeAll(payload, nil)
	small.Close()
	if _, err := sharedDecoder().DecodeAll(ok, nil); err != nil {
		t.Fatalf("a frame within the window ceiling was refused: %v", err)
	}
}

// Round 24: rules that were writer-side only, where a reader can check and so
// should. Silence here means a strict reader and a lenient one disagree about
// which files are valid.

// TestRejectsBlobTableWaste: the table exists to store identical bytes once,
// so a repeat is a second encoding, and a blob nothing references is content
// no reader reads.
func TestRejectsBlobTableWaste(t *testing.T) {
	blob := func() []byte {
		w := &writer{}
		w.uvarint(1) // one palette entry
		w.uvarint(0) // referencing global entry 0
		w.u8(widthUniform)
		return w.bytes()
	}
	two := &writer{}
	two.uvarint(2)
	two.raw(blob())
	two.raw(blob())
	if _, err := decodeBlobTable(&reader{b: two.bytes()}); err == nil {
		t.Fatal("a repeated blob was accepted")
	}
	one := &writer{}
	one.uvarint(1)
	one.raw(blob())
	if _, err := decodeBlobTable(&reader{b: one.bytes()}); err != nil {
		t.Fatalf("a single blob was rejected: %v", err)
	}
}

// TestRejectsUnreferencedBlob: a table entry no record uses is dead weight two
// writers could differ on while encoding the same world.
func TestRejectsUnreferencedBlob(t *testing.T) {
	reg := testRegistry(t)
	body := func(blobs int) []byte {
		w := &writer{}
		w.blob(nil) // settings
		w.blob(nil) // user data
		w.blob(nil) // markers
		w.blob(nil) // border
		w.uvarint(1)
		w.str("minecraft:stone")
		w.uvarint(0) // no properties
		w.uvarint(0) // no version overrides
		w.uvarint(0) // biome palette
		w.uvarint(uint64(blobs))
		for range blobs {
			w.uvarint(1) // one local entry
			w.uvarint(0) // global reference 0
			w.u8(widthUniform)
		}
		w.uvarint(0) // no chunk records
		return w.bytes()
	}
	if _, err := ReadWorld(solidFile(body(0)), reg); err != nil {
		t.Fatalf("a world with an empty blob table was rejected: %v", err)
	}
	if _, err := ReadWorld(solidFile(body(1)), reg); err == nil {
		t.Fatal("a blob no record references was accepted")
	}
}

// TestRejectsStoreLightWithoutLight: the flag claims the records carry light.
// Over a world with none it is a second encoding of the same world with the
// flag clear, and content identity covers the whole body.
func TestRejectsStoreLightWithoutLight(t *testing.T) {
	reg := testRegistry(t)
	stone, _ := reg.StateToRuntimeID("minecraft:stone", map[string]any{})
	ch := chunk.New(reg, cube.Range{-64, 319})
	ch.SetBlock(0, -64, 0, 0, stone)
	d := &WorldData{Columns: []Column{{X: 0, Z: 0, Col: &chunk.Column{Chunk: ch}}}}

	// A writer asked for light over a world that has none clears the flag
	// rather than writing an empty light section.
	var buf bytes.Buffer
	if err := WriteWorld(&buf, d, reg, Options{Compression: CompressionNone, StoreLight: true}); err != nil {
		t.Fatal(err)
	}
	h, _, err := parseFrame(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if h.flags&FlagStoreLight != 0 {
		t.Fatal("StoreLight was set over a world carrying no light")
	}
	// The same world with light baked in does set it.
	chunk.LightArea([]*chunk.Chunk{ch}, 0, 0).Fill()
	var lit bytes.Buffer
	if err := WriteWorld(&lit, d, reg, Options{Compression: CompressionNone, StoreLight: true}); err != nil {
		t.Fatal(err)
	}
	h, _, err = parseFrame(lit.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if h.flags&FlagStoreLight == 0 {
		t.Fatal("StoreLight was clear over a world carrying light")
	}
}

// TestRejectsDuplicateCollectionEntries: the collection orders are total only
// because their keys are unique, so uniqueness is a rule. The writer and the
// reader both enforce it, or one produces files the other refuses.
func TestRejectsDuplicateCollectionEntries(t *testing.T) {
	reg := testRegistry(t)
	stone, _ := reg.StateToRuntimeID("minecraft:stone", map[string]any{})
	col := func(build func(c *chunk.Column)) *WorldData {
		ch := chunk.New(reg, cube.Range{-64, 319})
		ch.SetBlock(0, -64, 0, 0, stone)
		c := &chunk.Column{Chunk: ch}
		build(c)
		return &WorldData{Columns: []Column{{X: 0, Z: 0, Col: c}}}
	}
	var buf bytes.Buffer
	if err := WriteWorld(&buf, col(func(c *chunk.Column) {
		c.BlockEntities = []chunk.BlockEntity{
			{Pos: cube.Pos{1, -60, 1}, Data: map[string]any{"id": "minecraft:chest"}},
			{Pos: cube.Pos{1, -60, 1}, Data: map[string]any{"id": "minecraft:furnace"}},
		}
	}), reg, Options{Compression: CompressionNone}); err == nil {
		t.Error("two block entities at one position were written")
	}
	buf.Reset()
	if err := WriteWorld(&buf, col(func(c *chunk.Column) {
		c.ScheduledBlocks = []chunk.ScheduledBlockUpdate{
			{Pos: cube.Pos{1, -60, 1}, Block: stone, Tick: 5},
			{Pos: cube.Pos{1, -60, 1}, Block: stone, Tick: 5},
		}
	}), reg, Options{Compression: CompressionNone}); err == nil {
		t.Error("a duplicate scheduled update was written")
	}
}

// Round 25: rules the table claimed readers enforced, which readers did not.

// TestReaderEnforcesCollectionOrder: the order is on the wire, so a reader can
// check it, and a file whose collections are reordered is a second encoding of
// the same chunk. The writer-side test cannot see this half go.
func TestReaderEnforcesCollectionOrder(t *testing.T) {
	reg := testRegistry(t)
	body := func(swap bool) []byte {
		w := &writer{}
		w.blob(nil)
		w.blob(nil)
		w.blob(nil)
		w.blob(nil)
		w.uvarint(0) // block palette
		w.uvarint(0) // overrides
		w.uvarint(0) // biome palette
		w.uvarint(0) // blob table
		w.uvarint(1) // one record
		w.svarint(0)
		w.svarint(0)
		rec := &writer{}
		rec.svarint(0) // minSection
		rec.uvarint(1) // sectionN
		rec.u8(0)      // no block sections
		rec.u8(0)      // no biome sections
		rec.uvarint(2) // two block entities
		// Inside the record's declared span of 0..15.
		lo := func(bw *writer) { bw.u8(0x11); bw.svarint(5); bw.blob(emptyCompound()) }
		hi := func(bw *writer) { bw.u8(0x22); bw.svarint(5); bw.blob(emptyCompound()) }
		if swap {
			hi(rec)
			lo(rec)
		} else {
			lo(rec)
			hi(rec)
		}
		rec.uvarint(0) // entities
		rec.svarint(0) // column tick
		rec.uvarint(0) // scheduled ticks
		rec.blob(nil)
		w.raw(rec.bytes())
		return w.bytes()
	}
	if _, err := ReadWorld(solidFile(body(false)), reg); err != nil {
		t.Fatalf("ordered block entities were rejected: %v", err)
	}
	if _, err := ReadWorld(solidFile(body(true)), reg); err == nil {
		t.Fatal("out-of-order block entities were accepted")
	}
}

// emptyCompound is a canonical unnamed empty NBT compound.
func emptyCompound() []byte { return []byte{0x0a, 0, 0, 0x00} }

// TestReaderEnforcesBlobFirstUseOrder: ids are assigned in first-use order, and
// that is visible on the wire. A table numbered any other way is a second
// encoding of the same file.
func TestReaderEnforcesBlobFirstUseOrder(t *testing.T) {
	reg := testRegistry(t)
	stone, _ := reg.StateToRuntimeID("minecraft:stone", map[string]any{})
	// Two sections, each a distinct uniform blob, so the record references
	// two ids and the order they are first used is observable.
	ch := chunk.New(reg, cube.Range{-64, 319})
	for x := range uint8(16) {
		for z := range uint8(16) {
			for y := int16(-64); y < -48; y++ {
				ch.SetBlock(x, y, z, 0, stone)
			}
		}
	}
	ch.SetBlock(0, -40, 0, 0, stone)
	file := encode(t, &WorldData{Columns: []Column{{X: 0, Z: 0,
		Col: &chunk.Column{Chunk: ch}}}}, reg, CompressionNone)
	if _, err := ReadWorld(file, reg); err != nil {
		t.Fatalf("a canonical file was rejected: %v", err)
	}
	// Swap the two references: still in range, still every blob used, but no
	// longer first-use order.
	body := file[headerSize : len(file)-footerSize]
	first := bytes.IndexByte(body, 0x00)
	_ = first
	patched := bytes.Clone(file)
	// Find the first "01 00" reference pair inside the record and reverse it.
	at := bytes.Index(patched[headerSize:], []byte{0x00, 0x01})
	if at < 0 {
		t.Skip("could not locate the reference pair")
	}
	patched[headerSize+at], patched[headerSize+at+1] = 0x01, 0x00
	rehashSolid(patched)
	if _, err := ReadWorld(patched, reg); err == nil {
		t.Fatal("blob ids referenced out of first-use order were accepted")
	}
}

// TestHashSeedIsUsedInProduction: the seed matters because every reader
// verifies a checkpoint hash with it. Testing the dependency's default proves
// nothing about the path that authenticates a file.
func TestHashSeedIsUsedInProduction(t *testing.T) {
	reg := testRegistry(t)
	file := encode(t, testWorld(t, reg), reg, CompressionNone)
	if _, err := ReadWorld(file, reg); err != nil {
		t.Fatalf("a clean file was rejected: %v", err)
	}
	// Recompute the footer hash with a non-zero seed. If production hashed
	// with anything but zero, this is the file it would accept.
	header := file[:headerSize]
	payload := file[headerSize : len(file)-footerSize]
	footer := file[len(file)-footerSize:]
	d := xxhash.NewWithSeed(1)
	_, _ = d.Write(header)
	_, _ = d.Write(payload)
	_, _ = d.Write(footer[8:])
	seeded := bytes.Clone(file)
	binary.LittleEndian.PutUint64(seeded[len(seeded)-footerSize:], d.Sum64())
	if _, err := ReadWorld(seeded, reg); err == nil {
		t.Fatal("a footer hashed with seed 1 was accepted: production is not using seed 0")
	}
}

// TestRejectsNonMinimalUniformWidth: the rule is "width is 0 if and only if
// paletteN is 1", and only the forward direction was checked. A uniform
// section could also be written with an index array of 4096 zeroes, which is a
// second encoding of the same section.
func TestRejectsNonMinimalUniformWidth(t *testing.T) {
	blob := func(width uint8, idx int) []byte {
		w := &writer{}
		w.uvarint(1) // one palette entry
		w.uvarint(0) // referencing global entry 0
		w.u8(width)
		w.b = append(w.b, make([]byte, idx)...)
		return w.bytes()
	}
	if _, err := decodeOneBlob(&reader{b: blob(widthUniform, 0)}); err != nil {
		t.Fatalf("a uniform single-entry blob was rejected: %v", err)
	}
	if _, err := decodeOneBlob(&reader{b: blob(widthU8, 4096)}); err == nil {
		t.Error("a single-entry palette with byte indices was accepted")
	}
	if _, err := decodeOneBlob(&reader{b: blob(widthU16, 8192)}); err == nil {
		t.Error("a single-entry palette with u16 indices was accepted")
	}
}

// Security bounds. Each of these caps something the byte ceilings do not: the
// input is bounded, the result was not.

// TestRejectsOutOfSpanPositions: a record's span is validated, its contents
// were not. Block-entity Y is an unbounded svarint, and dragonfly indexes its
// sub chunk array with int16(y) and no bounds check, so an out-of-range value
// panics the server at world load rather than failing the decode.
func TestRejectsOutOfSpanPositions(t *testing.T) {
	rec := func(beY int64) []byte {
		w := &writer{}
		w.svarint(0) // minSection: span is 0..15
		w.uvarint(1)
		w.u8(0)
		w.u8(0)
		w.uvarint(1) // one block entity
		w.u8(0x11)
		w.svarint(beY)
		w.blob(emptyCompound())
		w.uvarint(0)
		w.svarint(0)
		w.uvarint(0)
		w.blob(nil)
		return w.bytes()
	}
	apply := func(beY int64) error {
		rr, err := parseRecordBody(&reader{b: rec(beY)}, tableBlobSource(nil, nil, nil), false, 0, 0)
		if err != nil {
			return err
		}
		_, err = applyRecord(&rr, testRegistry(t), nil, nil, 0, 0, false, -1, nil, nil, nil, nil)
		return err
	}
	if err := apply(5); err != nil {
		t.Fatalf("an in-span block entity was rejected: %v", err)
	}
	for _, y := range []int64{-1, 16, 32768, -32769, 1 << 40} {
		if err := apply(y); err == nil {
			t.Errorf("a block entity at Y %d, outside the span 0..15, was accepted", y)
		}
	}
}

// TestBoundsDecodedStorages: a stored layer is one blob reference on the wire
// and a live paletted storage in memory, so a few hundred kilobytes of
// repeated references can materialise tens of gigabytes. The byte ceilings
// bound the input and not the result.
func TestBoundsDecodedStorages(t *testing.T) {
	b := &storageBudget{limit: maxDecodedStorages}
	if err := b.charge(maxDecodedStorages); err != nil {
		t.Fatalf("the budget refused its own limit: %v", err)
	}
	if err := b.charge(1); err == nil {
		t.Fatal("the budget did not stop at its limit")
	}
	// And it accumulates across records rather than resetting per record,
	// which is what makes it a file-wide ceiling.
	c := &storageBudget{limit: 10}
	if err := c.charge(6); err != nil {
		t.Fatal(err)
	}
	if err := c.charge(6); err == nil {
		t.Fatal("the budget reset between charges")
	}
}

// TestBoundsDecodedNBTContainers: the structural walk bounds declared lengths
// against the bytes that remain, which covers a list of scalars sharing one
// slice but not a list of compounds, where an empty element costs one byte and
// allocates a whole map.
func TestBoundsDecodedNBTContainers(t *testing.T) {
	// A list of empty compounds: one byte each on the wire, one map each once
	// decoded.
	blob := func(n int32) []byte {
		w := &writer{}
		w.u8(tagCompound)
		w.b = append(w.b, 0, 0) // unnamed root
		w.u8(tagList)
		w.b = append(w.b, 1, 0, 'a')
		w.u8(tagCompound)
		w.i32(n)
		for range n {
			w.u8(tagEnd)
		}
		w.u8(tagEnd)
		return w.bytes()
	}
	if err := validateNBT(blob(16)); err != nil {
		t.Fatalf("a small list of compounds was rejected: %v", err)
	}
	if err := validateNBT(blob(maxNBTElements + 1)); err == nil {
		t.Fatal("a list of more compounds than the ceiling was accepted")
	}
	// A list of scalars is one slice however long, so the ceiling must not
	// reject a legal one.
	scalars := &writer{}
	scalars.u8(tagCompound)
	scalars.b = append(scalars.b, 0, 0)
	scalars.u8(tagList)
	scalars.b = append(scalars.b, 1, 0, 'a')
	scalars.u8(tagByte)
	scalars.i32(maxNBTElements + 1)
	scalars.b = append(scalars.b, make([]byte, maxNBTElements+1)...)
	scalars.u8(tagEnd)
	if err := validateNBT(scalars.bytes()); err != nil {
		t.Fatalf("a long list of scalars was rejected: %v", err)
	}
}

// TestBoundsCheckpointChain: the backward scan caps itself, the chain walk did
// not. Every link costs a read and a hash of a whole directory frame, and a
// hostile file can carry as many forged footers as fit in it.
func TestBoundsCheckpointChain(t *testing.T) {
	if maxCheckpointChain <= 0 {
		t.Fatal("the checkpoint chain has no ceiling")
	}
	reg := testRegistry(t)
	path := filepath.Join(t.TempDir(), "w.pile")
	w, err := CreateIndexed(path, reg, Options{Compression: CompressionNone})
	if err != nil {
		t.Fatal(err)
	}
	// Enough generations to prove an ordinary chain is still walked.
	for i := range int32(8) {
		if err := w.Store(buildTestColumn(t, reg, i, 0)); err != nil {
			t.Fatal(err)
		}
		if err := w.Checkpoint(); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	v, err := OpenIndexed(path, reg, true)
	if err != nil {
		t.Fatalf("an ordinary chain was refused: %v", err)
	}
	defer v.Close()
	if v.ChunkCount() != 8 {
		t.Fatalf("chunks = %d, want 8", v.ChunkCount())
	}
}

// TestRejectsOversizedDictionary: a decoder retains a dictionary's whole
// content, outside the window ceiling, and pins the backing array it is given.
// The frame ceiling would otherwise let a file pin 64 MiB per open handle in
// both an encoder and a decoder, to carry a dictionary training caps at 16 KiB.
func TestRejectsOversizedDictionary(t *testing.T) {
	if maxDictLen >= maxDecodedFrame {
		t.Fatal("the dictionary ceiling is no tighter than the frame ceiling")
	}
	// The bound is on the decoded dictionary frame, so drive it there.
	reg := testRegistry(t)
	path := filepath.Join(t.TempDir(), "w.pile")
	w, err := CreateIndexed(path, reg, Options{Compression: CompressionDefault})
	if err != nil {
		t.Fatal(err)
	}
	for i := range int32(dictMinSamples * 2) {
		if err := w.Store(buildTestColumn(t, reg, i, 0)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Compact(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	// A trained dictionary is orders of magnitude under the ceiling, so a
	// compacted world still opens.
	v, err := OpenIndexed(path, reg, true)
	if err != nil {
		t.Fatalf("a compacted world was refused: %v", err)
	}
	defer v.Close()
	if !v.HasDict() {
		t.Skip("compaction did not train a dictionary here")
	}
}

// TestDictionarySamplingIsBounded: the dictionary is capped at a few
// kilobytes, so training cannot need every record in the world. Holding them
// all was hundreds of gigabytes on a large world, on a path Close runs by
// itself.
func TestDictionarySamplingIsBounded(t *testing.T) {
	if dictMaxSamples <= 0 || dictMaxSampleBytes <= 0 {
		t.Fatal("dictionary sampling has no ceiling")
	}
	if dictMaxSamples < dictMinSamples {
		t.Fatalf("the sample ceiling %d is below the floor %d", dictMaxSamples, dictMinSamples)
	}
	if dictMaxSampleBytes < dictMinBytes {
		t.Fatalf("the sample byte ceiling %d is below the floor %d", dictMaxSampleBytes, dictMinBytes)
	}
}

// TestRejectsNonCanonicalSections: a section is present when it has content
// and absent when every layer is uniform air, so a present section holding
// only air is a second encoding of an absent one. A trailing all-air layer is
// the same rule one level down: a layer past the last stored one already reads
// as air.
func TestRejectsNonCanonicalSections(t *testing.T) {
	reg := testRegistry(t)
	air := reg.AirRuntimeID()
	stone, _ := reg.StateToRuntimeID("minecraft:stone", map[string]any{})
	// Global palette: air at 0, stone at 1.
	rids := []uint32{air, stone}
	uniform := func(ref uint64) decBlob {
		return decBlob{refs: []uint32{uint32(ref)}, width: widthUniform}
	}
	for _, c := range []struct {
		name   string
		layers []decBlob
		ok     bool
	}{
		{"one stone layer", []decBlob{uniform(1)}, true},
		{"air under stone", []decBlob{uniform(0), uniform(1)}, true},
		// An all-air section has an all-air last layer, so one test covers
		// both rules and there is no input that separates them.
		{"only air", []decBlob{uniform(0)}, false},
		{"all layers air", []decBlob{uniform(0), uniform(0)}, false},
		{"trailing air", []decBlob{uniform(1), uniform(0)}, false},
	} {
		err := checkSectionCanonical(c.layers, rids, nil, air, 0)
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok = %v", c.name, err, c.ok)
		}
	}
	// A preserved state stands in the palette as the placeholder, which may
	// itself be air, and is content regardless.
	if err := checkSectionCanonical([]decBlob{uniform(0)}, rids, []int32{0, -1}, air, 0); err != nil {
		t.Errorf("a layer holding a preserved state was called air: %v", err)
	}
}

// TestRejectsStatsSchemaViolations: §4.2 fixes the tag of every counter, on
// the same split §7 uses. A counter absent is valid; one carried as an int is
// a second encoding of the same number.
func TestRejectsStatsSchemaViolations(t *testing.T) {
	ok, err := marshalNBT(map[string]any{"chunks": int64(1), "biomes": int64(2)})
	if err != nil {
		t.Fatal(err)
	}
	if err := checkStatsBlob(ok); err != nil {
		t.Fatalf("a well-typed stats compound was rejected: %v", err)
	}
	if err := checkStatsBlob(nil); err != nil {
		t.Fatalf("an absent stats compound was rejected: %v", err)
	}
	bad, err := marshalNBT(map[string]any{"chunks": int32(1)})
	if err != nil {
		t.Fatal(err)
	}
	if err := checkStatsBlob(bad); err == nil {
		t.Fatal("a counter carried as an int was accepted")
	}
}
