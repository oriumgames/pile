package pile

import (
	"fmt"
	"os"
	"slices"

	"github.com/df-mc/dragonfly/server/world/chunk"
)

// cloneColumn deep-copies a column. Dragonfly mutates the columns it holds,
// and the provider keeps its own state, so columns crossing the provider
// boundary are always copied in both directions.
func cloneColumn(c *chunk.Column) *chunk.Column {
	out := &chunk.Column{
		Chunk: c.Chunk.Clone(),
		Tick:  c.Tick,
	}
	if c.Entities != nil {
		out.Entities = make([]chunk.Entity, len(c.Entities))
		for i, e := range c.Entities {
			out.Entities[i] = chunk.Entity{ID: e.ID, Data: deepCopyCompound(e.Data)}
		}
	}
	if c.BlockEntities != nil {
		out.BlockEntities = make([]chunk.BlockEntity, len(c.BlockEntities))
		for i, be := range c.BlockEntities {
			out.BlockEntities[i] = chunk.BlockEntity{Pos: be.Pos, Data: deepCopyCompound(be.Data)}
		}
	}
	out.ScheduledBlocks = slices.Clone(c.ScheduledBlocks)
	return out
}

// createExclusive creates a file that must not already exist.
//
// os.Create is O_WRONLY|O_CREATE|O_TRUNC, which follows a symlink at the
// target: in a world-readable directory another user can pre-create the
// predictable ".tmp" name pointing anywhere this process can write, and the
// save goes through it. O_EXCL refuses to follow one, which is why the indexed
// writer has always used it and why every other path that stages a file should
// too. A stale temp file from a crashed run is removed first, deliberately and
// by this process, rather than being silently written through.
func createExclusive(path string) (*os.File, error) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("pile: clear stale temporary file: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, fmt.Errorf("pile: create %s: %w", path, err)
	}
	return f, nil
}

// safeCompact reduces a chunk's palettes the way dragonfly's Compact does,
// but only when doing so cannot renumber a layer.
//
// Compact drops every all-air storage and closes the gap, which is exactly
// what the format forbids: layer numbers are semantic, so an all-air layer 0
// beneath a populated layer 1 is a waterlogged section, and removing it turns
// the water into a liquid block. Vanilla content never has that shape, which
// is why the operation looks harmless, so the check is cheap and the common
// case still gets compacted.
func safeCompact(ch *chunk.Chunk, air uint32) {
	for _, sub := range ch.Sub() {
		if renumbersLayers(sub, air) {
			return
		}
	}
	ch.Compact()
}

// renumbersLayers reports whether dropping this sub chunk's all-air storages
// would move any surviving layer to a different index.
func renumbersLayers(sub *chunk.SubChunk, air uint32) bool {
	layers := sub.Layers()
	seenAir := false
	for _, st := range layers {
		if st.Palette().Len() == 1 && st.At(0, 0, 0) == air {
			seenAir = true
			continue
		}
		if seenAir {
			// A populated layer above an all-air one: compaction would pull it
			// down into the empty slot.
			return true
		}
	}
	return false
}

// deepCopyCompound copies an NBT-shaped map, recursing into nested maps and
// slices so no mutable state is shared.
func deepCopyCompound(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = deepCopyValue(v)
	}
	return out
}

func deepCopyValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return deepCopyCompound(x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = deepCopyValue(e)
		}
		return out
	case []map[string]any:
		out := make([]map[string]any, len(x))
		for i, e := range x {
			out[i] = deepCopyCompound(e)
		}
		return out
	case []byte:
		return slices.Clone(x)
	case []int16:
		return slices.Clone(x)
	case []int32:
		return slices.Clone(x)
	case []int64:
		return slices.Clone(x)
	case []float32:
		return slices.Clone(x)
	case []float64:
		return slices.Clone(x)
	case []string:
		return slices.Clone(x)
	default:
		return v // scalar
	}
}
