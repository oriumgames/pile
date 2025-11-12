# Pile Format

Independent Go library for reading and writing the Pile world file format.

## Overview

The `format` package provides low-level access to the Pile file format without any dependencies on Dragonfly or other Minecraft server implementations. This makes it perfect for:

- Building world analysis tools
- Creating world converters
- Reference for implementing the format in other languages/systems

## Installation

```bash
go get github.com/oriumgames/pile/format
```

## Dependencies

- Go standard library
- `github.com/klauspost/compress` (zstd compression)
- `github.com/google/uuid` (entity UUIDs)

## Quick Start

```go
package main

import (
    "os"
    "github.com/oriumgames/pile/format"
)

func main() {
    // Create a new world
    world := format.NewWorld(-4, 20) // sections from -4 to 20 (Y: -64 to 319)

    // Create a chunk
    chunk := &format.Chunk{
        X: 0,
        Z: 0,
        Sections: make([]*format.Section, 24),
    }

    // Add a simple section with stone blocks
    chunk.Sections[0] = &format.Section{
        BlockPalette: []string{"minecraft:stone"},
        BlockData:    []int64{},
        BiomePalette: []string{"minecraft:plains"},
        BiomeData:    []int64{},
    }

    world.SetChunk(chunk)

    // Write to file
    f, _ := os.Create("world.pile")
    defer f.Close()
    format.Write(f, world)

    // Read from file
    f2, _ := os.Open("world.pile")
    defer f2.Close()
    loadedWorld, _ := format.Read(f2)
}
```

## Core Types

### World
```go
type World struct {
    Version     int16
    MinSection  int32  // Minimum section Y index
    MaxSection  int32  // Maximum section Y index
    UserData    []byte // Custom metadata
}

// Create a new world
world := format.NewWorld(minSection, maxSection)

// Manage chunks
world.SetChunk(chunk)
chunk := world.Chunk(x, z)
chunks := world.Chunks()
count := world.ChunkCount()

// Track changes
if world.IsDirty() {
    // Save world
}
world.ClearDirty()
```

### Chunk
```go
type Chunk struct {
    X              int32
    Z              int32
    Sections       []*Section
    BlockEntities  []BlockEntity
    Entities       []Entity
    ScheduledTicks []ScheduledTick
    Heightmaps     []byte
    UserData       []byte
}
```

### Section (16x16x16)
```go
type Section struct {
    BlockPalette []string // e.g., ["minecraft:stone", "minecraft:dirt"]
    BlockData    []int64  // Paletted block indices
    BiomePalette []string // e.g., ["minecraft:plains"]
    BiomeData    []int64  // Paletted biome indices
}

// Empty section (all air)
section := &Section{
    BlockPalette: []string{"minecraft:air"},
    BlockData:    []int64{},
    BiomePalette: []string{"minecraft:plains"},
    BiomeData:    []int64{},
}
```

### Block Entity
```go
type BlockEntity struct {
    PackedXZ uint8  // 4 bits X, 4 bits Z (0-15)
    Y        int32  // Absolute Y coordinate
    ID       string // e.g., "minecraft:chest"
    Data     []byte // NBT data
}

// Get position
x, y, z := blockEntity.Position()
```

### Entity
```go
type Entity struct {
    UUID     uuid.UUID
    ID       string     // e.g., "minecraft:zombie"
    Position [3]float32 // X, Y, Z position
    Rotation [2]float32 // Yaw, Pitch rotation
    Velocity [3]float32 // VX, VY, VZ velocity
    Data     []byte     // NBT data (additional attributes)
}
```

### Scheduled Tick
```go
type ScheduledTick struct {
    PackedXZ uint8  // Local XZ in chunk
    Y        int32  // Absolute Y
    Block    string // Block identifier
    Tick     int64  // Tick time
}
```

## Reading & Writing

### Basic I/O
```go
// Write with default compression
f, _ := os.Create("world.pile")
format.Write(f, world)

// Write with specific compression level
format.WriteWithCompression(f, world, format.CompressionLevelBest)

// Read
f, _ := os.Open("world.pile")
world, err := format.Read(f)
```

### Compression Levels
```go
format.CompressionLevelNone    // No compression
format.CompressionLevelFast    // Fast compression
format.CompressionLevelDefault // Default compression
format.CompressionLevelBest    // Best compression
```

### Streaming Writes
For large worlds, use streaming to reduce memory usage:
```go
f, _ := os.Create("large_world.pile")
format.WriteStreaming(f, world, format.CompressionLevelDefault)
```

## Examples

### Creating a Flat World
```go
world := format.NewWorld(-4, 20)

for x := int32(-10); x < 10; x++ {
    for z := int32(-10); z < 10; z++ {
        chunk := &format.Chunk{
            X:        x,
            Z:        z,
            Sections: make([]*format.Section, 24),
        }

        // Bedrock layer
        chunk.Sections[4] = &format.Section{
            BlockPalette: []string{"minecraft:bedrock"},
            BlockData:    []int64{},
            BiomePalette: []string{"minecraft:plains"},
            BiomeData:    []int64{},
        }

        // Dirt layers
        for i := 5; i < 8; i++ {
            chunk.Sections[i] = &format.Section{
                BlockPalette: []string{"minecraft:dirt"},
                BlockData:    []int64{},
                BiomePalette: []string{"minecraft:plains"},
                BiomeData:    []int64{},
            }
        }

        // Grass layer
        chunk.Sections[8] = &format.Section{
            BlockPalette: []string{"minecraft:grass_block"},
            BlockData:    []int64{},
            BiomePalette: []string{"minecraft:plains"},
            BiomeData:    []int64{},
        }

        world.SetChunk(chunk)
    }
}

f, _ := os.Create("flat_world.pile")
format.WriteWithCompression(f, world, format.CompressionLevelBest)
f.Close()
```

### Working with Entities
```go
world := format.NewWorld(-4, 20)

chunk := &format.Chunk{
    X: 0,
    Z: 0,
    Sections: make([]*format.Section, 24),
}

// Add entities with position, rotation, velocity
chunk.Entities = []format.Entity{
    {
        UUID:     uuid.New(),
        ID:       "minecraft:zombie",
        Position: [3]float32{10.5, 64.0, 20.3},
        Rotation: [2]float32{90.0, 0.0},  // Yaw, Pitch
        Velocity: [3]float32{0.1, 0.0, -0.2},
        Data:     []byte{}, // Additional NBT (fire, age, etc.)
    },
}

world.SetChunk(chunk)

f, _ := os.Create("world.pile")
format.Write(f, world)
f.Close()

// Read and access entity data
f2, _ := os.Open("world.pile")
loaded, _ := format.Read(f2)
f2.Close()

chunk = loaded.Chunk(0, 0)
for _, entity := range chunk.Entities {
    fmt.Printf("Entity: %s at [%.1f, %.1f, %.1f]\n",
        entity.ID,
        entity.Position[0],
        entity.Position[1],
        entity.Position[2])
}
```

### World Analysis Tool
```go
f, _ := os.Open("world.pile")
world, _ := format.Read(f)
f.Close()

fmt.Printf("World Info:\n")
fmt.Printf("  Chunks: %d\n", world.ChunkCount())
fmt.Printf("  Y Range: %d to %d\n", world.MinSection*16, world.MaxSection*16)

blockCount := make(map[string]int)
entityCount := make(map[string]int)

for _, chunk := range world.Chunks() {
    for _, section := range chunk.Sections {
        if section != nil {
            for _, block := range section.BlockPalette {
                blockCount[block]++
            }
        }
    }

    for _, entity := range chunk.Entities {
        entityCount[entity.ID]++
    }
}

fmt.Println("\nBlock Types:")
for block, count := range blockCount {
    fmt.Printf("  %s: %d sections\n", block, count)
}

fmt.Println("\nEntities:")
for entity, count := range entityCount {
    fmt.Printf("  %s: %d\n", entity, count)
}
```

### Format Converter
```go
// Read from another format
sourceWorld := readFromOtherFormat("world.dat")

// Convert to Pile format
pileWorld := format.NewWorld(-4, 20)

for _, sourceChunk := range sourceWorld.Chunks {
    chunk := &format.Chunk{
        X:        sourceChunk.X,
        Z:        sourceChunk.Z,
        Sections: convertSections(sourceChunk),
    }
    pileWorld.SetChunk(chunk)
}

// Write Pile format
f, _ := os.Create("converted.pile")
format.Write(f, pileWorld)
f.Close()
```

## Custom World Sizes

The format supports **any world size** through MinSection and MaxSection parameters:

```go
// Standard Overworld (Y: -64 to 319)
world := format.NewWorld(-4, 20)

// Tall world (Y: -1024 to 1023)
world := format.NewWorld(-64, 64)

// Custom underground world (Y: -512 to 0)
world := format.NewWorld(-32, 0)

// Skyblock world (Y: 0 to 255)
world := format.NewWorld(0, 16)
```

### Validation

Use `ValidateDimensions()` to check if world size is reasonable (advisory only):

```go
world := format.NewWorld(-64, 64)
if err := world.ValidateDimensions(); err != nil {
    // World size exceeds recommended limits
    // This is a warning - the format still supports it
}
```

**Recommended limits** (not enforced):
- MinSection: -128 (Y: -2048)
- MaxSection: 128 (Y: 2047)
- Section count: < 512

The format technically supports int32 range, but very large worlds may cause memory issues.

## Read-Only Mode

Load worlds in read-only mode to prevent accidental modifications:

```go
// Load in read-only mode
f, _ := os.Open("world.pile")
world, _ := format.ReadOnly(f)
f.Close()

// Reading is allowed
chunk := world.Chunk(0, 0)

// Modifications will panic
world.SetChunk(chunk) // panic: cannot modify read-only world

// Check read-only status
if world.IsReadOnly() {
    fmt.Println("World is read-only")
}

// Can disable read-only mode if needed
world.SetReadOnly(false)
```

**Use cases for read-only mode:**
- Analysis tools that shouldn't modify worlds
- Format inspection/debugging
- Safe world conversion (prevent accidental writes to source)
- Multi-threaded read-only access

## Format Specification

See [format.md](format.md) for the complete binary format specification.

## License

See the main pile repository for license information.
