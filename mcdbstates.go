package pile

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/goleveldb/leveldb"
	"github.com/df-mc/goleveldb/leveldb/opt"
	"github.com/oriumgames/pile/format"
	"github.com/sandertv/gophertunnel/minecraft/nbt"
)

// MCDBBlockStates returns every distinct block state stored in a Bedrock
// world, read straight from the sub-chunk palettes.
//
// No registry is involved and none is needed: a palette entry carries the
// identifier and the properties, which is the whole of a state. That is what
// makes this usable on a world holding blocks from a behaviour pack, where
// resolving anything against a vanilla registry is exactly what fails.
//
// The database is opened READ-ONLY. goleveldb's default is read-write and
// rewrites the manifest and journal on open even when nothing is stored, which
// is a change to somebody's world that reading it must not make.
func MCDBBlockStates(dir string) ([]format.BlockState, error) {
	db, err := leveldb.OpenFile(filepath.Join(dir, "db"), &opt.Options{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("pile: open %s: %w", dir, err)
	}
	defer func() { _ = db.Close() }()

	var out []format.BlockState
	seen := map[string]bool{}
	it := db.NewIterator(nil, nil)
	defer it.Release()
	for it.Next() {
		k := it.Key()
		// Sub-chunk keys: eight bytes of position, the tag, and the index.
		// Thirteen-byte keys carry a dimension between the two; those are the
		// nether and the end, which have their own palettes and are included.
		if !((len(k) == 10 && k[8] == 0x2F) || (len(k) == 14 && k[12] == 0x2F)) {
			continue
		}
		entries, err := mcdbSubChunkPalette(it.Value())
		if err != nil {
			// Not skipped. An earlier version treated an unwalkable sub-chunk
			// as noise and carried on, which is defensible for a report and
			// wrong here: the states this misses are exactly the ones a
			// conversion then fails on, thousands of chunks later, with an
			// error naming a block rather than a parser.
			return nil, fmt.Errorf("pile: sub-chunk at %x: %w", k, err)
		}
		for _, e := range entries {
			key := e.Name + "\x00" + stateKey(e.State)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, format.BlockState{Name: e.Name, Properties: e.State})
		}
	}
	if err := it.Error(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("pile: no block palettes found; is this a Bedrock world?")
	}
	slices.SortFunc(out, func(a, b format.BlockState) int {
		if a.Name != b.Name {
			return strings.Compare(a.Name, b.Name)
		}
		return strings.Compare(stateKey(a.Properties), stateKey(b.Properties))
	})
	return out, nil
}

// stateKey renders a property map in a stable order, so two spellings of one
// state compare equal and the scan's output is deterministic.
func stateKey(props map[string]any) string {
	if len(props) == 0 {
		return ""
	}
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%v\x00", k, props[k])
	}
	return b.String()
}

// RegisterMCDBStates registers, on reg, every block state in the Bedrock world
// at dir that reg does not already know, and returns how many it added.
//
// This is what makes a world holding custom blocks convertible without the
// behaviour pack. dragonfly's chunk decoder resolves each palette entry against
// the registry and fails outright on one it cannot find -- "cannot get runtime
// ID of block state" -- so a single block from a pack stops the conversion at
// whichever chunk happens to hold it.
//
// What is registered is the bare state, through RegisterBlockState, which is
// dragonfly's own placeholder for a block nothing implements. That is enough to
// decode, and it is all that is wanted here: pile stores a palette entry as its
// identifier and properties, so the file that comes out carries the real
// cubecraft:portal_side and not a substitute. A server that registers the block
// properly later resolves it from the same file.
//
// It is not a way to make the block behave. A placeholder has no model, no
// collision and no behaviour; nothing short of implementing the pack gives it
// those. It is a way to move the world.
//
// The registry must not be finalized yet, since registration is what Finalize
// closes off.
func RegisterMCDBStates(dir string, reg world.BlockRegistry) (int, error) {
	if reg == nil {
		reg = world.DefaultBlockRegistry
	}
	states, err := MCDBBlockStates(dir)
	if err != nil {
		return 0, err
	}
	added := 0
	for _, s := range states {
		if registerIfUnknown(reg, world.BlockState{Name: s.Name, Properties: s.Properties}) {
			added++
		}
	}
	return added, nil
}

// registerIfUnknown registers a state unless the registry already has it, and
// reports whether it added one.
//
// Asking first is not available: StateToRuntimeID panics on a registry that is
// not finalized, and finalizing is what closes registration off, so there is no
// order in which both calls work. What the registry does offer is a panic with
// a documented message when a state is registered twice, which is the same
// question answered by attempting it.
//
// Only that panic is absorbed. Anything else -- a property type the registry
// refuses, a registry finalized behind the caller's back -- is re-raised, since
// swallowing those would turn a broken conversion into a quiet one.
func registerIfUnknown(reg world.BlockRegistry, s world.BlockState) (added bool) {
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		if msg, ok := r.(string); ok && strings.Contains(msg, "cannot register the same state twice") {
			added = false
			return
		}
		panic(r)
	}()
	reg.RegisterBlockState(s)
	return true
}

// mcdbBlockEntry is one palette entry as stored on disk.
type mcdbBlockEntry struct {
	Name    string         `nbt:"name"`
	State   map[string]any `nbt:"states"`
	Version int32          `nbt:"version"`
}

// mcdbSubChunkPalette returns every palette entry of a stored sub-chunk without
// decoding its indices. The layout is version 8 or 9: a version byte, a storage
// count, an index byte for version 9, then per storage a header, the packed
// indices, a palette count, and that many NBT compounds.
func mcdbSubChunkPalette(v []byte) ([]mcdbBlockEntry, error) {
	if len(v) < 2 {
		return nil, fmt.Errorf("sub-chunk of %d bytes", len(v))
	}
	ver, storages := v[0], int(v[1])
	b := v[2:]
	if ver == 9 {
		if len(b) < 1 {
			return nil, errors.New("version 9 sub-chunk with no index byte")
		}
		b = b[1:]
	}
	var out []mcdbBlockEntry
	for s := range storages {
		if len(b) < 1 {
			return nil, fmt.Errorf("storage %d: truncated header", s)
		}
		bits := int(b[0] >> 1)
		b = b[1:]
		// 0x7F marks a storage that is not there: dragonfly's decoder returns
		// immediately on it, consuming neither indices nor a palette.
		if bits == 0x7F {
			continue
		}
		if bits > 32 {
			return nil, fmt.Errorf("storage %d: %d bits per entry", s, bits)
		}
		// Zero bits is a uniform storage: one palette entry and no index words
		// at all. Rejecting it as malformed is what made this scan miss 3 258
		// of a world's 21 512 sub-chunks -- and with them every state that
		// appeared only in a uniform one, which for a map built out of large
		// single-block volumes is most of what a behaviour pack contributes.
		words := 0
		if bits > 0 {
			per := 32 / bits
			words = (4096 + per - 1) / per
		}
		if len(b) < words*4 {
			return nil, fmt.Errorf("storage %d: truncated indices", s)
		}
		b = b[words*4:]
		// A uniform storage writes no count: one entry is implied by the zero
		// width, and reading four bytes for it eats the first tag of the NBT
		// that follows.
		count := int32(1)
		if bits > 0 {
			if len(b) < 4 {
				return nil, fmt.Errorf("storage %d: truncated palette count", s)
			}
			count = int32(binary.LittleEndian.Uint32(b[:4]))
			b = b[4:]
			if count < 1 {
				return nil, fmt.Errorf("storage %d: palette count %d", s, count)
			}
		}
		r := bytes.NewReader(b)
		dec := nbt.NewDecoderWithEncoding(r, nbt.LittleEndian)
		for i := range count {
			var e mcdbBlockEntry
			if err := dec.Decode(&e); err != nil {
				return nil, fmt.Errorf("storage %d entry %d: %w", s, i, err)
			}
			out = append(out, e)
		}
		b = b[len(b)-r.Len():]
	}
	return out, nil
}
