package pile

import (
	"github.com/oriumgames/pile/format"
)

// Rotate returns a copy of the structure rotated clockwise (viewed from
// above) by quarters*90 degrees around the Y axis. Positions of blocks, block
// entities and entities rotate exactly; the paste anchor resets to zero.
//
// Block state rotation is best effort: the common Bedrock direction
// properties (facing_direction, direction, weirdo_direction, pillar_axis,
// ground_sign_direction, minecraft:cardinal_direction and string facing
// properties) rotate correctly, which covers stairs, logs, signs, doors,
// furnaces, chests and most other rotatable blocks. Exotic properties (rail
// curves and similar) keep their state and may need manual fixing.
func (s *Structure) Rotate(quarters int) *Structure {
	quarters = ((quarters % 4) + 4) % 4
	out := s
	for range quarters {
		out = out.rotateOnce()
	}
	return out
}

// rotateOnce rotates a structure a single quarter turn clockwise.
func (s *Structure) rotateOnce() *Structure {
	sz := s.data.Size
	nsz := [3]int32{sz[2], sz[1], sz[0]}
	data, err := format.NewStructureData(nsz)
	if err != nil {
		panic("pile: rotate: " + err.Error()) // same volume; cannot fail
	}
	out := newStructure(data)
	out.reg, out.air, out.skipAir = s.reg, s.air, s.skipAir
	out.data.UserData = append([]byte(nil), s.data.UserData...)
	out.data.UnknownStates = append([]format.BlockState(nil), s.data.UnknownStates...)

	// Preserved unknown states, keyed by their source cell/layer/index.
	type cellKey struct {
		cell  int32
		layer uint8
		idx   uint16
	}
	srcUnknown := make(map[cellKey]uint32, len(s.data.Unknown))
	uniform := make(map[cellKey]uint32, len(s.data.Unknown))
	for _, u := range s.data.Unknown {
		if u.Index == format.WholeStorage {
			uniform[cellKey{cell: u.Section, layer: u.Layer}] = u.State
			continue
		}
		srcUnknown[cellKey{cell: u.Section, layer: u.Layer, idx: u.Index}] = u.State
	}
	layerN := uint8(2)
	for _, c := range s.data.Cells {
		if c == nil {
			continue
		}
		if l := uint8(len(c.Layers())); l > layerN {
			layerN = min(l, 4)
		}
	}

	// Clockwise from above: (x, z) -> (sizeZ-1-z, x).
	rotPos := func(x, z int32) (int32, int32) { return sz[2] - 1 - z, x }

	stateCache := map[uint32]uint32{}
	for x := int32(0); x < sz[0]; x++ {
		for y := int32(0); y < sz[1]; y++ {
			for z := int32(0); z < sz[2]; z++ {
				for layer := range layerN {
					rid := s.cellRid(x, y, z, layer)
					if rid == s.air {
						continue
					}
					nx, nz := rotPos(x, z)
					out.setLocal(int(nx), int(y), int(nz), layer, s.rotateRid(rid, stateCache))

					if len(s.data.Unknown) == 0 {
						continue
					}
					srcCell := int32(format.CellIndex(sz, x>>4, y>>4, z>>4))
					srcIdx := uint16(x&15)<<8 | uint16(z&15)<<4 | uint16(y&15)
					state, ok := srcUnknown[cellKey{cell: srcCell, layer: layer, idx: srcIdx}]
					if !ok {
						state, ok = uniform[cellKey{cell: srcCell, layer: layer}]
					}
					if ok {
						out.data.Unknown = append(out.data.Unknown, format.UnknownBlock{
							Section: int32(format.CellIndex(nsz, nx>>4, y>>4, nz>>4)),
							Layer:   layer,
							Index:   uint16(nx&15)<<8 | uint16(nz&15)<<4 | uint16(y&15),
							State:   state,
						})
					}
				}
			}
		}
	}
	for _, be := range s.data.BlockEntities {
		nx, nz := rotPos(be.Pos[0], be.Pos[2])
		data := deepCopyCompound(be.Data)
		npos := [3]int32{nx, be.Pos[1], nz}
		out.data.BlockEntities = append(out.data.BlockEntities, format.StructureBlockEntity{Pos: npos, Data: data})
		out.bes[npos] = data
	}
	for _, e := range s.data.Entities {
		data := deepCopyCompound(e)
		if pos, ok := entityPos(e); ok {
			// Continuous coordinates rotate as (x, z) -> (sizeZ - z, x).
			data["Pos"] = []any{
				float32(float64(sz[2]) - pos[2]),
				float32(pos[1]),
				float32(pos[0]),
			}
		}
		if yaw, ok := data["Yaw"].(float32); ok {
			data["Yaw"] = yaw + 90
		}
		out.data.Entities = append(out.data.Entities, data)
	}
	return out
}

// cellRid reads a runtime ID at a local position on any storage layer.
func (s *Structure) cellRid(x, y, z int32, layer uint8) uint32 {
	sz := s.data.Size
	if x < 0 || y < 0 || z < 0 || x >= sz[0] || y >= sz[1] || z >= sz[2] {
		return s.air
	}
	cell := s.data.Cells[format.CellIndex(sz, x>>4, y>>4, z>>4)]
	if cell == nil {
		return s.air
	}
	return cell.Block(uint8(x&15), uint8(y&15), uint8(z&15), layer)
}

// rotateRid rotates a block state's direction properties one quarter turn
// clockwise, resolving through the registry. Unrotatable or unresolvable
// states pass through unchanged. cache memoises per source runtime ID.
func (s *Structure) rotateRid(rid uint32, cache map[uint32]uint32) uint32 {
	if out, ok := cache[rid]; ok {
		return out
	}
	out := rid
	name, props, found := s.reg.RuntimeIDToState(rid)
	if found && len(props) > 0 {
		rotated := rotateStateProps(props)
		if rotated != nil {
			if nrid, ok := s.reg.StateToRuntimeID(name, rotated); ok {
				out = nrid
			}
		}
	}
	cache[rid] = out
	return out
}

// cardinalCW maps cardinal direction names one quarter turn clockwise.
var cardinalCW = map[string]string{
	"north": "east", "east": "south", "south": "west", "west": "north",
}

// rotateStateProps returns a rotated copy of props, or nil when nothing in
// the state rotates.
func rotateStateProps(props map[string]any) map[string]any {
	changed := false
	out := make(map[string]any, len(props))
	for k, v := range props {
		out[k] = v
		switch k {
		case "minecraft:cardinal_direction", "minecraft:block_face", "facing":
			if sv, ok := v.(string); ok {
				if nv, ok := cardinalCW[sv]; ok {
					out[k] = nv
					changed = true
				}
			}
		case "minecraft:facing_direction":
			if sv, ok := v.(string); ok {
				if nv, ok := cardinalCW[sv]; ok {
					out[k] = nv
					changed = true
				}
			}
		case "facing_direction":
			// 0 down, 1 up, 2 north, 3 south, 4 west, 5 east.
			if iv, ok := toInt32(v); ok {
				m := map[int32]int32{2: 5, 5: 3, 3: 4, 4: 2}
				if nv, ok := m[iv]; ok {
					out[k] = nv
					changed = true
				}
			}
		case "direction":
			// 0 south, 1 west, 2 north, 3 east (clockwise ring).
			if iv, ok := toInt32(v); ok && iv >= 0 && iv < 4 {
				out[k] = (iv + 1) % 4
				changed = true
			}
		case "weirdo_direction":
			// Stairs: 0 east, 1 west, 2 south, 3 north.
			if iv, ok := toInt32(v); ok {
				m := map[int32]int32{0: 2, 2: 1, 1: 3, 3: 0}
				if nv, ok := m[iv]; ok {
					out[k] = nv
					changed = true
				}
			}
		case "ground_sign_direction":
			// 16 steps around the circle.
			if iv, ok := toInt32(v); ok && iv >= 0 && iv < 16 {
				out[k] = (iv + 4) % 16
				changed = true
			}
		case "pillar_axis":
			if sv, ok := v.(string); ok {
				switch sv {
				case "x":
					out[k] = "z"
					changed = true
				case "z":
					out[k] = "x"
					changed = true
				}
			}
		}
	}
	if !changed {
		return nil
	}
	return out
}

// toInt32 normalises NBT integer property values.
func toInt32(v any) (int32, bool) {
	switch x := v.(type) {
	case int32:
		return x, true
	case uint8:
		return int32(x), true
	}
	return 0, false
}
