package pile

import (
	"bytes"
	"fmt"

	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/sandertv/gophertunnel/minecraft/nbt"
)

// encodeWorld encodes a World into a buffer.
func encodeWorld(buf *buffer, w *World) {
	// Write section range
	buf.WriteInt32(w.MinSection)
	buf.WriteInt32(w.MaxSection)

	// Write user data
	buf.WriteBytes(w.UserData)

	// Write chunks
	chunks := w.Chunks()
	chunkCount := int64(len(chunks))
	buf.WriteVarInt(chunkCount)

	for _, chunk := range chunks {
		encodeChunk(buf, chunk, w.MinSection, w.MaxSection)
	}
}

// encodeChunk encodes a Chunk into a buffer.
func encodeChunk(buf *buffer, c *Chunk, minSection, maxSection int32) {
	// Write coordinates
	buf.WriteInt32(c.X)
	buf.WriteInt32(c.Z)

	// Calculate section count
	sectionCount := int(maxSection - minSection)

	// Write sections (pad with empty sections if needed)
	for i := range sectionCount {
		if i < len(c.Sections) && c.Sections[i] != nil {
			encodeSection(buf, c.Sections[i])
		} else {
			encodeEmptySection(buf)
		}
	}

	// Write block entities
	buf.WriteVarInt(int64(len(c.BlockEntities)))
	for _, be := range c.BlockEntities {
		encodeBlockEntity(buf, &be)
	}

	// Write entities
	// Note: Position/rotation/velocity should be present in the NBT payload as part of e.Data.
	buf.WriteVarInt(int64(len(c.Entities)))
	for _, e := range c.Entities {
		// Entity identifier and UUID are written explicitly for fast indexing.
		buf.WriteString(e.ID)
		buf.WriteString(e.UUID.String())
		buf.WriteBytes(e.Data)
	}

	// Write scheduled ticks (v4)
	buf.WriteVarInt(int64(len(c.ScheduledTicks)))
	for _, t := range c.ScheduledTicks {
		buf.WriteUInt32(t.PackedXZ)
		buf.WriteInt32(t.Y)
		buf.WriteString(t.Block)
		buf.WriteVarInt(t.Tick)
	}

	// Write heightmaps (currently empty)
	buf.WriteBytes(c.Heightmaps)

	// Write user data
	buf.WriteBytes(c.UserData)
}

// encodeSection encodes a Section into a buffer.
func encodeSection(buf *buffer, s *Section) {
	// Write block palette
	buf.WriteVarInt(int64(len(s.BlockPalette)))
	for _, block := range s.BlockPalette {
		buf.WriteString(block)
	}

	// Write block data
	buf.WriteVarInt(int64(len(s.BlockData)))
	for _, val := range s.BlockData {
		buf.WriteInt64(val)
	}

	// Write biome palette
	buf.WriteVarInt(int64(len(s.BiomePalette)))
	for _, biome := range s.BiomePalette {
		buf.WriteString(biome)
	}

	// Write biome data
	buf.WriteVarInt(int64(len(s.BiomeData)))
	for _, val := range s.BiomeData {
		buf.WriteInt64(val)
	}

	// Write light data
	writeLightData(buf, s.BlockLight)
	writeLightData(buf, s.SkyLight)
}

// encodeEmptySection encodes an empty section (all air).
func encodeEmptySection(buf *buffer) {
	// Empty block palette
	buf.WriteVarInt(1)
	buf.WriteString("minecraft:air")
	buf.WriteVarInt(0) // No block data needed for single palette entry

	// Empty biome palette
	buf.WriteVarInt(1)
	buf.WriteString("minecraft:plains")
	buf.WriteVarInt(0) // No biome data needed

	// No light data
	buf.WriteInt8(0) // No block light
	buf.WriteInt8(0) // No sky light
}

// writeLightData writes light data to the buffer.
// Format: byte flag (0=none, 1=all zeros, 2=all 15s, 3=variable data)
func writeLightData(buf *buffer, light []byte) {
	if len(light) == 0 {
		buf.WriteInt8(0) // No light data
		return
	}

	// Check if all zeros
	allZero := true
	allFifteen := true
	for _, b := range light {
		if b != 0 {
			allZero = false
		}
		if b != 0xFF {
			allFifteen = false
		}
	}

	if allZero {
		buf.WriteInt8(1)
		return
	}
	if allFifteen {
		buf.WriteInt8(2)
		return
	}

	// Variable data
	buf.WriteInt8(3)
	_, _ = buf.Write(light)
}

// encodeBlockEntity encodes a BlockEntity into a buffer.
func encodeBlockEntity(buf *buffer, be *BlockEntity) {
	buf.WriteUInt32(be.PackedXZ)
	buf.WriteInt32(be.Y)
	buf.WriteString(be.ID)
	buf.WriteBytes(be.Data)
}

// encodeSettings encodes world settings to bytes.
func encodeSettings(s *Settings) []byte {
	buf := new(bytes.Buffer)
	// Use NBT encoding for settings
	data := map[string]any{
		"name":            s.Name,
		"spawnX":          int32(s.Spawn.X()),
		"spawnY":          int32(s.Spawn.Y()),
		"spawnZ":          int32(s.Spawn.Z()),
		"time":            s.Time,
		"timeCycle":       s.TimeCycle,
		"rainTime":        s.RainTime,
		"raining":         s.Raining,
		"thunderTime":     s.ThunderTime,
		"thundering":      s.Thundering,
		"weatherCycle":    s.WeatherCycle,
		"currentTick":     s.CurrentTick,
		"defaultGameMode": int32(s.DefaultGameMode),
		"difficulty":      int32(s.Difficulty),
	}

	_ = nbt.NewEncoder(buf).Encode(data)
	return buf.Bytes()
}

// decodeSettings decodes world settings from bytes.
func decodeSettings(data []byte, s *Settings) error {
	if len(data) == 0 {
		return fmt.Errorf("no settings data")
	}

	var m map[string]any
	if err := nbt.NewDecoder(bytes.NewReader(data)).Decode(&m); err != nil {
		return err
	}

	if name, ok := m["name"].(string); ok {
		s.Name = name
	}
	if x, ok := m["spawnX"].(int32); ok {
		if y, ok := m["spawnY"].(int32); ok {
			if z, ok := m["spawnZ"].(int32); ok {
				s.Spawn = cube.Pos{int(x), int(y), int(z)}
			}
		}
	}
	if t, ok := m["time"].(int64); ok {
		s.Time = t
	}
	if tc, ok := m["timeCycle"].(uint8); ok {
		s.TimeCycle = tc != 0
	}
	if rt, ok := m["rainTime"].(int64); ok {
		s.RainTime = rt
	}
	if r, ok := m["raining"].(uint8); ok {
		s.Raining = r != 0
	}
	if tt, ok := m["thunderTime"].(int64); ok {
		s.ThunderTime = tt
	}
	if t, ok := m["thundering"].(uint8); ok {
		s.Thundering = t != 0
	}
	if wc, ok := m["weatherCycle"].(uint8); ok {
		s.WeatherCycle = wc != 0
	}
	if ct, ok := m["currentTick"].(int64); ok {
		s.CurrentTick = ct
	}
	if gm, ok := m["defaultGameMode"].(int32); ok {
		s.DefaultGameMode = gm
	}
	if d, ok := m["difficulty"].(int32); ok {
		s.Difficulty = d
	}

	return nil
}
