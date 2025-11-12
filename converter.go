package pile

import (
	"bytes"
	"fmt"
	"math/bits"

	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
	"github.com/oriumgames/pile/format"
	"github.com/sandertv/gophertunnel/minecraft/nbt"
)

// chunkToColumn converts a Pile Chunk to a Dragonfly chunk.Column.
func chunkToColumn(c *format.Chunk, dimRange cube.Range) (*chunk.Column, error) {
	// Get air block and its runtime ID
	air, _ := world.BlockByName("minecraft:air", nil)
	airRID := world.BlockRuntimeID(air)

	// Create Dragonfly chunk
	ch := chunk.New(airRID, dimRange)

	// Convert sections
	for i, section := range c.Sections {
		// Skip nil or empty sections
		if section == nil {
			continue
		}

		// Calculate Y index for this section
		sectionY := int16(i) + int16(dimRange[0]>>4)

		// Convert blocks (skip if empty)
		if !section.IsEmpty() {
			if err := convertSectionBlocks(ch, section, sectionY, airRID); err != nil {
				return nil, fmt.Errorf("convert section %d blocks: %w", i, err)
			}
		}

		// Convert biomes
		if len(section.BiomePalette) > 0 {
			if err := convertSectionBiomes(ch, section, sectionY); err != nil {
				return nil, fmt.Errorf("convert section %d biomes: %w", i, err)
			}
		}
	}

	// Convert block entities
	blockEntities := make([]chunk.BlockEntity, 0, len(c.BlockEntities))
	for _, be := range c.BlockEntities {
		// Get local position within chunk
		localX, y, localZ := be.Position()
		// Convert to absolute world coordinates
		absX := int(c.X)*16 + int(localX)
		absZ := int(c.Z)*16 + int(localZ)
		pos := cube.Pos{absX, int(y), absZ}

		var data map[string]any
		if len(be.Data) > 0 {
			if err := nbt.NewDecoder(bytes.NewReader(be.Data)).Decode(&data); err != nil {
				return nil, fmt.Errorf("decode block entity NBT: %w", err)
			}
		}

		blockEntities = append(blockEntities, chunk.BlockEntity{
			Pos:  pos,
			Data: data,
		})
	}

	// Convert entities
	entities := make([]chunk.Entity, 0, len(c.Entities))
	for _, e := range c.Entities {
		var data map[string]any
		if len(e.Data) > 0 {
			if err := nbt.NewDecoder(bytes.NewReader(e.Data)).Decode(&data); err != nil {
				return nil, fmt.Errorf("decode entity NBT: %w", err)
			}
		}
		// Ensure identifier exists in NBT for world consumption.
		if data == nil {
			data = make(map[string]any)
		}
		if _, ok := data["identifier"].(string); !ok {
			if e.ID != "" {
				data["identifier"] = e.ID
			}
		}

		// Inject position, rotation, velocity back into NBT (Dragonfly format)
		data["Pos"] = []float32{
			e.Position[0],
			e.Position[1],
			e.Position[2],
		}
		data["Yaw"] = e.Rotation[0]
		data["Pitch"] = e.Rotation[1]
		data["Motion"] = []float32{
			e.Velocity[0],
			e.Velocity[1],
			e.Velocity[2],
		}

		// Preserve UniqueID if present in NBT.
		var id int64
		if v, ok := data["UniqueID"].(int64); ok {
			id = v
		}
		entities = append(entities, chunk.Entity{ID: id, Data: data})
	}

	// Convert scheduled ticks
	scheduled := make([]chunk.ScheduledBlockUpdate, 0, len(c.ScheduledTicks))
	for _, t := range c.ScheduledTicks {
		// Get local position within chunk
		localX, y, localZ := t.Position()
		// Convert to absolute world coordinates
		absX := int(c.X)*16 + int(localX)
		absZ := int(c.Z)*16 + int(localZ)
		var rid uint32
		if b, ok := world.BlockByName(t.Block, nil); ok {
			rid = world.BlockRuntimeID(b)
		} else {
			air, _ := world.BlockByName("minecraft:air", nil)
			rid = world.BlockRuntimeID(air)
		}
		scheduled = append(scheduled, chunk.ScheduledBlockUpdate{
			Pos:   cube.Pos{absX, int(y), absZ},
			Block: rid,
			Tick:  t.Tick,
		})
	}

	return &chunk.Column{
		Chunk:           ch,
		Entities:        entities,
		BlockEntities:   blockEntities,
		ScheduledBlocks: scheduled,
	}, nil
}

// convertSectionBlocks converts block data from Pile to Dragonfly format.
func convertSectionBlocks(ch *chunk.Chunk, section *format.Section, sectionY int16, airRID uint32) error {
	if len(section.BlockPalette) == 0 {
		return nil
	}

	// Convert palette strings to runtime IDs
	runtimePalette := make([]uint32, len(section.BlockPalette))
	for i, blockName := range section.BlockPalette {
		// Try to parse block name and get block
		block, ok := world.BlockByName(blockName, nil)
		if !ok {
			// Unknown block, use air
			block, _ = world.BlockByName("minecraft:air", nil)
		}
		runtimePalette[i] = world.BlockRuntimeID(block)
	}

	// If only one entry and it's air, skip
	if len(runtimePalette) == 1 && runtimePalette[0] == airRID {
		return nil
	}

	// Decode block indices
	bitsPerBlock := calculateBitsPerBlock(len(runtimePalette))
	indices := decodeIndices(section.BlockData, bitsPerBlock, 4096)

	// Set blocks in chunk
	baseY := sectionY << 4
	for i := range 4096 {
		x := uint8(i & 0xF)
		y := baseY + int16((i>>8)&0xF)
		z := uint8((i >> 4) & 0xF)

		var paletteIdx int
		if i < len(indices) {
			paletteIdx = indices[i]
		}
		if paletteIdx >= len(runtimePalette) {
			paletteIdx = 0
		}

		rid := runtimePalette[paletteIdx]
		if rid != airRID {
			ch.SetBlock(x, y, z, 0, rid)
		}
	}

	return nil
}

// convertSectionBiomes converts biome data from Pile to Dragonfly format.
func convertSectionBiomes(ch *chunk.Chunk, section *format.Section, sectionY int16) error {
	if len(section.BiomePalette) == 0 {
		return nil
	}

	// Convert palette strings to biome IDs
	biomePalette := make([]uint32, len(section.BiomePalette))
	for i, biomeName := range section.BiomePalette {
		biome, ok := world.BiomeByName(biomeName)
		if !ok || biome == nil {
			// Unknown biome, use plains
			biome, ok = world.BiomeByName("minecraft:plains")
			if !ok || biome == nil {
				// Last resort: use biome ID 1 (plains)
				biomePalette[i] = 1
				continue
			}
		}
		biomePalette[i] = uint32(biome.EncodeBiome())
	}

	// Decode biome indices
	bitsPerBiome := calculateBitsPerBlock(len(biomePalette))
	indices := decodeIndices(section.BiomeData, bitsPerBiome, 4096)

	// Set biomes in chunk
	baseY := sectionY << 4
	for i := range 4096 {
		x := uint8(i & 0xF)
		y := baseY + int16((i>>8)&0xF)
		z := uint8((i >> 4) & 0xF)

		var paletteIdx int
		if i < len(indices) {
			paletteIdx = indices[i]
		}
		if paletteIdx >= len(biomePalette) {
			paletteIdx = 0
		}

		biomeID := biomePalette[paletteIdx]
		ch.SetBiome(x, y, z, biomeID)
	}

	return nil
}

// columnToChunk converts a Dragonfly chunk.Column to a Pile Chunk.
func columnToChunk(col *chunk.Column, x, z int32, dimRange cube.Range) (*format.Chunk, error) {
	ch := col.Chunk

	// Calculate section count
	minSection := int32(dimRange[0] >> 4)
	maxSection := int32(dimRange[1] >> 4)
	sectionCount := int(maxSection - minSection)

	// Create Pile sections
	sections := make([]*format.Section, sectionCount)
	subs := ch.Sub()

	for i := range sectionCount {
		// Bounds check to prevent panic if chunk has fewer sections than expected
		if i >= len(subs) {
			break
		}
		sub := subs[i]

		if sub.Empty() {
			continue
		}

		section := &format.Section{}

		// Convert blocks (layer 0 only for now)
		if len(sub.Layers()) > 0 {
			storage := sub.Layer(0)
			blockPalette, blockData := convertStorageToPile(storage)
			section.BlockPalette = blockPalette
			section.BlockData = blockData
		}

		// Convert biomes - access through chunk's internal biome storage
		// Since biomes field is private, we need to extract them by reading individual biome values
		biomePalette, biomeData := extractBiomesFromChunk(ch, i)
		section.BiomePalette = biomePalette
		section.BiomeData = biomeData

		sections[i] = section
	}

	// Convert block entities
	blockEntities := make([]format.BlockEntity, 0, len(col.BlockEntities))
	for _, be := range col.BlockEntities {
		var data []byte
		if be.Data != nil {
			buf := new(bytes.Buffer)
			if err := nbt.NewEncoder(buf).Encode(be.Data); err != nil {
				return nil, fmt.Errorf("encode block entity NBT: %w", err)
			}
			data = buf.Bytes()
		}

		// Calculate relative position and pack
		relX := uint8(be.Pos.X() & 0xF)
		relZ := uint8(be.Pos.Z() & 0xF)
		packedXZ := relX | (relZ << 4)

		// Extract ID from NBT data if available
		id := "minecraft:unknown"
		if idVal, ok := be.Data["id"].(string); ok {
			id = idVal
		}

		blockEntities = append(blockEntities, format.BlockEntity{
			PackedXZ: packedXZ,
			Y:        int32(be.Pos.Y()),
			ID:       id,
			Data:     data,
		})
	}

	// Convert entities
	entities := make([]format.Entity, 0, len(col.Entities))
	for _, e := range col.Entities {
		var data []byte
		if e.Data != nil {
			// Ensure UniqueID is present in NBT to preserve across providers.
			e.Data["UniqueID"] = e.ID
			buf := new(bytes.Buffer)
			if err := nbt.NewEncoder(buf).Encode(e.Data); err != nil {
				return nil, fmt.Errorf("encode entity NBT: %w", err)
			}
			data = buf.Bytes()
		}

		id := "minecraft:unknown"
		if idVal, ok := e.Data["identifier"].(string); ok {
			id = idVal
		}

		// Extract position, rotation, velocity from NBT (Dragonfly format)
		var position [3]float32
		var rotation [2]float32
		var velocity [3]float32

		if e.Data != nil {
			// Position: "Pos" [float32, float32, float32]
			if pos, ok := e.Data["Pos"].([]float32); ok && len(pos) == 3 {
				position = [3]float32{pos[0], pos[1], pos[2]}
			}
			// Rotation: "Yaw" and "Pitch" (float32)
			if yaw, ok := e.Data["Yaw"].(float32); ok {
				rotation[0] = yaw
			}
			if pitch, ok := e.Data["Pitch"].(float32); ok {
				rotation[1] = pitch
			}
			// Velocity: "Motion" [float32, float32, float32]
			if motion, ok := e.Data["Motion"].([]float32); ok && len(motion) == 3 {
				velocity = [3]float32{motion[0], motion[1], motion[2]}
			}
		}

		entities = append(entities, format.Entity{
			ID:       id,
			Position: position,
			Rotation: rotation,
			Velocity: velocity,
			Data:     data,
		})
	}

	// Convert scheduled ticks
	ticks := make([]format.ScheduledTick, 0, len(col.ScheduledBlocks))
	for _, t := range col.ScheduledBlocks {
		relX := uint8(t.Pos.X() & 0xF)
		relZ := uint8(t.Pos.Z() & 0xF)
		packedXZ := relX | (relZ << 4)

		name, _, _ := chunk.RuntimeIDToState(t.Block)
		if name == "" {
			name = "minecraft:air"
		}

		ticks = append(ticks, format.ScheduledTick{
			PackedXZ: packedXZ,
			Y:        int32(t.Pos.Y()),
			Block:    name,
			Tick:     t.Tick,
		})
	}

	return &format.Chunk{
		X:              x,
		Z:              z,
		Sections:       sections,
		BlockEntities:  blockEntities,
		Entities:       entities,
		ScheduledTicks: ticks,
	}, nil
}

// convertStorageToPile converts a Dragonfly PalettedStorage to Pile format.
func convertStorageToPile(storage *chunk.PalettedStorage) ([]string, []int64) {
	palette := storage.Palette()
	paletteLen := palette.Len()

	// Convert runtime IDs to block names
	blockNames := make([]string, paletteLen)
	for i := range paletteLen {
		rid := palette.Value(uint16(i))
		name, _, _ := chunk.RuntimeIDToState(rid)
		if name == "" {
			name = "minecraft:air"
		}
		blockNames[i] = name
	}

	// Encode indices
	bitsPerBlock := calculateBitsPerBlock(paletteLen)
	indices := make([]int, 4096)
	for i := range 4096 {
		x := uint8(i & 0xF)
		y := uint8((i >> 8) & 0xF)
		z := uint8((i >> 4) & 0xF)
		rid := storage.At(x, y, z)
		indices[i] = int(palette.Index(rid))
	}

	data := encodeIndices(indices, bitsPerBlock)
	return blockNames, data
}

// extractBiomesFromChunk extracts biome data from a chunk at a specific section index.
func extractBiomesFromChunk(ch *chunk.Chunk, sectionIdx int) ([]string, []int64) {
	// Build a map of biome ID to palette index for O(1) lookups
	biomeMap := make(map[uint32]int)      // Maps biome ID to palette index
	biomePaletteList := make([]string, 0) // Ordered list of biome names
	biomeIndices := make([]int, 4096)

	// Calculate base Y from chunk's range and section index
	chunkRange := ch.Range()
	baseY := int16(chunkRange[0]) + (int16(sectionIdx) << 4)

	for i := range 4096 {
		x := uint8(i & 0xF)
		y := baseY + int16((i>>8)&0xF)
		z := uint8((i >> 4) & 0xF)

		biomeID := ch.Biome(x, y, z)

		// Check if this biome is already in our palette
		paletteIdx, exists := biomeMap[biomeID]
		if !exists {
			biome, ok := world.BiomeByID(int(biomeID))
			if !ok || biome == nil {
				// Fallback to plains if biome not found
				biome, ok = world.BiomeByName("minecraft:plains")
				if !ok || biome == nil {
					// Last resort fallback - use hardcoded plains
					paletteIdx = len(biomePaletteList)
					biomeMap[biomeID] = paletteIdx
					biomePaletteList = append(biomePaletteList, "minecraft:plains")
					biomeIndices[i] = paletteIdx
					continue
				}
			}
			biomeName := biome.String()
			paletteIdx = len(biomePaletteList)
			biomeMap[biomeID] = paletteIdx
			biomePaletteList = append(biomePaletteList, biomeName)
		}

		biomeIndices[i] = paletteIdx
	}

	// Encode indices
	bitsPerBiome := calculateBitsPerBlock(len(biomePaletteList))
	data := encodeIndices(biomeIndices, bitsPerBiome)

	return biomePaletteList, data
}

// calculateBitsPerBlock calculates the number of bits needed for a palette of the given size.
func calculateBitsPerBlock(paletteSize int) int {
	if paletteSize <= 1 {
		return 0
	}
	return bits.Len(uint(paletteSize - 1))
}

// encodeIndices encodes block indices into int64 array with the given bits per block.
func encodeIndices(indices []int, bitsPerBlock int) []int64 {
	if bitsPerBlock == 0 || len(indices) == 0 {
		return nil
	}

	// Calculate how many values fit in one int64
	valuesPerLong := 64 / bitsPerBlock
	longCount := (len(indices) + valuesPerLong - 1) / valuesPerLong

	result := make([]int64, longCount)
	for i, idx := range indices {
		longIdx := i / valuesPerLong
		bitOffset := (i % valuesPerLong) * bitsPerBlock
		result[longIdx] |= int64(idx) << bitOffset
	}

	return result
}

// decodeIndices decodes block indices from int64 array.
func decodeIndices(data []int64, bitsPerBlock, count int) []int {
	if bitsPerBlock == 0 || len(data) == 0 {
		// All values are 0 (first palette entry)
		return make([]int, count)
	}

	valuesPerLong := 64 / bitsPerBlock
	mask := (1 << bitsPerBlock) - 1

	indices := make([]int, count)
	for i := range count {
		longIdx := i / valuesPerLong
		if longIdx >= len(data) {
			break
		}
		bitOffset := (i % valuesPerLong) * bitsPerBlock
		indices[i] = int((data[longIdx] >> bitOffset) & int64(mask))
	}

	return indices
}
