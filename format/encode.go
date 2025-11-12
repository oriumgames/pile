package format

// EncodeWorld encodes a World into a buffer.
func EncodeWorld(buf *buffer, w *World) {
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
		EncodeChunk(buf, chunk, w.MinSection, w.MaxSection)
	}
}

// EncodeChunk encodes a Chunk into a buffer.
func EncodeChunk(buf *buffer, c *Chunk, minSection, maxSection int32) {
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
	buf.WriteVarInt(int64(len(c.Entities)))
	for _, e := range c.Entities {
		// Entity identifier and UUID are written explicitly for fast indexing.
		buf.WriteString(e.ID)
		buf.WriteString(e.UUID.String())
		// Write position (float32)
		buf.WriteFloat32(e.Position[0])
		buf.WriteFloat32(e.Position[1])
		buf.WriteFloat32(e.Position[2])
		// Write rotation (float32)
		buf.WriteFloat32(e.Rotation[0])
		buf.WriteFloat32(e.Rotation[1])
		// Write velocity (float32)
		buf.WriteFloat32(e.Velocity[0])
		buf.WriteFloat32(e.Velocity[1])
		buf.WriteFloat32(e.Velocity[2])
		// Write additional data
		buf.WriteBytes(e.Data)
	}

	// Write scheduled ticks (v4)
	buf.WriteVarInt(int64(len(c.ScheduledTicks)))
	for _, t := range c.ScheduledTicks {
		buf.WriteByte(t.PackedXZ)
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
}

// encodeBlockEntity encodes a BlockEntity into a buffer.
func encodeBlockEntity(buf *buffer, be *BlockEntity) {
	buf.WriteByte(be.PackedXZ)
	buf.WriteInt32(be.Y)
	buf.WriteString(be.ID)
	buf.WriteBytes(be.Data)
}
