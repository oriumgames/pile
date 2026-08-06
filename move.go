package pile

import (
	"errors"
	"math"

	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
	"github.com/oriumgames/pile/format"
)

// ErrWouldClip is returned by MoveWorld when the offset would push content
// outside the dimension's vertical range and MoveOptions.Clip is false.
var ErrWouldClip = errors.New("pile: move would clip content outside the world's vertical range")

// MoveOptions configures MoveWorld.
type MoveOptions struct {
	// Offset is the block translation applied to everything in the world.
	Offset cube.Pos
	// Clip permits cutting content that lands outside the vertical range.
	// Without it, a move that would lose anything fails with ErrWouldClip.
	Clip bool
	// DryRun computes the report without writing anything.
	DryRun bool
	// Backup copies the current files into snapshots/pre-move before writing.
	Backup bool
	// Registry used for block resolution; nil uses world.DefaultBlockRegistry.
	Registry world.BlockRegistry
}

// MoveReport describes what a move did (or, for a dry run, would do).
type MoveReport struct {
	Offset cube.Pos
	// Chunks is the number of columns translated across all dimensions.
	Chunks int
	// FastPath is true when the offset was chunk-aligned horizontally with no
	// vertical component, so blocks were re-keyed without being rewritten.
	FastPath bool

	// Clipped content counts (non-air blocks, and entries dropped).
	ClippedBlocks        int
	ClippedBlockEntities int
	ClippedEntities      int
	ClippedTicks         int
}

// ClippedTotal sums all clipped content.
func (r *MoveReport) ClippedTotal() int {
	return r.ClippedBlocks + r.ClippedBlockEntities + r.ClippedEntities + r.ClippedTicks
}

// MoveWorld translates a pile world on disk: all blocks, biomes, entities,
// block entities, scheduled ticks, chunk metadata, plus spawn, markers and
// world border. Files keep their mode (solid or indexed) and are replaced
// atomically. The move is all-or-nothing across dimensions.
func MoveWorld(dir string, opt MoveOptions) (*MoveReport, error) {
	reg := opt.Registry
	if reg == nil {
		reg = world.DefaultBlockRegistry
	}
	reg.Finalize()
	report := &MoveReport{Offset: opt.Offset, FastPath: isFastOffset(opt.Offset)}

	wf, err := LoadWorldFiles(dir, reg)
	if err != nil {
		return nil, err
	}

	// Translate all dimensions.
	for i := range wf.Dims {
		out, err := translateColumns(wf.Dims[i].Columns, reg, opt.Offset, report)
		if err != nil {
			return nil, err
		}
		wf.Dims[i].Columns = out
		report.Chunks += len(out)
	}
	if report.ClippedTotal() > 0 && !opt.Clip {
		return report, ErrWouldClip
	}
	if opt.DryRun {
		return report, nil
	}

	// Translate metadata.
	if wf.Settings, err = moveSettings(wf.Settings, opt.Offset); err != nil {
		return nil, err
	}
	if wf.Markers, err = moveMarkers(wf.Markers, opt.Offset); err != nil {
		return nil, err
	}
	wf.Border = moveBorder(wf.Border, opt.Offset)

	// Backup, then write every file back atomically in its original mode.
	if opt.Backup {
		if err := wf.Backup(dir, "pre-move"); err != nil {
			return nil, err
		}
	}
	return report, wf.Write(dir, reg)
}

// maxSourceLayers returns the highest storage layer count across a chunk's
// sections, bounded by what the format accepts.
func maxSourceLayers(ch *chunk.Chunk) uint8 {
	n := 1
	for _, sub := range ch.Sub() {
		if l := len(sub.Layers()); l > n {
			n = l
		}
	}
	if n > 4 {
		n = 4
	}
	return uint8(n)
}

// isFastOffset reports whether the offset allows chunk re-keying without
// rewriting block data.
func isFastOffset(off cube.Pos) bool {
	return off.Y() == 0 && off.X()%16 == 0 && off.Z()%16 == 0
}

// translateColumns translates one dimension's columns, accumulating clip
// counts into report.
func translateColumns(cols []format.Column, reg world.BlockRegistry, off cube.Pos, report *MoveReport) ([]format.Column, error) {
	if isFastOffset(off) {
		out := make([]format.Column, len(cols))
		for i, c := range cols {
			moveColumnExtras(c.Col, off, report)
			out[i] = format.Column{
				X: c.X + int32(off.X()>>4), Z: c.Z + int32(off.Z()>>4),
				Col: c.Col, UserData: c.UserData,
				Unknown: c.Unknown, UnknownTicks: c.UnknownTicks, UnknownStates: c.UnknownStates,
			}
		}
		return out, nil
	}

	air := reg.AirRuntimeID()
	targets := map[[2]int32]*format.Column{}
	target := func(cx, cz int32, rng cube.Range) *format.Column {
		k := [2]int32{cx, cz}
		t, ok := targets[k]
		if !ok {
			t = &format.Column{X: cx, Z: cz, Col: &chunk.Column{Chunk: chunk.New(reg, rng)}}
			targets[k] = t
		}
		return t
	}

	for _, c := range cols {
		ch := c.Col.Chunk
		r := ch.Range()
		layerN := maxSourceLayers(ch)

		// Preserved unknown states, keyed by their source position so they
		// can be re-anchored at the destination. Each destination column
		// receives this source column's state table once; stateBase gives the
		// offset its references must be shifted by.
		type srcKey struct {
			sec   int32
			layer uint8
			idx   uint16
		}
		srcUnknown := make(map[srcKey]uint32, len(c.Unknown))
		uniform := make(map[srcKey]uint32, len(c.Unknown))
		for _, u := range c.Unknown {
			k := srcKey{sec: u.Section, layer: u.Layer, idx: u.Index}
			if u.Index == format.WholeStorage {
				uniform[srcKey{sec: u.Section, layer: u.Layer}] = u.State
				continue
			}
			srcUnknown[k] = u.State
		}
		bases := map[*format.Column]uint32{}
		stateBase := func(t *format.Column) uint32 {
			if base, ok := bases[t]; ok {
				return base
			}
			base := uint32(len(t.UnknownStates))
			t.UnknownStates = append(t.UnknownStates, c.UnknownStates...)
			bases[t] = base
			return base
		}

		for lx := range uint8(16) {
			for lz := range uint8(16) {
				wx := int(c.X)*16 + int(lx) + off.X()
				wz := int(c.Z)*16 + int(lz) + off.Z()
				tcx, tcz := int32(wx>>4), int32(wz>>4)
				tx, tz := uint8(wx&15), uint8(wz&15)
				var tcol *format.Column
				for yi := r[0]; yi <= r[1]; yi++ {
					y := int16(yi)
					wy := yi + off.Y()
					if wy < r[0] || wy > r[1] {
						for layer := range layerN {
							if ch.Block(lx, y, lz, layer) != air {
								report.ClippedBlocks++
							}
						}
						continue
					}
					if tcol == nil {
						tcol = target(tcx, tcz, r)
					}
					tch := tcol.Col.Chunk
					tch.SetBiome(tx, int16(wy), tz, ch.Biome(lx, y, lz))
					srcSec := int32(int(y) >> 4)
					srcIdx := uint16(lx)<<8 | uint16(lz)<<4 | uint16(uint16(y)&15)
					dstSec := int32(wy >> 4)
					dstIdx := uint16(tx)<<8 | uint16(tz)<<4 | uint16(uint16(wy)&15)
					for layer := range layerN {
						rid := ch.Block(lx, y, lz, layer)
						if rid == air {
							continue
						}
						tch.SetBlock(tx, int16(wy), tz, layer, rid)
						state, ok := srcUnknown[srcKey{sec: srcSec, layer: layer, idx: srcIdx}]
						if !ok {
							state, ok = uniform[srcKey{sec: srcSec, layer: layer}]
						}
						if ok {
							tcol.Unknown = append(tcol.Unknown, format.UnknownBlock{
								Section: dstSec, Layer: layer, Index: dstIdx,
								State: stateBase(tcol) + state,
							})
						}
					}
				}
			}
		}

		// Entities, block entities and scheduled ticks land in the chunk
		// containing their translated position.
		r0, r1 := r[0], r[1]
		for _, be := range c.Col.BlockEntities {
			pos := be.Pos.Add(off)
			if pos.Y() < r0 || pos.Y() > r1 {
				report.ClippedBlockEntities++
				continue
			}
			t := target(int32(pos.X()>>4), int32(pos.Z()>>4), r)
			data := deepCopyCompound(be.Data)
			data["x"], data["y"], data["z"] = int32(pos.X()), int32(pos.Y()), int32(pos.Z())
			t.Col.BlockEntities = append(t.Col.BlockEntities, chunk.BlockEntity{Pos: pos, Data: data})
		}
		for _, e := range c.Col.Entities {
			pos, ok := entityPos(e.Data)
			if !ok {
				// An entity whose Pos cannot be read cannot be translated,
				// but dropping it would lose data: keep it, untouched, in the
				// column its source chunk maps to.
				t := target(int32((int(c.X)*16+off.X())>>4), int32((int(c.Z)*16+off.Z())>>4), r)
				t.Col.Entities = append(t.Col.Entities, chunk.Entity{ID: e.ID, Data: deepCopyCompound(e.Data)})
				continue
			}
			wy := pos[1] + float64(off.Y())
			if wy < float64(r0) || wy > float64(r1)+1 {
				report.ClippedEntities++
				continue
			}
			wx, wz := pos[0]+float64(off.X()), pos[2]+float64(off.Z())
			t := target(int32(int(math.Floor(wx))>>4), int32(int(math.Floor(wz))>>4), r)
			data := deepCopyCompound(e.Data)
			data["Pos"] = []any{float32(wx), float32(wy), float32(wz)}
			t.Col.Entities = append(t.Col.Entities, chunk.Entity{ID: e.ID, Data: data})
		}
		type tickKey struct {
			pos [3]int32
			at  int64
		}
		tickState := make(map[tickKey]uint32, len(c.UnknownTicks))
		for _, ut := range c.UnknownTicks {
			tickState[tickKey{pos: ut.Pos, at: ut.At}] = ut.State
		}
		for _, st := range c.Col.ScheduledBlocks {
			pos := st.Pos.Add(off)
			if pos.Y() < r0 || pos.Y() > r1 {
				report.ClippedTicks++
				continue
			}
			t := target(int32(pos.X()>>4), int32(pos.Z()>>4), r)
			src := tickKey{pos: [3]int32{int32(st.Pos.X()), int32(st.Pos.Y()), int32(st.Pos.Z())}, at: st.Tick}
			if state, ok := tickState[src]; ok {
				t.UnknownTicks = append(t.UnknownTicks, format.UnknownTick{
					Pos:   [3]int32{int32(pos.X()), int32(pos.Y()), int32(pos.Z())},
					At:    st.Tick,
					State: stateBase(t) + state,
				})
			}
			t.Col.ScheduledBlocks = append(t.Col.ScheduledBlocks, chunk.ScheduledBlockUpdate{
				Pos: pos, Block: st.Block, Tick: st.Tick,
			})
		}
		if len(c.UserData) > 0 {
			// Chunk metadata follows the chunk containing the translated
			// minimum corner (unique per source chunk).
			t := target(int32((int(c.X)*16+off.X())>>4), int32((int(c.Z)*16+off.Z())>>4), r)
			t.UserData = cloneBytes(c.UserData)
		}
		// Column tick follows the same rule.
		if c.Col.Tick != 0 {
			t := target(int32((int(c.X)*16+off.X())>>4), int32((int(c.Z)*16+off.Z())>>4), r)
			t.Col.Tick = c.Col.Tick
		}
	}

	out := make([]format.Column, 0, len(targets))
	for _, t := range targets {
		out = append(out, *t)
	}
	return out, nil
}

// moveColumnExtras translates entity, block entity and scheduled tick
// positions in place (fast path; the chunk itself is untouched).
func moveColumnExtras(col *chunk.Column, off cube.Pos, _ *MoveReport) {
	for i, be := range col.BlockEntities {
		pos := be.Pos.Add(off)
		col.BlockEntities[i].Pos = pos
		if be.Data != nil {
			be.Data["x"], be.Data["y"], be.Data["z"] = int32(pos.X()), int32(pos.Y()), int32(pos.Z())
		}
	}
	for _, e := range col.Entities {
		if pos, ok := entityPos(e.Data); ok {
			e.Data["Pos"] = []any{
				float32(pos[0] + float64(off.X())),
				float32(pos[1] + float64(off.Y())),
				float32(pos[2] + float64(off.Z())),
			}
		}
	}
	for i := range col.ScheduledBlocks {
		col.ScheduledBlocks[i].Pos = col.ScheduledBlocks[i].Pos.Add(off)
	}
}

// moveSettings translates the spawn position inside a settings blob.
func moveSettings(blob []byte, off cube.Pos) ([]byte, error) {
	s, extra, err := settingsFromNBT(blob)
	if err != nil {
		return nil, err
	}
	s.Spawn = s.Spawn.Add(off)
	return settingsToNBT(s, extra)
}

// moveMarkers translates all marker positions.
func moveMarkers(blob []byte, off cube.Pos) ([]byte, error) {
	ms, err := markersFromNBT(blob)
	if err != nil {
		return nil, err
	}
	for i := range ms {
		ms[i].Pos[0] += float64(off.X())
		ms[i].Pos[1] += float64(off.Y())
		ms[i].Pos[2] += float64(off.Z())
	}
	return markersToNBT(ms)
}

// moveBorder translates a world border blob ({min: [2]int32, max: [2]int32})
// horizontally. Blobs that do not match the shape pass through unchanged.
func moveBorder(blob []byte, off cube.Pos) []byte {
	if len(blob) == 0 {
		return nil
	}
	m, err := format.UnmarshalNBT(blob)
	if err != nil {
		return blob
	}
	shift := func(v any) (any, bool) {
		// The border schema fixes these as int arrays, so the translated
		// value must stay a fixed-size array: a slice would re-encode as a
		// TAG_List and no longer match the schema.
		switch pair := v.(type) {
		case []int32:
			if len(pair) == 2 {
				return [2]int32{pair[0] + int32(off.X()), pair[1] + int32(off.Z())}, true
			}
		case [2]int32:
			return [2]int32{pair[0] + int32(off.X()), pair[1] + int32(off.Z())}, true
		}
		return v, false
	}
	minV, ok1 := shift(m["min"])
	maxV, ok2 := shift(m["max"])
	if !ok1 || !ok2 {
		return blob
	}
	m["min"], m["max"] = minV, maxV
	out, err := format.MarshalNBT(m)
	if err != nil {
		return blob
	}
	return out
}
