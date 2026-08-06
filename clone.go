package pile

import (
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
