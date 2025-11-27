package format

import (
	"fmt"

	"github.com/google/uuid"
)

const (
	// MagicNumber is the Pile file format identifier "Pile".
	MagicNumber = 0x50696C65

	// CurrentVersion is the latest supported Pile format version.
	CurrentVersion = 1

	// Compression types
	CompressionNone = 0
	CompressionZstd = 1

	// Recommended world size limits (not enforced, for validation helpers)
	MaxReasonableSections = 128  // 2048 blocks tall
	MinReasonableSections = -128 // Supports deep underground builds
)

// World represents a Pile world containing chunks.
type World struct {
	Version     int16
	MinSection  int32
	MaxSection  int32
	UserData    []byte
	chunks      map[int64]*Chunk
	dirtyChunks map[int64]bool // Track which chunks have been modified

	streaming  bool             // Enable streaming mode when saving
	chunkIndex map[int64]uint64 // Optional chunk offset index for streaming encoder
	readOnly   bool             // If true, prevents modifications to the world
}

// NewWorld creates a new Pile world with the given section range.
func NewWorld(minSection, maxSection int32) *World {
	return &World{
		Version:     CurrentVersion,
		MinSection:  minSection,
		MaxSection:  maxSection,
		chunks:      make(map[int64]*Chunk),
		dirtyChunks: make(map[int64]bool),
		chunkIndex:  make(map[int64]uint64),
	}
}

// ValidateDimensions checks if the world dimensions are reasonable.
// Returns an error if dimensions exceed recommended limits.
// This is advisory only - the format supports any int32 range.
func (w *World) ValidateDimensions() error {
	if w.MinSection < MinReasonableSections {
		return fmt.Errorf("MinSection %d is below recommended minimum %d", w.MinSection, MinReasonableSections)
	}
	if w.MaxSection > MaxReasonableSections {
		return fmt.Errorf("MaxSection %d exceeds recommended maximum %d", w.MaxSection, MaxReasonableSections)
	}
	if w.MinSection >= w.MaxSection {
		return fmt.Errorf("MinSection %d must be less than MaxSection %d", w.MinSection, w.MaxSection)
	}
	sectionCount := w.MaxSection - w.MinSection
	if sectionCount > 512 {
		return fmt.Errorf("section count %d is very large and may cause memory issues", sectionCount)
	}
	return nil
}

// SetReadOnly marks the world as read-only, preventing modifications.
func (w *World) SetReadOnly(readOnly bool) {
	w.readOnly = readOnly
}

// IsReadOnly returns true if the world is marked as read-only.
func (w *World) IsReadOnly() bool {
	return w.readOnly
}

// Chunk returns the chunk at the given coordinates, or nil if not found.
func (w *World) Chunk(x, z int32) *Chunk {
	if w.chunks == nil {
		return nil
	}
	return w.chunks[chunkKey(x, z)]
}

// SetChunk sets a chunk at the given coordinates.
// Silently ignores the operation if the world is read-only.
func (w *World) SetChunk(c *Chunk) {
	if w.readOnly {
		return // Silently ignore modifications to read-only worlds
	}
	w.setChunk(c)
}

// setChunk is an internal method that bypasses read-only checks.
// Used during decoding to populate the world.
func (w *World) setChunk(c *Chunk) {
	if w.chunks == nil {
		w.chunks = make(map[int64]*Chunk)
	}
	if w.dirtyChunks == nil {
		w.dirtyChunks = make(map[int64]bool)
	}
	key := chunkKey(c.X, c.Z)
	w.chunks[key] = c
	w.dirtyChunks[key] = true
}

// Chunks returns all chunks in the world.
func (w *World) Chunks() []*Chunk {
	chunks := make([]*Chunk, 0, len(w.chunks))
	for _, c := range w.chunks {
		chunks = append(chunks, c)
	}
	return chunks
}

// DirtyChunks returns all chunks that have been modified since the last save.
func (w *World) DirtyChunks() []*Chunk {
	if w.dirtyChunks == nil {
		return nil
	}
	chunks := make([]*Chunk, 0, len(w.dirtyChunks))
	for key := range w.dirtyChunks {
		if c, ok := w.chunks[key]; ok {
			chunks = append(chunks, c)
		}
	}
	return chunks
}

// ClearDirty clears the dirty flag for all chunks.
func (w *World) ClearDirty() {
	w.dirtyChunks = make(map[int64]bool)
}

// IsDirty returns true if any chunks have been modified.
func (w *World) IsDirty() bool {
	return len(w.dirtyChunks) > 0
}

// ChunkCount returns the number of chunks in the world.
func (w *World) ChunkCount() int {
	return len(w.chunks)
}

// Chunk represents a 16x16 column of sections spanning the entire height of a dimension.
type Chunk struct {
	X        int32      // Chunk X coordinate in world space
	Z        int32      // Chunk Z coordinate in world space
	Sections []*Section // Array of sections from bottom to top of dimension
	// BlockEntities stores block entity data (chests, signs, etc.)
	BlockEntities []BlockEntity
	// Entities stores dynamic entity data (players, mobs, items).
	Entities []Entity
	// ScheduledTicks stores scheduled block updates (scheduled ticks).
	ScheduledTicks []ScheduledTick
	// UserData stores arbitrary chunk metadata (reserved for future use)
	UserData []byte
}

// Section represents a 16x16x16 section of blocks and biomes.
// Data is stored in a paletted format for efficiency:
// - Palettes contain unique block/biome names
// - Data arrays contain packed indices into the palette
type Section struct {
	// Block palette and data
	BlockPalette []string // Unique block names in this section
	BlockData    []int64  // Packed palette indices (bits per entry = ceil(log2(palette size)))

	// Biome palette and data
	BiomePalette []string // Unique biome names in this section
	BiomeData    []int64  // Packed palette indices
}

// IsEmpty returns true if the section contains only air.
func (s *Section) IsEmpty() bool {
	return len(s.BlockPalette) == 0 || (len(s.BlockPalette) == 1 && s.BlockPalette[0] == "minecraft:air")
}

// BlockEntity represents a block with NBT data (chest, sign, etc).
type BlockEntity struct {
	// Packed position within chunk (4 bits X, 4 bits Z = 8 bits total)
	PackedXZ uint8
	Y        int32
	ID       string
	Data     []byte // NBT encoded data
}

// Position returns the block entity's position within the chunk.
func (b *BlockEntity) Position() (x, y, z int32) {
	x = int32(b.PackedXZ & 0xF)        // Lower 4 bits
	z = int32((b.PackedXZ >> 4) & 0xF) // Next 4 bits
	y = b.Y
	return
}

// Entity represents a dynamic entity (player, mob, item, etc.) stored in a chunk.
type Entity struct {
	UUID     uuid.UUID  // Stable entity UUID
	ID       string     // Entity identifier, e.g. "minecraft:zombie"
	Position [3]float32 // X, Y, Z position
	Rotation [2]float32 // Yaw, Pitch rotation
	Velocity [3]float32 // VX, VY, VZ velocity
	Data     []byte     // NBT-encoded entity data (additional attributes)
}

// ScheduledTick represents a scheduled block update stored at chunk granularity.
type ScheduledTick struct {
	PackedXZ uint8  // Local XZ in chunk (lower 4 bits X, next 4 bits Z)
	Y        int32  // Absolute Y
	Block    string // Optional: Block identifier responsible for the tick
	Tick     int64  // Tick at which the update should fire
}

// Position returns the scheduled tick's position within the chunk.
func (t *ScheduledTick) Position() (x, y, z int32) {
	x = int32(t.PackedXZ & 0xF)
	z = int32((t.PackedXZ >> 4) & 0xF)
	y = t.Y
	return
}

// chunkKey creates a unique key for chunk coordinates.
func chunkKey(x, z int32) int64 {
	return int64(x)<<32 | int64(uint32(z))
}
