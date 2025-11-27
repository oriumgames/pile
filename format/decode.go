package format

import (
	"fmt"
	"io"

	"github.com/google/uuid"
)

// DecodeWorld decodes a World from a reader.
func DecodeWorld(r io.Reader) (*World, error) {
	rd := newReader(r)

	w := &World{
		Version: CurrentVersion,
		chunks:  make(map[int64]*Chunk),
	}

	// Read section range
	minSection, err := rd.ReadInt32()
	if err != nil {
		return nil, fmt.Errorf("read min section: %w", err)
	}
	maxSection, err := rd.ReadInt32()
	if err != nil {
		return nil, fmt.Errorf("read max section: %w", err)
	}
	w.MinSection = minSection
	w.MaxSection = maxSection

	// Read user data
	userData, err := rd.ReadBytes()
	if err != nil {
		return nil, fmt.Errorf("read user data: %w", err)
	}
	w.UserData = userData

	// Read chunk count
	chunkCount, err := rd.ReadVarInt()
	if err != nil {
		return nil, fmt.Errorf("read chunk count: %w", err)
	}

	if chunkCount < 0 || chunkCount > 1000000 {
		return nil, fmt.Errorf("invalid chunk count: %d", chunkCount)
	}

	// Read chunks
	for i := range chunkCount {
		chunk, err := decodeChunk(rd, minSection, maxSection)
		if err != nil {
			return nil, fmt.Errorf("decode chunk %d (total: %d): %w", i, chunkCount, err)
		}
		w.setChunk(chunk)
	}

	return w, nil
}

// decodeChunk decodes a Chunk from a reader.
func decodeChunk(rd *reader, minSection, maxSection int32) (*Chunk, error) {
	chunk := &Chunk{}

	// Read coordinates
	x, err := rd.ReadInt32()
	if err != nil {
		return nil, fmt.Errorf("read x: %w", err)
	}
	z, err := rd.ReadInt32()
	if err != nil {
		return nil, fmt.Errorf("read z: %w", err)
	}
	chunk.X = x
	chunk.Z = z

	// Read sections
	sectionCount := int(maxSection - minSection)
	chunk.Sections = make([]*Section, sectionCount)

	for i := range sectionCount {
		section, err := decodeSection(rd)
		if err != nil {
			return nil, fmt.Errorf("decode section %d: %w", i, err)
		}
		// Only store non-empty sections
		if !section.IsEmpty() {
			chunk.Sections[i] = section
		}
	}

	// Read block entities
	beCount, err := rd.ReadVarInt()
	if err != nil {
		return nil, fmt.Errorf("read block entity count: %w", err)
	}
	if beCount < 0 {
		return nil, fmt.Errorf("invalid block entity count: %d", beCount)
	}

	chunk.BlockEntities = make([]BlockEntity, beCount)
	for i := range beCount {
		be, err := decodeBlockEntity(rd)
		if err != nil {
			return nil, fmt.Errorf("decode block entity %d: %w", i, err)
		}
		chunk.BlockEntities[i] = *be
	}

	// Read entities
	entCount, err := rd.ReadVarInt()
	if err != nil {
		return nil, fmt.Errorf("read entity count: %w", err)
	}
	if entCount < 0 {
		return nil, fmt.Errorf("invalid entity count: %d", entCount)
	}
	chunk.Entities = make([]Entity, 0, entCount)
	for i := range entCount {
		id, err := rd.ReadString()
		if err != nil {
			return nil, fmt.Errorf("read entity %d id: %w", i, err)
		}
		uidStr, err := rd.ReadString()
		if err != nil {
			return nil, fmt.Errorf("read entity %d uuid: %w", i, err)
		}
		// Read position (float32)
		posX, err := rd.ReadFloat32()
		if err != nil {
			return nil, fmt.Errorf("read entity %d position X: %w", i, err)
		}
		posY, err := rd.ReadFloat32()
		if err != nil {
			return nil, fmt.Errorf("read entity %d position Y: %w", i, err)
		}
		posZ, err := rd.ReadFloat32()
		if err != nil {
			return nil, fmt.Errorf("read entity %d position Z: %w", i, err)
		}
		// Read rotation (float32)
		yaw, err := rd.ReadFloat32()
		if err != nil {
			return nil, fmt.Errorf("read entity %d rotation yaw: %w", i, err)
		}
		pitch, err := rd.ReadFloat32()
		if err != nil {
			return nil, fmt.Errorf("read entity %d rotation pitch: %w", i, err)
		}
		// Read velocity (float32)
		velX, err := rd.ReadFloat32()
		if err != nil {
			return nil, fmt.Errorf("read entity %d velocity X: %w", i, err)
		}
		velY, err := rd.ReadFloat32()
		if err != nil {
			return nil, fmt.Errorf("read entity %d velocity Y: %w", i, err)
		}
		velZ, err := rd.ReadFloat32()
		if err != nil {
			return nil, fmt.Errorf("read entity %d velocity Z: %w", i, err)
		}
		// Read additional data
		data, err := rd.ReadBytes()
		if err != nil {
			return nil, fmt.Errorf("read entity %d data: %w", i, err)
		}
		u, _ := uuid.Parse(uidStr)
		chunk.Entities = append(chunk.Entities, Entity{
			UUID:     u,
			ID:       id,
			Position: [3]float32{posX, posY, posZ},
			Rotation: [2]float32{yaw, pitch},
			Velocity: [3]float32{velX, velY, velZ},
			Data:     data,
		})
	}

	// Read scheduled ticks
	tickCount, err := rd.ReadVarInt()
	if err != nil {
		return nil, fmt.Errorf("read scheduled tick count: %w", err)
	}
	if tickCount < 0 {
		return nil, fmt.Errorf("invalid scheduled tick count: %d", tickCount)
	}
	chunk.ScheduledTicks = make([]ScheduledTick, 0, tickCount)
	for i := range tickCount {
		pxz, err := rd.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("read scheduled tick %d packed xz: %w", i, err)
		}
		y, err := rd.ReadInt32()
		if err != nil {
			return nil, fmt.Errorf("read scheduled tick %d y: %w", i, err)
		}
		block, err := rd.ReadString()
		if err != nil {
			return nil, fmt.Errorf("read scheduled tick %d block: %w", i, err)
		}
		t, err := rd.ReadVarInt()
		if err != nil {
			return nil, fmt.Errorf("read scheduled tick %d tick: %w", i, err)
		}
		chunk.ScheduledTicks = append(chunk.ScheduledTicks, ScheduledTick{
			PackedXZ: pxz,
			Y:        y,
			Block:    block,
			Tick:     t,
		})
	}

	// Read user data
	userData, err := rd.ReadBytes()
	if err != nil {
		return nil, fmt.Errorf("read user data: %w", err)
	}
	chunk.UserData = userData

	return chunk, nil
}

// decodeSection decodes a Section from a reader.
func decodeSection(rd *reader) (*Section, error) {
	section := &Section{}

	// Read block palette
	paletteSize, err := rd.ReadVarInt()
	if err != nil {
		return nil, fmt.Errorf("read block palette size: %w", err)
	}

	section.BlockPalette = make([]string, paletteSize)
	for i := range paletteSize {
		block, err := rd.ReadString()
		if err != nil {
			return nil, fmt.Errorf("read block palette entry %d: %w", i, err)
		}
		section.BlockPalette[i] = block
	}

	// Read block data
	blockDataSize, err := rd.ReadVarInt()
	if err != nil {
		return nil, fmt.Errorf("read block data size: %w", err)
	}

	section.BlockData = make([]int64, blockDataSize)
	for i := range blockDataSize {
		val, err := rd.ReadInt64()
		if err != nil {
			return nil, fmt.Errorf("read block data %d: %w", i, err)
		}
		section.BlockData[i] = val
	}

	// Read biome palette
	biomePaletteSize, err := rd.ReadVarInt()
	if err != nil {
		return nil, fmt.Errorf("read biome palette size: %w", err)
	}

	section.BiomePalette = make([]string, biomePaletteSize)
	for i := range biomePaletteSize {
		biome, err := rd.ReadString()
		if err != nil {
			return nil, fmt.Errorf("read biome palette entry %d: %w", i, err)
		}
		section.BiomePalette[i] = biome
	}

	// Read biome data
	biomeDataSize, err := rd.ReadVarInt()
	if err != nil {
		return nil, fmt.Errorf("read biome data size: %w", err)
	}

	section.BiomeData = make([]int64, biomeDataSize)
	for i := range biomeDataSize {
		val, err := rd.ReadInt64()
		if err != nil {
			return nil, fmt.Errorf("read biome data %d: %w", i, err)
		}
		section.BiomeData[i] = val
	}

	return section, nil
}

// decodeBlockEntity decodes a BlockEntity from a reader.
func decodeBlockEntity(rd *reader) (*BlockEntity, error) {
	be := &BlockEntity{}

	packedXZ, err := rd.ReadByte()
	if err != nil {
		return nil, fmt.Errorf("read packed xz: %w", err)
	}
	be.PackedXZ = packedXZ

	y, err := rd.ReadInt32()
	if err != nil {
		return nil, fmt.Errorf("read y: %w", err)
	}
	be.Y = y

	id, err := rd.ReadString()
	if err != nil {
		return nil, fmt.Errorf("read id: %w", err)
	}
	be.ID = id

	data, err := rd.ReadBytes()
	if err != nil {
		return nil, fmt.Errorf("read data: %w", err)
	}
	be.Data = data

	return be, nil
}
