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
	// Kind is an application-defined category, e.g. "spawn" or "npc". It is
	// not how an area is recognised -- Bounds is -- so it stays free for
	// whatever the map means by it.
	Kind string
	// Pos is the marker's point, or nil if it has none. A marker with Bounds
	// and no Pos is a region with no distinguished point in it, which is the
	// ordinary case for an area: writing one anyway would raise the question
	// of whether it meant the centre or a corner, and nothing would answer it.
	Pos *[3]float64
	// Bounds makes the marker a region. Nil for a point marker.
	Bounds *Bounds
	// Extra holds arbitrary additional NBT-encodable fields. The keys "name",
	// "kind", "pos", "min" and "max" are reserved.
	Extra map[string]any
}

// Bounds is a marker's region: two opposite corners, Min no greater than Max
// on every axis.
//
// Doubles rather than block coordinates, so a region can carry a margin or sit
// at sub-block precision. Whether the bounds are inclusive is the
// application's business; the format stores two corners and orders them.
type Bounds struct{ Min, Max [3]float64 }

// Point returns a marker at a position.
func Point(name, kind string, pos [3]float64) Marker {
	return Marker{Name: name, Kind: kind, Pos: &pos}
}

// Area returns a marker covering a region. The corners are ordered per axis,
// so a caller may pass them in either order -- unlike the wire, where an
// inverted box is refused rather than repaired, because a file that two
// readers disagree about is worse than one that neither accepts.
func Area(name, kind string, a, b [3]float64) Marker {
	var bo Bounds
	for i := range 3 {
		bo.Min[i], bo.Max[i] = min(a[i], b[i]), max(a[i], b[i])
	}
	return Marker{Name: name, Kind: kind, Bounds: &bo}
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
		e := make(map[string]any, len(m.Extra)+5)
		for k, v := range m.Extra {
			switch k {
			case "name", "kind", "pos", "min", "max":
				return nil, fmt.Errorf("pile: marker %q uses reserved extra key %q", m.Name, k)
			}
			e[k] = v
		}
		e["name"] = m.Name
		e["kind"] = m.Kind
		if m.Pos == nil && m.Bounds == nil {
			return nil, fmt.Errorf("pile: marker %q has neither a position nor bounds, so it marks nothing", m.Name)
		}
		if m.Pos != nil {
			e["pos"] = []any{m.Pos[0], m.Pos[1], m.Pos[2]}
		}
		if b := m.Bounds; b != nil {
			e["min"] = []any{b.Min[0], b.Min[1], b.Min[2]}
			e["max"] = []any{b.Max[0], b.Max[1], b.Max[2]}
		}
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
		if t, ok := markerTriple(em["pos"]); ok {
			mk.Pos = &t
		}
		lo, okLo := markerTriple(em["min"])
		hi, okHi := markerTriple(em["max"])
		if okLo && okHi {
			mk.Bounds = &Bounds{Min: lo, Max: hi}
		}
		for k, v := range em {
			switch k {
			case "name", "kind", "pos", "min", "max":
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

// markerTriple reads a list of three doubles, however the NBT decoder shaped
// it. The wire rules of §7.2 are enforced by the codec on the way in and out;
// this only has to recognise the shape.
func markerTriple(v any) ([3]float64, bool) {
	var out [3]float64
	switch l := v.(type) {
	case []float64:
		if len(l) != 3 {
			return out, false
		}
		copy(out[:], l)
		return out, true
	case []any:
		if len(l) != 3 {
			return out, false
		}
		for i, e := range l {
			f, ok := e.(float64)
			if !ok {
				return out, false
			}
			out[i] = f
		}
		return out, true
	}
	return out, false
}
