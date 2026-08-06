package format

import (
	"bytes"

	"github.com/cespare/xxhash/v2"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
)

// chunkCurrentBlockVersion returns the runtime's block version; indexed
// palette segments are always written at the writer's current version.
func chunkCurrentBlockVersion() int32 { return chunk.CurrentBlockVersion }

// UnresolvedStates returns the canonical strings of all block states in a
// solid world or structure file that do not resolve against reg, after block
// version upgrading. An empty result means every block in the file will load
// exactly; non-empty means those states would decode as placeholder blocks.
func UnresolvedStates(file []byte, reg world.BlockRegistry) ([]string, error) {
	reg.Finalize()
	h, stored, err := parseFrame(file)
	if err != nil {
		return nil, err
	}
	body, err := decompressBody(h, stored)
	if err != nil {
		return nil, err
	}
	r := &reader{b: body}
	if _, _, _, _, _, err := readMetaBlobs(r, h.flags); err != nil {
		return nil, err
	}
	entries, err := parseStatePalette(r)
	if err != nil {
		return nil, err
	}
	return unresolvedOf(entries, reg, h.blockVersion), nil
}

// UnresolvedStates re-reads the world's palette segments and reports states
// that do not resolve against the registry the world was opened with.
func (w *IndexedWorld) UnresolvedStates() ([]string, error) {
	w.mu.Lock()
	segs := make([]frameRef, len(w.blockSegs))
	copy(segs, w.blockSegs)
	w.mu.Unlock()

	var out []string
	for _, seg := range segs {
		body, err := w.readFrame(seg)
		if err != nil {
			return nil, err
		}
		r := &reader{b: body}
		segVersion, err := r.u32()
		if err != nil {
			return nil, err
		}
		entries, err := parseStatePalette(r)
		if err != nil {
			return nil, err
		}
		out = append(out, unresolvedOf(entries, w.reg, int32(segVersion))...)
	}
	return out, nil
}

func unresolvedOf(entries []parsedState, reg world.BlockRegistry, blockVersion int32) []string {
	var out []string
	for _, e := range entries {
		if _, ok := resolveState(e, reg, blockVersion); !ok {
			out = append(out, stateKey(e.name, e.props))
		}
	}
	return out
}

// ContentHash returns a stable identity for a world file's content: the
// canonical uncompressed body, hashed. Unlike the file's own bytes it does
// not depend on the compressor's version or settings, so it is the value to
// use for "is this the same map" comparisons, cache keys and map versioning.
//
// Two files with the same ContentHash hold the same world; a compressor
// upgrade changes file bytes but not this value.
func ContentHash(file []byte, reg world.BlockRegistry) (uint64, error) {
	h, _, err := parseFrame(file)
	if err != nil {
		return 0, err
	}
	if h.kind == KindStructure {
		s, err := ReadStructure(file, reg)
		if err != nil {
			return 0, err
		}
		var buf bytes.Buffer
		if err := WriteStructure(&buf, s, reg, Options{Compression: CompressionNone}); err != nil {
			return 0, err
		}
		out := buf.Bytes()
		return xxhash.Sum64(out[headerSize : len(out)-footerSize]), nil
	}
	// Decode and re-encode into the canonical projection: derived and
	// advisory content (the stats block, cached light) is excluded, while
	// header state that changes how the world decodes (the block version and
	// the default-biome interpretation, which decoding resolves into the
	// biome sections) is included by construction. Hashing the stored body
	// directly would do the opposite on both counts.
	d, err := ReadWorld(file, reg)
	if err != nil {
		return 0, err
	}
	var buf bytes.Buffer
	if err := WriteWorld(&buf, d, reg, Options{Compression: CompressionNone}); err != nil {
		return 0, err
	}
	out := buf.Bytes()
	return xxhash.Sum64(out[headerSize : len(out)-footerSize]), nil
}
