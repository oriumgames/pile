package format

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/cespare/xxhash/v2"
	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
)

// Meta is the cheap-to-read portion of a file: header fields and metadata
// blobs, no chunk data.
type Meta struct {
	Kind         uint8
	Mode         uint8
	Flags        uint32
	BlockVersion int32

	Settings []byte
	UserData []byte
	Markers  []byte
	Border   []byte
	Stats    []byte
}

// header is the parsed fixed-size file header.
type header struct {
	kind         uint8
	mode         uint8
	flags        uint32
	blockVersion int32
}

// parseFrame validates header, footer and body checksum of a complete file
// and returns the parsed header and the stored (possibly compressed) body.
func parseFrame(file []byte) (header, []byte, error) {
	var h header
	if len(file) < headerSize+footerSize {
		return h, nil, corruptf("file too short (%d bytes)", len(file))
	}
	if [4]byte(file[0:4]) != headerMagic {
		return h, nil, corruptf("bad header magic")
	}
	r := &reader{b: file, off: 4}
	ver, _ := r.take(2)
	version := uint16(ver[0]) | uint16(ver[1])<<8
	if version != Version {
		return h, nil, ErrUnsupportedVersion
	}
	h.kind, _ = r.u8()
	h.mode, _ = r.u8()
	h.flags, _ = r.u32()
	bv, _ := r.u32()
	h.blockVersion = int32(bv)

	if h.kind > KindStructure {
		return h, nil, corruptf("file kind %d is not defined", h.kind)
	}
	if h.flags&^knownFlags != 0 {
		return h, nil, ErrUnknownFlags
	}
	// The default-biome reference occupies the high bits only when its flag
	// is set; requiring them to be zero otherwise keeps one encoding per
	// header.
	if h.flags&FlagDefaultBiome == 0 && h.flags>>defaultBiomeShift != 0 {
		return h, nil, corruptf("default biome reference set without its flag")
	}
	// Reserved dimension values are rejected rather than ignored, so the
	// encoding stays available to a later version.
	if d := Dimension((h.flags & dimensionMask) >> dimensionShift); d > maxDimension {
		return h, nil, corruptf("dimension %d is reserved", uint8(d))
	}
	if h.mode != ModeSolid {
		return h, nil, ErrUnsupportedMode
	}

	ftr := file[len(file)-footerSize:]
	if [4]byte(ftr[footerSize-4:]) != footerMagic {
		return h, nil, corruptf("bad footer magic")
	}
	fr := &reader{b: ftr}
	wantHash, _ := fr.u64()

	// Solid files have no directory or checkpoint chain: those control words
	// must be zero, so a file has exactly one valid encoding.
	for off, name := range map[int]string{8: "directory offset", 16: "directory length", 24: "generation", 32: "previous footer"} {
		if v := binary.LittleEndian.Uint64(ftr[off : off+8]); v != 0 {
			return h, nil, corruptf("solid footer %s must be zero, got %d", name, v)
		}
	}

	stored := file[headerSize : len(file)-footerSize]
	if checkpointHash(file[:headerSize], stored, ftr[8:]) != wantHash {
		return h, nil, ErrChecksum
	}
	return h, stored, nil
}

// decompressBody returns the raw body bytes, decompressing when needed.
func decompressBody(h header, stored []byte) ([]byte, error) {
	if h.flags&FlagUncompressed != 0 {
		// The ceiling is a validity rule about the body, not a guard against
		// decompression: an uncompressed body past it is just as invalid, and
		// accepting one here would make the same content legal in one
		// encoding and illegal in the other.
		if len(stored) > maxDecodedBody {
			return nil, corruptf("body is %d bytes, limit %d", len(stored), maxDecodedBody)
		}
		return stored, nil
	}
	body, err := sharedDecoder().DecodeAll(stored, nil)
	if err != nil {
		return nil, corruptf("decompress body: %v", err)
	}
	return body, nil
}

// readMetaBlobs reads the meta block from the start of the body. Blobs the
// format designates as NBT are validated here: accepting a non-canonical one
// would admit two encodings of the same metadata.
func readMetaBlobs(r *reader, flags uint32) (settings, userData, markers, border, stats []byte, err error) {
	nbtBlob := func(what string) ([]byte, error) {
		b, err := r.blob()
		if err != nil {
			return nil, err
		}
		if len(b) > 0 {
			if err := validateNBT(b); err != nil {
				return nil, fmt.Errorf("%s: %w", what, err)
			}
		}
		return b, nil
	}
	if settings, err = nbtBlob("settings"); err != nil {
		return
	}
	if userData, err = r.blob(); err != nil {
		return
	}
	if markers, err = nbtBlob("markers"); err != nil {
		return
	}
	if border, err = nbtBlob("border"); err != nil {
		return
	}
	if flags&FlagStats != 0 {
		stats, err = nbtBlob("stats")
		if err != nil {
			return
		}
	}
	// The §7 schemas are validity rules, not writer conventions, so the reader
	// applies exactly what the writer refuses to produce. Without this a file
	// whose border is a list, or whose marker list is unsorted, is rejected by
	// one half of this package and accepted by the other, and an unsorted
	// marker list is a second encoding of one marker collection.
	if err = checkMetaSchemas(settings, markers, border); err != nil {
		return
	}
	return
}

// ReadMeta parses header fields and metadata blobs from a complete file
// without decoding any chunk data.
func ReadMeta(file []byte) (*Meta, error) {
	h, stored, err := parseFrame(file)
	if err != nil {
		return nil, err
	}
	body, err := decompressBody(h, stored)
	if err != nil {
		return nil, err
	}
	r := &reader{b: body}
	settings, userData, markers, border, stats, err := readMetaBlobs(r, h.flags)
	if err != nil {
		return nil, err
	}
	return &Meta{
		Kind: h.kind, Mode: h.mode, Flags: h.flags, BlockVersion: h.blockVersion,
		Settings: cloneBytes(settings), UserData: cloneBytes(userData),
		Markers: cloneBytes(markers), Border: cloneBytes(border), Stats: cloneBytes(stats),
	}, nil
}

// ReadWorld decodes a complete world file. The registry must be finalized;
// palette entries that cannot be resolved against it decode as
// minecraft:info_update. Records are parsed serially and applied in parallel.
func ReadWorld(file []byte, reg world.BlockRegistry) (*WorldData, error) {
	h, stored, err := parseFrame(file)
	if err != nil {
		return nil, err
	}
	if h.kind != KindWorld {
		return nil, corruptf("file kind %d is not a world", h.kind)
	}
	body, err := decompressBody(h, stored)
	if err != nil {
		return nil, err
	}

	r := &reader{b: body}
	d := &WorldData{Dimension: Dimension((h.flags & dimensionMask) >> dimensionShift)}
	var stats []byte
	if d.Settings, d.UserData, d.Markers, d.Border, stats, err = readMetaBlobs(r, h.flags); err != nil {
		return nil, err
	}
	_ = stats
	d.Settings = cloneBytes(d.Settings)
	d.UserData = cloneBytes(d.UserData)
	d.Markers = cloneBytes(d.Markers)
	d.Border = cloneBytes(d.Border)

	rids, unknown, unkStates, err := decodeBlockPalette(r, reg, h.blockVersion)
	if err != nil {
		return nil, err
	}
	biomeIDs, bioUnknown, bioNames, err := decodeBiomePalette(r)
	if err != nil {
		return nil, err
	}
	blobs, err := decodeBlobTable(r)
	if err != nil {
		return nil, err
	}

	// Absent biome sections take the file's default when it names one, and
	// otherwise the same fallback an unresolved name gets. Naming a numeric id
	// here instead would make the decode depend on the running game version.
	defaultBiome := fallbackBiomeID()
	defaultUnknown := int32(-1)
	haveDefault := h.flags&FlagDefaultBiome != 0
	if haveDefault {
		ref := h.flags >> defaultBiomeShift
		if int(ref) >= len(biomeIDs) {
			return nil, corruptf("default biome reference %d out of range", ref)
		}
		defaultBiome = biomeIDs[ref]
		if bioUnknown != nil {
			defaultUnknown = bioUnknown[ref]
		}
	}

	chunkN, err := r.count(maxChunks, "chunk")
	if err != nil {
		return nil, err
	}
	haveLight := h.flags&FlagStoreLight != 0

	// Parse phase (serial): record boundaries are implicit, so records must
	// be walked in order; parsing is cheap slicing. Capacity is bounded by
	// what the input could actually contain (a record is >= 8 bytes).
	raws := make([]recRaw, 0, min(chunkN, r.remaining()/8+1))
	used := make([]bool, len(blobs))
	src := tableBlobSource(blobs, used)
	var prevX, prevZ int64
	var prevKey uint64
	for range chunkN {
		dx, err := r.svarint()
		if err != nil {
			return nil, err
		}
		dz, err := r.svarint()
		if err != nil {
			return nil, err
		}
		// Accumulate in 64 bits and range-check: letting the sum wrap would
		// let two different byte sequences denote the same chunk position.
		sx, sz := prevX+dx, prevZ+dz
		if sx < math.MinInt32 || sx > math.MaxInt32 || sz < math.MinInt32 || sz > math.MaxInt32 {
			return nil, corruptf("chunk position (%d,%d) out of int32 range", sx, sz)
		}
		x, z := int32(sx), int32(sz)
		// Records are ordered by Morton key and positions are unique, so keys
		// strictly ascend. Enforcing it rejects both duplicate chunks (which
		// have no defined meaning: a reader would have to pick one) and
		// reordered records (which would be a second encoding of the same
		// world).
		if key := mortonKey(x, z); len(raws) > 0 && key <= prevKey {
			return nil, corruptf("chunk (%d,%d) is out of order or duplicated", x, z)
		} else {
			prevKey = key
		}
		rr, err := parseRecordBody(r, src, haveLight, x, z)
		if err != nil {
			return nil, err
		}
		prevX, prevZ = int64(x), int64(z)
		raws = append(raws, rr)
	}
	if r.remaining() != 0 {
		return nil, corruptf("%d trailing bytes after last chunk", r.remaining())
	}
	if haveLight {
		any := false
		for i := range raws {
			for _, b := range raws[i].lightPresence {
				if b != 0 {
					any = true
					break
				}
			}
		}
		if !any {
			// The flag says the records carry light. If none does, the file is
			// a second encoding of the same world with the flag clear, and
			// ContentHash covers the whole body, so the difference is real.
			return nil, corruptf("StoreLight is set but no section carries light")
		}
	}
	// A blob no record references is content the file carries and nothing
	// reads: dead weight that two writers could differ on while encoding the
	// same world.
	for i, u := range used {
		if !u {
			return nil, corruptf("blob %d is never referenced", i)
		}
	}

	// Apply phase (parallel): chunk construction and NBT decoding.
	air := reg.AirRuntimeID()
	cols := make([]Column, len(raws))
	errs := make([]error, len(raws))
	parallelFor(len(raws), 0, func(i int) {
		cols[i], errs[i] = applyRecord(&raws[i], reg, rids, biomeIDs, air, defaultBiome, haveDefault, defaultUnknown, unknown, unkStates, bioUnknown, bioNames)
	})
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	d.Columns = cols
	return d, nil
}

// blobSource resolves the next section blob of a record: solid mode reads a
// blob table reference, indexed mode parses inline blob bytes.
type blobSource func(r *reader) (decBlob, error)

// tableBlobSource returns a blobSource reading references into a blob table.
func tableBlobSource(blobs []decBlob, used []bool) blobSource {
	return func(r *reader) (decBlob, error) {
		ref, err := r.uvarint()
		if err != nil {
			return decBlob{}, err
		}
		if int(ref) >= len(blobs) {
			return decBlob{}, corruptf("section blob reference %d out of range", ref)
		}
		if used != nil {
			used[ref] = true
		}
		return blobs[ref], nil
	}
}

// recRaw is a parsed chunk record before the (parallelisable) apply phase.
// Byte slices alias the body buffer. Section data is stored compactly (one
// entry per present section, in ascending section order) with the presence
// bitsets retained, so hostile presence-free records cannot force large
// allocations.
type recRaw struct {
	x, z          int32
	minSection    int64
	sectionN      int
	presence      []byte      // block presence bitset (aliases body)
	bioPresence   []byte      // biome presence bitset (aliases body)
	lightPresence []byte      // light presence bitset (aliases body)
	layers        [][]decBlob // one per present block section
	biomes        []decBlob   // one per present biome section
	light         []lightPair // one per present block section (aliasing body)
	bes           []beRawEntry
	ents          [][]byte
	tick          int64
	ticks         []tickRawEntry
	userData      []byte
}

type beRawEntry struct {
	packedXZ uint8
	y        int64
	nbt      []byte
}

type tickRawEntry struct {
	packedXZ uint8
	y        int64
	ref      uint64
	at       int64
}

// parseRecordBody reads one chunk record without applying it.
func parseRecordBody(r *reader, src blobSource, haveLight bool, x, z int32) (recRaw, error) {
	rr := recRaw{x: x, z: z}
	var err error
	if rr.minSection, err = r.svarint(); err != nil {
		return rr, err
	}
	if rr.sectionN, err = r.count(maxSectionCnt, "section"); err != nil {
		return rr, err
	}
	if rr.sectionN == 0 {
		return rr, corruptf("chunk (%d,%d) has no sections", x, z)
	}
	// The span has to describe addressable blocks. sectionN alone does not
	// bound it: a one-section chunk based far outside the int16 block Y domain
	// is small and still unrepresentable, and its section index would be
	// silently narrowed everywhere it is used.
	if rr.minSection < minSectionIdx || rr.minSection > maxSectionIdx ||
		rr.minSection+int64(rr.sectionN) > maxSectionIdx+1 {
		return rr, corruptf("chunk (%d,%d) spans sections %d..%d, outside the addressable range %d..%d",
			x, z, rr.minSection, rr.minSection+int64(rr.sectionN)-1, minSectionIdx, maxSectionIdx)
	}

	var err2 error
	if rr.presence, err2 = r.take((rr.sectionN + 7) / 8); err2 != nil {
		return rr, err2
	}
	if err := bitsetTail(rr.presence, rr.sectionN); err != nil {
		return rr, err
	}
	for i := range rr.sectionN {
		if rr.presence[i/8]&(1<<(i%8)) == 0 {
			continue
		}
		layerN, err := r.count(maxLayers, "layer")
		if err != nil {
			return rr, err
		}
		if layerN == 0 {
			// A present section with no layers is a second encoding of an
			// absent section.
			return rr, corruptf("section %d is present but declares no layers", i)
		}
		layers := make([]decBlob, layerN)
		for l := range layerN {
			if layers[l], err = src(r); err != nil {
				return rr, err
			}
		}
		rr.layers = append(rr.layers, layers)
	}

	if rr.bioPresence, err2 = r.take((rr.sectionN + 7) / 8); err2 != nil {
		return rr, err2
	}
	if err := bitsetTail(rr.bioPresence, rr.sectionN); err != nil {
		return rr, err
	}
	for i := range rr.sectionN {
		if rr.bioPresence[i/8]&(1<<(i%8)) == 0 {
			continue
		}
		blob, err := src(r)
		if err != nil {
			return rr, err
		}
		rr.biomes = append(rr.biomes, blob)
	}

	if haveLight {
		if rr.lightPresence, err2 = r.take((rr.sectionN + 7) / 8); err2 != nil {
			return rr, err2
		}
		if err := bitsetTail(rr.lightPresence, rr.sectionN); err != nil {
			return rr, err
		}
		for i := range rr.sectionN {
			if rr.lightPresence[i/8]&(1<<(i%8)) == 0 {
				continue
			}
			flags, err := r.u8()
			if err != nil {
				return rr, err
			}
			if flags&^0x03 != 0 {
				return rr, corruptf("light flags %#x set reserved bits", flags)
			}
			if flags == 0 {
				return rr, corruptf("light entry for section %d carries no arrays", i)
			}
			var lp lightPair
			if flags&1 != 0 {
				if lp.block, err = r.take(lightArrayLen); err != nil {
					return rr, err
				}
			}
			if flags&2 != 0 {
				if lp.sky, err = r.take(lightArrayLen); err != nil {
					return rr, err
				}
			}
			rr.light = append(rr.light, lp)
		}
	}

	beN, err := r.count(maxPerChunk, "block entity")
	if err != nil {
		return rr, err
	}
	for range beN {
		packed, err := r.u8()
		if err != nil {
			return rr, err
		}
		y, err := r.svarint()
		if err != nil {
			return rr, err
		}
		blob, err := r.blob()
		if err != nil {
			return rr, err
		}
		rr.bes = append(rr.bes, beRawEntry{packedXZ: packed, y: y, nbt: blob})
	}

	entN, err := r.count(maxPerChunk, "entity")
	if err != nil {
		return rr, err
	}
	for range entN {
		blob, err := r.blob()
		if err != nil {
			return rr, err
		}
		rr.ents = append(rr.ents, blob)
	}

	if rr.tick, err = r.svarint(); err != nil {
		return rr, err
	}

	stN, err := r.count(maxPerChunk, "scheduled tick")
	if err != nil {
		return rr, err
	}
	for range stN {
		packed, err := r.u8()
		if err != nil {
			return rr, err
		}
		y, err := r.svarint()
		if err != nil {
			return rr, err
		}
		ref, err := r.uvarint()
		if err != nil {
			return rr, err
		}
		at, err := r.svarint()
		if err != nil {
			return rr, err
		}
		rr.ticks = append(rr.ticks, tickRawEntry{packedXZ: packed, y: y, ref: ref, at: at})
	}

	userData, err := r.blob()
	if err != nil {
		return rr, err
	}
	rr.userData = userData
	return rr, nil
}

// applyRecord turns a parsed record into a Column: chunk construction, blob
// application and NBT decoding. Safe to run in parallel across records.
func applyRecord(rr *recRaw, reg world.BlockRegistry, rids, biomeIDs []uint32, air, defaultBiome uint32, haveDefault bool, defaultUnknown int32, unknown []int32, unkStates []BlockState, bioUnknown []int32, bioNames []string) (Column, error) {
	x, z := rr.x, rr.z
	rng := cube.Range{int(rr.minSection) * 16, int(rr.minSection+int64(rr.sectionN))*16 - 1}
	ch := chunk.New(reg, rng)
	subs := ch.Sub()
	if len(subs) != rr.sectionN {
		return Column{}, corruptf("chunk (%d,%d): range yields %d sections, record has %d", x, z, len(subs), rr.sectionN)
	}

	col := Column{X: x, Z: z}
	blockCur := 0
	for i := range rr.sectionN {
		if rr.presence[i/8]&(1<<(i%8)) == 0 {
			continue
		}
		for l, blob := range rr.layers[blockCur] {
			var report func(idx uint16, state uint32)
			if unknown != nil {
				report = func(idx uint16, state uint32) {
					col.Unknown = append(col.Unknown, UnknownBlock{
						Section: int32(rr.minSection) + int32(i), Layer: uint8(l),
						Index: idx, State: state,
					})
				}
			}
			if err := applyBlockBlob(subs[i], uint8(l), blob, rids, air, unknown, report); err != nil {
				return Column{}, err
			}
		}
		blockCur++
	}
	lightCur := 0
	for i := range rr.sectionN {
		if rr.lightPresence == nil || rr.lightPresence[i/8]&(1<<(i%8)) == 0 {
			continue
		}
		lp := rr.light[lightCur]
		lightCur++
		subSetLight(subs[i], cloneBytes(lp.block), cloneBytes(lp.sky))
	}
	bioCur := 0
	for i := range rr.sectionN {
		if rr.bioPresence[i/8]&(1<<(i%8)) == 0 {
			fillBiome(ch, i, defaultBiome)
			// An elided section is uniformly the default biome, and the
			// default can itself be a name no registry resolves. Without a
			// sidecar entry the name survives in the file but not through a
			// read and rewrite, which would quietly rename it to the fallback.
			if defaultUnknown >= 0 {
				col.UnknownBiomes = append(col.UnknownBiomes, UnknownBlock{
					Section: int32(rr.minSection) + int32(i), Index: WholeStorage,
					State: uint32(defaultUnknown),
				})
			}
			continue
		}
		blob := rr.biomes[bioCur]
		if err := applyBiomeBlob(ch, i, blob, biomeIDs); err != nil {
			return Column{}, err
		}
		if bioUnknown != nil {
			sec := int32(rr.minSection) + int32(i)
			if blob.width == widthUniform {
				if st := bioUnknown[blob.refs[0]]; st >= 0 {
					col.UnknownBiomes = append(col.UnknownBiomes, UnknownBlock{
						Section: sec, Index: WholeStorage, State: uint32(st),
					})
				}
			} else {
				var idx [4096]uint16
				if err := blobIndices(blob, len(blob.refs), &idx); err == nil {
					for pos, li := range idx {
						if st := bioUnknown[blob.refs[li]]; st >= 0 {
							col.UnknownBiomes = append(col.UnknownBiomes, UnknownBlock{
								Section: sec, Index: uint16(pos), State: uint32(st),
							})
						}
					}
				}
			}
		}
		bioCur++
	}

	col.Col = &chunk.Column{Chunk: ch}
	seenBE := make(map[[2]int32]struct{}, len(rr.bes))
	for _, be := range rr.bes {
		// At most one block entity per position: two a reader cannot tell
		// apart leave their order decided by nothing, which is what §4.8's
		// totality argument rests on.
		k := [2]int32{int32(be.packedXZ), int32(be.y)}
		if _, dup := seenBE[k]; dup {
			return Column{}, corruptf("two block entities at one position in chunk (%d,%d)", x, z)
		}
		seenBE[k] = struct{}{}
		data, err := unmarshalNBT(be.nbt)
		if err != nil {
			return Column{}, err
		}
		pos := cube.Pos{int(x)*16 + int(be.packedXZ&0xF), int(be.y), int(z)*16 + int(be.packedXZ>>4)}
		data["x"], data["y"], data["z"] = int32(pos.X()), int32(pos.Y()), int32(pos.Z())
		col.Col.BlockEntities = append(col.Col.BlockEntities, chunk.BlockEntity{Pos: pos, Data: data})
	}
	for i, blob := range rr.ents {
		data, err := unmarshalNBT(blob)
		if err != nil {
			return Column{}, err
		}
		id, ok := data["UniqueID"].(int64)
		if !ok {
			// A conforming writer always stores UniqueID as a long, so this
			// is only reachable for foreign input. Zero is a legal value and
			// is kept verbatim: rewriting it would make encode, decode and
			// encode again produce different bytes. Only a missing or
			// wrongly typed field is replaced, with a value derived from the
			// chunk position and index so the same record always yields the
			// same ID.
			id = syntheticEntityID(x, z, i)
			data["UniqueID"] = id
		}
		col.Col.Entities = append(col.Col.Entities, chunk.Entity{ID: id, Data: data})
	}
	col.Col.Tick = rr.tick
	type tickKey struct {
		packedXZ uint8
		y        int64
		at       int64
		ref      uint64
	}
	seenTick := make(map[tickKey]struct{}, len(rr.ticks))
	for _, t := range rr.ticks {
		if int(t.ref) >= len(rids) {
			return Column{}, corruptf("scheduled tick block reference %d out of range", t.ref)
		}
		// Two updates identical in position, firing tick and block reference
		// are indistinguishable, so their order is decided by nothing.
		k := tickKey{packedXZ: t.packedXZ, y: t.y, at: t.at, ref: t.ref}
		if _, dup := seenTick[k]; dup {
			return Column{}, corruptf("duplicate scheduled update in chunk (%d,%d)", x, z)
		}
		seenTick[k] = struct{}{}
		pos := cube.Pos{int(x)*16 + int(t.packedXZ&0xF), int(t.y), int(z)*16 + int(t.packedXZ>>4)}
		if unknown != nil && unknown[t.ref] >= 0 {
			col.UnknownTicks = append(col.UnknownTicks, UnknownTick{
				Pos:   [3]int32{int32(pos.X()), int32(pos.Y()), int32(pos.Z())},
				At:    t.at,
				State: uint32(unknown[t.ref]),
			})
		}
		col.Col.ScheduledBlocks = append(col.Col.ScheduledBlocks, chunk.ScheduledBlockUpdate{
			Pos: pos, Block: rids[t.ref], Tick: t.at,
		})
	}
	col.UserData = cloneBytes(rr.userData)
	if len(col.Unknown) > 0 || len(col.UnknownTicks) > 0 {
		col.UnknownStates = unkStates
	}
	if len(col.UnknownBiomes) > 0 {
		col.UnknownBiomeNames = bioNames
	}
	return col, nil
}

// decodeRecordBody parses and applies a chunk record in one step (indexed
// mode's single-column path).
func decodeRecordBody(r *reader, reg world.BlockRegistry, rids, biomeIDs []uint32, air, defaultBiome uint32, haveDefault, haveLight bool, defaultUnknown int32, unknown []int32, unkStates []BlockState, bioUnknown []int32, bioNames []string, src blobSource, x, z int32) (Column, error) {
	rr, err := parseRecordBody(r, src, haveLight, x, z)
	if err != nil {
		return Column{}, err
	}
	return applyRecord(&rr, reg, rids, biomeIDs, air, defaultBiome, haveDefault, defaultUnknown, unknown, unkStates, bioUnknown, bioNames)
}

// blobIndices decodes a blob's 4096 local indices into out, validating them
// against the local palette size.
func blobIndices(blob decBlob, paletteLen int, out *[4096]uint16) error {
	n := uint16(paletteLen)
	switch blob.width {
	case widthU8:
		for i := range 4096 {
			li := uint16(blob.idx[i])
			if li >= n {
				return corruptf("section index %d out of palette range %d", li, n)
			}
			out[i] = li
		}
	case widthU16:
		for i := range 4096 {
			li := uint16(blob.idx[i*2]) | uint16(blob.idx[i*2+1])<<8
			if li >= n {
				return corruptf("section index %d out of palette range %d", li, n)
			}
			out[i] = li
		}
	}
	return nil
}

// applyBlockBlob constructs a storage from a decoded section blob and places
// it into a sub chunk layer directly (unsafe fast path). When unknown is
// non-nil, positions referencing unresolved palette entries are reported so
// callers can preserve the original states.
func applyBlockBlob(sub *chunk.SubChunk, layer uint8, blob decBlob, rids []uint32, air uint32, unknown []int32, report func(idx uint16, state uint32)) error {
	local := make([]uint32, len(blob.refs))
	localUnk := make([]int32, len(blob.refs))
	anyUnk := false
	for i, ref := range blob.refs {
		if int(ref) >= len(rids) {
			return corruptf("block palette reference %d out of range", ref)
		}
		local[i] = rids[ref]
		localUnk[i] = -1
		if unknown != nil && unknown[ref] >= 0 {
			localUnk[i] = unknown[ref]
			anyUnk = true
		}
	}
	if blob.width == widthUniform {
		// Record the preserved state before any air elision: with a registry
		// that resolves neither the state nor the placeholder block, the
		// entry would otherwise be dropped and the state lost on re-encode.
		if anyUnk && report != nil && localUnk[0] >= 0 {
			report(WholeStorage, uint32(localUnk[0]))
		}
		if local[0] == air && !anyUnk {
			// A uniform-air layer stays unrepresented, keeping the sub chunk
			// empty (the encoder never writes one; this is defensive).
			return nil
		}
		subSetLayer(sub, int(layer), makeStorage(local, nil))
		return nil
	}
	var idx [4096]uint16
	if err := blobIndices(blob, len(local), &idx); err != nil {
		return err
	}
	if anyUnk && report != nil {
		for i, li := range idx {
			if st := localUnk[li]; st >= 0 {
				report(uint16(i), uint32(st))
			}
		}
	}
	subSetLayer(sub, int(layer), makeStorage(local, &idx))
	return nil
}

// applyBiomeBlob constructs a biome storage from a decoded blob and installs
// it into one section of a chunk directly (unsafe fast path).
func applyBiomeBlob(ch *chunk.Chunk, secIdx int, blob decBlob, biomeIDs []uint32) error {
	local := make([]uint32, len(blob.refs))
	for i, ref := range blob.refs {
		if int(ref) >= len(biomeIDs) {
			return corruptf("biome palette reference %d out of range", ref)
		}
		local[i] = biomeIDs[ref]
	}
	if blob.width == widthUniform {
		chunkSetBiomes(ch, secIdx, makeStorage(local, nil))
		return nil
	}
	var idx [4096]uint16
	if err := blobIndices(blob, len(local), &idx); err != nil {
		return err
	}
	chunkSetBiomes(ch, secIdx, makeStorage(local, &idx))
	return nil
}

// fillBiome sets one section of a chunk to a uniform biome.
func fillBiome(ch *chunk.Chunk, secIdx int, id uint32) {
	chunkSetBiomes(ch, secIdx, makeStorage([]uint32{id}, nil))
}

// syntheticEntityID derives a stable non-zero entity ID for a record that
// carries none.
func syntheticEntityID(x, z int32, index int) int64 {
	h := xxhash.New()
	var b [16]byte
	binary.LittleEndian.PutUint32(b[0:4], uint32(x))
	binary.LittleEndian.PutUint32(b[4:8], uint32(z))
	binary.LittleEndian.PutUint64(b[8:16], uint64(index))
	_, _ = h.Write(b[:])
	id := int64(h.Sum64() &^ (1 << 63)) // keep it positive
	if id == 0 {
		id = 1
	}
	return id
}

// cloneBytes copies b, returning nil for empty input.
func cloneBytes(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
