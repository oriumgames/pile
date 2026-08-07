package format

import (
	"bytes"
	"cmp"
	"fmt"
	"io"
	"maps"
	"math"
	"slices"

	"github.com/df-mc/dragonfly/server/block/cube"

	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
)

// Column pairs a chunk column with its position and optional per-chunk
// application metadata.
type Column struct {
	X, Z     int32
	Col      *chunk.Column
	UserData []byte

	// Unknown, UnknownTicks and UnknownStates preserve block states that did
	// not resolve against the registry at decode time: the runtime sees
	// placeholder blocks, but re-encoding emits the original states wherever
	// the placeholder is still in place, so a load/save round trip is
	// lossless.
	Unknown       []UnknownBlock
	UnknownTicks  []UnknownTick
	UnknownStates []BlockState

	// UnknownBiomes and UnknownBiomeNames do the same for biome names the
	// runtime could not resolve (which decode as plains).
	UnknownBiomes     []UnknownBlock
	UnknownBiomeNames []string
}

// UnknownBlock records one preserved unknown-state occurrence in block data.
type UnknownBlock struct {
	// Section identifies the storage: for world columns it is the absolute
	// section Y (blockY >> 4), so the entry survives a change of the
	// dimension's vertical range; for structures it is the cell index.
	Section int32
	// Layer is the storage layer within the section.
	Layer uint8
	// Index is the section-local block index ((x<<8)|(z<<4)|y), or
	// WholeStorage for a uniform section.
	Index uint16
	// State indexes Column.UnknownStates.
	State uint32
}

// UnknownTick records a scheduled block update whose block state did not
// resolve, so it is not lost on re-encode. The update is identified by its
// position and firing tick rather than by slice index: dragonfly builds the
// scheduled-block slice in arbitrary order and range adaptation may drop
// entries, so an index would not survive a load/store cycle.
type UnknownTick struct {
	// Pos is the absolute block position of the update.
	Pos [3]int32
	// At is the absolute tick the update fires at.
	At int64
	// State indexes Column.UnknownStates.
	State uint32
}

// WholeStorage marks an UnknownBlock covering its whole storage.
const WholeStorage = 0xFFFF

// WorldData is the decoded content of a world file: metadata blobs plus all
// columns. Metadata blobs are little-endian NBT (or empty).
type WorldData struct {
	Settings []byte
	UserData []byte
	Markers  []byte
	Border   []byte
	Columns  []Column
}

// rawBlockSec is one extracted block storage before global palette
// resolution: local palette of runtime IDs plus 4096 indices (nil = uniform).
// states, when non-nil, marks palette entries that stand for preserved
// unknown states (-1 = a real runtime ID, >= 0 = index into the column's
// UnknownStates).
type rawBlockSec struct {
	rids   []uint32
	idx    []uint16
	states []int32
}

// rawBiomeSec is one extracted biome storage before global palette
// resolution. A nil names slice means the section holds no biome data.
type rawBiomeSec struct {
	names []string
	idx   []uint16
}

// rawTick is a scheduled tick before palette resolution. state >= 0 marks a
// preserved unknown state (index into the column's UnknownStates).
type rawTick struct {
	packedXZ uint8
	y        int32
	rid      uint32
	state    int32
	at       int64
}

// colRaw holds one column after the parallel extraction stage.
type colRaw struct {
	x, z       int32
	minSection int32
	sectionN   int
	blockSecs  [][]rawBlockSec
	biomeSecs  []rawBiomeSec
	light      []lightPair
	bes        []beIntermediate
	ents       [][]byte
	tick       int64
	ticks      []rawTick
	userData   []byte
	unkStates  []BlockState
	err        error
}

// colIntermediate holds one column after global palette resolution.
type colIntermediate struct {
	x, z       int32
	minSection int32
	sectionN   int
	blockSecs  [][]secData // per section; nil = absent, else one secData per layer
	biomeSecs  []secData   // per section; pal == nil = no data
	light      []lightPair // per section; nil slices when absent
	bes        []beIntermediate
	ents       [][]byte
	tick       int64
	ticks      []tickIntermediate
	userData   []byte
}

// lightPair holds one section's baked light nibble arrays (2048 bytes each,
// nil when unset).
type lightPair struct {
	block, sky []byte
}

// beSortable and entSortable pair an entry's ordering key with the exact
// bytes that will be written, so the sort and the output agree.
type beSortable struct {
	pos cube.Pos
	nbt []byte
}

type entSortable struct {
	id  int64
	nbt []byte
}

type beIntermediate struct {
	packedXZ uint8
	y        int32
	nbt      []byte
}

type tickIntermediate struct {
	packedXZ   uint8
	y          int32
	blockBuild uint32
	at         int64
}

// colBlobs holds the canonical section blob bytes of one column, computed in
// parallel after palette finalization. A nil biome entry means the section is
// absent (no data, or uniformly the world default).
type colBlobs struct {
	block [][][]byte // per section, per layer
	biome [][]byte   // per section; nil = absent
}

// WriteWorld encodes a world to out in solid mode. Chunks are read-only
// inputs: extraction canonicalizes (used-only palettes, air-only layers
// dropped) without mutating them. Output is deterministic unless
// Options.FastCompression is set: identical content, registry and options
// produce identical bytes.
func WriteWorld(out io.Writer, d *WorldData, reg world.BlockRegistry, opts Options) error {
	if err := validateWorldData(d); err != nil {
		return err
	}
	cols := slices.Clone(d.Columns)
	slices.SortFunc(cols, func(a, b Column) int {
		ka, kb := mortonKey(a.X, a.Z), mortonKey(b.X, b.Z)
		switch {
		case ka < kb:
			return -1
		case ka > kb:
			return 1
		default:
			return 0
		}
	})

	placeholder := placeholderRid(reg)

	// Stage A (parallel): heavy per-column extraction, no shared state.
	raws := make([]colRaw, len(cols))
	parallelFor(len(cols), opts.Workers, func(i int) {
		raws[i] = extractColumnRaw(cols[i], opts.SkipBiomes, opts.StoreLight, placeholder)
	})
	// StoreLight claims the records carry light. If none does, setting it
	// anyway is a second encoding of the same world with the flag clear, and
	// the content hash covers the whole body, so the difference is real.
	storeLight := opts.StoreLight
	if storeLight {
		storeLight = false
		for i := range raws {
			for _, lp := range raws[i].light {
				if len(lp.block) > 0 || len(lp.sky) > 0 {
					storeLight = true
					break
				}
			}
		}
	}
	for i := range raws {
		if raws[i].err != nil {
			return fmt.Errorf("pile: encode chunk (%d,%d): %w", raws[i].x, raws[i].z, raws[i].err)
		}
	}

	// Stage B (serial): global palette resolution in deterministic order.
	blockPal := newBlockPaletteBuilder(reg)
	biomePal := newBiomePaletteBuilder()
	uniformBiomes := make(map[uint32]uint64)
	addBiome := func(name string) uint32 { return biomePal.add(name, 1) }
	inter := make([]colIntermediate, len(raws))
	for i := range raws {
		inter[i] = resolveColumn(&raws[i], blockPal.add, addBiome, blockPal.addState, blockPal.uncount, biomePal.uncount)
		for _, sd := range inter[i].biomeSecs {
			if build, ok := sd.uniform(); ok {
				uniformBiomes[build]++
			}
		}
	}

	blockPalBytes, blockRemap, err := blockPal.finalize()
	if err != nil {
		return err
	}
	biomePalBytes, biomeRemap, err := biomePal.finalize()
	if err != nil {
		return err
	}

	// Pick the default biome: the build index with the most uniform sections.
	defaultRef, haveDefault := uint32(0), false
	{
		var best uint64
		var bestBuild uint32
		for build, n := range uniformBiomes {
			final := biomeRemap[build]
			if n > best || (n == best && haveDefault && final < defaultRef) {
				best, bestBuild, haveDefault = n, build, true
				defaultRef = biomeRemap[bestBuild]
			}
		}
		if haveDefault && defaultRef > 0xFFFF {
			haveDefault = false // cannot express in flags; store all sections
		}
	}

	// Stage C (parallel): canonical section blob bytes per column.
	blobs := make([]colBlobs, len(inter))
	parallelFor(len(inter), opts.Workers, func(i int) {
		blobs[i] = buildColBlobs(&inter[i], blockRemap, biomeRemap, defaultRef, haveDefault)
	})

	// Stage D (serial): dedup into the blob table, write records.
	table := newBlobTable()
	records := &writer{b: make([]byte, 0, 64<<10)}
	records.uvarint(uint64(len(inter)))
	var prevX, prevZ int32
	for i := range inter {
		records.svarint(int64(inter[i].x) - int64(prevX))
		records.svarint(int64(inter[i].z) - int64(prevZ))
		encodeRecordBody(records, &inter[i], &blobs[i], blockRemap, storeLight, func(w *writer, blob []byte) {
			w.uvarint(uint64(table.add(blob)))
		})
		prevX, prevZ = inter[i].x, inter[i].z
	}

	// Assemble the body: meta, palettes, blob table, records.
	body := &writer{b: make([]byte, 0, 128<<10)}
	body.blob(d.Settings)
	body.blob(d.UserData)
	body.blob(d.Markers)
	body.blob(d.Border)
	if opts.Stats {
		var filled int
		for i := range inter {
			for _, secs := range inter[i].blockSecs {
				if secs != nil {
					filled++
				}
			}
		}
		// Counters are longs: the format's ceilings exceed int32, so an int
		// tag could not represent a legal world.
		stats, err := marshalNBT(map[string]any{
			"chunks":         int64(len(inter)),
			"filledSections": int64(filled),
			"uniqueBlobs":    int64(len(table.blobs)),
			"blockStates":    int64(len(blockRemap)),
			"biomes":         int64(len(biomeRemap)),
		})
		if err != nil {
			return fmt.Errorf("pile: encode stats: %w", err)
		}
		if err := checkStatsBlob(stats); err != nil {
			return err
		}
		body.blob(stats)
	}
	body.raw(blockPalBytes)
	body.raw(biomePalBytes)
	table.encode(body)
	body.raw(records.bytes())

	flags := uint32(0)
	if haveDefault {
		flags |= FlagDefaultBiome | defaultRef<<defaultBiomeShift
	}
	if storeLight {
		flags |= FlagStoreLight
	}
	if opts.Stats {
		flags |= FlagStats
	}

	// The decoder caps a decompressed body at maxDecodedBody, so a writer that
	// sails past it produces a file it cannot read back.
	if n := body.len(); n > maxDecodedBody {
		return fmt.Errorf("pile: body is %d bytes, limit %d", n, maxDecodedBody)
	}
	stored := body.bytes()
	if opts.Compression == CompressionNone {
		flags |= FlagUncompressed
	} else {
		stored = compressBody(stored, opts.Compression, opts.FastCompression)
	}

	// Header.
	hdr := &writer{b: make([]byte, 0, headerSize)}
	hdr.raw(headerMagic[:])
	hdr.u16(Version)
	hdr.u8(KindWorld)
	hdr.u8(ModeSolid)
	hdr.u32(flags)
	hdr.i32(chunk.CurrentBlockVersion)

	// Footer. The hash authenticates the header and the footer's own control
	// words as well as the payload (see checkpointHash).
	tail := &writer{b: make([]byte, 0, footerSize-8)}
	tail.u64(0) // directory offset (indexed mode only)
	tail.u64(0) // directory length
	tail.u64(0) // generation
	tail.u64(0) // previous footer offset (indexed mode only)
	tail.raw(footerMagic[:])
	ftr := &writer{b: make([]byte, 0, footerSize)}
	ftr.u64(checkpointHash(hdr.bytes(), stored, tail.bytes()))
	ftr.raw(tail.bytes())

	for _, part := range [][]byte{hdr.bytes(), stored, ftr.bytes()} {
		if _, err := out.Write(part); err != nil {
			return fmt.Errorf("pile: write: %w", err)
		}
	}
	return nil
}

// validateWorldData rejects content that would encode into a file the
// decoder refuses to read back (oversized blobs or counts).
func validateWorldData(d *WorldData) error {
	if d == nil {
		return fmt.Errorf("pile: nil world data")
	}
	for _, b := range []struct {
		p    []byte
		what string
		nbt  bool
	}{
		{d.Settings, "world settings blob", true},
		{d.UserData, "world user data", false},
		{d.Markers, "markers blob", true},
		{d.Border, "border blob", true},
	} {
		if err := checkBlob(b.p, b.what); err != nil {
			return err
		}
		// The format designates these blobs as NBT, so they must satisfy the
		// canonical NBT rules: a writer that emits a malformed or
		// non-canonical one produces a file conforming readers must reject.
		// User data stays opaque.
		if b.nbt && len(b.p) > 0 {
			if err := validateNBT(b.p); err != nil {
				return fmt.Errorf("pile: %s: %w", b.what, err)
			}
		}
	}
	if err := checkMetaSchemas(d.Settings, d.Markers, d.Border); err != nil {
		return err
	}
	// The reader bounds how many section storages a file decodes into; the
	// writer is bounded by the same number, so it cannot produce a file it
	// would then refuse to read.
	storages := 0
	for _, c := range d.Columns {
		if c.Col == nil || c.Col.Chunk == nil {
			continue
		}
		for _, sub := range c.Col.Chunk.Sub() {
			storages += len(sub.Layers())
		}
	}
	if storages > maxDecodedStorages {
		return fmt.Errorf("pile: world holds %d section storages, limit %d", storages, maxDecodedStorages)
	}
	if len(d.Columns) > maxChunks {
		return fmt.Errorf("pile: %d chunks exceeds limit %d", len(d.Columns), maxChunks)
	}
	// Chunk positions are unique. Two columns at one position have equal
	// Morton keys, so their record order would be decided by the caller's
	// slice order rather than by the content, and a reader has no defined way
	// to choose between them.
	seen := make(map[[2]int32]struct{}, len(d.Columns))
	for _, c := range d.Columns {
		if _, dup := seen[[2]int32{c.X, c.Z}]; dup {
			return fmt.Errorf("pile: duplicate chunk (%d,%d)", c.X, c.Z)
		}
		seen[[2]int32{c.X, c.Z}] = struct{}{}
		if err := validateColumn(c); err != nil {
			return err
		}
	}
	return nil
}

// checkMetaSchemas applies every §7 schema. The tag of each specified field is
// fixed, and a dynamically typed decoder cannot tell afterwards which one a
// value came from, so the blobs have to be right on the way in.
func checkMetaSchemas(settings, markers, border []byte) error {
	if err := checkSettingsBlob(settings); err != nil {
		return err
	}
	if err := checkMarkersBlob(markers); err != nil {
		return err
	}
	return checkBorderBlob(border)
}

// settingsSchema fixes the tag of every field §7.1 names. Unlisted keys are
// preserved verbatim and unconstrained, but a listed one carrying the wrong
// tag makes the same settings expressible two ways.
var settingsSchema = map[string]string{
	"name": "string", "time": "int64", "timeCycle": "uint8",
	"spawnX": "int32", "spawnY": "int32", "spawnZ": "int32",
	"rainTime": "int64", "raining": "uint8",
	"thunderTime": "int64", "thundering": "uint8", "weatherCycle": "uint8",
	"requiredSleepTicks": "int64", "currentTick": "int64",
	"defaultGameMode": "int32", "difficulty": "int32", "tickRange": "int32",
}

// checkSettingsBlob enforces the settings schema of §7.1.
func checkSettingsBlob(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	m, err := unmarshalNBT(b)
	if err != nil {
		return fmt.Errorf("pile: settings blob: %w", err)
	}
	for k, want := range settingsSchema {
		v, ok := m[k]
		if !ok {
			continue
		}
		if got := fmt.Sprintf("%T", v); got != want {
			return fmt.Errorf("pile: settings blob: %q is %s, want %s", k, got, want)
		}
	}
	return nil
}

// statsSchema fixes the tag of every field §4.2 names. As in §7, presence is a
// convention and spelling is a rule: a summary missing a counter is valid, one
// carrying a counter as an int is a second encoding of the same number.
var statsSchema = map[string]string{
	"chunks": "int64", "filledSections": "int64", "uniqueBlobs": "int64",
	"blockStates": "int64", "biomes": "int64",
}

// checkStatsBlob enforces the stats schema of §4.2.
func checkStatsBlob(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	m, err := unmarshalNBT(b)
	if err != nil {
		return fmt.Errorf("pile: stats blob: %w", err)
	}
	for k, want := range statsSchema {
		v, ok := m[k]
		if !ok {
			continue
		}
		if got := fmt.Sprintf("%T", v); got != want {
			return fmt.Errorf("pile: stats blob: %q is %s, want %s", k, got, want)
		}
	}
	return nil
}

// checkMarkersBlob enforces the marker schema of §7.2, including the list
// order. Two blobs listing the same markers in different orders would
// otherwise both be accepted and copied through verbatim.
func checkMarkersBlob(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	m, err := unmarshalNBT(b)
	if err != nil {
		return fmt.Errorf("pile: markers blob: %w", err)
	}
	raw, ok := m["markers"]
	if !ok {
		return nil // no markers is not a malformed marker list
	}
	list, err := compoundList(raw)
	if err != nil {
		return fmt.Errorf("pile: markers blob: %w", err)
	}
	prev := ""
	for i, mk := range list {
		name, ok := mk["name"].(string)
		if !ok {
			return fmt.Errorf("pile: markers blob: marker %d has no string name", i)
		}
		if _, ok := mk["kind"].(string); !ok {
			return fmt.Errorf("pile: markers blob: marker %q has no string kind", name)
		}
		_, hasPos := mk["pos"]
		if hasPos {
			if _, err := finiteTriple(mk["pos"]); err != nil {
				return fmt.Errorf("pile: markers blob: marker %q pos: %w", name, err)
			}
		}
		// Bounds make the marker an area (§7.4). Both or neither: a marker
		// carrying one corner describes nothing, and which corner it is would
		// have no answer.
		rawMin, hasMin := mk["min"]
		rawMax, hasMax := mk["max"]
		if hasMin != hasMax {
			return fmt.Errorf("pile: markers blob: marker %q carries one of min/max, want both or neither", name)
		}
		if hasMin {
			lo, err := finiteTriple(rawMin)
			if err != nil {
				return fmt.Errorf("pile: markers blob: marker %q min: %w", name, err)
			}
			hi, err := finiteTriple(rawMax)
			if err != nil {
				return fmt.Errorf("pile: markers blob: marker %q max: %w", name, err)
			}
			for axis := range lo {
				if lo[axis] > hi[axis] {
					// Refused rather than normalised by swapping: swapping
					// would give one area two encodings, and a reader that
					// repaired the file would disagree with one that did not.
					return fmt.Errorf("pile: markers blob: marker %q min[%d] %v exceeds max[%d] %v",
						name, axis, lo[axis], axis, hi[axis])
				}
			}
		}
		if !hasPos && !hasMin {
			return fmt.Errorf("pile: markers blob: marker %q has neither pos nor min/max, so it marks nothing", name)
		}
		if i > 0 && name <= prev {
			return fmt.Errorf("pile: markers blob: marker %q follows %q, want ascending unique names", name, prev)
		}
		prev = name
	}
	return nil
}

// finiteTriple is doubleTriple plus §7.4's rules on the doubles themselves.
//
// A double admits values that are equal but not identical, and a format whose
// whole doctrine is one content one encoding cannot carry them. NaN has many
// bit patterns and makes every comparison false, so an inverted-box check
// would silently pass over one. Negative zero is a second spelling of zero.
// Infinities describe a bound no reader can act on.
//
// These apply to pos as well, which carried none of them until areas arrived
// and the question had to be answered: a marker at -0.0 and one at +0.0 are
// the same point stored as different bytes, and that has been true since
// markers existed.
func finiteTriple(v any) ([3]float64, error) {
	out, err := doubleTriple(v)
	if err != nil {
		return out, err
	}
	for i, f := range out {
		switch {
		case math.IsNaN(f):
			return out, fmt.Errorf("element %d is NaN", i)
		case math.IsInf(f, 0):
			return out, fmt.Errorf("element %d is infinite", i)
		case f == 0 && math.Signbit(f):
			return out, fmt.Errorf("element %d is negative zero, which is a second spelling of zero", i)
		}
	}
	return out, nil
}

// compoundList normalises a decoded NBT list of compounds, which arrives as a
// typed slice or a slice of any depending on how it was built.
func compoundList(v any) ([]map[string]any, error) {
	switch l := v.(type) {
	case []map[string]any:
		return l, nil
	case []any:
		out := make([]map[string]any, len(l))
		for i, e := range l {
			c, ok := e.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("element %d is %T, want a compound", i, e)
			}
			out[i] = c
		}
		return out, nil
	}
	return nil, fmt.Errorf("value is %T, want a list of compounds", v)
}

// doubleTriple normalises a decoded list of exactly three doubles.
func doubleTriple(v any) ([3]float64, error) {
	var out [3]float64
	switch l := v.(type) {
	case []float64:
		if len(l) != 3 {
			return out, fmt.Errorf("has %d elements, want 3", len(l))
		}
		copy(out[:], l)
		return out, nil
	case []any:
		if len(l) != 3 {
			return out, fmt.Errorf("has %d elements, want 3", len(l))
		}
		for i, e := range l {
			d, ok := e.(float64)
			if !ok {
				return out, fmt.Errorf("element %d is %T, want a double", i, e)
			}
			out[i] = d
		}
		return out, nil
	}
	return out, fmt.Errorf("is %T, want a list of three doubles", v)
}

// checkBorderBlob enforces the border schema of the specification: min and max
// are two-element int arrays, not lists. Structural NBT validation cannot catch
// this, and a decoder into a dynamically typed map cannot tell the two apart
// after the fact, so the tags have to be right on the way in.
func checkBorderBlob(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	m, err := unmarshalNBT(b)
	if err != nil {
		return fmt.Errorf("pile: border blob: %w", err)
	}
	// Presence is a convention and spelling is a rule (§7): a field that is
	// absent is fine, one that is present has to carry the stated tag.
	for _, k := range []string{"min", "max"} {
		v, ok := m[k]
		if !ok {
			continue
		}
		if _, ok := v.([2]int32); !ok {
			return fmt.Errorf("pile: border blob: %q is %T, want a two-element int array", k, v)
		}
	}
	return nil
}

// compareNBT orders two compounds by their canonical encodings, giving a
// total order for entries that are otherwise equal.
func compareNBT(a, b map[string]any) int {
	ea, erra := marshalNBT(a)
	eb, errb := marshalNBT(b)
	if erra != nil || errb != nil {
		return 0
	}
	return bytes.Compare(ea, eb)
}

// comparePos orders block positions canonically by y, then z, then x.
func comparePos(a, b cube.Pos) int {
	if v := cmp.Compare(a.Y(), b.Y()); v != 0 {
		return v
	}
	if v := cmp.Compare(a.Z(), b.Z()); v != 0 {
		return v
	}
	return cmp.Compare(a.X(), b.X())
}

// AlignedRange reports whether a vertical range is representable: records
// store a section index and a section count, so the range must start on a
// 16-block boundary and span whole sections. Every Bedrock dimension does;
// a custom one that does not must be rejected rather than silently re-based.
func AlignedRange(r cube.Range) bool {
	return r[0]%16 == 0 && (r[1]-r[0]+1)%16 == 0 && r[1] > r[0]
}

// validateColumn rejects a column that would encode into a record the
// decoder refuses to read back.
func validateColumn(c Column) error {
	if err := checkBlob(c.UserData, fmt.Sprintf("chunk (%d,%d) user data", c.X, c.Z)); err != nil {
		return err
	}
	if c.Col == nil || c.Col.Chunk == nil {
		return fmt.Errorf("pile: chunk (%d,%d) has no chunk data", c.X, c.Z)
	}
	r := c.Col.Chunk.Range()
	if !AlignedRange(r) {
		return fmt.Errorf("pile: chunk (%d,%d) has vertical range %v, which is not 16-block aligned and cannot be stored exactly", c.X, c.Z, r)
	}
	// Block Y is an int16 throughout dragonfly's chunk API and the section
	// index is stored as an int32, so a range outside that is not
	// representable. Alignment alone would let one through to be silently
	// narrowed into a completely different range.
	if r[0] < math.MinInt16 || r[1] > math.MaxInt16 {
		return fmt.Errorf("pile: chunk (%d,%d) has vertical range %v, which is outside the representable block Y domain", c.X, c.Z, r)
	}
	if n := len(c.Col.Entities); n > maxPerChunk {
		return fmt.Errorf("pile: chunk (%d,%d) has %d entities, limit %d", c.X, c.Z, n, maxPerChunk)
	}
	if n := len(c.Col.BlockEntities); n > maxPerChunk {
		return fmt.Errorf("pile: chunk (%d,%d) has %d block entities, limit %d", c.X, c.Z, n, maxPerChunk)
	}
	if n := len(c.Col.ScheduledBlocks); n > maxPerChunk {
		return fmt.Errorf("pile: chunk (%d,%d) has %d scheduled ticks, limit %d", c.X, c.Z, n, maxPerChunk)
	}
	// Every position a record carries has to lie inside the column it is stored
	// in, and neither half of that was checked. A record stores a block entity's
	// x and z as one packed nibble pair, so a position outside the column's
	// 16x16 footprint is silently folded back into it: (100,0,200) in chunk
	// (0,0) was written, and read back, as (4,0,8). Two positions 16 apart fold
	// onto one, and the reader then refuses the file for repeating a position —
	// so the same gap produced both a wrong answer reported as success and a
	// file this writer's own reader rejects. The Y is not folded but is not
	// bounded either, and the reader requires it to lie inside the span the
	// record declares.
	//
	// Computed in int64: X reaches MaxInt32 and 16*MaxInt32 does not fit an
	// int32, nor an int on a 32-bit build.
	loX, loZ := int64(c.X)*16, int64(c.Z)*16
	inColumn := func(p cube.Pos, what string) error {
		if x := int64(p.X()); x < loX || x > loX+15 {
			return fmt.Errorf("pile: chunk (%d,%d) has a %s at %v, whose X is outside the column's span %d..%d",
				c.X, c.Z, what, p, loX, loX+15)
		}
		if z := int64(p.Z()); z < loZ || z > loZ+15 {
			return fmt.Errorf("pile: chunk (%d,%d) has a %s at %v, whose Z is outside the column's span %d..%d",
				c.X, c.Z, what, p, loZ, loZ+15)
		}
		if y := p.Y(); y < r[0] || y > r[1] {
			return fmt.Errorf("pile: chunk (%d,%d) has a %s at %v, whose Y is outside the chunk's range %d..%d",
				c.X, c.Z, what, p, r[0], r[1])
		}
		return nil
	}
	// The collection orders are total only because their keys are unique, so
	// uniqueness is a rule and not an assumption. The reader rejects these, and
	// a writer that emits them produces a file it cannot read back.
	bePos := make(map[cube.Pos]struct{}, len(c.Col.BlockEntities))
	for _, be := range c.Col.BlockEntities {
		if err := inColumn(be.Pos, "block entity"); err != nil {
			return err
		}
		if _, dup := bePos[be.Pos]; dup {
			return fmt.Errorf("pile: chunk (%d,%d) has two block entities at %v", c.X, c.Z, be.Pos)
		}
		bePos[be.Pos] = struct{}{}
	}
	type tickID struct {
		pos   cube.Pos
		tick  int64
		block uint32
	}
	ticks := make(map[tickID]struct{}, len(c.Col.ScheduledBlocks))
	for _, t := range c.Col.ScheduledBlocks {
		if err := inColumn(t.Pos, "scheduled update"); err != nil {
			return err
		}
		k := tickID{pos: t.Pos, tick: t.Tick, block: t.Block}
		if _, dup := ticks[k]; dup {
			return fmt.Errorf("pile: chunk (%d,%d) has a duplicate scheduled update at %v", c.X, c.Z, t.Pos)
		}
		ticks[k] = struct{}{}
	}
	return nil
}

// projectCollections marshals a column's block entities and entities exactly
// as they will be written, then puts them in canonical order.
//
// Dragonfly builds these slices by ranging maps, so their order varies between
// saves of identical content. Determinism is a format guarantee, not an
// accident of the caller's iteration, so the order is imposed here.
//
// Ties break on the bytes that will actually be written, which is why each
// entry is projected and marshalled before the sort rather than after.
// Ordering on the caller's map instead would let discarded values decide the
// file: a block entity's x/y/z are stripped on encode and an entity's UniqueID
// is overwritten with its authoritative ID, yet both are still present in the
// data the caller handed over.
func projectCollections(c Column) ([]beSortable, []entSortable, error) {
	bes := make([]beSortable, 0, len(c.Col.BlockEntities))
	for _, be := range c.Col.BlockEntities {
		data := make(map[string]any, len(be.Data))
		for k, v := range be.Data {
			if k == "x" || k == "y" || k == "z" {
				continue // reinjected from the stored position on decode
			}
			data[k] = v
		}
		blob, err := marshalNBT(data)
		if err != nil {
			return nil, nil, fmt.Errorf("block entity at %v: %w", be.Pos, err)
		}
		if err := checkBlob(blob, fmt.Sprintf("block entity NBT at %v", be.Pos)); err != nil {
			return nil, nil, err
		}
		bes = append(bes, beSortable{pos: be.Pos, nbt: blob})
	}
	slices.SortFunc(bes, func(a, b beSortable) int {
		if v := comparePos(a.pos, b.pos); v != 0 {
			return v
		}
		return bytes.Compare(a.nbt, b.nbt)
	})

	ents := make([]entSortable, 0, len(c.Col.Entities))
	for _, e := range c.Col.Entities {
		data := make(map[string]any, len(e.Data)+1)
		maps.Copy(data, e.Data)
		data["UniqueID"] = e.ID
		blob, err := marshalNBT(data)
		if err != nil {
			return nil, nil, fmt.Errorf("entity %d: %w", e.ID, err)
		}
		if err := checkBlob(blob, fmt.Sprintf("entity %d NBT", e.ID)); err != nil {
			return nil, nil, err
		}
		ents = append(ents, entSortable{id: e.ID, nbt: blob})
	}
	slices.SortFunc(ents, func(a, b entSortable) int {
		if v := cmp.Compare(a.id, b.id); v != 0 {
			return v
		}
		return bytes.Compare(a.nbt, b.nbt)
	})
	return bes, ents, nil
}

// extractColumnRaw converts a column into raw form without touching shared
// state or mutating the chunk (safe to run in parallel and on columns shared
// with concurrent readers). Canonicalization that dragonfly's Compact would
// perform happens during extraction instead: palettes are reduced to used
// entries and air-only layers are dropped.
func extractColumnRaw(c Column, skipBiomes, storeLight bool, placeholder uint32) colRaw {
	ch := c.Col.Chunk
	air := chunkAir(ch)

	bes, ents, projErr := projectCollections(c)
	// Scheduled updates are ordered by position and firing tick here; ties
	// are broken later by the final palette reference (see sortTicks), which
	// is registry-independent. Sorting by runtime ID at this point would make
	// the bytes depend on how a registry happened to number its states.
	//
	// This sort has no negative control and cannot have one, which HARNESS.md
	// records under §6.2 so it is not mistaken for a coverage hole. sortTicks
	// imposes a total order on (y, x, z, tick, final palette reference)
	// afterwards, and two updates that tie on all five encode as identical
	// bytes, so no input distinguishes this order from any other. It is kept
	// because a stable sort wants a deterministic base, not because the wire
	// can tell.
	ticks := slices.Clone(c.Col.ScheduledBlocks)
	slices.SortStableFunc(ticks, func(a, b chunk.ScheduledBlockUpdate) int {
		if v := comparePos(a.Pos, b.Pos); v != 0 {
			return v
		}
		return cmp.Compare(a.Tick, b.Tick)
	})

	// Preserved unknown states, grouped by section and layer.
	var unknownBySec map[secLayer][]UnknownBlock
	if len(c.Unknown) > 0 {
		unknownBySec = make(map[secLayer][]UnknownBlock)
		for _, u := range c.Unknown {
			// Sidecar sections are absolute; entries outside this chunk's
			// range (after a range change) are dropped.
			k := secLayer{sec: u.Section, layer: u.Layer}
			unknownBySec[k] = append(unknownBySec[k], u)
		}
	}
	sidecarN := sidecarLayerCounts(unknownBySec)

	r := ch.Range()
	subs := ch.Sub()
	cr := colRaw{
		x: c.X, z: c.Z,
		minSection: int32(r[0] >> 4),
		sectionN:   len(subs),
		blockSecs:  make([][]rawBlockSec, len(subs)),
		biomeSecs:  make([]rawBiomeSec, len(subs)),
		tick:       c.Col.Tick,
		userData:   c.UserData,
	}
	if projErr != nil {
		cr.err = projErr
		return cr
	}
	if cr.sectionN > maxSectionCnt {
		cr.err = fmt.Errorf("chunk has %d sections (limit %d)", cr.sectionN, maxSectionCnt)
		return cr
	}

	for i, sub := range subs {
		layers := sub.Layers()
		sec := int32(r[0]>>4) + int32(i)
		// A preserved state can name a layer the runtime never allocated: on a
		// registry whose placeholder resolves to air, a layer holding only
		// unresolved blocks has no storage, and neither does the section
		// beneath it. Walking only the allocated layers would drop exactly
		// those entries, so the walk covers whatever the sidecar reaches.
		n := max(len(layers), sidecarN[sec])
		if n == 0 {
			continue
		}
		if n > maxLayers {
			cr.err = fmt.Errorf("section %d has %d layers (limit %d)", i, n, maxLayers)
			return cr
		}
		secs := make([]rawBlockSec, 0, n)
		for l := range n {
			rs := rawBlockSec{rids: []uint32{air}}
			if l < len(layers) {
				rs = extractBlockRaw(layers[l])
			}
			// Inject preserved states first: with a registry where the
			// placeholder resolves to air, an air-only test before injection
			// would discard the layer and lose them.
			if entries := unknownBySec[secLayer{sec: sec, layer: uint8(l)}]; len(entries) > 0 {
				injectUnknown(&rs, entries, placeholder, len(c.UnknownStates))
			}
			secs = append(secs, rs)
		}
		// Layer numbers are semantic: layer 1 is the waterlogging layer, so an
		// all-air layer 0 under a populated layer 1 is meaningful state.
		// Only trailing all-air layers may be dropped, because a layer past
		// the last stored one already reads as air.
		for len(secs) > 0 && airOnlyLayer(secs[len(secs)-1], air) {
			secs = secs[:len(secs)-1]
		}
		if len(secs) == 0 {
			continue
		}
		cr.blockSecs[i] = secs
	}
	cr.unkStates = c.UnknownStates

	if !skipBiomes {
		// Preserved biome names, keyed by absolute section and index.
		bioUnknown := make(map[bioKey]uint32, len(c.UnknownBiomes))
		bioUniform := make(map[int32]uint32, len(c.UnknownBiomes))
		for _, u := range c.UnknownBiomes {
			if u.Index == WholeStorage {
				bioUniform[u.Section] = u.State
				continue
			}
			bioUnknown[bioKey{sec: u.Section, idx: u.Index}] = u.State
		}
		plains := plainsBiomeName()
		name := func(state uint32) (string, bool) {
			// A sidecar entry naming a state the column does not carry is
			// malformed input, not a reason to panic.
			if int(state) >= len(c.UnknownBiomeNames) {
				return "", false
			}
			return c.UnknownBiomeNames[state], true
		}
		for i := range subs {
			rs := extractBiomeRaw(ch, i)
			sec := int32(r[0]>>4) + int32(i)
			if len(c.UnknownBiomes) > 0 {
				// Re-emit the original name wherever the fallback biome is
				// still in place.
				if state, ok := bioUniform[sec]; ok && len(rs.names) == 1 && rs.names[0] == plains {
					n, ok := name(state)
					if !ok {
						cr.err = fmt.Errorf("unknown biome state %d in section %d has no name", state, sec)
						return cr
					}
					rs.names[0] = n
				} else if len(bioUnknown) > 0 {
					var err error
					rs, err = splitUnknownBiomes(rs, sec, plains, bioUnknown, name)
					if err != nil {
						cr.err = err
						return cr
					}
				}
			}
			cr.biomeSecs[i] = rs
		}
	}
	if storeLight {
		// Light is collected per section independently of block presence:
		// dragonfly's lighting pass fills air-only sections with full sky
		// light, so tying light to block presence would silently drop it.
		cr.light = make([]lightPair, len(subs))
		for i, sub := range subs {
			bl, sk := subLight(sub)
			if len(bl) != lightArrayLen && len(sk) != lightArrayLen {
				continue
			}
			cr.light[i] = lightPair{block: cloneBytes(bl), sky: cloneBytes(sk)}
		}
	}
	for _, be := range bes {
		cr.bes = append(cr.bes, beIntermediate{
			packedXZ: uint8(be.pos.X()&0xF) | uint8(be.pos.Z()&0xF)<<4,
			y:        int32(be.pos.Y()),
			nbt:      be.nbt,
		})
	}
	for _, e := range ents {
		cr.ents = append(cr.ents, e.nbt)
	}
	type tickKey struct {
		pos [3]int32
		at  int64
	}
	tickState := make(map[tickKey]int32, len(c.UnknownTicks))
	for _, ut := range c.UnknownTicks {
		if int(ut.State) < len(c.UnknownStates) {
			tickState[tickKey{pos: ut.Pos, at: ut.At}] = int32(ut.State)
		}
	}
	for _, t := range ticks {
		st := int32(-1)
		// Only honour the preserved state while the update still points at
		// the placeholder: if the server replaced it, the new block wins.
		k := tickKey{pos: [3]int32{int32(t.Pos.X()), int32(t.Pos.Y()), int32(t.Pos.Z())}, at: t.Tick}
		if s, ok := tickState[k]; ok && t.Block == placeholder {
			st = s
		}
		cr.ticks = append(cr.ticks, rawTick{
			packedXZ: uint8(t.Pos.X()&0xF) | uint8(t.Pos.Z()&0xF)<<4,
			y:        int32(t.Pos.Y()),
			rid:      t.Block,
			state:    st,
			at:       t.Tick,
		})
	}
	return cr
}

// injectUnknown rewrites positions still holding the placeholder runtime ID
// to reference their preserved original states, then recompacts the local
// palette to used entries so the output stays canonical.
func injectUnknown(rs *rawBlockSec, entries []UnknownBlock, placeholder uint32, nStates int) {
	// Materialise indices (a uniform storage points everything at slot 0).
	if rs.idx == nil {
		rs.idx = make([]uint16, 4096)
	}
	if rs.states == nil {
		rs.states = make([]int32, len(rs.rids))
		for i := range rs.states {
			rs.states[i] = -1
		}
	}
	isPlaceholderSlot := func(slot uint16) bool {
		return rs.states[slot] == -1 && rs.rids[slot] == placeholder
	}
	slotFor := func(state uint32) uint16 {
		for i, st := range rs.states {
			if st == int32(state) {
				return uint16(i)
			}
		}
		rs.rids = append(rs.rids, placeholder)
		rs.states = append(rs.states, int32(state))
		return uint16(len(rs.rids) - 1)
	}
	for _, u := range entries {
		if int(u.State) >= nStates {
			continue
		}
		if u.Index == WholeStorage {
			slot := uint16(0)
			assigned := false
			for i, li := range rs.idx {
				if isPlaceholderSlot(li) {
					if !assigned {
						slot, assigned = slotFor(u.State), true
					}
					rs.idx[i] = slot
				}
			}
			continue
		}
		if int(u.Index) >= len(rs.idx) {
			continue
		}
		if li := rs.idx[u.Index]; isPlaceholderSlot(li) {
			rs.idx[u.Index] = slotFor(u.State)
		}
	}

	// Used-only recompaction (injection may have orphaned the placeholder).
	used := make([]bool, len(rs.rids))
	for _, li := range rs.idx {
		used[li] = true
	}
	remap := make([]uint16, len(rs.rids))
	rids := make([]uint32, 0, len(rs.rids))
	states := make([]int32, 0, len(rs.rids))
	for i, u := range used {
		if u {
			remap[i] = uint16(len(rids))
			rids = append(rids, rs.rids[i])
			states = append(states, rs.states[i])
		}
	}
	if len(rids) == 1 {
		rs.rids, rs.states, rs.idx = rids, states, nil
		return
	}
	for i, li := range rs.idx {
		rs.idx[i] = remap[li]
	}
	rs.rids, rs.states = rids, states
}

// extractBlockRaw reads a storage into a local runtime ID palette + indices.
func extractBlockRaw(storage *chunk.PalettedStorage) rawBlockSec {
	var out rawBlockSec
	sd := extractStorage(storage, func(v uint32) uint32 {
		out.rids = append(out.rids, v)
		return uint32(len(out.rids) - 1)
	})
	out.idx = sd.idx
	return out
}

// plainsBiomeName is the fallback unresolved biomes decode to.
func plainsBiomeName() string { return "minecraft:plains" }

// foldDuplicates collapses local palette slots that resolved to one global
// entry. Aliases are merged globally, so a section using two of them is
// uniform even though its local palette has two slots, and the uniform test
// that drives default-biome elision runs before the blob encoder would have
// folded them.
func foldDuplicates(sd secData) secData {
	if len(sd.pal) < 2 || sd.idx == nil {
		return sd
	}
	remap := make([]uint16, len(sd.pal))
	out := make([]uint32, 0, len(sd.pal))
	seen := make(map[uint32]uint16, len(sd.pal))
	for i, g := range sd.pal {
		if at, ok := seen[g]; ok {
			remap[i] = at
			continue
		}
		at := uint16(len(out))
		seen[g] = at
		out = append(out, g)
		remap[i] = at
	}
	if len(out) == len(sd.pal) {
		return sd
	}
	idx := make([]uint16, len(sd.idx))
	for i, li := range sd.idx {
		idx[i] = remap[li]
	}
	if len(out) == 1 {
		idx = nil
	}
	return secData{pal: out, idx: idx}
}

// bioKey identifies one preserved biome position: absolute section index plus
// section-local index.
type bioKey struct {
	sec int32
	idx uint16
}

// splitUnknownBiomes re-emits preserved biome names at the exact positions
// they were read from. Renaming a palette entry in place cannot do this: a
// fallback slot is shared by every position that resolved to it, so renaming
// it would give one preserved name to all of them, and a uniform section has
// no indices to consult at all. The section is rebuilt per position instead,
// which is also what makes a uniform section able to carry them.
func splitUnknownBiomes(rs rawBiomeSec, sec int32, plains string,
	unknown map[bioKey]uint32, name func(uint32) (string, bool),
) (rawBiomeSec, error) {
	want := make([]string, 4096)
	changed := false
	for pos := range want {
		li := uint16(0)
		if rs.idx != nil {
			li = rs.idx[pos]
		}
		if int(li) >= len(rs.names) {
			return rs, fmt.Errorf("biome index %d out of range in section %d", li, sec)
		}
		want[pos] = rs.names[li]
		if want[pos] != plains {
			continue
		}
		state, ok := unknown[bioKey{sec: sec, idx: uint16(pos)}]
		if !ok {
			continue
		}
		n, ok := name(state)
		if !ok {
			return rs, fmt.Errorf("unknown biome state %d in section %d has no name", state, sec)
		}
		want[pos], changed = n, true
	}
	if !changed {
		return rs, nil
	}
	out := rawBiomeSec{idx: make([]uint16, 4096)}
	seen := map[string]uint16{}
	for pos, n := range want {
		li, ok := seen[n]
		if !ok {
			li = uint16(len(out.names))
			seen[n] = li
			out.names = append(out.names, n)
		}
		out.idx[pos] = li
	}
	if len(out.names) == 1 {
		out.idx = nil // uniform again
	}
	return out, nil
}

// extractBiomeRaw reads one section's biome storage into a local biome name
// palette + indices.
func extractBiomeRaw(ch *chunk.Chunk, secIdx int) rawBiomeSec {
	var out rawBiomeSec
	sd := extractStorage(chunkBiomeStorage(ch, secIdx), func(id uint32) uint32 {
		out.names = append(out.names, biomeName(id))
		return uint32(len(out.names) - 1)
	})
	out.idx = sd.idx
	return out
}

// secLayer identifies one section's storage layer.
type secLayer struct {
	sec   int32
	layer uint8
}

// sidecarLayerCounts folds a sidecar's (section, layer) keys into one entry per
// section: how many layers that section's preserved-state entries reach, so a
// layer the runtime never allocated is still written.
//
// This used to be a per-section scan of the whole key set, which made both
// writers quadratic in (sections or cells) x (sidecar keys) — a 155-byte
// structure file of 1,048,576 cells and 4,096 preserved states took 125 seconds
// to re-encode, and ContentHash re-encodes what it decodes. One pass over the
// keys answers every section, and the answer is the same one, so no byte moves.
func sidecarLayerCounts(unknownBySec map[secLayer][]UnknownBlock) map[int32]int {
	if len(unknownBySec) == 0 {
		return nil
	}
	out := make(map[int32]int, len(unknownBySec))
	for k := range unknownBySec {
		if n := int(k.layer) + 1; n > out[k.sec] {
			out[k.sec] = n
		}
	}
	return out
}

// airOnlyLayer reports whether a layer holds nothing but air, carrying no
// preserved state that would make it worth storing.
func airOnlyLayer(rs rawBlockSec, air uint32) bool {
	return len(rs.rids) == 1 && rs.rids[0] == air && (rs.states == nil || rs.states[0] < 0)
}

// resolveColumn maps a raw column's local palettes into the global palette
// builders. The call order matches the historical serial encoder, keeping
// palette build order (and therefore output) identical.
func resolveColumn(cr *colRaw, addBlock func(rid uint32) uint32, addBiome func(name string) uint32, addState func(bs BlockState) uint32, uncount, uncountBiome func(i uint32)) colIntermediate {
	ci := colIntermediate{
		x: cr.x, z: cr.z,
		minSection: cr.minSection,
		sectionN:   cr.sectionN,
		blockSecs:  make([][]secData, cr.sectionN),
		biomeSecs:  make([]secData, cr.sectionN),
		light:      cr.light,
		bes:        cr.bes,
		ents:       cr.ents,
		tick:       cr.tick,
		userData:   cr.userData,
	}
	for i, secs := range cr.blockSecs {
		if secs == nil {
			continue
		}
		out := make([]secData, len(secs))
		for l, rs := range secs {
			pal := make([]uint32, len(rs.rids))
			seen := make(map[uint32]struct{}, len(rs.rids))
			for j, rid := range rs.rids {
				if rs.states != nil && rs.states[j] >= 0 {
					pal[j] = addState(cr.unkStates[rs.states[j]])
				} else {
					pal[j] = addBlock(rid)
				}
				// The reference count is appearances in local palettes, not
				// slots: a local palette holding two aliases of one state
				// contains it once.
				if _, dup := seen[pal[j]]; dup {
					uncount(pal[j])
				}
				seen[pal[j]] = struct{}{}
			}
			out[l] = secData{pal: pal, idx: rs.idx}
		}
		ci.blockSecs[i] = out
	}
	for i, bs := range cr.biomeSecs {
		if bs.names == nil {
			continue
		}
		pal := make([]uint32, len(bs.names))
		bseen := make(map[uint32]struct{}, len(bs.names))
		for j, name := range bs.names {
			pal[j] = addBiome(name)
			// As for block states: a registry may number one biome twice, and
			// a local palette naming it twice contains it once.
			if _, dup := bseen[pal[j]]; dup {
				uncountBiome(pal[j])
			}
			bseen[pal[j]] = struct{}{}
		}
		ci.biomeSecs[i] = foldDuplicates(secData{pal: pal, idx: bs.idx})
	}
	ci.ticks = make([]tickIntermediate, len(cr.ticks))
	for i, t := range cr.ticks {
		build := uint32(0)
		if t.state >= 0 {
			build = addState(cr.unkStates[t.state])
		} else {
			build = addBlock(t.rid)
		}
		ci.ticks[i] = tickIntermediate{
			packedXZ: t.packedXZ, y: t.y, blockBuild: build, at: t.at,
		}
	}
	return ci
}

// buildColBlobs computes the canonical blob bytes of one column's sections.
func buildColBlobs(ci *colIntermediate, blockRemap, biomeRemap []uint32, defaultRef uint32, haveDefault bool) colBlobs {
	cb := colBlobs{
		block: make([][][]byte, ci.sectionN),
		biome: make([][]byte, ci.sectionN),
	}
	for i, secs := range ci.blockSecs {
		if secs == nil {
			continue
		}
		out := make([][]byte, len(secs))
		for l, sd := range secs {
			out[l] = canonicalBlob(sd, blockRemap)
		}
		cb.block[i] = out
	}
	for i := range ci.biomeSecs {
		sd := &ci.biomeSecs[i]
		if sd.pal == nil {
			continue
		}
		if build, ok := sd.uniform(); ok && haveDefault && biomeRemap[build] == defaultRef {
			continue // uniformly the world default: absent
		}
		cb.biome[i] = canonicalBlob(*sd, biomeRemap)
	}
	return cb
}

// blobSink writes an encoded section blob into a record: solid mode writes a
// blob table reference, indexed mode inlines the blob bytes.
type blobSink func(w *writer, blob []byte)

// sortTicks applies the canonical scheduled-update order, breaking ties on
// position and firing tick with the final global block reference. It runs
// after palette finalization because only then is that reference known.
func sortTicks(ticks []tickIntermediate, blockRemap []uint32) {
	slices.SortStableFunc(ticks, func(a, b tickIntermediate) int {
		if v := cmp.Compare(a.y, b.y); v != 0 {
			return v
		}
		if v := cmp.Compare(a.packedXZ>>4, b.packedXZ>>4); v != 0 {
			return v
		}
		if v := cmp.Compare(a.packedXZ&0xF, b.packedXZ&0xF); v != 0 {
			return v
		}
		if v := cmp.Compare(a.at, b.at); v != 0 {
			return v
		}
		return cmp.Compare(blockRemap[a.blockBuild], blockRemap[b.blockBuild])
	})
}

// encodeRecordBody writes a chunk record without position, emitting the
// precomputed section blobs through sink.
func encodeRecordBody(w *writer, ci *colIntermediate, cb *colBlobs, blockRemap []uint32, storeLight bool, sink blobSink) {
	w.svarint(int64(ci.minSection))
	w.uvarint(uint64(ci.sectionN))

	// Block presence bitset + per-section layer blobs.
	presence := make([]byte, (ci.sectionN+7)/8)
	for i, secs := range cb.block {
		if secs != nil {
			presence[i/8] |= 1 << (i % 8)
		}
	}
	w.raw(presence)
	for _, secs := range cb.block {
		if secs == nil {
			continue
		}
		w.uvarint(uint64(len(secs)))
		for _, blob := range secs {
			sink(w, blob)
		}
	}

	// Biome presence bitset + per-section blobs.
	bioBits := make([]byte, (ci.sectionN+7)/8)
	for i, blob := range cb.biome {
		if blob != nil {
			bioBits[i/8] |= 1 << (i % 8)
		}
	}
	w.raw(bioBits)
	for _, blob := range cb.biome {
		if blob != nil {
			sink(w, blob)
		}
	}

	if storeLight {
		// Light presence is independent of block presence (§4.6).
		lightBits := make([]byte, (ci.sectionN+7)/8)
		for i := range ci.sectionN {
			if i < len(ci.light) && (len(ci.light[i].block) == lightArrayLen || len(ci.light[i].sky) == lightArrayLen) {
				lightBits[i/8] |= 1 << (i % 8)
			}
		}
		w.raw(lightBits)
		for i := range ci.sectionN {
			if lightBits[i/8]&(1<<(i%8)) == 0 {
				continue
			}
			lp := ci.light[i]
			var flags uint8
			if len(lp.block) == lightArrayLen {
				flags |= 1
			}
			if len(lp.sky) == lightArrayLen {
				flags |= 2
			}
			w.u8(flags)
			if flags&1 != 0 {
				w.raw(lp.block)
			}
			if flags&2 != 0 {
				w.raw(lp.sky)
			}
		}
	}

	w.uvarint(uint64(len(ci.bes)))
	for _, be := range ci.bes {
		w.u8(be.packedXZ)
		w.svarint(int64(be.y))
		w.blob(be.nbt)
	}
	w.uvarint(uint64(len(ci.ents)))
	for _, e := range ci.ents {
		w.blob(e)
	}
	w.svarint(ci.tick)
	sortTicks(ci.ticks, blockRemap)
	w.uvarint(uint64(len(ci.ticks)))
	for _, t := range ci.ticks {
		w.u8(t.packedXZ)
		w.svarint(int64(t.y))
		w.uvarint(uint64(blockRemap[t.blockBuild]))
		w.svarint(t.at)
	}
	w.blob(ci.userData)
}
