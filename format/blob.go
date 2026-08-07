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
		if pn > 256 {
			return decBlob{}, corruptf("u8 indices with %d palette entries", pn)
		}
		if idx, err = r.take(4096); err != nil {
			return decBlob{}, err
		}
	case widthU16:
		if pn <= 256 {
			return decBlob{}, corruptf("non-minimal index width for %d palette entries", pn)
		}
		if idx, err = r.take(8192); err != nil {
			return decBlob{}, err
		}
	default:
		return decBlob{}, corruptf("unknown index width %d", width)
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
	blobs := make([]decBlob, 0, n)
	seen := make(map[string]int, n)
	for i := range n {
		start := r.off
		b, err := decodeOneBlob(r)
		if err != nil {
			return nil, fmt.Errorf("blob %d: %w", i, err)
		}
		// The table exists to store identical bytes once, so a repeat is a
		// second encoding of the same file. A reader can check this even
		// though it cannot check the palette's order, and where it can check
		// it should.
		if prev, dup := seen[string(r.b[start:r.off])]; dup {
			return nil, corruptf("blob %d repeats blob %d", i, prev)
		}
		seen[string(r.b[start:r.off])] = i
		blobs = append(blobs, b)
	}
	return blobs, nil
}
