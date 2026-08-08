package format

// Negative conformance vectors: files a conforming reader must reject, and the
// rule each one breaks.
//
// A second implementation needs to know what to refuse as much as what to
// accept — most of this project's recent correctness work has been about rules
// readers were failing to enforce, and a rule nobody checks is a rule that
// quietly stops holding. Each vector here is a named mutation of a positive
// vector, checked in as bytes, with its checkpoint hash repaired so it fails
// for the rule it is named after and not for a checksum. The one exception is
// the checksum vector itself, which deliberately leaves the hash stale.
//
// Every case asserts three things: the mutated bytes are exactly the ones
// checked in, this package's reader rejects them with an error naming the
// rule, and (unless the rule lies outside what the walker models) the
// independent walker of vectorwalk_test.go rejects them too. The last is what
// stops a negative vector from testing this reader's idiosyncrasies rather
// than the specification.

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/cespare/xxhash/v2"
	"github.com/df-mc/dragonfly/server/world"
)

// ---------------------------------------------------------------------------
// Byte surgery
// ---------------------------------------------------------------------------

// vecEdit replaces remove bytes at off with insert. Offsets are absolute file
// offsets, taken from the independent walker's vecField spans, so every mutation
// is located by the specification's own vecField names rather than by searching
// for a byte pattern.
type vecEdit struct {
	off    int
	remove int
	insert []byte
}

// vecSet replaces the bytes a named vecField occupies.
func vecSet(t *testing.T, l *vecLayout, path string, b ...byte) vecEdit {
	t.Helper()
	s := vecField(t, l, path)
	return vecEdit{off: s.off, remove: s.size, insert: b}
}

// vecSpanOf is the byte range a vecField occupies, for edits that need to copy or
// splice around it.
func vecSpanOf(t *testing.T, l *vecLayout, path string) (off, size int) {
	t.Helper()
	s := vecField(t, l, path)
	return s.off, s.size
}

// vecApply applies non-overlapping edits to a copy of base and repairs the
// footer's §2.4 checkpoint hash, so the file fails for the rule under test
// rather than for the integrity check that would otherwise mask it.
func vecApply(base []byte, edits ...vecEdit) []byte {
	return vecApplyKeepingHash(base, false, edits...)
}

func vecApplyKeepingHash(base []byte, keep bool, edits ...vecEdit) []byte {
	out := bytes.Clone(base)
	e := slices.Clone(edits)
	// Apply from the back, so an earlier edit's offset is still valid after a
	// later one has changed the length.
	sort.Slice(e, func(i, j int) bool { return e[i].off > e[j].off })
	for i, ed := range e {
		if i > 0 && ed.off+ed.remove > e[i-1].off {
			panic("vector edits overlap")
		}
		out = append(out[:ed.off:ed.off], append(slices.Clone(ed.insert), out[ed.off+ed.remove:]...)...)
	}
	if !keep {
		vecRehash(out)
	}
	return out
}

// vecRehash rewrites a solid file's checkpoint hash in place.
func vecRehash(file []byte) {
	h := xxhash.New()
	_, _ = h.Write(file[:headerSize])
	_, _ = h.Write(file[headerSize : len(file)-footerSize])
	_, _ = h.Write(file[len(file)-footerSize+8:])
	binary.LittleEndian.PutUint64(file[len(file)-footerSize:], h.Sum64())
}

// vecFlags returns an edit that replaces the header's flags word.
func vecFlags(f uint32) vecEdit {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], f)
	return vecEdit{off: 8, remove: 4, insert: b[:]}
}

// vecUvarint encodes a value as the format's minimal uvarint.
func vecUvarint(v uint64) []byte {
	var b [10]byte
	return b[:binary.PutUvarint(b[:], v)]
}

// vecStr encodes the format's length-prefixed string.
func vecStr(s string) []byte {
	return append(vecUvarint(uint64(len(s))), s...)
}

// vecBlobBytes encodes the format's length-prefixed blob.
func vecBlobBytes(b []byte) []byte {
	return append(vecUvarint(uint64(len(b))), b...)
}

// Hand-built NBT, used for the metadata vectors: the package's own encoder
// sorts keys and refuses the wrong tags, which is exactly what these vectors
// have to violate.
func vecNBTName(s string) []byte {
	b := []byte{byte(len(s)), byte(len(s) >> 8)}
	return append(b, s...)
}

func vecNBTEntry(tag byte, name string, payload []byte) []byte {
	return append(append([]byte{tag}, vecNBTName(name)...), payload...)
}

func vecNBTRoot(rootName string, entries ...[]byte) []byte {
	out := append([]byte{tagCompound}, vecNBTName(rootName)...)
	for _, e := range entries {
		out = append(out, e...)
	}
	return append(out, tagEnd)
}

func vecNBTI32(v int32) []byte {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], uint32(v))
	return b[:]
}

func vecNBTI64(v int64) []byte {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], uint64(v))
	return b[:]
}

// ---------------------------------------------------------------------------
// Indexed bases (§5)
// ---------------------------------------------------------------------------

// An indexed base is built rather than mutated out of a positive vector,
// because the walker of vectorwalk_test.go models a solid body and an indexed
// file has none: its content is a set of frames located by a directory. Two
// things about §5 shape everything below.
//
// A file records more than one checkpoint. CreateIndexed takes one before any
// column exists and Close takes another, and §5.6 says a reader that cannot
// adopt the newest falls back to an older one. A mutation applied only to the
// newest checkpoint therefore does not produce a file a reader rejects; it
// produces a file that opens at the previous generation, which is a different
// claim and one indexed_torn already makes. vecIndexedBase blanks the
// creation-time footer so exactly one checkpoint remains, and opens the result
// to prove the blanking alone leaves a valid file. Without that the cases
// below could "reject" a base that was already broken.
//
// The mutations are all in the directory's prologue or its frame references,
// never in the header. That is §5.5: the prologue is the authority over the
// header, so a rule about what an indexed file may contain has to be enforced
// on the prologue's copy for it to be enforced at all. Leaving the physical
// header intact is what makes these vectors say so — a reader that took its
// answer from the header would accept every one of them.

// vecIndexedBase writes the smallest indexed world that carries a directory
// prologue, a block palette segment and a biome palette segment, then reduces
// it to a single checkpoint. Frames are stored raw, so the directory can be
// edited in place.
func vecIndexedBase(t *testing.T, reg world.BlockRegistry) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "base.pile")
	w, err := CreateIndexed(path, reg, Options{Compression: CompressionNone})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Store(minimalWorld(t, reg).Columns[0]); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b = bytes.Clone(b)
	ftr := b[len(b)-footerSize:]
	prev := int(binary.LittleEndian.Uint64(ftr[32:40]))
	if prev < headerSize || prev+footerSize > len(b)-footerSize {
		t.Fatalf("the base has no creation-time checkpoint to blank (prevFooter %d)", prev)
	}
	clear(b[prev : prev+footerSize])
	binary.LittleEndian.PutUint64(ftr[32:40], 0)
	vecIndexedRehash(b)
	if err := vecOpenIndexed(t, b, reg); err != nil {
		t.Fatalf("the one-checkpoint base does not open: %v", err)
	}
	return b
}

// vecIndexedDir is the stored directory frame's range, read from the footer at
// EOF. The base stores frames raw, so this is also the directory's plaintext.
func vecIndexedDir(file []byte) (off, length int) {
	f := file[len(file)-footerSize:]
	return int(binary.LittleEndian.Uint64(f[8:16])), int(binary.LittleEndian.Uint64(f[16:24]))
}

// vecIndexedRehash repairs the §2.4 checkpoint hash after the directory frame
// has been edited. The preimage is the physical header, the stored directory
// bytes and the footer's control words; every mutation below leaves the
// directory's length alone, so the footer's own offset and length still hold.
func vecIndexedRehash(file []byte) {
	off, length := vecIndexedDir(file)
	f := file[len(file)-footerSize:]
	binary.LittleEndian.PutUint64(f[:8], checkpointHash(file[:headerSize], file[off:off+length], f[8:]))
}

// vecIndexedBlockSeg locates the first block palette segment: the file offset
// of the frame itself, and the offsets within the file of its reference's
// length and hash fields. It walks the directory exactly as loadDirectory
// does, so a case cannot patch the wrong field when the layout moves.
func vecIndexedBlockSeg(t *testing.T, file []byte) (segOff, lenAt, hashAt int) {
	t.Helper()
	dOff, dLen := vecIndexedDir(file)
	r := &reader{b: file[dOff : dOff+dLen]}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("walking the directory: %v", err)
		}
	}
	for _, read := range []func() error{
		func() error { _, err := r.u8(); return err },  // kind
		func() error { _, err := r.u8(); return err },  // mode
		func() error { _, err := r.u32(); return err }, // flags
		func() error { _, err := r.u32(); return err }, // blockVersion
	} {
		must(read())
	}
	skipRef := func() {
		t.Helper()
		_, err := r.uvarint()
		must(err)
		_, err = r.uvarint()
		must(err)
		_, err = r.u64()
		must(err)
	}
	skipRef() // meta
	skipRef() // dict
	n, err := r.uvarint()
	must(err)
	if n == 0 {
		t.Fatal("the base has no block palette segment to empty")
	}
	off, err := r.uvarint()
	must(err)
	lenAt = dOff + r.off
	_, err = r.uvarint()
	must(err)
	hashAt = dOff + r.off
	return int(off), lenAt, hashAt
}

// vecOpenIndexed drives a file through OpenIndexed, which is the only reader
// that will look at one.
func vecOpenIndexed(t *testing.T, file []byte, reg world.BlockRegistry) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "vector.pile")
	if err := os.WriteFile(path, file, 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := OpenIndexed(path, reg, true)
	if err == nil {
		_ = w.Close()
	}
	return err
}

// ---------------------------------------------------------------------------
// Cases
// ---------------------------------------------------------------------------

// negCase is one file a conforming reader must reject.
type negCase struct {
	name string
	// base names the positive vector the mutation starts from, or "" when the
	// case builds its own base.
	base string
	// rule is the specification clause the file breaks, quoted in vectors.md.
	rule string
	// mutate produces the invalid bytes. l is the independent walk of the base
	// vector, so mutations are located by vecField name.
	mutate func(t *testing.T, l *vecLayout, base []byte) []byte
	// wantErr is a substring the rejection must contain. Without it a vector
	// rejected for the wrong reason — a length that no longer adds up, say —
	// would look like a passing test.
	wantErr string
	// walkerBlind marks a rule the independent walker does not model, so only
	// this package's reader is required to reject the file. Every such case
	// names why in vectors.md.
	walkerBlind bool
	// structure selects the reader the case is driven through.
	structure bool
	// indexed marks a §5 case: its base is vecIndexedBase rather than a
	// positive vector, it is driven through OpenIndexed, and it is never
	// offered to the walker, which models a solid body. mutate receives a nil
	// layout, so an indexed case locates its edits with the helpers above.
	indexed bool
}

func negCases() []negCase {
	return []negCase{
		// -- §2.1 header --------------------------------------------------
		{
			name: "header_magic", base: "world_minimal", rule: "§2.1: the first four bytes are PILE",
			wantErr: "magic",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				return vecApply(b, vecEdit{off: 0, remove: 4, insert: []byte("PILF")})
			},
		},
		{
			name: "header_version", base: "world_minimal", rule: "§2.1: readers reject unknown versions; there is no forward-compatibility lane",
			wantErr: "unsupported format version",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				return vecApply(b, vecEdit{off: 4, remove: 2, insert: []byte{3, 0}})
			},
		},
		{
			name: "header_kind", base: "world_minimal", rule: "§2.1: any kind the table does not list is rejected",
			wantErr: "kind",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				return vecApply(b, vecEdit{off: 6, remove: 1, insert: []byte{2}})
			},
		},
		{
			name: "header_mode", base: "world_minimal", rule: "§2.1: any mode the table does not list is rejected",
			wantErr: "mode",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				return vecApply(b, vecEdit{off: 7, remove: 1, insert: []byte{2}})
			},
		},
		{
			name: "header_structure_indexed", base: "world_minimal", rule: "§2.1: a structure is always solid, so kind 1 with mode 1 is invalid",
			wantErr: "mode",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				return vecApply(b, vecEdit{off: 6, remove: 2, insert: []byte{1, 1}})
			},
		},
		{
			name: "header_block_version_zero", base: "world_minimal", rule: "§2.1: blockVersion must be non-zero; zero means \"the palette's own version\"",
			wantErr: "blockVersion",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				return vecApply(b, vecEdit{off: 12, remove: 4, insert: []byte{0, 0, 0, 0}})
			},
		},
		// -- §2.3 flags ---------------------------------------------------
		{
			name: "flag_reserved_bit2", base: "world_minimal", rule: "§2.3: bit 2 is reserved and must be zero",
			wantErr: "unknown required feature flags",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				return vecApply(b, vecFlags(l.flags|1<<2))
			},
		},
		{
			name: "flag_reserved_bit8", base: "world_minimal", rule: "§2.3: bits 8-15 are reserved and must be zero",
			wantErr: "unknown required feature flags",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				return vecApply(b, vecFlags(l.flags|1<<8))
			},
		},
		{
			name: "flag_dimension_reserved", base: "world_minimal", rule: "§2.3: bits 5-7 are reserved and must be zero",
			wantErr: "unknown required feature flags",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				return vecApply(b, vecFlags(l.flags|vecReservedDimBits))
			},
		},
		{
			name: "flag_default_biome_ref_without_flag", base: "world_minimal", rule: "§2.3: bits 16-31 must be zero when the DefaultBiome flag is clear",
			wantErr: "default biome reference",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				return vecApply(b, vecFlags((l.flags&^vecFlagDefaultBiome)|1<<vecDefaultBiomeShift))
			},
		},
		// -- §2.2 footer --------------------------------------------------
		{
			name: "footer_magic", base: "world_minimal", rule: "§2.2: the last four bytes are ELIP",
			wantErr: "footer magic",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				return vecApply(b, vecEdit{off: len(b) - 4, remove: 4, insert: []byte("ELIQ")})
			},
		},
		{
			name: "footer_generation_nonzero", base: "world_minimal", rule: "§2.2: a solid file's directory, generation and previous-footer words must be zero",
			wantErr: "generation",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				return vecApply(b, vecEdit{off: len(b) - footerSize + 24, remove: 8, insert: []byte{1, 0, 0, 0, 0, 0, 0, 0}})
			},
		},
		{
			name: "checkpoint_hash", base: "world_minimal", rule: "§2.4: the footer hash covers the header, the stored payload and the footer's control words",
			wantErr: "checksum",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				// One flipped payload byte, hash left stale: this is the only
				// vector here that is meant to fail the integrity check.
				off, _ := vecSpanOf(t, l, "chunkN")
				return vecApplyKeepingHash(b, true, vecEdit{off: off, remove: 1, insert: []byte{b[off] ^ 0x01}})
			},
		},
		// -- §1 primitives -------------------------------------------------
		{
			name: "uvarint_overlong", base: "world_minimal", rule: "§1: uvarints must be minimal; decoders reject overlong encodings",
			wantErr: "non-minimal uvarint",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				off, size := vecSpanOf(t, l, "chunkN")
				if size != 1 {
					t.Fatalf("chunkN is %d bytes; the vector assumes one", size)
				}
				return vecApply(b, vecEdit{off: off, remove: 1, insert: []byte{b[off] | 0x80, 0x00}})
			},
		},
		{
			name: "bitset_padding_bits", base: "world_minimal", rule: "§1: a bitset's padding bits above n must be zero",
			wantErr: "padding",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				off, size := vecSpanOf(t, l, "record[0].blockPresence")
				if size != 1 {
					t.Fatalf("blockPresence is %d bytes; the vector assumes one", size)
				}
				return vecApply(b, vecEdit{off: off, remove: 1, insert: []byte{b[off] | 0x02}})
			},
		},
		{
			name: "string_not_utf8", base: "world_minimal", rule: "§1: strings must be valid UTF-8, since palettes are ordered bytewise",
			wantErr: "UTF-8",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				off, _ := vecSpanOf(t, l, "blockPalette.entry[0].name")
				return vecApply(b, vecEdit{off: off, remove: 1, insert: []byte{0xFF}})
			},
		},
		// -- §3.1 block palette ---------------------------------------------
		{
			name: "palette_duplicate_entry", base: "world_minimal", rule: "§3.1: two entries that encode identically are the same state and must be merged",
			wantErr: "duplicate block palette entry",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				cOff, cSize := vecSpanOf(t, l, "blockPalette.count")
				nOff, _ := vecSpanOf(t, l, "blockPalette.entry[0].name.len")
				pOff, pSize := vecSpanOf(t, l, "blockPalette.entry[0].propN")
				entry := bytes.Clone(b[nOff : pOff+pSize])
				return vecApply(b,
					vecEdit{off: cOff, remove: cSize, insert: vecUvarint(2)},
					vecEdit{off: pOff + pSize, insert: entry},
				)
			},
		},
		{
			name: "palette_property_order", base: "world_preserved", rule: "§3.1: property keys are unique and ascend bytewise",
			wantErr: "properties must ascend",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				i := vecEntryWithProps(t, l, 2)
				p0k, _ := vecSpanOf(t, l, fmt.Sprintf("blockPalette.entry[%d].prop[0].key.len", i))
				p1k, _ := vecSpanOf(t, l, fmt.Sprintf("blockPalette.entry[%d].prop[1].key.len", i))
				p1v, p1vs := vecSpanOf(t, l, fmt.Sprintf("blockPalette.entry[%d].prop[1].value", i))
				first := bytes.Clone(b[p0k:p1k])
				second := bytes.Clone(b[p1k : p1v+p1vs])
				return vecApply(b, vecEdit{off: p0k, remove: (p1v + p1vs) - p0k, insert: append(second, first...)})
			},
		},
		{
			name: "override_zero_delta", base: "world_preserved", rule: "§3.1: override indices strictly ascend, so every delta past the first is non-zero",
			wantErr: "strictly ascending",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				return vecApply(b, vecSet(t, l, "blockPalette.override[1].indexDelta", 0x00))
			},
		},
		{
			name: "override_index_chain_wraps", base: "world_preserved",
			rule:    "§3.1: the override indices strictly ascend, and a delta whose running sum wraps descends onto an index a bounds test cannot see is wrong",
			wantErr: "wraps",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				// The base vector's first override is at index 0, which nothing
				// can step back from, so move it to the last entry first. Both
				// indices stay legal; what is illegal is the descent between
				// them. The second delta is the modular representative of
				// -(n-1), so the running sum wraps and lands back on index 0.
				n := uint64(len(l.blockPalette))
				if n < 2 {
					t.Fatalf("the base vector's block palette holds %d entries; a wrap needs two indices", n)
				}
				return vecApply(b,
					vecSet(t, l, "blockPalette.override[0].indexDelta", vecUvarint(n-1)...),
					vecSet(t, l, "blockPalette.override[1].indexDelta", vecUvarint(-(n-1))...),
				)
			},
		},
		{
			name: "override_zero_version", base: "world_preserved", rule: "§3.1: an override's version must be non-zero",
			wantErr: "version",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				return vecApply(b, vecSet(t, l, "blockPalette.override[0].version", 0, 0, 0, 0))
			},
		},
		{
			name: "override_same_version", base: "world_preserved", rule: "§3.1: an override that repeats the palette's own version is a second encoding of no override",
			wantErr: "version",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				var v [4]byte
				binary.LittleEndian.PutUint32(v[:], uint32(l.blockVersion))
				return vecApply(b, vecSet(t, l, "blockPalette.override[0].version", v[:]...))
			},
		},
		// -- §3.2 biome palette ---------------------------------------------
		{
			name: "biome_bare_name", base: "world_minimal", rule: "§3.2: biome names are fully qualified; a bare name is invalid",
			wantErr: "namespaced",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				lOff, lSize := vecSpanOf(t, l, "biomePalette.name[0].len")
				nOff, nSize := vecSpanOf(t, l, "biomePalette.name[0]")
				name := l.biomePalette[0]
				bare := name[strings.IndexByte(name, ':')+1:]
				return vecApply(b,
					vecEdit{off: lOff, remove: lSize + nSize, insert: vecStr(bare)},
					vecEdit{off: nOff + nSize},
				)
			},
		},
		{
			name: "biome_duplicate_name", base: "world_default_biome", rule: "§3.2: one entry per biome; a repeated name is rejected",
			wantErr: "duplicate biome palette entry",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				l0, l0s := vecSpanOf(t, l, "biomePalette.name[0].len")
				n0, n0s := vecSpanOf(t, l, "biomePalette.name[0]")
				l1, _ := vecSpanOf(t, l, "biomePalette.name[1].len")
				n1, n1s := vecSpanOf(t, l, "biomePalette.name[1]")
				first := bytes.Clone(b[l0 : n0+n0s])
				_ = l0s
				return vecApply(b, vecEdit{off: l1, remove: (n1 + n1s) - l1, insert: first})
			},
		},
		// -- §3.3 section blob ----------------------------------------------
		{
			name: "blob_uniform_width_nonzero", base: "world_minimal", rule: "§3.3: width is 0 if and only if paletteN is 1",
			wantErr: "uniform width",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				off, size := vecSpanOf(t, l, "blobTable.blob[0].width")
				return vecApply(b, vecEdit{off: off, remove: size, insert: append([]byte{1}, make([]byte, 4096)...)})
			},
		},
		{
			name: "blob_width_not_minimal", base: "world_waterlogged", rule: "§3.3: the narrowest sufficient index width is the only valid one",
			wantErr: "non-minimal index width",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				i := vecBlobWithWidth(t, l, 1)
				wOff, wSize := vecSpanOf(t, l, fmt.Sprintf("blobTable.blob[%d].width", i))
				iOff, iSize := vecSpanOf(t, l, fmt.Sprintf("blobTable.blob[%d].indices", i))
				wide := make([]byte, 2*iSize)
				for j := range iSize {
					wide[2*j] = b[iOff+j]
				}
				return vecApply(b,
					vecEdit{off: wOff, remove: wSize, insert: []byte{2}},
					vecEdit{off: iOff, remove: iSize, insert: wide},
				)
			},
		},
		{
			name: "blob_refs_not_ascending", base: "world_waterlogged", rule: "§3.3: local palette references ascend strictly",
			wantErr: "ascending",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				i := vecBlobWithWidth(t, l, 1)
				r0, s0 := vecSpanOf(t, l, fmt.Sprintf("blobTable.blob[%d].ref[0]", i))
				r1, s1 := vecSpanOf(t, l, fmt.Sprintf("blobTable.blob[%d].ref[1]", i))
				if s0 != 1 || s1 != 1 {
					t.Fatalf("the vector assumes single-byte references, got %d and %d", s0, s1)
				}
				return vecApply(b,
					vecEdit{off: r0, remove: 1, insert: []byte{b[r1]}},
					vecEdit{off: r1, remove: 1, insert: []byte{b[r0]}},
				)
			},
		},
		{
			name: "blob_refs_duplicate", base: "world_waterlogged", rule: "§3.3: a section cannot carry a duplicate local palette entry",
			wantErr: "ascending",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				i := vecBlobWithWidth(t, l, 1)
				r0, _ := vecSpanOf(t, l, fmt.Sprintf("blobTable.blob[%d].ref[0]", i))
				r1, s1 := vecSpanOf(t, l, fmt.Sprintf("blobTable.blob[%d].ref[1]", i))
				return vecApply(b, vecEdit{off: r1, remove: s1, insert: []byte{b[r0]}})
			},
		},
		{
			name: "blob_unused_palette_entry", base: "world_waterlogged", rule: "§3.3: the local palette holds only entries the indices actually use",
			wantErr: "never used",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				i := vecBlobWithWidth(t, l, 1)
				off, size := vecSpanOf(t, l, fmt.Sprintf("blobTable.blob[%d].indices", i))
				return vecApply(b, vecEdit{off: off, remove: size, insert: make([]byte, size)})
			},
		},
		// -- §3.4 blob table ------------------------------------------------
		{
			name: "blob_table_duplicate", base: "world_minimal", rule: "§3.4: identical bytes share one table entry",
			wantErr: "repeats blob",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				cOff, cSize := vecSpanOf(t, l, "blobTable.count")
				pOff, _ := vecSpanOf(t, l, "blobTable.blob[0].paletteN")
				wOff, wSize := vecSpanOf(t, l, "blobTable.blob[0].width")
				blob := bytes.Clone(b[pOff : wOff+wSize])
				return vecApply(b,
					vecEdit{off: cOff, remove: cSize, insert: vecUvarint(2)},
					vecEdit{off: wOff + wSize, insert: blob},
				)
			},
		},
		{
			name: "blob_table_unreferenced", base: "world_waterlogged", rule: "§3.4: a table entry no record references is content nothing reads",
			wantErr: "never referenced",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				cOff, cSize := vecSpanOf(t, l, "blobTable.count")
				last := len(l.blobs) - 1
				end := vecBlobEnd(t, l, last)
				// A uniform blob naming the highest global palette entry: legal
				// on its own, and nothing points at it.
				extra := append(vecUvarint(1), vecUvarint(uint64(len(l.blockPalette)-1))...)
				extra = append(extra, 0)
				return vecApply(b,
					vecEdit{off: cOff, remove: cSize, insert: vecUvarint(uint64(len(l.blobs) + 1))},
					vecEdit{off: end, insert: extra},
				)
			},
		},
		{
			name: "blob_id_first_use_order", base: "world_waterlogged", rule: "§3.4: blob ids are assigned in first-use order",
			wantErr: "first-use order",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				r0, s0 := vecSpanOf(t, l, "record[0].section[0].blobRef[0]")
				r1, s1 := vecSpanOf(t, l, "record[0].section[0].blobRef[1]")
				if s0 != 1 || s1 != 1 {
					t.Fatalf("the vector assumes single-byte references")
				}
				return vecApply(b,
					vecEdit{off: r0, remove: 1, insert: []byte{b[r1]}},
					vecEdit{off: r1, remove: 1, insert: []byte{b[r0]}},
				)
			},
		},
		// -- §4 records -----------------------------------------------------
		{
			name: "record_keys_not_ascending", base: "world_dedup_morton", rule: "§4: record Morton keys strictly ascend",
			wantErr: "out of order",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				// Put record 0 at (1,0) and record 1 at (0,0): both legal
				// positions, in the wrong order.
				d0, s0 := vecSpanOf(t, l, "record[0].dx")
				d1, s1 := vecSpanOf(t, l, "record[1].dx")
				if s0 != 1 || s1 != 1 {
					t.Fatalf("the vector assumes single-byte deltas")
				}
				return vecApply(b,
					vecEdit{off: d0, remove: 1, insert: []byte{0x02}},
					vecEdit{off: d1, remove: 1, insert: []byte{0x01}},
				)
			},
		},
		{
			name: "record_duplicate_position", base: "world_dedup_morton", rule: "§4: chunk positions are unique",
			wantErr: "duplicated",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				d1, s1 := vecSpanOf(t, l, "record[1].dx")
				return vecApply(b, vecEdit{off: d1, remove: s1, insert: []byte{0x00}})
			},
		},
		{
			name: "record_trailing_bytes", base: "world_minimal", rule: "§4: nothing may follow the last record",
			wantErr: "trailing bytes",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				return vecApply(b, vecEdit{off: len(b) - footerSize, insert: []byte{0x00}})
			},
		},
		{
			name: "section_present_with_no_layers", base: "world_minimal", rule: "§4.3: a present section declares at least one layer",
			wantErr: "declares no layers",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				nOff, nSize := vecSpanOf(t, l, "record[0].section[0].layerN")
				rOff, rSize := vecSpanOf(t, l, "record[0].section[0].blobRef[0]")
				return vecApply(b,
					vecEdit{off: nOff, remove: nSize, insert: vecUvarint(0)},
					vecEdit{off: rOff, remove: rSize},
				)
			},
		},
		{
			name: "section_trailing_air_layer", base: "world_waterlogged", rule: "§4.3: trailing all-air layers are dropped",
			wantErr: "all-air layer",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				// Append a third layer pointing at the uniform-air blob the
				// section's layer 0 already uses.
				nOff, nSize := vecSpanOf(t, l, "record[0].section[0].layerN")
				air := vecField(t, l, "record[0].section[0].blobRef[0]")
				last, lastSize := vecSpanOf(t, l, "record[0].section[0].blobRef[1]")
				return vecApply(b,
					vecEdit{off: nOff, remove: nSize, insert: vecUvarint(3)},
					vecEdit{off: last + lastSize, insert: vecUvarint(air.val)},
				)
			},
			// The walker reports the same file as a trailing all-air layer only
			// by resolving the blob to a palette entry named air, which it does
			// not do: it models the wire, not the block registry.
			walkerBlind: true,
		},
		{
			name: "section_all_air", base: "world_waterlogged", rule: "§4.3: a section every one of whose layers is uniform air must be absent",
			wantErr: "all-air layer",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				// Drop layer 1 and the blob it referenced, leaving a present
				// section whose only layer is the uniform-air one.
				nOff, nSize := vecSpanOf(t, l, "record[0].section[0].layerN")
				r1, r1s := vecSpanOf(t, l, "record[0].section[0].blobRef[1]")
				cOff, cSize := vecSpanOf(t, l, "blobTable.count")
				bOff, _ := vecSpanOf(t, l, "blobTable.blob[1].paletteN")
				bEnd := vecBlobEnd(t, l, 1)
				return vecApply(b,
					vecEdit{off: cOff, remove: cSize, insert: vecUvarint(1)},
					vecEdit{off: bOff, remove: bEnd - bOff},
					vecEdit{off: nOff, remove: nSize, insert: vecUvarint(1)},
					vecEdit{off: r1, remove: r1s},
				)
			},
			// As with the trailing-air vector, telling air from any other block
			// means resolving a palette entry, which the walker does not do.
			walkerBlind: true,
		},
		{
			name: "layer_count_over_max", base: "world_minimal", rule: "§4.3 and §8: layerN is at most 255, because the 256th layer is unaddressable",
			wantErr: "layer count 256 exceeds limit 255",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				off, size := vecSpanOf(t, l, "record[0].section[0].layerN")
				return vecApply(b, vecEdit{off: off, remove: size, insert: vecUvarint(256)})
			},
		},
		{
			name: "blob_index_out_of_range", base: "world_waterlogged", rule: "§3.3: an index at or past paletteN selects nothing",
			wantErr: "out of palette range",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				i := vecBlobWithWidth(t, l, 1)
				off, size := vecSpanOf(t, l, fmt.Sprintf("blobTable.blob[%d].indices", i))
				n := len(l.blobs[i].refs)
				// Rewrite the whole array so every entry the canonical form
				// requires is still used and only the last one is out of range.
				idx := bytes.Clone(b[off : off+size])
				idx[len(idx)-1] = byte(n)
				return vecApply(b, vecEdit{off: off, remove: size, insert: idx})
			},
		},
		// -- §4.6 light -----------------------------------------------------
		{
			name: "light_flags_zero", base: "world_light", rule: "§4.6: a present light entry carries at least one array",
			wantErr: "carries no arrays",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				s, arrays := vecFirstLight(t, l)
				edits := []vecEdit{vecSet(t, l, fmt.Sprintf("record[0].light[%d].flags", s), 0)}
				for _, a := range arrays {
					off, size := vecSpanOf(t, l, a)
					edits = append(edits, vecEdit{off: off, remove: size})
				}
				return vecApply(b, edits...)
			},
		},
		{
			name: "light_flags_reserved_bits", base: "world_light", rule: "§4.6: light flag bits 2-7 must be zero",
			wantErr: "reserved bits",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				s, _ := vecFirstLight(t, l)
				f := vecField(t, l, fmt.Sprintf("record[0].light[%d].flags", s))
				return vecApply(b, vecEdit{off: f.off, remove: 1, insert: []byte{byte(f.val) | 0x04}})
			},
		},
		// -- §4.8 collection order and uniqueness ---------------------------
		{
			name: "block_entity_duplicate_position", base: "world_collections", rule: "§4.8: at most one block entity may occupy a position",
			wantErr: "repeat a position",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				p0 := vecField(t, l, "record[0].be[0].packedXZ")
				y0 := vecField(t, l, "record[0].be[0].y")
				p1 := vecField(t, l, "record[0].be[1].packedXZ")
				y1 := vecField(t, l, "record[0].be[1].y")
				if y0.size != y1.size {
					t.Fatalf("the vector assumes equal-width y varints")
				}
				return vecApply(b,
					vecEdit{off: p1.off, remove: 1, insert: []byte{byte(p0.val)}},
					vecEdit{off: y1.off, remove: y1.size, insert: bytes.Clone(b[y0.off : y0.off+y0.size])},
				)
			},
		},
		{
			name: "block_entity_out_of_order", base: "world_collections", rule: "§4.8: block entities are ordered by (y, z, x)",
			wantErr: "order",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				y0 := vecField(t, l, "record[0].be[0].y")
				y1 := vecField(t, l, "record[0].be[1].y")
				if y0.size != y1.size {
					t.Fatalf("the vector assumes equal-width y varints")
				}
				return vecApply(b,
					vecEdit{off: y0.off, remove: y0.size, insert: bytes.Clone(b[y1.off : y1.off+y1.size])},
					vecEdit{off: y1.off, remove: y1.size, insert: bytes.Clone(b[y0.off : y0.off+y0.size])},
				)
			},
		},
		{
			name: "block_entity_outside_span", base: "world_collections", rule: "§8: a record's block-entity positions lie inside its declared span",
			wantErr: "outside",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				// The record spans y -64..-49; -48 is one block above it and
				// still encodes in one byte, so nothing else in the file moves.
				y1 := vecField(t, l, "record[0].be[1].y")
				enc := vecSvarint(-48)
				if len(enc) != y1.size {
					t.Fatalf("the replacement y is %d bytes, the original %d", len(enc), y1.size)
				}
				return vecApply(b, vecEdit{off: y1.off, remove: y1.size, insert: enc})
			},
		},
		{
			name: "scheduled_update_out_of_order", base: "world_collections", rule: "§4.8: scheduled updates are ordered by (y, z, x), then tick, then block reference",
			wantErr: "order",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				y0 := vecField(t, l, "record[0].st[0].y")
				enc := vecSvarint(-50)
				if len(enc) != y0.size {
					t.Fatalf("the replacement y is %d bytes, the original %d", len(enc), y0.size)
				}
				return vecApply(b, vecEdit{off: y0.off, remove: y0.size, insert: enc})
			},
		},
		// -- §1 and §7 metadata ---------------------------------------------
		{
			name: "nbt_keys_not_ascending", base: "world_collections", rule: "§1: NBT compound keys are unique and strictly ascending",
			wantErr: "keys must ascend",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				return vecReplaceMeta(t, l, b, "meta.settings", vecNBTRoot("",
					vecNBTEntry(tagInt, "b", vecNBTI32(1)),
					vecNBTEntry(tagInt, "a", vecNBTI32(2)),
				))
			},
			walkerBlind: false,
		},
		{
			name: "nbt_duplicate_keys", base: "world_collections", rule: "§1: NBT compound keys are unique",
			wantErr: "duplicate compound key",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				return vecReplaceMeta(t, l, b, "meta.settings", vecNBTRoot("",
					vecNBTEntry(tagInt, "a", vecNBTI32(1)),
					vecNBTEntry(tagInt, "a", vecNBTI32(2)),
				))
			},
		},
		{
			name: "nbt_named_root", base: "world_collections", rule: "§1: the root compound is unnamed",
			wantErr: "root compound must be unnamed",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				return vecReplaceMeta(t, l, b, "meta.settings", vecNBTRoot("root",
					vecNBTEntry(tagInt, "a", vecNBTI32(1)),
				))
			},
		},
		{
			name: "settings_wrong_tag", base: "world_collections", rule: "§7.1: each named settings vecField carries a fixed tag; time is a long",
			wantErr: "want int64",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				return vecReplaceMeta(t, l, b, "meta.settings", vecNBTRoot("",
					vecNBTEntry(tagInt, "time", vecNBTI32(1)),
				))
			},
			// The walker checks NBT structure, not the §7 schemas: a wrong tag
			// is well-formed NBT and only the schema says otherwise.
			walkerBlind: true,
		},
		// -- §4.2 stats -----------------------------------------------------
		{
			name: "stats_wrong_tag", base: "world_stats", rule: "§4.2: a stats counter that is present carries a long",
			wantErr: "want int64",
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				return vecReplaceMeta(t, l, b, "meta.stats", vecNBTRoot("",
					vecNBTEntry(tagLong, "biomes", vecNBTI64(1)),
					vecNBTEntry(tagInt, "chunks", vecNBTI32(1)),
				))
			},
			walkerBlind: true,
		},
		// -- §6 structures ---------------------------------------------------
		{
			name: "structure_flag_set", base: "structure_edge_padding", rule: "§6: a structure sets no flag other than Uncompressed",
			wantErr: "not valid for a structure", structure: true,
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				return vecApply(b, vecFlags(l.flags|vecFlagStats))
			},
		},
		{
			name: "structure_biome_palette_nonempty", base: "structure_edge_padding", rule: "§6: a structure's biome palette has zero entries",
			wantErr: "biome palette entries", structure: true,
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				off, size := vecSpanOf(t, l, "biomePalette.count")
				return vecApply(b, vecEdit{off: off, remove: size,
					insert: append(vecUvarint(1), vecStr("minecraft:plains")...)})
			},
		},
		{
			name: "structure_block_entity_outside_box", base: "structure_full", rule: "§6: a structure's block entities lie inside the declared box",
			wantErr: "outside structure size", structure: true,
			mutate: func(t *testing.T, l *vecLayout, b []byte) []byte {
				s := vecField(t, l, "structure.be[1].pos[0]")
				enc := vecUvarint(l.size[0])
				if len(enc) != s.size {
					t.Fatalf("the replacement coordinate is %d bytes, the original %d", len(enc), s.size)
				}
				return vecApply(b, vecEdit{off: s.off, remove: s.size, insert: enc})
			},
		},

		// -- §5 indexed mode -------------------------------------------------
		{
			name: "indexed_prologue_stats_flag", rule: "§5.4: an indexed file leaves Stats and DefaultBiome clear",
			wantErr: "not valid for an indexed file", indexed: true, walkerBlind: true,
			mutate: func(t *testing.T, _ *vecLayout, b []byte) []byte {
				// Indexed mode has no stats field to hold what the flag
				// promises, so the flag is a claim the layout cannot keep.
				b = bytes.Clone(b)
				off, _ := vecIndexedDir(b)
				f := b[off+2 : off+6]
				binary.LittleEndian.PutUint32(f, binary.LittleEndian.Uint32(f)|FlagStats)
				vecIndexedRehash(b)
				return b
			},
		},
		{
			name: "indexed_prologue_block_version_zero", rule: "§5.5: the prologue is the authority, so §2.1's non-zero blockVersion binds there",
			wantErr: "directory blockVersion is zero", indexed: true, walkerBlind: true,
			mutate: func(t *testing.T, _ *vecLayout, b []byte) []byte {
				// The physical header keeps a valid version. A reader that
				// took its answer from the header would accept this file,
				// which is exactly what §5.5 forbids.
				b = bytes.Clone(b)
				off, _ := vecIndexedDir(b)
				binary.LittleEndian.PutUint32(b[off+6:off+10], 0)
				vecIndexedRehash(b)
				return b
			},
		},
		{
			name: "indexed_empty_palette_segment", rule: "§5.3: a palette segment with no entries is never written",
			wantErr: "has no entries", indexed: true, walkerBlind: true,
			mutate: func(t *testing.T, _ *vecLayout, b []byte) []byte {
				// The segment frame is shrunk in place to a §3.1 palette of
				// zero entries and zero overrides -- six bytes after its i32
				// version. Its reference's length and hash move with it; the
				// bytes past the new end stay in the file and are simply no
				// longer part of any frame, which is what garbage from an
				// overwrite looks like anyway. Shrinking rather than blanking
				// keeps the frame ending where its content ends, so §5.1
				// cannot refuse the file before §5.3 does.
				b = bytes.Clone(b)
				segOff, lenAt, hashAt := vecIndexedBlockSeg(t, b)
				const emptySeg = 6
				b[segOff+4] = 0 // palette entries
				b[segOff+5] = 0 // version overrides
				if b[lenAt] < emptySeg || b[lenAt] >= 0x80 {
					t.Fatalf("the segment reference's length is %#x, which this edit cannot rewrite in place", b[lenAt])
				}
				b[lenAt] = emptySeg
				binary.LittleEndian.PutUint64(b[hashAt:hashAt+8], xxhash.Sum64(b[segOff:segOff+emptySeg]))
				vecIndexedRehash(b)
				return b
			},
		},
	}
}

// vecSvarint encodes a value as the format's zigzag varint.
func vecSvarint(v int64) []byte {
	var b [10]byte
	return b[:binary.PutVarint(b[:], v)]
}

// vecEntryWithProps returns the index of the first palette entry carrying at
// least n properties.
func vecEntryWithProps(t *testing.T, l *vecLayout, n int) int {
	t.Helper()
	for i, e := range l.blockPalette {
		if len(e.props) >= n {
			return i
		}
	}
	t.Fatalf("no palette entry carries %d properties", n)
	return 0
}

// vecBlobWithWidth returns the index of the first blob stored at width w.
func vecBlobWithWidth(t *testing.T, l *vecLayout, w uint8) int {
	t.Helper()
	for i, b := range l.blobs {
		if b.width == w {
			return i
		}
	}
	t.Fatalf("no blob is stored at width %d", w)
	return 0
}

// vecBlobEnd returns the offset one past the last byte of a blob.
func vecBlobEnd(t *testing.T, l *vecLayout, i int) int {
	t.Helper()
	if l.blobs[i].idx != nil {
		off, size := vecSpanOf(t, l, fmt.Sprintf("blobTable.blob[%d].indices", i))
		return off + size
	}
	off, size := vecSpanOf(t, l, fmt.Sprintf("blobTable.blob[%d].width", i))
	return off + size
}

// vecFirstLight returns the first section carrying light and the paths of the
// arrays that follow its flags byte.
func vecFirstLight(t *testing.T, l *vecLayout) (int, []string) {
	t.Helper()
	secs := make([]int, 0, len(l.records[0].light))
	for s := range l.records[0].light {
		secs = append(secs, s)
	}
	sort.Ints(secs)
	if len(secs) == 0 {
		t.Fatal("the base vector carries no light")
	}
	s := secs[0]
	var arrays []string
	f := l.records[0].light[s]
	if f&1 != 0 {
		arrays = append(arrays, fmt.Sprintf("record[0].light[%d].blockLight", s))
	}
	if f&2 != 0 {
		arrays = append(arrays, fmt.Sprintf("record[0].light[%d].skyLight", s))
	}
	return s, arrays
}

// vecReplaceMeta swaps one metadata blob for hand-built bytes, length prefix
// and all.
func vecReplaceMeta(t *testing.T, l *vecLayout, b []byte, path string, blob []byte) []byte {
	t.Helper()
	lOff, lSize := vecSpanOf(t, l, path+".len")
	_, dSize := vecSpanOf(t, l, path)
	return vecApply(b, vecEdit{off: lOff, remove: lSize + dSize, insert: vecBlobBytes(blob)})
}

// ---------------------------------------------------------------------------
// The test
// ---------------------------------------------------------------------------

// TestConformanceVectorsNegative generates (under -update) and verifies the
// files a conforming reader must reject.
func TestConformanceVectorsNegative(t *testing.T) {
	requireUnfrozen(t)
	reg := testRegistry(t)
	prevVersion, prev := readVectorManifest(t, negManifest)
	next := map[string]vectorRecord{}
	t.Cleanup(func() {
		vectorManifestGuard(t, negManifest, negManifestHeader, prevVersion, prev, next)
	})
	for _, c := range negCases() {
		t.Run(c.name, func(t *testing.T) {
			var base []byte
			var l *vecLayout
			if c.indexed {
				base = vecIndexedBase(t, reg)
			} else {
				base = vectorBase(t, reg, c.base)
				var err error
				if l, err = vecWalk(base); err != nil {
					t.Fatalf("the base vector %s does not walk: %v", c.base, err)
				}
			}
			got := c.mutate(t, l, base)
			next[c.name] = vectorRecord{fileHash: xxhash.Sum64(got)}
			path := filepath.Join(vectorDir, "neg_"+c.name+".pile")
			if *update {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (regenerate with -update)", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("the mutation no longer produces %s.\ngot  %d bytes: %s\nwant %d bytes: %s",
					path, len(got), hex.EncodeToString(got[:min(64, len(got))]),
					len(want), hex.EncodeToString(want[:min(64, len(want))]))
			}

			err = vecReject(t, want, reg, c)
			if err == nil {
				t.Fatalf("%s was accepted; %s", path, c.rule)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("rejected for the wrong reason: %v\nwant an error mentioning %q (%s)", err, c.wantErr, c.rule)
			}
			if !c.walkerBlind {
				if _, werr := vecWalk(want); werr == nil {
					t.Errorf("the independent walker accepted %s, which %s forbids", path, c.rule)
				}
			}
		})
	}
}

// vectorBase rebuilds the positive vector a negative case mutates. It is built
// rather than read: test files run in file order, so reading would make the
// negative suite depend on the positive suite having already regenerated.
func vectorBase(t *testing.T, reg world.BlockRegistry, name string) []byte {
	t.Helper()
	for _, v := range vectorCases() {
		if v.name == name {
			return v.build(t, reg)
		}
	}
	t.Fatalf("no positive vector named %q", name)
	return nil
}

// vecReject drives a negative vector through the reader its kind belongs to.
func vecReject(t *testing.T, file []byte, reg world.BlockRegistry, c negCase) error {
	t.Helper()
	switch {
	case c.indexed:
		return vecOpenIndexed(t, file, reg)
	case c.structure:
		_, err := ReadStructure(file, reg)
		return err
	}
	_, err := ReadWorld(file, reg)
	return err
}
