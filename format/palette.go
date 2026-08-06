package format

import (
	"bytes"
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
	"github.com/df-mc/worldupgrader/blockupgrader"
)

// Block state property value type codes. Bedrock block state properties are
// exactly these three NBT types, which makes this encoding lossless.
const (
	propByte   = 0
	propInt    = 1
	propString = 2
)

// blockPaletteBuilder accumulates the unique block states referenced by a file
// and assigns them global palette indices. Entries are collected in
// first-seen order, then finalized into the canonical order: descending
// reference count, ties broken by the entry's encoded bytes and then by its
// state version.
type blockPaletteBuilder struct {
	reg   world.BlockRegistry
	idx   map[uint32]uint32 // runtime ID → build-order index
	byKey map[string]uint32 // canonical state string → build-order index (unknown states)
	ent   []blockPaletteEntry
}

type blockPaletteEntry struct {
	rid     uint32
	name    string
	props   map[string]any
	key     string // dedup key while building; ordering uses the encoded bytes
	count   uint64
	version int32 // 0 = the palette's own version
}

func newBlockPaletteBuilder(reg world.BlockRegistry) *blockPaletteBuilder {
	return &blockPaletteBuilder{reg: reg, idx: make(map[uint32]uint32), byKey: make(map[string]uint32)}
}

// add records one reference to the state with the given runtime ID and
// returns its build-order index. The index is provisional: encode remaps it
// through the table returned by finalize.
func (b *blockPaletteBuilder) add(rid uint32) uint32 {
	if i, ok := b.idx[rid]; ok {
		b.ent[i].count++
		return i
	}
	name, props, found := b.reg.RuntimeIDToState(rid)
	if !found {
		// A runtime ID outside the registry cannot be represented; store air.
		// This cannot occur for chunks produced by the same registry.
		name, props = "minecraft:air", nil
	}
	i := uint32(len(b.ent))
	b.ent = append(b.ent, blockPaletteEntry{
		rid: rid, name: name, props: props, key: stateKey(name, props), count: 1,
	})
	b.idx[rid] = i
	return i
}

// normaliseStateVersion folds a state version that equals the palette's own
// into the zero that means exactly that. A decoder hands preserved states an
// explicit version (the file's, so a later save at a different runtime version
// still says what the state was expressed at), and without this fold that
// explicit value would come back as an override the writer did not need,
// growing the file on a round trip that changed nothing.
func normaliseStateVersion(v int32) int32 {
	if v == chunk.CurrentBlockVersion {
		return 0
	}
	return v
}

// addState records one reference to an explicit block state that has no
// runtime ID (an unknown state being preserved through a save). Unknown
// states never collide with registry-resolvable ones, so keying by canonical
// state string is safe.
func (b *blockPaletteBuilder) addState(bs BlockState) uint32 {
	version := normaliseStateVersion(bs.Version)
	key := stateIdentity(bs.Name, bs.Properties) + "@" + strconv.Itoa(int(version))
	if i, ok := b.byKey[key]; ok {
		b.ent[i].count++
		return i
	}
	i := uint32(len(b.ent))
	b.ent = append(b.ent, blockPaletteEntry{
		name: bs.Name, props: bs.Properties, key: key, count: 1, version: version,
	})
	b.byKey[key] = i
	return i
}

// finalize sorts entries into canonical order and returns the encoded palette
// along with a remap table from build-order indices to final indices.
//
// The sort key is the entry's own encoded bytes. That is exactly what the
// file carries, so an implementation in any language reproduces the order
// without first having to agree on a host-language string form. The readable
// state key cannot serve: preserved states are keyed with their version and
// registry states are not, so the two key spaces would be compared against
// each other and the resulting order would follow neither rule.
func (b *blockPaletteBuilder) finalize() (encoded []byte, remap []uint32, err error) {
	// Encode every entry once, up front. Entries that encode identically at
	// the same version are the same state and are merged: leaving both in
	// would put two indistinguishable entries in the palette whose relative
	// order nothing could decide.
	type unique struct {
		key     []byte
		version int32
		count   uint64
	}
	var uniq []unique
	seen := make(map[string]uint32, len(b.ent))
	remap = make([]uint32, len(b.ent))
	for i := range b.ent {
		e := &b.ent[i]
		if err := checkState(e.name, e.props); err != nil {
			return nil, nil, err
		}
		kw := &writer{}
		kw.str(e.name)
		encodeProps(kw, e.props)
		key := kw.bytes()
		dedup := string(key) + "@" + strconv.Itoa(int(e.version))
		if j, ok := seen[dedup]; ok {
			uniq[j].count += e.count
			remap[i] = j
			continue
		}
		j := uint32(len(uniq))
		seen[dedup] = j
		uniq = append(uniq, unique{key: key, version: e.version, count: e.count})
		remap[i] = j
	}
	if len(uniq) > maxPalette {
		return nil, nil, fmt.Errorf("pile: %d block states exceeds limit %d", len(uniq), maxPalette)
	}

	order := make([]uint32, len(uniq))
	for i := range order {
		order[i] = uint32(i)
	}
	slices.SortFunc(order, func(x, y uint32) int {
		ex, ey := &uniq[x], &uniq[y]
		if ex.count != ey.count {
			if ex.count > ey.count {
				return -1
			}
			return 1
		}
		if v := bytes.Compare(ex.key, ey.key); v != 0 {
			return v
		}
		return cmp.Compare(ex.version, ey.version)
	})

	// remap currently points at unique entries in build order; compose it
	// with the sort so callers get final palette indices directly.
	final := make([]uint32, len(uniq))
	for f, u := range order {
		final[u] = uint32(f)
	}
	for i := range remap {
		remap[i] = final[remap[i]]
	}

	w := &writer{}
	w.uvarint(uint64(len(order)))
	type override struct {
		index   uint64
		version int32
	}
	var overrides []override
	for f, u := range order {
		e := &uniq[u]
		w.raw(e.key)
		if e.version != 0 {
			overrides = append(overrides, override{index: uint64(f), version: e.version})
		}
	}
	// Sparse version overrides: entries whose states are expressed at a
	// different block version than the palette's own. Empty (one byte) for
	// every file that holds no preserved unknown states.
	slices.SortFunc(overrides, func(x, y override) int { return cmp.Compare(x.index, y.index) })
	w.uvarint(uint64(len(overrides)))
	var prev uint64
	for _, o := range overrides {
		w.uvarint(o.index - prev)
		w.i32(o.version)
		prev = o.index
	}
	return w.bytes(), remap, nil
}

// stateIdentity builds a collision-free identity for a block state: every
// field is length-prefixed, so no combination of names, keys and values can
// produce the same bytes as a different state. stateKey (below) is a
// human-readable form used for build-time dedup and for reporting, never for
// ordering: the canonical order is defined on the encoded palette entry.
func stateIdentity(name string, props map[string]any) string {
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var sb strings.Builder
	writeField := func(s string) {
		sb.WriteString(strconv.Itoa(len(s)))
		sb.WriteByte(':')
		sb.WriteString(s)
	}
	writeField(name)
	sb.WriteString(strconv.Itoa(len(keys)))
	sb.WriteByte(';')
	for _, k := range keys {
		writeField(k)
		switch v := props[k].(type) {
		case uint8:
			sb.WriteByte('b')
			writeField(strconv.Itoa(int(v)))
		case bool:
			sb.WriteByte('b')
			if v {
				writeField("1")
			} else {
				writeField("0")
			}
		case int32:
			sb.WriteByte('i')
			writeField(strconv.Itoa(int(v)))
		case string:
			sb.WriteByte('s')
			writeField(v)
		default:
			sb.WriteByte('?')
			writeField(fmt.Sprintf("%v", v))
		}
	}
	return sb.String()
}

// stateKey builds the readable string form of a block state, used only for
// deterministic ordering: name[key=value,...] with keys sorted.
func stateKey(name string, props map[string]any) string {
	if len(props) == 0 {
		return name
	}
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var sb strings.Builder
	sb.WriteString(name)
	sb.WriteByte('[')
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(k)
		sb.WriteByte('=')
		switch v := props[k].(type) {
		case uint8:
			sb.WriteString(strconv.Itoa(int(v)))
		case bool:
			if v {
				sb.WriteString("1")
			} else {
				sb.WriteString("0")
			}
		case int32:
			sb.WriteString(strconv.Itoa(int(v)))
		case string:
			sb.WriteString(v)
		default:
			sb.WriteString("?")
		}
	}
	sb.WriteByte(']')
	return sb.String()
}

// checkState reports whether a block state's identifiers fit the decoder's
// string limit.
func checkState(name string, props map[string]any) error {
	if err := checkString(name, "block state name"); err != nil {
		return err
	}
	if len(props) > 64 {
		return fmt.Errorf("pile: block state %q has %d properties, limit 64", name, len(props))
	}
	for k, v := range props {
		if err := checkString(k, "block state property name"); err != nil {
			return err
		}
		switch tv := v.(type) {
		case uint8, bool, int32:
		case string:
			if err := checkString(tv, "block state property value"); err != nil {
				return err
			}
		default:
			return fmt.Errorf("pile: block state %q property %q has unsupported type %T", name, k, v)
		}
	}
	return nil
}

// encodeProps writes a state's properties with keys in sorted order.
func encodeProps(w *writer, props map[string]any) {
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	w.uvarint(uint64(len(keys)))
	for _, k := range keys {
		w.str(k)
		switch v := props[k].(type) {
		case uint8:
			w.u8(propByte)
			w.u8(v)
		case bool:
			// Bedrock stores boolean properties as bytes; a custom registry
			// may present them as Go bools. Without this they would both
			// serialise to the same placeholder string and collapse into one
			// state.
			w.u8(propByte)
			if v {
				w.u8(1)
			} else {
				w.u8(0)
			}
		case int32:
			w.u8(propInt)
			w.i32(v)
		case string:
			w.u8(propString)
			w.str(v)
		default:
			// Non-Bedrock property type; store its string form. Should not
			// occur with dragonfly registries.
			w.u8(propString)
			w.str(stateKey("", map[string]any{k: v}))
		}
	}
}

// BlockState is a block state as stored in a palette: name plus Bedrock
// properties (byte/int32/string values). Used to preserve states that do not
// resolve against the runtime registry.
type BlockState struct {
	Name       string
	Properties map[string]any
	// Version is the Minecraft block version the name and properties are
	// expressed at. Zero means "the version recorded for the palette that
	// holds this entry". Preserved unknown states keep the version of the
	// file they came from, so a later runtime that learns the state upgrades
	// it from the right starting point instead of the writer's own version.
	Version int32
}

// parsedState is a block state read from a palette, before resolution.
type parsedState struct {
	name    string
	props   map[string]any
	version int32 // 0 = the palette's own version
}

// parseStatePalette reads a count-prefixed list of encoded block states.
// parseStatePalette reads a count-prefixed list of encoded block states
// followed by the sparse version-override table.
func parseStatePalette(r *reader) ([]parsedState, error) {
	n, err := r.count(maxPalette, "block palette")
	if err != nil {
		return nil, err
	}
	// Each entry consumes at least 2 bytes; a hostile count cannot demand
	// more memory than the input could possibly describe.
	if n > r.remaining()/2+1 {
		return nil, corruptf("block palette count %d exceeds input", n)
	}
	entries := make([]parsedState, n)
	for i := range n {
		name, err := r.str()
		if err != nil {
			return nil, err
		}
		propN, err := r.count(64, "state property")
		if err != nil {
			return nil, err
		}
		props := make(map[string]any, propN)
		for range propN {
			k, err := r.str()
			if err != nil {
				return nil, err
			}
			t, err := r.u8()
			if err != nil {
				return nil, err
			}
			switch t {
			case propByte:
				v, err := r.u8()
				if err != nil {
					return nil, err
				}
				props[k] = v
			case propInt:
				v, err := r.u32()
				if err != nil {
					return nil, err
				}
				props[k] = int32(v)
			case propString:
				v, err := r.str()
				if err != nil {
					return nil, err
				}
				props[k] = v
			default:
				return nil, corruptf("unknown property type %d", t)
			}
		}
		entries[i] = parsedState{name: name, props: props}
	}
	overrideN, err := r.count(uint64(len(entries)), "palette version override")
	if err != nil {
		return nil, err
	}
	var prev uint64
	for i := range overrideN {
		delta, err := r.uvarint()
		if err != nil {
			return nil, err
		}
		if i > 0 && delta == 0 {
			return nil, corruptf("palette version overrides must be strictly ascending")
		}
		idx := prev + delta
		if idx >= uint64(len(entries)) {
			return nil, corruptf("palette version override index %d out of range", idx)
		}
		v, err := r.u32()
		if err != nil {
			return nil, err
		}
		if int32(v) == 0 {
			return nil, corruptf("palette version override must not be zero")
		}
		entries[idx].version = int32(v)
		prev = idx
	}
	return entries, nil
}

// resolveState upgrades a parsed state to the current block version and looks
// it up in the registry.
func resolveState(e parsedState, reg world.BlockRegistry, blockVersion int32) (uint32, bool) {
	if e.version != 0 {
		blockVersion = e.version // preserved state: upgrade from its own version
	}
	up := blockupgrader.Upgrade(blockupgrader.BlockState{
		Name: e.name, Properties: e.props, Version: blockVersion,
	})
	return reg.StateToRuntimeID(up.Name, up.Properties)
}

// placeholderRid returns the runtime ID that unresolvable states decode as.
func placeholderRid(reg world.BlockRegistry) uint32 {
	if rid, ok := reg.StateToRuntimeID("minecraft:info_update", map[string]any{}); ok {
		return rid
	}
	return reg.AirRuntimeID()
}

// decodeBlockPalette reads the global block palette and resolves every entry
// to a runtime ID in reg. States from files written against older Minecraft
// block versions are upgraded once per unique state. Unresolvable states fall
// back to minecraft:info_update (the Bedrock placeholder block), then air;
// their original states are returned so callers can preserve them:
// unknown[i] is -1 for resolved entries, otherwise an index into states.
func decodeBlockPalette(r *reader, reg world.BlockRegistry, blockVersion int32) (rids []uint32, unknown []int32, states []BlockState, err error) {
	entries, err := parseStatePalette(r)
	if err != nil {
		return nil, nil, nil, err
	}
	placeholder := placeholderRid(reg)
	rids = make([]uint32, len(entries))
	unknown = make([]int32, len(entries))
	for i, e := range entries {
		unknown[i] = -1
		rid, ok := resolveState(e, reg, blockVersion)
		if !ok {
			rid = placeholder
			unknown[i] = int32(len(states))
			v := e.version
			if v == 0 {
				v = blockVersion // the file's version applies to this entry
			}
			states = append(states, BlockState{Name: e.name, Properties: e.props, Version: v})
		}
		rids[i] = rid
	}
	return rids, unknown, states, nil
}

// biomePaletteBuilder accumulates unique biome names, canonically ordered the
// same way as block states: descending reference count, then name.
type biomePaletteBuilder struct {
	idx map[string]uint32
	ent []biomePaletteEntry
}

type biomePaletteEntry struct {
	name  string
	count uint64
}

func newBiomePaletteBuilder() *biomePaletteBuilder {
	return &biomePaletteBuilder{idx: make(map[string]uint32)}
}

// addName records a biome by its stored name, used to re-emit names the
// runtime could not resolve.
func (b *biomePaletteBuilder) addName(name string) uint32 { return b.add(name, 1) }

func (b *biomePaletteBuilder) add(name string, weight uint64) uint32 {
	if i, ok := b.idx[name]; ok {
		b.ent[i].count += weight
		return i
	}
	i := uint32(len(b.ent))
	b.ent = append(b.ent, biomePaletteEntry{name: name, count: weight})
	b.idx[name] = i
	return i
}

func (b *biomePaletteBuilder) finalize() (encoded []byte, remap []uint32, err error) {
	order := make([]uint32, len(b.ent))
	for i := range order {
		order[i] = uint32(i)
	}
	slices.SortFunc(order, func(x, y uint32) int {
		ex, ey := &b.ent[x], &b.ent[y]
		if ex.count != ey.count {
			if ex.count > ey.count {
				return -1
			}
			return 1
		}
		return strings.Compare(ex.name, ey.name)
	})
	if len(order) > maxPalette {
		return nil, nil, fmt.Errorf("pile: %d biomes exceeds limit %d", len(order), maxPalette)
	}
	remap = make([]uint32, len(b.ent))
	w := &writer{}
	w.uvarint(uint64(len(order)))
	for final, build := range order {
		remap[build] = uint32(final)
		if err := checkString(b.ent[build].name, "biome name"); err != nil {
			return nil, nil, err
		}
		if !strings.Contains(b.ent[build].name, ":") {
			return nil, nil, fmt.Errorf("pile: biome name %q is not namespaced", b.ent[build].name)
		}
		w.str(b.ent[build].name)
	}
	return w.bytes(), remap, nil
}

// decodeBiomePalette reads the global biome palette and resolves entries to
// numeric biome IDs. Unknown biomes fall back to minecraft:plains.
// decodeBiomePalette resolves biome names to IDs. Names the runtime does not
// know fall back to plains; unknown[i] is -1 for resolved entries, otherwise
// an index into names, so callers can preserve the original on re-encode.
func decodeBiomePalette(r *reader) (ids []uint32, unknown []int32, names []string, err error) {
	n, err := r.count(maxPalette, "biome palette")
	if err != nil {
		return nil, nil, nil, err
	}
	if n > r.remaining()/2+1 {
		return nil, nil, nil, corruptf("biome palette count %d exceeds input", n)
	}
	ids = make([]uint32, n)
	unknown = make([]int32, n)
	for i := range n {
		name, err := r.str()
		if err != nil {
			return nil, nil, nil, err
		}
		// Biome names are fully qualified on the wire. Accepting the bare
		// form too would give one biome two encodings.
		if !strings.Contains(name, ":") {
			return nil, nil, nil, corruptf("biome name %q is not namespaced", name)
		}
		unknown[i] = -1
		bio, ok := lookupBiome(name)
		if !ok {
			unknown[i] = int32(len(names))
			names = append(names, name)
			bio, ok = lookupBiome(plainsBiomeName())
		}
		if ok {
			ids[i] = uint32(bio.EncodeBiome())
		} else {
			ids[i] = 1 // plains, last resort
		}
	}
	return ids, unknown, names, nil
}

// biomeName resolves a numeric biome ID to its canonical name, falling back to
// plains.
func biomeName(id uint32) string {
	if b, ok := world.BiomeByID(int(id)); ok {
		return qualifyBiome(b.String())
	}
	return plainsBiomeName()
}

// qualifyBiome returns a biome identifier in the canonical namespaced form.
// Dragonfly names vanilla biomes without a namespace, but a bare name is not a
// stable identifier outside its own registry, and the wire form has to mean the
// same thing to an implementation that has never heard of dragonfly.
func qualifyBiome(name string) string {
	if strings.Contains(name, ":") {
		return name
	}
	return "minecraft:" + name
}

// lookupBiome resolves a canonical biome name, accounting for dragonfly
// storing vanilla biomes unnamespaced.
func lookupBiome(name string) (world.Biome, bool) {
	if b, ok := world.BiomeByName(name); ok {
		return b, true
	}
	if rest, cut := strings.CutPrefix(name, "minecraft:"); cut {
		if b, ok := world.BiomeByName(rest); ok {
			return b, true
		}
	}
	return nil, false
}
