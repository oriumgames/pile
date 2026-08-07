package format

import (
	"bytes"
	"fmt"
	"slices"

	"github.com/cespare/xxhash/v2"
	"github.com/df-mc/dragonfly/server/world/chunk"
)

// Index width codes for section blobs.
const (
	widthUniform = 0 // palette has one entry; no index bytes
	widthU8      = 1 // 4096 × u8
	widthU16     = 2 // 4096 × u16le
)

// secData is the intermediate form of one section storage (a block layer or a
// biome storage) between the two encoder passes: a local palette of
// build-order global palette indices plus 4096 local indices. idx is nil when
// the palette has a single entry.
type secData struct {
	pal []uint32
	idx []uint16
}

// extractStorage reads a PalettedStorage into secData via the unsafe mirror:
// the packed index words are unpacked directly and the palette is compacted
// to used entries only, so uncompacted storages (biomes, live chunks) still
// produce canonical output. add maps a palette value (block runtime ID or
// biome ID) to a build-order global palette index, counting the reference.
func extractStorage(storage *chunk.PalettedStorage, add func(v uint32) uint32) secData {
	palette, bits, words := storageRaw(storage)
	if len(palette) == 0 {
		return secData{pal: []uint32{add(0)}}
	}
	if bits == 0 || len(palette) == 1 {
		return secData{pal: []uint32{add(palette[0])}}
	}

	var idx [4096]uint16
	unpackStorage(bits, words, &idx)

	used := make([]bool, len(palette))
	for i, li := range idx {
		if int(li) >= len(palette) {
			idx[i] = 0
			li = 0
		}
		used[li] = true
	}
	remap := make([]uint16, len(palette))
	pal := make([]uint32, 0, len(palette))
	for i, u := range used {
		if u {
			remap[i] = uint16(len(pal))
			pal = append(pal, add(palette[i]))
		}
	}
	if len(pal) == 1 {
		return secData{pal: pal}
	}
	sd := secData{pal: pal, idx: make([]uint16, 4096)}
	for i, li := range idx {
		sd.idx[i] = remap[li]
	}
	return sd
}

// uniform reports whether the section resolved to a single palette entry and
// returns the build-order global index of that entry.
func (sd secData) uniform() (uint32, bool) {
	if len(sd.pal) == 1 {
		return sd.pal[0], true
	}
	return 0, false
}

// canonicalBlob encodes secData as a canonical section blob: global references
// remapped through the finalized palette order, sorted ascending, indices
// remapped accordingly. Identical content therefore yields identical bytes
// regardless of the order values were first seen in.
func canonicalBlob(sd secData, remap []uint32) []byte {
	final := make([]uint32, len(sd.pal))
	for i, b := range sd.pal {
		final[i] = remap[b]
	}

	// order[newLocal] = oldLocal, sorted by final reference.
	order := make([]uint16, len(final))
	for i := range order {
		order[i] = uint16(i)
	}
	slices.SortFunc(order, func(a, b uint16) int {
		return int(final[a]) - int(final[b])
	})

	// Two local entries can point at one global entry: the global palette
	// merges states that encode identically, so distinct runtime IDs that
	// describe the same state arrive here already collapsed. Local references
	// must strictly ascend, so the duplicates are folded away here too. Left
	// in, they would emit a reference twice (which readers reject) and could
	// hold a section at a wider index than its true entry count needs.
	inv := make([]uint16, len(final))
	refs := make([]uint32, 0, len(final))
	for _, oldLocal := range order {
		ref := final[oldLocal]
		if len(refs) == 0 || refs[len(refs)-1] != ref {
			refs = append(refs, ref)
		}
		inv[oldLocal] = uint16(len(refs) - 1)
	}
	n := len(refs)

	w := &writer{b: make([]byte, 0, 16+4096)}
	w.uvarint(uint64(n))
	for _, ref := range refs {
		w.uvarint(uint64(ref))
	}
	switch {
	case n <= 1:
		w.u8(widthUniform)
	case n <= 256:
		w.u8(widthU8)
		start := w.len()
		w.b = append(w.b, make([]byte, 4096)...)
		out := w.b[start:]
		for i, li := range sd.idx {
			out[i] = uint8(inv[li])
		}
	default:
		w.u8(widthU16)
		start := w.len()
		w.b = append(w.b, make([]byte, 8192)...)
		out := w.b[start:]
		for i, li := range sd.idx {
			v := inv[li]
			out[i*2] = uint8(v)
			out[i*2+1] = uint8(v >> 8)
		}
	}
	return w.bytes()
}

// blobTable deduplicates section blobs. Identical blobs (compared by content,
// with an xxhash64 prefilter) are stored once; ids are assigned in first-use
// order.
type blobTable struct {
	byHash map[uint64][]uint32
	blobs  [][]byte
	size   int
}

func newBlobTable() *blobTable {
	return &blobTable{byHash: make(map[uint64][]uint32)}
}

// add returns the id for blob, storing it if it is new. The blob must not be
// mutated afterwards.
func (t *blobTable) add(blob []byte) uint32 {
	h := xxhash.Sum64(blob)
	for _, id := range t.byHash[h] {
		if bytes.Equal(t.blobs[id], blob) {
			return id
		}
	}
	id := uint32(len(t.blobs))
	t.blobs = append(t.blobs, blob)
	t.byHash[h] = append(t.byHash[h], id)
	t.size += len(blob)
	return id
}

// encode writes the table: a count followed by the blobs, which are
// self-delimiting.
func (t *blobTable) encode(w *writer) {
	w.uvarint(uint64(len(t.blobs)))
	for _, b := range t.blobs {
		w.raw(b)
	}
}

// decBlob is a decoded section blob. idx aliases the body buffer.
type decBlob struct {
	refs  []uint32
	width uint8
	idx   []byte
}

// decodeOneBlob parses a single section blob from r.
func decodeOneBlob(r *reader) (decBlob, error) {
	pn, err := r.count(1<<16, "section palette")
	if err != nil {
		return decBlob{}, err
	}
	if pn == 0 {
		return decBlob{}, corruptf("empty section palette in blob")
	}
	// A reference is at least one byte and the width code follows them, so a
	// count larger than what is left cannot be honest. Sizing the slice from
	// the declared count alone let four bytes of input ask for 65,536 entries.
	if pn > r.remaining() {
		return decBlob{}, corruptf("section palette count %d exceeds input", pn)
	}
	refs := make([]uint32, pn)
	for j := range pn {
		v, err := r.uvarint()
		if err != nil {
			return decBlob{}, err
		}
		if v > maxPalette {
			return decBlob{}, corruptf("palette reference %d out of range", v)
		}
		// Canonical form: references ascend strictly, so a section has
		// exactly one encoding and duplicates cannot hide.
		if j > 0 && uint32(v) <= refs[j-1] {
			return decBlob{}, corruptf("section palette references are not strictly ascending")
		}
		refs[j] = uint32(v)
	}
	width, err := r.u8()
	if err != nil {
		return decBlob{}, err
	}
	var idx []byte
	switch width {
	case widthUniform:
		if pn != 1 {
			return decBlob{}, corruptf("uniform blob with %d palette entries", pn)
		}
	case widthU8:
		if pn == 1 {
			// The rule is "width is 0 if and only if paletteN is 1". Only the
			// forward direction was checked, so a uniform section could also
			// be written with a byte index array of 4096 zeroes: a second
			// encoding of the same section, which is the one thing a canonical
			// blob may not have.
			return decBlob{}, corruptf("single-entry palette must use the uniform width")
		}
		// A byte index cannot name an entry past 255, so a wider palette always
		// leaves one unnamed and §3.3's used-entry rule below refuses it anyway:
		// there is no input this condition alone rejects. It is kept as a cheap
		// pre-bound, not as enforcement -- it refuses before the 4096-byte scan
		// and the pn-sized bitmap that rule needs -- and it is the one that names
		// the width relation in its message.
		if pn > 256 {
			return decBlob{}, corruptf("u8 indices with %d palette entries", pn)
		}
		if idx, err = r.take(4096); err != nil {
			return decBlob{}, err
		}
	case widthU16:
		// pn == 1 is not tested separately here as it is for widthU8: it is
		// covered by the same condition that refuses every other palette too
		// small for 16-bit indices, so a check for it could not fail. The
		// widthU8 arm needs its own because a single-entry palette passes its
		// upper bound.
		if pn <= 256 {
			return decBlob{}, corruptf("non-minimal index width for %d palette entries", pn)
		}
		if idx, err = r.take(8192); err != nil {
			return decBlob{}, err
		}
	default:
		return decBlob{}, corruptf("unknown index width %d", width)
	}
	// Canonical form: the local palette holds only entries the indices actually
	// use. An unused entry is not merely dead weight two writers could differ
	// on; it hides what the section really is. A blob whose palette is
	// [air, stone] with all 4096 indices zero is uniform air, and every rule
	// that turns on uniformity -- §4.3's "an all-air section MUST be absent",
	// §4.3's trailing-all-air-layer rule, §4.7's uniform-default biome omission
	// -- reads the palette length to decide. With an unused entry admitted,
	// each of those is bypassed by padding the palette, so this check is what
	// makes them hold at all.
	//
	// An index at or past paletteN marks nothing here and is rejected where the
	// section is applied; two entries cannot both be justified by one index, so
	// a blob that survives this has a use for every entry it declares.
	if idx != nil {
		used := make([]bool, pn)
		unseen := pn
		step := 1
		if width == widthU16 {
			step = 2
		}
		for i := 0; i < len(idx) && unseen > 0; i += step {
			li := int(idx[i])
			if step == 2 {
				li |= int(idx[i+1]) << 8
			}
			if li < pn && !used[li] {
				used[li] = true
				unseen--
			}
		}
		if unseen > 0 {
			for li, u := range used {
				if !u {
					return decBlob{}, corruptf("section palette entry %d is never used by the indices", li)
				}
			}
		}
	}
	return decBlob{refs: refs, width: width, idx: idx}, nil
}

// decodeBlobTable parses the section blob table.
func decodeBlobTable(r *reader) ([]decBlob, error) {
	n, err := r.count(maxBlobs, "blob table")
	if err != nil {
		return nil, err
	}
	// A blob is at least 3 bytes; bound the allocation by the input size.
	if n > r.remaining()/3+1 {
		return nil, corruptf("blob table count %d exceeds input", n)
	}
	// The count is bounded by the input, and that is not the same as bounding
	// the allocation: a decBlob is 56 bytes, and a map entry and a span are
	// another forty between them, so three bytes of input reserved about a
	// hundred here and a 1,634-byte file asked for 2.42 GiB before a single
	// blob was parsed. Cap every hint and let append and the map grow, which
	// costs one reallocation on a file that really does hold that many blobs.
	hint := min(n, maxPrealloc)
	blobs := make([]decBlob, 0, hint)
	// Duplicate detection by hash over spans of the body, not by copying every
	// blob into a map key: a section blob is 4 KiB or 8 KiB of indices, so
	// keying on the bytes made reading a file cost more than the file. The
	// xxhash-then-compare prefilter is the one blobTable.add already uses to
	// deduplicate on the way in.
	seen := make(map[uint64][]int, hint)
	spans := make([][2]int, 0, hint)
	for i := range n {
		start := r.off
		b, err := decodeOneBlob(r)
		if err != nil {
			return nil, fmt.Errorf("blob %d: %w", i, err)
		}
		raw := r.b[start:r.off]
		h := xxhash.Sum64(raw)
		// The table exists to store identical bytes once, so a repeat is a
		// second encoding of the same file. A reader can check this even
		// though it cannot check the palette's order, and where it can check
		// it should.
		for _, prev := range seen[h] {
			if bytes.Equal(r.b[spans[prev][0]:spans[prev][1]], raw) {
				return nil, corruptf("blob %d repeats blob %d", i, prev)
			}
		}
		seen[h] = append(seen[h], i)
		spans = append(spans, [2]int{start, r.off})
		blobs = append(blobs, b)
	}
	return blobs, nil
}
