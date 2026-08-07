package format

// A second reader, written from format.md rather than from the encoder.
//
// The conformance vectors would prove very little if they were only decoded by
// the decoder that wrote them: a change made in both halves at once would move
// the bytes and every test would stay green. This file therefore parses a
// vector the way an independent implementer would, from the layout tables in
// the specification, and records a named span for every byte it consumes. The
// vector test then checks three things a round trip cannot:
//
//   - that the spans tile the file exactly, so no vecField was added, dropped or
//     reordered without a test noticing;
//   - that the spec's own validity rules (varint minimality, bitset padding,
//     blob canonicality, Morton ordering, NBT key order) hold on the bytes,
//     independent of what decode.go checks;
//   - that specific fields sit at specific offsets with specific widths, which
//     is what the vector documentation quotes.
//
// It deliberately shares nothing with the production reader but xxhash, and
// restates the specification's constants as literals so a change to
// format.go's constants shows up as a disagreement rather than as agreement.
//
// It handles uncompressed solid files only (kind 0 and 1, mode 0). Every
// positive vector is written uncompressed for exactly that reason: compressed
// bytes are not part of the format's identity (§4.8), so there is nothing in
// them for a conformance vector to pin.

import (
	"errors"
	"fmt"
	"math"
	"unicode/utf8"

	"github.com/cespare/xxhash/v2"
)

// Specification constants, restated from format.md rather than imported from
// format.go. If the two ever disagree, a vector fails.
const (
	vecHeaderSize = 16
	vecFooterSize = 44

	vecFlagStoreLight    = uint32(1) << 0
	vecFlagStats         = uint32(1) << 1
	vecFlagReserved2     = uint32(1) << 2
	vecFlagDefaultBiome  = uint32(1) << 3
	vecFlagUncompressed  = uint32(1) << 4
	vecDimShift          = 5
	vecDimMask           = uint32(0b111) << vecDimShift
	vecDefaultBiomeShift = 16
	// Bits 8-15 are reserved (§2.3) and bit 2 with them.
	vecReservedFlags = vecFlagReserved2 | uint32(0xFF00)

	vecMaxString        = 65535
	vecMaxBlob          = 16 << 20
	vecMaxGlobalPalette = 1 << 20
	vecMaxBlobTable     = 1 << 24
	vecMaxLocalPalette  = 1 << 16
	vecMaxLayers        = 255
	vecMaxSections      = 4096
	vecMinSectionIdx    = -2048
	vecMaxSectionIdx    = 2047
	vecMaxPerChunk      = 1 << 20
	vecMaxProps         = 64
	vecMaxStructAxis    = 1 << 20
	vecMaxStructCells   = 1 << 20
	vecNBTDepth         = 64
	vecLightArrayLen    = 2048
)

// vecSpan names one contiguous run of bytes the walker consumed.
type vecSpan struct {
	path string
	off  int // absolute file offset
	size int
	val  uint64 // scalar value, where the vecField is a number
	text string // decoded text, where the vecField is a string
}

// vecLayout is the result of walking one file.
type vecLayout struct {
	file  []byte
	spans []vecSpan
	index map[string]vecSpan

	kind         uint8
	mode         uint8
	flags        uint32
	blockVersion int32

	blockPalette []vecState
	biomePalette []string
	blobs        []vecBlob
	records      []vecRecord
	// Structure fields, set when kind == 1.
	size   [3]uint64
	origin [3]int64
	cells  []int // indices of present cells
}

// vecState is one global block palette entry as it appears on the wire.
type vecState struct {
	name    string
	props   []vecProp
	version int32 // 0 = no override, i.e. the palette's own version
}

type vecProp struct {
	key   string
	typ   uint8
	value any
}

// vecBlob is one decoded section blob from the blob table.
type vecBlob struct {
	refs  []uint32
	width uint8
	// indices, expanded to one entry per position; nil when width == 0.
	idx []uint32
}

type vecRecord struct {
	x, z       int32
	minSection int32
	sectionN   int
	// blockSections maps section index to the blob ids of its layers.
	blockSections map[int][]uint32
	biomeSections map[int]uint32
	light         map[int]uint8
	blockEntities int
	entities      int
	tick          int64
	ticks         int
}

// look returns the span at path, failing loudly when the walker never
// recorded one: a test asserting on a vecField that does not exist is a test
// asserting nothing.
func (l *vecLayout) look(path string) (vecSpan, error) {
	s, ok := l.index[path]
	if !ok {
		return vecSpan{}, fmt.Errorf("no vecField named %q in the layout", path)
	}
	return s, nil
}

// vecReader walks a file with absolute offsets, recording a span per vecField.
type vecReader struct {
	b   []byte
	off int
	l   *vecLayout
}

func (r *vecReader) record(path string, off, size int, val uint64, text string) {
	s := vecSpan{path: path, off: off, size: size, val: val, text: text}
	r.l.spans = append(r.l.spans, s)
	if _, dup := r.l.index[path]; dup {
		// Paths are indexed, so a repeat means the walker built two fields with
		// one name and every assertion on that name is ambiguous.
		panic("vector walker: duplicate vecField path " + path)
	}
	r.l.index[path] = s
}

func (r *vecReader) take(path string, n int) ([]byte, error) {
	if n < 0 || r.off+n > len(r.b) {
		return nil, fmt.Errorf("%s: want %d bytes, %d remain", path, n, len(r.b)-r.off)
	}
	off := r.off
	r.off += n
	r.record(path, off, n, 0, "")
	return r.b[off : off+n], nil
}

func (r *vecReader) u8(path string) (uint8, error) {
	if r.off >= len(r.b) {
		return 0, fmt.Errorf("%s: truncated", path)
	}
	v := r.b[r.off]
	r.record(path, r.off, 1, uint64(v), "")
	r.off++
	return v, nil
}

func (r *vecReader) fixed(path string, n int) (uint64, error) {
	if r.off+n > len(r.b) {
		return 0, fmt.Errorf("%s: truncated", path)
	}
	var v uint64
	for i := range n {
		v |= uint64(r.b[r.off+i]) << (8 * i)
	}
	r.record(path, r.off, n, v, "")
	r.off += n
	return v, nil
}

// uvarint decodes an unsigned LEB128 and enforces §1's minimality rule: a
// value whose final byte is zero after a continuation byte is overlong, and
// two byte sequences would denote one number.
func (r *vecReader) uvarint(path string) (uint64, error) {
	var v uint64
	var shift uint
	for i := 0; ; i++ {
		if r.off+i >= len(r.b) {
			return 0, fmt.Errorf("%s: truncated uvarint", path)
		}
		c := r.b[r.off+i]
		if i == 9 {
			if c > 1 {
				return 0, fmt.Errorf("%s: uvarint overflows 64 bits", path)
			}
			if c&0x80 != 0 {
				return 0, fmt.Errorf("%s: uvarint longer than 10 bytes", path)
			}
		}
		v |= uint64(c&0x7f) << shift
		if c&0x80 == 0 {
			if i > 0 && c == 0 {
				return 0, fmt.Errorf("%s: overlong uvarint encoding", path)
			}
			r.record(path, r.off, i+1, v, "")
			r.off += i + 1
			return v, nil
		}
		shift += 7
	}
}

// svarint decodes a zigzag LEB128 with the same minimality rule.
func (r *vecReader) svarint(path string) (int64, error) {
	u, err := r.uvarint(path)
	if err != nil {
		return 0, err
	}
	v := int64(u>>1) ^ -int64(u&1)
	s := r.l.index[path]
	s.val = uint64(v)
	r.l.index[path] = s
	r.l.spans[len(r.l.spans)-1] = s
	return v, nil
}

// str reads a length-prefixed UTF-8 string, enforcing §1's ceiling and the
// requirement that the bytes decode: palettes are ordered by byte comparison,
// so an implementation that decodes before comparing must see the same bytes.
func (r *vecReader) str(path string) (string, error) {
	n, err := r.uvarint(path + ".len")
	if err != nil {
		return "", err
	}
	if n > vecMaxString {
		return "", fmt.Errorf("%s: string of %d bytes exceeds %d", path, n, vecMaxString)
	}
	b, err := r.take(path, int(n))
	if err != nil {
		return "", err
	}
	if !utf8.Valid(b) {
		return "", fmt.Errorf("%s: string is not valid UTF-8", path)
	}
	s := string(b)
	sp := r.l.index[path]
	sp.text = s
	r.l.index[path] = sp
	r.l.spans[len(r.l.spans)-1] = sp
	return s, nil
}

// blob reads a length-prefixed opaque byte string.
func (r *vecReader) blob(path string) ([]byte, error) {
	n, err := r.uvarint(path + ".len")
	if err != nil {
		return nil, err
	}
	if n > vecMaxBlob {
		return nil, fmt.Errorf("%s: blob of %d bytes exceeds %d", path, n, vecMaxBlob)
	}
	return r.take(path, int(n))
}

// bitset reads ceil(n/8) bytes and enforces that the padding bits above n are
// zero, so one set of presence bits has one encoding.
func (r *vecReader) bitset(path string, n int) ([]byte, error) {
	b, err := r.take(path, (n+7)/8)
	if err != nil {
		return nil, err
	}
	if n%8 != 0 && len(b) > 0 {
		if b[len(b)-1]>>(n%8) != 0 {
			return nil, fmt.Errorf("%s: padding bits above bit %d are not zero", path, n)
		}
	}
	return b, nil
}

func vecBit(bs []byte, i int) bool { return bs[i/8]&(1<<(i%8)) != 0 }

// vecMortonKey implements §1's Z-order key from the specification text.
func vecMortonKey(x, z int32) uint64 {
	spread := func(v uint32) uint64 {
		var out uint64
		for i := range 32 {
			if v&(1<<i) != 0 {
				out |= 1 << (2 * i)
			}
		}
		return out
	}
	return spread(uint32(x)^0x80000000) | spread(uint32(z)^0x80000000)<<1
}

// vecWalk parses a complete uncompressed pile file per format.md and returns
// its layout. Every error it reports is a specification violation.
func vecWalk(file []byte) (*vecLayout, error) {
	l := &vecLayout{file: file, index: map[string]vecSpan{}}
	r := &vecReader{b: file, l: l}

	if len(file) < vecHeaderSize+vecFooterSize {
		return nil, fmt.Errorf("file of %d bytes cannot hold a header and a footer", len(file))
	}

	// §2.1 Header, 16 bytes at offset 0.
	magic, err := r.take("header.magic", 4)
	if err != nil {
		return nil, err
	}
	if string(magic) != "PILE" {
		return nil, fmt.Errorf("header magic is %q, want \"PILE\"", magic)
	}
	version, err := r.fixed("header.version", 2)
	if err != nil {
		return nil, err
	}
	if version != 2 {
		return nil, fmt.Errorf("version %d, want 2", version)
	}
	kind, err := r.u8("header.kind")
	if err != nil {
		return nil, err
	}
	mode, err := r.u8("header.mode")
	if err != nil {
		return nil, err
	}
	flags32, err := r.fixed("header.flags", 4)
	if err != nil {
		return nil, err
	}
	bv, err := r.fixed("header.blockVersion", 4)
	if err != nil {
		return nil, err
	}
	l.kind, l.mode, l.flags, l.blockVersion = kind, mode, uint32(flags32), int32(uint32(bv))

	if kind > 1 {
		return nil, fmt.Errorf("kind %d is not defined", kind)
	}
	if mode > 1 {
		return nil, fmt.Errorf("mode %d is not defined", mode)
	}
	if kind == 1 && mode == 1 {
		return nil, errors.New("a structure is always solid: kind 1 with mode 1 is invalid")
	}
	if mode != 0 {
		return nil, errors.New("the vector walker handles solid files only")
	}
	if l.blockVersion == 0 {
		return nil, errors.New("blockVersion is zero, which means \"the palette's own version\"")
	}
	if l.flags&vecReservedFlags != 0 {
		return nil, fmt.Errorf("reserved flag bits set: %#x", l.flags&vecReservedFlags)
	}
	if d := (l.flags & vecDimMask) >> vecDimShift; d > 2 {
		return nil, fmt.Errorf("dimension %d is reserved", d)
	}
	if l.flags&vecFlagDefaultBiome == 0 && l.flags>>vecDefaultBiomeShift != 0 {
		return nil, errors.New("default biome reference is set without its flag")
	}
	if kind == 1 {
		// §6: a structure sets no flag but Uncompressed.
		if l.flags&^vecFlagUncompressed != 0 {
			return nil, fmt.Errorf("structure sets flags %#x; only Uncompressed is permitted", l.flags)
		}
	}
	if l.flags&vecFlagUncompressed == 0 {
		return nil, errors.New("the vector walker handles uncompressed bodies only")
	}

	// §2.2 Footer, 44 bytes at the end. Parsed before the body so its bytes can
	// be excluded from the body walk; the spans are re-sorted by offset at the
	// end, so recording it early does not disturb the coverage check.
	fr := &vecReader{b: file, off: len(file) - vecFooterSize, l: l}
	wantHash, err := fr.fixed("footer.hash", 8)
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"dirOffset", "dirLength", "generation", "prevFooter"} {
		v, err := fr.fixed("footer."+name, 8)
		if err != nil {
			return nil, err
		}
		if v != 0 {
			return nil, fmt.Errorf("solid footer %s must be zero, got %d", name, v)
		}
	}
	fmagic, err := fr.take("footer.magic", 4)
	if err != nil {
		return nil, err
	}
	if string(fmagic) != "ELIP" {
		return nil, fmt.Errorf("footer magic is %q, want \"ELIP\"", fmagic)
	}

	// §2.4 checkpoint hash: header image || stored payload || footer[8:].
	body := file[vecHeaderSize : len(file)-vecFooterSize]
	h := xxhash.New()
	_, _ = h.Write(file[:vecHeaderSize])
	_, _ = h.Write(body)
	_, _ = h.Write(file[len(file)-vecFooterSize+8:])
	if got := h.Sum64(); got != wantHash {
		return nil, fmt.Errorf("checkpoint hash is %016x, computed %016x", wantHash, got)
	}

	// Body.
	r.b = file[:len(file)-vecFooterSize]
	if err := vecWalkBody(r, l); err != nil {
		return nil, err
	}
	if r.off != len(file)-vecFooterSize {
		return nil, fmt.Errorf("%d bytes follow the last record", len(file)-vecFooterSize-r.off)
	}

	if err := vecCheckCoverage(l); err != nil {
		return nil, err
	}
	return l, nil
}

// vecCheckCoverage requires the recorded spans to tile the file exactly: in
// ascending offset order, with no gap and no overlap, from byte 0 to the end.
// A vecField added, removed or reordered breaks this before any value assertion
// gets a chance to.
func vecCheckCoverage(l *vecLayout) error {
	// Empty blobs and empty strings record a zero-width span, which consumes
	// no bytes and cannot participate in a tiling.
	spans := make([]vecSpan, 0, len(l.spans))
	for _, s := range l.spans {
		if s.size > 0 {
			spans = append(spans, s)
		}
	}
	// The footer is walked before the body, so sort by offset first.
	for i := 1; i < len(spans); i++ {
		for j := i; j > 0 && spans[j].off < spans[j-1].off; j-- {
			spans[j], spans[j-1] = spans[j-1], spans[j]
		}
	}
	l.spans = spans
	next := 0
	for _, s := range spans {
		if s.off != next {
			return fmt.Errorf("vecField %s starts at %d, expected %d (gap or overlap)", s.path, s.off, next)
		}
		next = s.off + s.size
	}
	if next != len(l.file) {
		return fmt.Errorf("%d trailing bytes are not covered by any vecField", len(l.file)-next)
	}
	return nil
}

func vecWalkBody(r *vecReader, l *vecLayout) error {
	// §4.1 meta block.
	for _, name := range []string{"settings", "userData", "markers", "border"} {
		b, err := r.blob("meta." + name)
		if err != nil {
			return err
		}
		if name != "userData" && len(b) > 0 {
			if err := vecNBT(b); err != nil {
				return fmt.Errorf("meta.%s: %w", name, err)
			}
		}
	}
	if l.flags&vecFlagStats != 0 {
		b, err := r.blob("meta.stats")
		if err != nil {
			return err
		}
		if len(b) > 0 {
			if err := vecNBT(b); err != nil {
				return fmt.Errorf("meta.stats: %w", err)
			}
		}
	}

	// §3.1 global block state palette.
	pal, err := vecWalkStatePalette(r, "blockPalette", l.blockVersion)
	if err != nil {
		return err
	}
	l.blockPalette = pal

	// §3.2 global biome palette.
	bn, err := r.uvarint("biomePalette.count")
	if err != nil {
		return err
	}
	if bn > vecMaxGlobalPalette {
		return fmt.Errorf("biome palette of %d entries exceeds %d", bn, vecMaxGlobalPalette)
	}
	seenBiome := map[string]bool{}
	for i := range int(bn) {
		name, err := r.str(fmt.Sprintf("biomePalette.name[%d]", i))
		if err != nil {
			return err
		}
		if seenBiome[name] {
			return fmt.Errorf("biome palette repeats %q", name)
		}
		seenBiome[name] = true
		// §3.2: names are fully qualified.
		if !vecQualified(name) {
			return fmt.Errorf("biome name %q has no namespace", name)
		}
		l.biomePalette = append(l.biomePalette, name)
	}
	if l.kind == 1 && bn != 0 {
		return fmt.Errorf("structure biome palette has %d entries, must have 0", bn)
	}

	// §3.4 blob table.
	tn, err := r.uvarint("blobTable.count")
	if err != nil {
		return err
	}
	if tn > vecMaxBlobTable {
		return fmt.Errorf("blob table of %d entries exceeds %d", tn, vecMaxBlobTable)
	}
	raw := make([][]byte, 0, min(tn, 4096))
	for i := range int(tn) {
		start := r.off
		b, err := vecWalkBlob(r, fmt.Sprintf("blobTable.blob[%d]", i), len(l.blockPalette), len(l.biomePalette))
		if err != nil {
			return err
		}
		bytes := r.b[start:r.off]
		for j, prev := range raw {
			if string(prev) == string(bytes) {
				return fmt.Errorf("blob %d repeats blob %d: the table stores each unique blob once", i, j)
			}
		}
		raw = append(raw, bytes)
		l.blobs = append(l.blobs, b)
	}

	if l.kind == 1 {
		return vecWalkStructure(r, l)
	}
	return vecWalkWorld(r, l)
}

// vecQualified reports whether a name carries a namespace, per §3.2.
func vecQualified(name string) bool {
	for i := range len(name) {
		if name[i] == ':' && i > 0 && i < len(name)-1 {
			return true
		}
	}
	return false
}

func vecWalkStatePalette(r *vecReader, path string, paletteVersion int32) ([]vecState, error) {
	n, err := r.uvarint(path + ".count")
	if err != nil {
		return nil, err
	}
	if n > vecMaxGlobalPalette {
		return nil, fmt.Errorf("%s: %d entries exceeds %d", path, n, vecMaxGlobalPalette)
	}
	out := make([]vecState, 0, min(n, 4096))
	encoded := make([]string, 0, min(n, 4096))
	for i := range int(n) {
		ep := fmt.Sprintf("%s.entry[%d]", path, i)
		start := r.off
		var st vecState
		if st.name, err = r.str(ep + ".name"); err != nil {
			return nil, err
		}
		pn, err := r.uvarint(ep + ".propN")
		if err != nil {
			return nil, err
		}
		if pn > vecMaxProps {
			return nil, fmt.Errorf("%s: %d properties exceeds %d", ep, pn, vecMaxProps)
		}
		prev := ""
		for j := range int(pn) {
			pp := fmt.Sprintf("%s.prop[%d]", ep, j)
			key, err := r.str(pp + ".key")
			if err != nil {
				return nil, err
			}
			if j > 0 && key <= prev {
				return nil, fmt.Errorf("%s: property key %q follows %q, want strictly ascending", pp, key, prev)
			}
			prev = key
			typ, err := r.u8(pp + ".type")
			if err != nil {
				return nil, err
			}
			var val any
			switch typ {
			case 0:
				v, err := r.u8(pp + ".value")
				if err != nil {
					return nil, err
				}
				val = v
			case 1:
				v, err := r.fixed(pp+".value", 4)
				if err != nil {
					return nil, err
				}
				val = int32(uint32(v))
			case 2:
				v, err := r.str(pp + ".value")
				if err != nil {
					return nil, err
				}
				val = v
			default:
				return nil, fmt.Errorf("%s: property type %d is not one of 0, 1, 2", pp, typ)
			}
			st.props = append(st.props, vecProp{key: key, typ: typ, value: val})
		}
		key := string(r.b[start:r.off])
		for j, prev := range encoded {
			if prev == key {
				return nil, fmt.Errorf("%s: entry %d encodes identically to entry %d", path, i, j)
			}
		}
		encoded = append(encoded, key)
		out = append(out, st)
	}

	// Sparse version override table.
	on, err := r.uvarint(path + ".overrideN")
	if err != nil {
		return nil, err
	}
	if on > uint64(len(out)) {
		return nil, fmt.Errorf("%s: %d overrides for %d entries", path, on, len(out))
	}
	var idx uint64
	for i := range int(on) {
		op := fmt.Sprintf("%s.override[%d]", path, i)
		d, err := r.uvarint(op + ".indexDelta")
		if err != nil {
			return nil, err
		}
		if i > 0 && d == 0 {
			return nil, fmt.Errorf("%s: zero delta, indices strictly ascend", op)
		}
		idx += d
		if idx >= uint64(len(out)) {
			return nil, fmt.Errorf("%s: index %d is past the palette", op, idx)
		}
		v, err := r.fixed(op+".version", 4)
		if err != nil {
			return nil, err
		}
		ver := int32(uint32(v))
		if ver == 0 {
			return nil, fmt.Errorf("%s: override version must be non-zero", op)
		}
		if ver == paletteVersion {
			return nil, fmt.Errorf("%s: override repeats the palette's own version %d", op, ver)
		}
		out[idx].version = ver
	}
	return out, nil
}

// vecWalkBlob parses one §3.3 section blob and applies every canonicality rule
// the specification states for it.
func vecWalkBlob(r *vecReader, path string, blockPaletteN, biomePaletteN int) (vecBlob, error) {
	var b vecBlob
	pn, err := r.uvarint(path + ".paletteN")
	if err != nil {
		return b, err
	}
	if pn < 1 || pn > vecMaxLocalPalette {
		return b, fmt.Errorf("%s: local palette of %d entries, want 1..%d", path, pn, vecMaxLocalPalette)
	}
	b.refs = make([]uint32, 0, min(pn, 4096))
	for i := range int(pn) {
		v, err := r.uvarint(fmt.Sprintf("%s.ref[%d]", path, i))
		if err != nil {
			return b, err
		}
		if v > vecMaxGlobalPalette {
			return b, fmt.Errorf("%s: reference %d is out of range", path, v)
		}
		if i > 0 && uint32(v) <= b.refs[i-1] {
			return b, fmt.Errorf("%s: reference %d does not exceed %d; references strictly ascend", path, v, b.refs[i-1])
		}
		b.refs = append(b.refs, uint32(v))
	}
	width, err := r.u8(path + ".width")
	if err != nil {
		return b, err
	}
	b.width = width
	switch width {
	case 0:
		if pn != 1 {
			return b, fmt.Errorf("%s: uniform width with %d palette entries", path, pn)
		}
	case 1:
		if pn == 1 {
			return b, fmt.Errorf("%s: a single-entry palette must use the uniform width", path)
		}
		if pn > 256 {
			return b, fmt.Errorf("%s: u8 indices cannot address %d palette entries", path, pn)
		}
		raw, err := r.take(path+".indices", 4096)
		if err != nil {
			return b, err
		}
		b.idx = make([]uint32, 4096)
		for i, v := range raw {
			b.idx[i] = uint32(v)
		}
	case 2:
		if pn <= 256 {
			return b, fmt.Errorf("%s: u16 indices for %d palette entries; the narrowest sufficient width is the only valid one", path, pn)
		}
		raw, err := r.take(path+".indices", 8192)
		if err != nil {
			return b, err
		}
		b.idx = make([]uint32, 4096)
		for i := range 4096 {
			b.idx[i] = uint32(raw[2*i]) | uint32(raw[2*i+1])<<8
		}
	default:
		return b, fmt.Errorf("%s: index width %d is not one of 0, 1, 2", path, width)
	}
	if b.idx != nil {
		used := make([]bool, pn)
		for _, i := range b.idx {
			if uint64(i) >= pn {
				return b, fmt.Errorf("%s: index %d is at or past the local palette of %d", path, i, pn)
			}
			used[i] = true
		}
		for i, u := range used {
			if !u {
				return b, fmt.Errorf("%s: local palette entry %d is never used; the palette holds only entries the indices use", path, i)
			}
		}
	}
	return b, nil
}

func vecWalkWorld(r *vecReader, l *vecLayout) error {
	n, err := r.uvarint("chunkN")
	if err != nil {
		return err
	}
	if n > math.MaxUint32 {
		return fmt.Errorf("chunkN %d exceeds 2^32-1", n)
	}
	blobUsed := make([]bool, len(l.blobs))
	// §3.4: blob ids are assigned in first-use order, which is checkable on the
	// wire: the first reference must be 0 and each later one must repeat an id
	// already seen or be exactly the next unseen id.
	nextID := uint32(0)
	useBlob := func(id uint32, where string) error {
		if int(id) >= len(l.blobs) {
			return fmt.Errorf("%s: blob reference %d is past the table of %d", where, id, len(l.blobs))
		}
		if id > nextID {
			return fmt.Errorf("%s: blob id %d used before %d; ids are assigned in first-use order", where, id, nextID)
		}
		if id == nextID {
			nextID++
		}
		blobUsed[id] = true
		return nil
	}

	var prevX, prevZ int64
	var prevKey uint64
	for i := range int(n) {
		rp := fmt.Sprintf("record[%d]", i)
		var rec vecRecord
		dx, err := r.svarint(rp + ".dx")
		if err != nil {
			return err
		}
		dz, err := r.svarint(rp + ".dz")
		if err != nil {
			return err
		}
		sx, sz := prevX+dx, prevZ+dz
		if sx < math.MinInt32 || sx > math.MaxInt32 || sz < math.MinInt32 || sz > math.MaxInt32 {
			return fmt.Errorf("%s: position (%d,%d) is outside int32", rp, sx, sz)
		}
		prevX, prevZ = sx, sz
		rec.x, rec.z = int32(sx), int32(sz)
		key := vecMortonKey(rec.x, rec.z)
		if i > 0 && key <= prevKey {
			return fmt.Errorf("%s: Morton key %016x does not exceed %016x; record keys strictly ascend", rp, key, prevKey)
		}
		prevKey = key

		ms, err := r.svarint(rp + ".minSection")
		if err != nil {
			return err
		}
		sn, err := r.uvarint(rp + ".sectionN")
		if err != nil {
			return err
		}
		if sn < 1 || sn > vecMaxSections {
			return fmt.Errorf("%s: sectionN %d, want 1..%d", rp, sn, vecMaxSections)
		}
		if ms < vecMinSectionIdx || ms+int64(sn) > vecMaxSectionIdx+1 {
			return fmt.Errorf("%s: sections %d..%d fall outside %d..%d", rp, ms, ms+int64(sn)-1, vecMinSectionIdx, vecMaxSectionIdx)
		}
		rec.minSection, rec.sectionN = int32(ms), int(sn)
		rec.blockSections = map[int][]uint32{}
		rec.biomeSections = map[int]uint32{}

		bits, err := r.bitset(rp+".blockPresence", rec.sectionN)
		if err != nil {
			return err
		}
		for s := range rec.sectionN {
			if !vecBit(bits, s) {
				continue
			}
			sp := fmt.Sprintf("%s.section[%d]", rp, s)
			ln, err := r.uvarint(sp + ".layerN")
			if err != nil {
				return err
			}
			if ln < 1 || ln > vecMaxLayers {
				return fmt.Errorf("%s: layerN %d, want 1..%d", sp, ln, vecMaxLayers)
			}
			refs := make([]uint32, 0, ln)
			for k := range int(ln) {
				where := fmt.Sprintf("%s.blobRef[%d]", sp, k)
				id, err := r.uvarint(where)
				if err != nil {
					return err
				}
				if err := useBlob(uint32(id), where); err != nil {
					return err
				}
				refs = append(refs, uint32(id))
			}
			rec.blockSections[s] = refs
		}

		bio, err := r.bitset(rp+".biomePresence", rec.sectionN)
		if err != nil {
			return err
		}
		for s := range rec.sectionN {
			if !vecBit(bio, s) {
				continue
			}
			where := fmt.Sprintf("%s.biomeSection[%d].blobRef", rp, s)
			id, err := r.uvarint(where)
			if err != nil {
				return err
			}
			if err := useBlob(uint32(id), where); err != nil {
				return err
			}
			rec.biomeSections[s] = uint32(id)
		}

		if l.flags&vecFlagStoreLight != 0 {
			rec.light = map[int]uint8{}
			lb, err := r.bitset(rp+".lightPresence", rec.sectionN)
			if err != nil {
				return err
			}
			for s := range rec.sectionN {
				if !vecBit(lb, s) {
					continue
				}
				lp := fmt.Sprintf("%s.light[%d]", rp, s)
				f, err := r.u8(lp + ".flags")
				if err != nil {
					return err
				}
				if f == 0 {
					return fmt.Errorf("%s: light flags are zero; an entry with no arrays is a second encoding of an absent one", lp)
				}
				if f&^0b11 != 0 {
					return fmt.Errorf("%s: light flag bits 2-7 must be zero, got %#x", lp, f)
				}
				if f&1 != 0 {
					if _, err := r.take(lp+".blockLight", vecLightArrayLen); err != nil {
						return err
					}
				}
				if f&2 != 0 {
					if _, err := r.take(lp+".skyLight", vecLightArrayLen); err != nil {
						return err
					}
				}
				rec.light[s] = f
			}
		}

		lowY := int64(rec.minSection) * 16
		highY := lowY + int64(rec.sectionN)*16 - 1

		ben, err := r.uvarint(rp + ".beN")
		if err != nil {
			return err
		}
		if ben > vecMaxPerChunk {
			return fmt.Errorf("%s: %d block entities exceeds %d", rp, ben, vecMaxPerChunk)
		}
		type bePos struct{ x, y, z int64 }
		var prevBE bePos
		var prevBENBT string
		seenBE := map[bePos]bool{}
		for k := range int(ben) {
			bp := fmt.Sprintf("%s.be[%d]", rp, k)
			packed, err := r.u8(bp + ".packedXZ")
			if err != nil {
				return err
			}
			y, err := r.svarint(bp + ".y")
			if err != nil {
				return err
			}
			blob, err := r.blob(bp + ".nbt")
			if err != nil {
				return err
			}
			if err := vecNBT(blob); err != nil {
				return fmt.Errorf("%s.nbt: %w", bp, err)
			}
			if y < lowY || y > highY {
				return fmt.Errorf("%s: y %d is outside the record's span %d..%d", bp, y, lowY, highY)
			}
			p := bePos{x: int64(packed & 0xF), y: y, z: int64(packed >> 4)}
			if seenBE[p] {
				return fmt.Errorf("%s: a second block entity occupies %v", bp, p)
			}
			seenBE[p] = true
			// §4.8: (y, z, x), then the encoded NBT as written.
			if k > 0 {
				cur := [3]int64{p.y, p.z, p.x}
				old := [3]int64{prevBE.y, prevBE.z, prevBE.x}
				if cur != old {
					if !vecLess3(old, cur) {
						return fmt.Errorf("%s: block entities are not in (y,z,x) order", bp)
					}
				} else if string(blob) <= prevBENBT {
					return fmt.Errorf("%s: block entities at one position are not ordered by their NBT", bp)
				}
			}
			prevBE, prevBENBT = p, string(blob)
			rec.blockEntities++
		}

		entn, err := r.uvarint(rp + ".entN")
		if err != nil {
			return err
		}
		if entn > vecMaxPerChunk {
			return fmt.Errorf("%s: %d entities exceeds %d", rp, entn, vecMaxPerChunk)
		}
		for k := range int(entn) {
			blob, err := r.blob(fmt.Sprintf("%s.ent[%d].nbt", rp, k))
			if err != nil {
				return err
			}
			if err := vecNBT(blob); err != nil {
				return fmt.Errorf("%s.ent[%d].nbt: %w", rp, k, err)
			}
			rec.entities++
		}

		if rec.tick, err = r.svarint(rp + ".tick"); err != nil {
			return err
		}

		stn, err := r.uvarint(rp + ".stN")
		if err != nil {
			return err
		}
		if stn > vecMaxPerChunk {
			return fmt.Errorf("%s: %d scheduled updates exceeds %d", rp, stn, vecMaxPerChunk)
		}
		type tickKey struct {
			x, y, z int64
			at      int64
			ref     uint64
		}
		seenTick := map[tickKey]bool{}
		var prevTick [5]int64
		for k := range int(stn) {
			tp := fmt.Sprintf("%s.st[%d]", rp, k)
			packed, err := r.u8(tp + ".packedXZ")
			if err != nil {
				return err
			}
			y, err := r.svarint(tp + ".y")
			if err != nil {
				return err
			}
			ref, err := r.uvarint(tp + ".blockRef")
			if err != nil {
				return err
			}
			at, err := r.svarint(tp + ".at")
			if err != nil {
				return err
			}
			if int(ref) >= len(l.blockPalette) {
				return fmt.Errorf("%s: block reference %d is past the palette of %d", tp, ref, len(l.blockPalette))
			}
			if y < lowY || y > highY {
				return fmt.Errorf("%s: y %d is outside the record's span %d..%d", tp, y, lowY, highY)
			}
			kk := tickKey{x: int64(packed & 0xF), y: y, z: int64(packed >> 4), at: at, ref: ref}
			if seenTick[kk] {
				return fmt.Errorf("%s: a second scheduled update names the same position, tick and block", tp)
			}
			seenTick[kk] = true
			// §4.8: (y, z, x), then firing tick, then block reference.
			cur := [5]int64{kk.y, kk.z, kk.x, at, int64(ref)}
			if k > 0 && !vecLess5(prevTick, cur) {
				return fmt.Errorf("%s: scheduled updates are not in (y,z,x,tick,ref) order", tp)
			}
			prevTick = cur
			rec.ticks++
		}

		if _, err := r.blob(rp + ".userData"); err != nil {
			return err
		}
		l.records = append(l.records, rec)
	}
	for id, u := range blobUsed {
		if !u {
			return fmt.Errorf("blob table entry %d is referenced by no record", id)
		}
	}
	return nil
}

func vecLess3(a, b [3]int64) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

func vecLess5(a, b [5]int64) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

func vecWalkStructure(r *vecReader, l *vecLayout) error {
	for i := range 3 {
		v, err := r.uvarint(fmt.Sprintf("structure.size[%d]", i))
		if err != nil {
			return err
		}
		if v < 1 || v > vecMaxStructAxis {
			return fmt.Errorf("structure.size[%d] is %d, want 1..%d", i, v, vecMaxStructAxis)
		}
		l.size[i] = v
	}
	for i := range 3 {
		v, err := r.svarint(fmt.Sprintf("structure.origin[%d]", i))
		if err != nil {
			return err
		}
		if v < math.MinInt32 || v > math.MaxInt32 {
			return fmt.Errorf("structure.origin[%d] is %d, outside int32", i, v)
		}
		l.origin[i] = v
	}
	// §6: cells{X,Y,Z} = ceil(size/16), computed in 64 bits.
	nx := (int64(l.size[0]) + 15) >> 4
	ny := (int64(l.size[1]) + 15) >> 4
	nz := (int64(l.size[2]) + 15) >> 4
	cells := nx * ny * nz
	if cells > vecMaxStructCells {
		return fmt.Errorf("structure has %d cells, limit %d", cells, vecMaxStructCells)
	}
	bits, err := r.bitset("structure.cellPresence", int(cells))
	if err != nil {
		return err
	}
	blobUsed := make([]bool, len(l.blobs))
	nextID := uint32(0)
	for c := range int(cells) {
		if !vecBit(bits, c) {
			continue
		}
		cp := fmt.Sprintf("structure.cell[%d]", c)
		ln, err := r.uvarint(cp + ".layerN")
		if err != nil {
			return err
		}
		if ln < 1 || ln > vecMaxLayers {
			return fmt.Errorf("%s: layerN %d, want 1..%d", cp, ln, vecMaxLayers)
		}
		for k := range int(ln) {
			where := fmt.Sprintf("%s.blobRef[%d]", cp, k)
			id, err := r.uvarint(where)
			if err != nil {
				return err
			}
			if int(id) >= len(l.blobs) {
				return fmt.Errorf("%s: blob reference %d is past the table of %d", where, id, len(l.blobs))
			}
			if uint32(id) > nextID {
				return fmt.Errorf("%s: blob id %d used before %d; ids are assigned in first-use order", where, id, nextID)
			}
			if uint32(id) == nextID {
				nextID++
			}
			blobUsed[id] = true
		}
		l.cells = append(l.cells, c)
	}

	ben, err := r.uvarint("structure.beN")
	if err != nil {
		return err
	}
	if ben > vecMaxPerChunk {
		return fmt.Errorf("structure has %d block entities, exceeds %d", ben, vecMaxPerChunk)
	}
	seen := map[[3]uint64]bool{}
	var prev [3]int64
	var prevNBT string
	for k := range int(ben) {
		bp := fmt.Sprintf("structure.be[%d]", k)
		var p [3]uint64
		for i := range 3 {
			v, err := r.uvarint(fmt.Sprintf("%s.pos[%d]", bp, i))
			if err != nil {
				return err
			}
			if v >= l.size[i] {
				return fmt.Errorf("%s: coordinate %d is %d, outside the declared box of %d", bp, i, v, l.size[i])
			}
			p[i] = v
		}
		blob, err := r.blob(bp + ".nbt")
		if err != nil {
			return err
		}
		if err := vecNBT(blob); err != nil {
			return fmt.Errorf("%s.nbt: %w", bp, err)
		}
		if seen[p] {
			return fmt.Errorf("%s: a second block entity occupies %v", bp, p)
		}
		seen[p] = true
		cur := [3]int64{int64(p[1]), int64(p[2]), int64(p[0])}
		if k > 0 {
			if cur != prev {
				if !vecLess3(prev, cur) {
					return fmt.Errorf("%s: structure block entities are not in (y,z,x) order", bp)
				}
			} else if string(blob) <= prevNBT {
				return fmt.Errorf("%s: block entities at one position are not ordered by their NBT", bp)
			}
		}
		prev, prevNBT = cur, string(blob)
	}

	entn, err := r.uvarint("structure.entN")
	if err != nil {
		return err
	}
	if entn > vecMaxPerChunk {
		return fmt.Errorf("structure has %d entities, exceeds %d", entn, vecMaxPerChunk)
	}
	prevEnt := ""
	for k := range int(entn) {
		blob, err := r.blob(fmt.Sprintf("structure.ent[%d].nbt", k))
		if err != nil {
			return err
		}
		if err := vecNBT(blob); err != nil {
			return fmt.Errorf("structure.ent[%d].nbt: %w", k, err)
		}
		// §4.8: structure entities order on the encoded NBT alone.
		if k > 0 && string(blob) < prevEnt {
			return fmt.Errorf("structure.ent[%d]: entities are not ordered by their encoded NBT", k)
		}
		prevEnt = string(blob)
	}
	for id, u := range blobUsed {
		if !u {
			return fmt.Errorf("blob table entry %d is referenced by no cell", id)
		}
	}
	return nil
}

// vecNBT applies §1's canonical NBT rules: little-endian Bedrock NBT, a single
// unnamed root compound, keys unique and strictly ascending bytewise, nesting
// no deeper than 64, and nothing after the root.
func vecNBT(b []byte) error {
	p := &vecNBTReader{b: b}
	t, err := p.u8()
	if err != nil {
		return err
	}
	if t != 10 {
		return fmt.Errorf("root tag is %d, want a compound", t)
	}
	name, err := p.str()
	if err != nil {
		return err
	}
	if name != "" {
		return fmt.Errorf("root compound is named %q, want unnamed", name)
	}
	if err := p.compound(1); err != nil {
		return err
	}
	if p.off != len(b) {
		return fmt.Errorf("%d bytes follow the root compound", len(b)-p.off)
	}
	return nil
}

type vecNBTReader struct {
	b   []byte
	off int
}

func (p *vecNBTReader) u8() (uint8, error) {
	if p.off >= len(p.b) {
		return 0, errors.New("truncated NBT")
	}
	v := p.b[p.off]
	p.off++
	return v, nil
}

func (p *vecNBTReader) fixed(n int) (uint64, error) {
	if p.off+n > len(p.b) {
		return 0, errors.New("truncated NBT")
	}
	var v uint64
	for i := range n {
		v |= uint64(p.b[p.off+i]) << (8 * i)
	}
	p.off += n
	return v, nil
}

func (p *vecNBTReader) str() (string, error) {
	n, err := p.fixed(2)
	if err != nil {
		return "", err
	}
	if p.off+int(n) > len(p.b) {
		return "", errors.New("truncated NBT string")
	}
	s := string(p.b[p.off : p.off+int(n)])
	p.off += int(n)
	return s, nil
}

func (p *vecNBTReader) compound(depth int) error {
	if depth > vecNBTDepth {
		return fmt.Errorf("NBT nested deeper than %d", vecNBTDepth)
	}
	prev := ""
	first := true
	for {
		t, err := p.u8()
		if err != nil {
			return err
		}
		if t == 0 {
			return nil
		}
		name, err := p.str()
		if err != nil {
			return err
		}
		if !first && name <= prev {
			return fmt.Errorf("NBT key %q follows %q; keys are unique and strictly ascending", name, prev)
		}
		prev, first = name, false
		if err := p.payload(t, depth); err != nil {
			return err
		}
	}
}

func (p *vecNBTReader) payload(t uint8, depth int) error {
	switch t {
	case 1:
		_, err := p.fixed(1)
		return err
	case 2:
		_, err := p.fixed(2)
		return err
	case 3, 5:
		_, err := p.fixed(4)
		return err
	case 4, 6:
		_, err := p.fixed(8)
		return err
	case 7, 11, 12:
		n, err := p.fixed(4)
		if err != nil {
			return err
		}
		cnt := int32(uint32(n))
		if cnt < 0 {
			return fmt.Errorf("NBT array of %d elements", cnt)
		}
		w := map[uint8]int{7: 1, 11: 4, 12: 8}[t]
		if p.off+int(cnt)*w > len(p.b) {
			return errors.New("truncated NBT array")
		}
		p.off += int(cnt) * w
		return nil
	case 8:
		_, err := p.str()
		return err
	case 9:
		et, err := p.u8()
		if err != nil {
			return err
		}
		n, err := p.fixed(4)
		if err != nil {
			return err
		}
		cnt := int32(uint32(n))
		if cnt < 0 {
			return fmt.Errorf("NBT list of %d elements", cnt)
		}
		if et == 0 && cnt > 0 {
			return errors.New("NBT list of TAG_End with elements")
		}
		for range int(cnt) {
			if err := p.payload(et, depth+1); err != nil {
				return err
			}
		}
		return nil
	case 10:
		return p.compound(depth + 1)
	}
	return fmt.Errorf("NBT tag %d is not defined", t)
}
