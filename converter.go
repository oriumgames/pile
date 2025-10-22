package pile

import (
	"bytes"
	"fmt"
	"math/bits"

	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
	"github.com/sandertv/gophertunnel/minecraft/nbt"
)

// chunkToColumn converts a Pile Chunk to a Dragonfly chunk.Column.
func chunkToColumn(c *Chunk, dimRange cube.Range) (*chunk.Column, error) {
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

		// Note: Skip light data restoration - let the world recalculate it
		// Light data in Dragonfly uses a special COW system that's difficult to restore
		// The world will automatically recalculate lighting when chunks are loaded
	}

	// Convert block entities
	blockEntities := make([]chunk.BlockEntity, 0, len(c.BlockEntities))
	for _, be := range c.BlockEntities {
		x, y, z := be.Position()
		pos := cube.Pos{int(x), int(y), int(z)}

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
		x, y, z := t.Position()
		var rid uint32
		if b, ok := world.BlockByName(t.Block, nil); ok {
			rid = world.BlockRuntimeID(b)
		} else {
			air, _ := world.BlockByName("minecraft:air", nil)
			rid = world.BlockRuntimeID(air)
		}
		scheduled = append(scheduled, chunk.ScheduledBlockUpdate{
			Pos:   cube.Pos{int(x), int(y), int(z)},
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
func convertSectionBlocks(ch *chunk.Chunk, section *Section, sectionY int16, airRID uint32) error {
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
func convertSectionBiomes(ch *chunk.Chunk, section *Section, sectionY int16) error {
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

// convertSectionLight converts light data from Pile to Dragonfly format.
func convertSectionLight(ch *chunk.Chunk, section *Section, sectionY int16) error {
	sub := ch.Sub()[ch.SubIndex(sectionY<<4)]

	// Set block light - iterate through all blocks in the section
	if len(section.BlockLight) == 2048 {
		for i := range 4096 {
			x := uint8(i & 0xF)
			y := uint8((i >> 8) & 0xF)
			z := uint8((i >> 4) & 0xF)

			// Extract 4-bit light value (2 values per byte)
			byteIdx := i / 2
			shift := uint(i%2) * 4
			lightLevel := (section.BlockLight[byteIdx] >> shift) & 0xF

			sub.SetBlockLight(x, y, z, lightLevel)
		}
	}

	// Set sky light
	if len(section.SkyLight) == 2048 {
		for i := range 4096 {
			x := uint8(i & 0xF)
			y := uint8((i >> 8) & 0xF)
			z := uint8((i >> 4) & 0xF)

			// Extract 4-bit light value
			byteIdx := i / 2
			shift := uint(i%2) * 4
			lightLevel := (section.SkyLight[byteIdx] >> shift) & 0xF

			sub.SetSkyLight(x, y, z, lightLevel)
		}
	}

	return nil
}

// columnToChunk converts a Dragonfly chunk.Column to a Pile Chunk.
func columnToChunk(col *chunk.Column, x, z int32, dimRange cube.Range) (*Chunk, error) {
	ch := col.Chunk

	// Calculate section count
	minSection := int32(dimRange[0] >> 4)
	maxSection := int32(dimRange[1] >> 4)
	sectionCount := int(maxSection - minSection)

	// Create Pile sections
	sections := make([]*Section, sectionCount)

	for i := range sectionCount {
		sub := ch.Sub()[i]

		if sub.Empty() {
			continue
		}

		section := &Section{}

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

		// Convert light data - extract light values block by block
		section.BlockLight = extractLightData(sub, true)
		section.SkyLight = extractLightData(sub, false)

		sections[i] = section
	}

	// Convert block entities
	blockEntities := make([]BlockEntity, 0, len(col.BlockEntities))
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
		relX := uint32(be.Pos.X() & 0xF)
		relZ := uint32(be.Pos.Z() & 0xF)
		packedXZ := relX | (relZ << 4)

		// Extract ID from NBT data if available
		id := "minecraft:unknown"
		if idVal, ok := be.Data["id"].(string); ok {
			id = idVal
		}

		blockEntities = append(blockEntities, BlockEntity{
			PackedXZ: packedXZ,
			Y:        int32(be.Pos.Y()),
			ID:       id,
			Data:     data,
		})
	}

	// Convert entities
	entities := make([]Entity, 0, len(col.Entities))
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

		entities = append(entities, Entity{
			ID:   id,
			Data: data,
		})
	}

	// Convert scheduled ticks
	ticks := make([]ScheduledTick, 0, len(col.ScheduledBlocks))
	for _, t := range col.ScheduledBlocks {
		relX := uint32(t.Pos.X() & 0xF)
		relZ := uint32(t.Pos.Z() & 0xF)
		packedXZ := relX | (relZ << 4)

		name, _, _ := chunk.RuntimeIDToState(t.Block)
		if name == "" {
			name = "minecraft:air"
		}

		ticks = append(ticks, ScheduledTick{
			PackedXZ: packedXZ,
			Y:        int32(t.Pos.Y()),
			Block:    name,
			Tick:     t.Tick,
		})
	}

	return &Chunk{
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

// extractLightData extracts light data from a sub-chunk.
func extractLightData(sub *chunk.SubChunk, blockLight bool) []byte {
	if sub.Empty() {
		return nil
	}

	// Check if we have light data
	// If not initialized, return nil (will be encoded as flag 0)
	defer func() {
		if r := recover(); r != nil {
			// Light data not initialized, return nil
		}
	}()

	lightData := make([]byte, 2048)
	hasNonZero := false

	for i := range 4096 {
		x := uint8(i & 0xF)
		y := uint8((i >> 8) & 0xF)
		z := uint8((i >> 4) & 0xF)

		var lightLevel uint8
		if blockLight {
			lightLevel = sub.BlockLight(x, y, z)
		} else {
			lightLevel = sub.SkyLight(x, y, z)
		}

		if lightLevel != 0 {
			hasNonZero = true
		}

		// Pack 2 light values per byte (4 bits each)
		byteIdx := i / 2
		shift := uint(i%2) * 4
		lightData[byteIdx] |= (lightLevel & 0xF) << shift
	}

	// If all zeros, return nil (will be encoded more efficiently)
	if !hasNonZero {
		return nil
	}

	return lightData
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
