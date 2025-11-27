package main

import (
	"fmt"
	"os"
	_ "unsafe"

	"github.com/df-mc/dragonfly/server/world"
	"github.com/google/uuid"
	"github.com/oriumgames/crocon"
	"github.com/oriumgames/nbt"
	pileformat "github.com/oriumgames/pile/format"
	schemformat "github.com/oriumgames/schem/format"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
)

func main() {
	// Parse command-line arguments
	if len(os.Args) < 3 {
		fmt.Println("Usage: convert <input.schem> <output.pile>")
		fmt.Println("Example: convert lobby.schem overworld.pile")
		os.Exit(1)
	}

	inputFile := os.Args[1]
	outputFile := os.Args[2]

	f, err := os.Open(inputFile)
	if err != nil {
		panic(err)
	}
	defer f.Close()

	schematic, err := schemformat.Read(f)
	if err != nil {
		panic(err)
	}

	world := pileformat.NewWorld(-4, 20)

	c, _ := crocon.NewConverter()

	width, height, length := schematic.Dimensions()
	offsetX, offsetY, offsetZ := schematic.Offset()

	fmt.Printf("Converting schematic: %dx%dx%d (offset: %d,%d,%d)\n", width, height, length, offsetX, offsetY, offsetZ)

	fromVersion := schematic.Version()
	if fromVersion == "" {
		fmt.Println("Warning: schematic has no version, skipping conversion")
		return
	}

	totalBlocks := width * height * length
	processedBlocks := 0
	lastPercent := -1

	// Convert blocks and biomes
	fmt.Println("Converting blocks and biomes...")
	for x := range width {
		for y := range height {
			for z := range length {
				processedBlocks++
				percent := (processedBlocks * 100) / totalBlocks
				if percent != lastPercent && percent%5 == 0 {
					fmt.Printf("  Progress: %d%% (%d/%d blocks)\n", percent, processedBlocks, totalBlocks)
					lastPercent = percent
				}
				worldX := x + offsetX
				worldY := y + offsetY
				worldZ := z + offsetZ

				chunkX := int32(worldX >> 4)
				chunkZ := int32(worldZ >> 4)

				// Get or create chunk
				chunk := world.Chunk(chunkX, chunkZ)
				if chunk == nil {
					sectionCount := int(world.MaxSection - world.MinSection)
					chunk = &pileformat.Chunk{
						X:              chunkX,
						Z:              chunkZ,
						Sections:       make([]*pileformat.Section, sectionCount),
						BlockEntities:  []pileformat.BlockEntity{},
						Entities:       []pileformat.Entity{},
						ScheduledTicks: []pileformat.ScheduledTick{},
						UserData:       []byte{},
					}
					world.SetChunk(chunk)
				}

				// Convert block
				state := schematic.Block(x, y, z)
				if state != nil && state.Name != "minecraft:air" && state.Name != "air" {
					if err := convertBlock(c, chunk, world, worldX, worldY, worldZ, state, fromVersion); err != nil {
						fmt.Printf("Warning: failed to convert block at (%d,%d,%d): %v\n", worldX, worldY, worldZ, err)
					}
				}

				// Convert biome
				biome := schematic.Biome(x, y, z)
				if biome != "" {
					if err := convertBiome(c, chunk, world, worldX, worldY, worldZ, biome, fromVersion); err != nil {
						fmt.Printf("Warning: failed to convert biome at (%d,%d,%d): %v\n", worldX, worldY, worldZ, err)
					}
				}
			}
		}
	}

	fmt.Println("Converting block entities...")
	processedBE := 0
	// Convert block entities
	for x := range width {
		for y := range height {
			for z := range length {
				be := schematic.BlockEntity(x, y, z)
				if be == nil {
					continue
				}

				worldX := x + offsetX
				worldY := y + offsetY
				worldZ := z + offsetZ

				chunkX := int32(worldX >> 4)
				chunkZ := int32(worldZ >> 4)
				chunk := world.Chunk(chunkX, chunkZ)
				if chunk == nil {
					continue
				}

				if err := convertBlockEntity(c, chunk, worldX, worldY, worldZ, be, fromVersion); err != nil {
					fmt.Printf("Warning: failed to convert block entity %v at (%d,%d,%d): %v\n", be.ID, worldX, worldY, worldZ, err)
				} else {
					processedBE++
				}
			}
		}
	}
	fmt.Printf("Converted %d block entities\n", processedBE)

	// Convert entities
	entities := schematic.Entities()
	fmt.Printf("Converting %d entities...\n", len(entities))
	processedEntities := 0
	//for i, entity := range entities {
	//	worldX := entity.Pos[0] + float64(offsetX)
	//	worldY := entity.Pos[1] + float64(offsetY)
	//	worldZ := entity.Pos[2] + float64(offsetZ)
	//
	//	chunkX := int32(int(worldX) >> 4)
	//	chunkZ := int32(int(worldZ) >> 4)
	//	chunk := world.Chunk(chunkX, chunkZ)
	//	if chunk == nil {
	//		sectionCount := int(world.MaxSection - world.MinSection)
	//		chunk = &pileformat.Chunk{
	//			X:              chunkX,
	//			Z:              chunkZ,
	//			Sections:       make([]*pileformat.Section, sectionCount),
	//			BlockEntities:  []pileformat.BlockEntity{},
	//			Entities:       []pileformat.Entity{},
	//			ScheduledTicks: []pileformat.ScheduledTick{},
	//			UserData:       []byte{},
	//		}
	//		world.SetChunk(chunk)
	//	}
	//
	//	if err := convertEntity(c, chunk, worldX, worldY, worldZ, entity, fromVersion); err != nil {
	//		fmt.Printf("Warning: failed to convert entity %s at (%.1f,%.1f,%.1f): %v\n", entity.ID, worldX, worldY, worldZ, err)
	//	} else {
	//		processedEntities++
	//	}
	//
	//	if len(entities) > 10 && (i+1)%(len(entities)/10) == 0 {
	//		fmt.Printf("  Progress: %d/%d entities\n", i+1, len(entities))
	//	}
	//}
	//fmt.Printf("Converted %d/%d entities\n", processedEntities, len(entities))
	fmt.Println("Converted no entities, this will be implemented later")

	fmt.Printf("\nConversion complete!\n")
	fmt.Printf("  Total chunks: %d\n", world.ChunkCount())
	fmt.Printf("  Block entities: %d\n", processedBE)
	fmt.Printf("  Entities: %d/%d\n", processedEntities, len(entities))

	// Write to file
	fmt.Printf("\nWriting to %s...\n", outputFile)
	out, err := os.Create(outputFile)
	if err != nil {
		panic(err)
	}
	defer out.Close()

	if err := pileformat.WriteWithCompression(out, world, pileformat.CompressionLevelBest); err != nil {
		panic(err)
	}

	fmt.Printf("Successfully wrote %s\n", outputFile)
}

// convertBlock converts and places a block in the chunk
func convertBlock(c *crocon.Converter, chunk *pileformat.Chunk, world *pileformat.World, worldX, worldY, worldZ int, state *schemformat.BlockState, fromVersion string) error {
	b, err := c.ConvertBlock(crocon.BlockRequest{
		ConversionRequest: crocon.ConversionRequest{
			FromVersion: fromVersion,
			ToVersion:   protocol.CurrentVersion,
			FromEdition: crocon.JavaEdition,
			ToEdition:   crocon.BedrockEdition,
		},
		Block: crocon.Block{
			ID:     state.Name,
			States: state.Properties,
		},
	})
	if err != nil {
		return err
	}

	// Filter to valid properties
	validProps := blockProperties[b.ID]
	for k := range b.States {
		if _, ok := validProps[k]; !ok {
			delete(b.States, k)
		}
	}

	// Calculate section and position within section
	sectionY := int32(worldY >> 4)
	sectionIndex := int(sectionY - world.MinSection)

	if sectionIndex < 0 || sectionIndex >= len(chunk.Sections) {
		return fmt.Errorf("block outside world bounds")
	}

	localX := worldX & 0xF
	localY := worldY & 0xF
	localZ := worldZ & 0xF

	// Get or create section
	section := chunk.Sections[sectionIndex]
	if section == nil {
		section = &pileformat.Section{
			BlockPalette: []string{"minecraft:air"},
			BlockData:    []int64{},
			BiomePalette: []string{"minecraft:plains"},
			BiomeData:    []int64{},
		}
		chunk.Sections[sectionIndex] = section
	}

	// Build block state string with properties
	blockStateStr := encodeBlockState(b.ID, b.States)

	// Find or add to palette
	oldPaletteSize := len(section.BlockPalette)
	paletteIndex := findOrAddToPalette(section.BlockPalette, blockStateStr)
	needsRepacking := false
	if paletteIndex >= oldPaletteSize {
		section.BlockPalette = append(section.BlockPalette, blockStateStr)
		// If palette grew and we already have data, we might need more bits
		if len(section.BlockData) > 0 {
			oldBits := calculateBitsPerEntry(oldPaletteSize)
			newBits := calculateBitsPerEntry(len(section.BlockPalette))
			needsRepacking = oldBits != newBits
		}
	}

	// Repack data if bits per entry changed
	if needsRepacking {
		section.BlockData = repackBlockData(section.BlockData, oldPaletteSize, len(section.BlockPalette))
	}

	// Update block data
	blockIndex := localY*256 + localZ*16 + localX
	bitsPerEntry := calculateBitsPerEntry(len(section.BlockPalette))

	if bitsPerEntry > 0 {
		valuesPerLong := 64 / bitsPerEntry
		longIndex := blockIndex / valuesPerLong
		bitOffset := (blockIndex % valuesPerLong) * bitsPerEntry

		// Ensure blockData array is large enough
		requiredLongs := (4096 + valuesPerLong - 1) / valuesPerLong
		if len(section.BlockData) < requiredLongs {
			newData := make([]int64, requiredLongs)
			copy(newData, section.BlockData)
			section.BlockData = newData
		}

		// Clear old value and set new value
		mask := int64((1 << bitsPerEntry) - 1)
		section.BlockData[longIndex] &= ^(mask << bitOffset)
		section.BlockData[longIndex] |= int64(paletteIndex) << bitOffset
	}

	return nil
}

// convertBiome converts and places a biome in the chunk
func convertBiome(c *crocon.Converter, chunk *pileformat.Chunk, w *pileformat.World, worldX, worldY, worldZ int, biome string, fromVersion string) error {
	b, err := c.ConvertBiome(crocon.BiomeRequest{
		ConversionRequest: crocon.ConversionRequest{
			FromVersion: fromVersion,
			ToVersion:   protocol.CurrentVersion,
			FromEdition: crocon.JavaEdition,
			ToEdition:   crocon.BedrockEdition,
		},
		Data: map[string]any{
			"name": biome,
		},
	})
	if err != nil {
		return err
	}

	// Calculate section and position within section
	sectionY := int32(worldY >> 4)
	sectionIndex := int(sectionY - w.MinSection)

	if sectionIndex < 0 || sectionIndex >= len(chunk.Sections) {
		return fmt.Errorf("biome outside world bounds")
	}

	// Biomes are stored at 4x4x4 resolution (1/4 of block resolution)
	localX := (worldX & 0xF) / 4
	localY := (worldY & 0xF) / 4
	localZ := (worldZ & 0xF) / 4

	// Get or create section
	section := chunk.Sections[sectionIndex]
	if section == nil {
		section = &pileformat.Section{
			BlockPalette: []string{"minecraft:air"},
			BlockData:    []int64{},
			BiomePalette: []string{"minecraft:plains"},
			BiomeData:    []int64{},
		}
		chunk.Sections[sectionIndex] = section
	}

	wb, ok := world.BiomeByID(int(b.ID))
	if !ok {
		return fmt.Errorf("invalid biome id: %d", b.ID)
	}

	// Find or add to biome palette
	oldPaletteSize := len(section.BiomePalette)
	paletteIndex := findOrAddToPalette(section.BiomePalette, wb.String())
	needsRepacking := false
	if paletteIndex >= oldPaletteSize {
		section.BiomePalette = append(section.BiomePalette, wb.String())
		// If palette grew and we already have data, we might need more bits
		if len(section.BiomeData) > 0 {
			oldBits := calculateBitsPerEntry(oldPaletteSize)
			newBits := calculateBitsPerEntry(len(section.BiomePalette))
			needsRepacking = oldBits != newBits
		}
	}

	// Repack biome data if bits per entry changed
	if needsRepacking {
		section.BiomeData = repackBiomeData(section.BiomeData, oldPaletteSize, len(section.BiomePalette))
	}

	// Update biome data (4x4x4 = 64 biomes per section)
	biomeIndex := localY*16 + localZ*4 + localX
	bitsPerEntry := calculateBitsPerEntry(len(section.BiomePalette))

	if bitsPerEntry > 0 {
		valuesPerLong := 64 / bitsPerEntry
		longIndex := biomeIndex / valuesPerLong
		bitOffset := (biomeIndex % valuesPerLong) * bitsPerEntry

		// Ensure biomeData array is large enough
		requiredLongs := (64 + valuesPerLong - 1) / valuesPerLong
		if len(section.BiomeData) < requiredLongs {
			newData := make([]int64, requiredLongs)
			copy(newData, section.BiomeData)
			section.BiomeData = newData
		}

		// Clear old value and set new value
		mask := int64((1 << bitsPerEntry) - 1)
		section.BiomeData[longIndex] &= ^(mask << bitOffset)
		section.BiomeData[longIndex] |= int64(paletteIndex) << bitOffset
	}

	return nil
}

// convertBlockEntity converts and adds a block entity to the chunk
func convertBlockEntity(c *crocon.Converter, chunk *pileformat.Chunk, worldX, worldY, worldZ int, be *schemformat.BlockEntity, fromVersion string) error {
	from := crocon.BlockEntity(be.Data)
	from["id"] = be.ID

	converted, err := c.ConvertBlockEntity(crocon.BlockEntityRequest{
		ConversionRequest: crocon.ConversionRequest{
			FromVersion: fromVersion,
			ToVersion:   protocol.CurrentVersion,
			FromEdition: crocon.JavaEdition,
			ToEdition:   crocon.BedrockEdition,
		},
		BlockEntity: from,
	})
	if err != nil {
		return err
	}

	m := map[string]any(*converted)
	tag, ok := m["tag"].(map[string]any)
	if !ok {
		return fmt.Errorf("block entity missing or invalid 'tag' field")
	}

	// Pack local XZ coordinates
	localX := uint8(worldX & 0xF)
	localZ := uint8(worldZ & 0xF)
	packedXZ := localX | (localZ << 4)

	// Encode NBT data
	nbtData, err := nbt.Marshal(tag)
	if err != nil {
		return err
	}

	// Extract ID safely
	id, ok := m["Name"].(string)
	if !ok {
		return fmt.Errorf("block entity missing or invalid 'Name' field")
	}

	chunk.BlockEntities = append(chunk.BlockEntities, pileformat.BlockEntity{
		PackedXZ: packedXZ,
		Y:        int32(worldY),
		ID:       id,
		Data:     nbtData,
	})

	return nil
}

// TODO: fix entity conversation
// convertEntity converts and adds an entity to the chunk
func convertEntity(c *crocon.Converter, chunk *pileformat.Chunk, worldX, worldY, worldZ float64, entity *schemformat.Entity, fromVersion string) error {
	data := map[string]any{}
	data["id"] = entity.ID
	data["Pos"] = []float64{
		float64(entity.Pos[0]), float64(entity.Pos[1]), float64(entity.Pos[2]),
	}
	data["Motion"] = []float64{
		float64(entity.Motion[0]), float64(entity.Motion[1]), float64(entity.Motion[2]),
	}
	data["Rotation"] = entity.Rotation[:]
	if entity.UUID != nil {
		data["UUID"] = (*entity.UUID)[:]
	}
	data["tag"] = entity.Data
	from := crocon.Entity(data)

	converted, err := c.ConvertEntity(crocon.EntityRequest{
		ConversionRequest: crocon.ConversionRequest{
			FromVersion: fromVersion,
			ToVersion:   protocol.CurrentVersion,
			FromEdition: crocon.JavaEdition,
			ToEdition:   crocon.BedrockEdition,
		},
		Entity: from,
	})
	if err != nil {
		return err
	}

	// Create or use existing UUID
	var entityUUID uuid.UUID
	if entity.UUID != nil {
		// Convert [4]int32 UUID to uuid.UUID
		uuidBytes := make([]byte, 16)
		for i := range 4 {
			val := uint32(entity.UUID[i])
			uuidBytes[i*4] = byte(val >> 24)
			uuidBytes[i*4+1] = byte(val >> 16)
			uuidBytes[i*4+2] = byte(val >> 8)
			uuidBytes[i*4+3] = byte(val)
		}
		entityUUID, _ = uuid.FromBytes(uuidBytes)
	} else {
		entityUUID = uuid.New()
	}

	// Encode NBT data
	nbtData, err := nbt.Marshal(converted)
	if err != nil {
		return err
	}

	// Extract ID safely
	id, ok := (*converted)["id"].(string)
	if !ok {
		return fmt.Errorf("entity missing or invalid 'id' field")
	}

	chunk.Entities = append(chunk.Entities, pileformat.Entity{
		UUID:     entityUUID,
		ID:       id,
		Position: [3]float32{float32(worldX), float32(worldY), float32(worldZ)},
		Rotation: entity.Rotation,
		Velocity: [3]float32{float32(entity.Motion[0]), float32(entity.Motion[1]), float32(entity.Motion[2])},
		Data:     nbtData,
	})

	return nil
}

// findOrAddToPalette finds an entry in the palette or returns the index where it should be added
func findOrAddToPalette(palette []string, value string) int {
	for i, v := range palette {
		if v == value {
			return i
		}
	}
	return len(palette)
}

// calculateBitsPerEntry calculates the number of bits needed per palette entry
func calculateBitsPerEntry(paletteSize int) int {
	if paletteSize <= 1 {
		return 0
	}
	bits := 0
	size := paletteSize - 1
	for size > 0 {
		bits++
		size >>= 1
	}
	return bits
}

// repackBlockData repacks block data when bits per entry changes
func repackBlockData(oldData []int64, oldPaletteSize, newPaletteSize int) []int64 {
	oldBits := calculateBitsPerEntry(oldPaletteSize)
	newBits := calculateBitsPerEntry(newPaletteSize)

	if oldBits == newBits || oldBits == 0 {
		return oldData
	}

	// Extract all values from old data
	oldValuesPerLong := 64 / oldBits
	values := make([]int, 4096)
	for i := range 4096 {
		longIndex := i / oldValuesPerLong
		bitOffset := (i % oldValuesPerLong) * oldBits
		if longIndex < len(oldData) {
			mask := int64((1 << oldBits) - 1)
			values[i] = int((oldData[longIndex] >> bitOffset) & mask)
		}
	}

	// Pack into new format
	newValuesPerLong := 64 / newBits
	requiredLongs := (4096 + newValuesPerLong - 1) / newValuesPerLong
	newData := make([]int64, requiredLongs)

	for i := range 4096 {
		longIndex := i / newValuesPerLong
		bitOffset := (i % newValuesPerLong) * newBits
		newData[longIndex] |= int64(values[i]) << bitOffset
	}

	return newData
}

// repackBiomeData repacks biome data when bits per entry changes
func repackBiomeData(oldData []int64, oldPaletteSize, newPaletteSize int) []int64 {
	oldBits := calculateBitsPerEntry(oldPaletteSize)
	newBits := calculateBitsPerEntry(newPaletteSize)

	if oldBits == newBits || oldBits == 0 {
		return oldData
	}

	// Extract all values from old data (64 biomes in 4x4x4)
	oldValuesPerLong := 64 / oldBits
	values := make([]int, 64)
	for i := range 64 {
		longIndex := i / oldValuesPerLong
		bitOffset := (i % oldValuesPerLong) * oldBits
		if longIndex < len(oldData) {
			mask := int64((1 << oldBits) - 1)
			values[i] = int((oldData[longIndex] >> bitOffset) & mask)
		}
	}

	// Pack into new format
	newValuesPerLong := 64 / newBits
	requiredLongs := (64 + newValuesPerLong - 1) / newValuesPerLong
	newData := make([]int64, requiredLongs)

	for i := range 64 {
		longIndex := i / newValuesPerLong
		bitOffset := (i % newValuesPerLong) * newBits
		newData[longIndex] |= int64(values[i]) << bitOffset
	}

	return newData
}

//go:linkname blockProperties github.com/df-mc/dragonfly/server/world.blockProperties
var blockProperties map[string]map[string]any

// encodeBlockState encodes a block name and properties into a string format.
// Format: "name" or "name[prop1=value1,prop2=value2]"
// Values are encoded with type-specific formats:
// - boolean: true/false
// - byte/uint8: 0x00 to 0xFF (hex prefix)
// - int32: plain number
// - float32: decimal number
// - string: "quoted"
func encodeBlockState(name string, properties map[string]any) string {
	if len(properties) == 0 {
		return name
	}

	result := name + "["
	first := true
	for k, v := range properties {
		if !first {
			result += ","
		}

		// Encode value with type-specific format
		var valueStr string
		switch val := v.(type) {
		case bool:
			valueStr = fmt.Sprintf("%v", val)
		case byte:
			valueStr = fmt.Sprintf("0x%02x", val)
		case int32:
			valueStr = fmt.Sprintf("%d", val)
		case int:
			valueStr = fmt.Sprintf("%d", val)
		case float32:
			valueStr = fmt.Sprintf("%.1f", val)
		case string:
			valueStr = fmt.Sprintf("\"%s\"", val)
		default:
			valueStr = fmt.Sprintf("%v", val)
		}

		result += fmt.Sprintf("%s=%s", k, valueStr)
		first = false
	}
	result += "]"
	return result
}
