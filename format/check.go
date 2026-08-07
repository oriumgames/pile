package format

import (
	"bytes"
	"errors"

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
	entries, err := parseStatePalette(r, h.blockVersion)
	if err != nil {
		return nil, err
	}
	return unresolvedOf(entries, reg, h.blockVersion), nil
}

// UnresolvedStates re-reads the world's palette segments and reports states
// that do not resolve against the registry the world was opened with.
func (w *IndexedWorld) UnresolvedStates() ([]string, error) {
	// Held for the whole call, not just for the copy of the segment list.
	// readFrame reads w.f, w.compressed and w.dictCodec, and Compact replaces
	// all three while it rewrites the file. Taking a snapshot of the offsets
	// and then reading them without the lock is a race the detector sees, and
	// worse than a race: readFrame does not re-check the frame hash, so
	// offsets from the old directory read against the new file decode whatever
	// happens to be at them. Close has the same shape with the file handle.
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil, errors.New("pile: indexed world is closed")
	}

	var out []string
	for _, seg := range w.blockSegs {
		body, err := w.readFrame(seg)
		if err != nil {
			return nil, err
		}
		r := &reader{b: body}
		segVersion, err := r.u32()
		if err != nil {
			return nil, err
		}
		entries, err := parseStatePalette(r, int32(segVersion))
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
// It identifies the body, not the file. The dimension is header state that
// decoding does not resolve into the body, so two files holding the same
// chunks in different dimensions have the same ContentHash — a real collision
// for the small, near-empty worlds this format is aimed at. Callers that span
// dimensions must key on the dimension alongside this value rather than treat
// it as a whole-file identity. Within one dimension, two files with the same
// ContentHash hold the same world, and a compressor upgrade changes the file's
// bytes but not this value.
//
// It takes ReadOptions because it decodes internally: without them it would be
// the one path a caller's ceiling could not reach, and identifying a file would
// cost more than reading it.
func ContentHash(file []byte, reg world.BlockRegistry, opts ...ReadOption) (uint64, error) {
	h, _, err := parseFrame(file)
	if err != nil {
		return 0, err
	}
	if h.kind == KindStructure {
		s, err := ReadStructure(file, reg, opts...)
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
	// header state that decoding resolves into the body (the block version,
	// and the default-biome interpretation, which becomes the biome sections)
	// is included by construction. Hashing the stored body directly would do
	// the opposite on both counts. Header state that decoding does *not*
	// resolve into the body is excluded by the same construction — the
	// dimension is the one such field, and the doc comment says so rather
	// than this being repaired here, because ContentHash is the format's
	// declared identity and moving it moves every recorded hash.
	d, err := ReadWorld(file, reg, opts...)
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
