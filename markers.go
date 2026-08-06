package pile

import (
	"fmt"
	"slices"
	"strings"

	"github.com/oriumgames/pile/format"
)

// Marker is a named point of interest stored in a world's metadata: spawn
// points, NPC locations, arena corners, portals. Maps become self-describing;
// game code reads markers instead of hardcoding coordinates.
type Marker struct {
	// Name uniquely identifies the marker within the world.
	Name string
	// Kind is an application-defined category, e.g. "spawn" or "npc".
	Kind string
	// Pos is the position of the marker.
	Pos [3]float64
	// Extra holds arbitrary additional NBT-encodable fields. The keys "name",
	// "kind" and "pos" are reserved.
	Extra map[string]any
}

// markersToNBT encodes markers sorted by name into a deterministic NBT blob.
// A nil/empty slice encodes to an empty blob.
func markersToNBT(markers []Marker) ([]byte, error) {
	if len(markers) == 0 {
		return nil, nil
	}
	ms := slices.Clone(markers)
	slices.SortFunc(ms, func(a, b Marker) int { return strings.Compare(a.Name, b.Name) })
	list := make([]map[string]any, len(ms))
	for i, m := range ms {
		e := make(map[string]any, len(m.Extra)+3)
		for k, v := range m.Extra {
			if k == "name" || k == "kind" || k == "pos" {
				return nil, fmt.Errorf("pile: marker %q uses reserved extra key %q", m.Name, k)
			}
			e[k] = v
		}
		e["name"] = m.Name
		e["kind"] = m.Kind
		e["pos"] = []any{m.Pos[0], m.Pos[1], m.Pos[2]}
		list[i] = e
	}
	return format.MarshalNBT(map[string]any{"markers": list})
}

// markersFromNBT decodes a markers blob.
func markersFromNBT(b []byte) ([]Marker, error) {
	if len(b) == 0 {
		return nil, nil
	}
	m, err := format.UnmarshalNBT(b)
	if err != nil {
		return nil, err
	}
	raw, _ := m["markers"].([]any)
	markers := make([]Marker, 0, len(raw))
	for _, e := range raw {
		em, ok := e.(map[string]any)
		if !ok {
			continue
		}
		var mk Marker
		mk.Name, _ = em["name"].(string)
		mk.Kind, _ = em["kind"].(string)
		if pos, ok := em["pos"].([]any); ok && len(pos) == 3 {
			for i := range 3 {
				if f, ok := pos[i].(float64); ok {
					mk.Pos[i] = f
				}
			}
		}
		for k, v := range em {
			if k == "name" || k == "kind" || k == "pos" {
				continue
			}
			if mk.Extra == nil {
				mk.Extra = make(map[string]any)
			}
			mk.Extra[k] = v
		}
		markers = append(markers, mk)
	}
	return markers, nil
}
