# Pile

minimal world format for small worlds

## Overview
Pile is a single-file world format and provider for Dragonfly server software. It targets small worlds, templates, and user-generated content. Each dimension is stored as a single `.pile` file with paletted blocks/biomes, optional Zstandard compression, and support for streaming/background saves.

## Key Features
- Single-file per dimension
- Configurable compression: none, fast, default, best (Zstd)
- Paletted storage for blocks and biomes
- Full chunk data: blocks, biomes, entities, block entities, scheduled ticks
- Embedded world metadata (settings)
- Thread-safe provider with read/write locks
- Background and streaming saves to reduce stalls/peak memory

## Installation
Use Go modules:
- `go get github.com/oriumgames/pile`

## Quick Start
- Create a provider: `provider, err := pile.New("world")`
- Use in a world config: `world.Config{Provider: provider}`
- Assign the provider to your world/server config before starting
- Save on shutdown: `defer provider.Close()`

## Options
- Compression:
  - New with level: `pile.NewWithCompression(dir, pile.CompressionLevelDefault)`
  - Change later: `provider.SetCompressionLevel(pile.CompressionLevelBest)`
- Read-only mode:
  - `pile.NewReadOnly(dir)` or `pile.NewReadOnlyWithCompression(dir, level)`
  - Prevents all modifications, useful for inspection or analysis
- Streaming saves:
  - `provider.SetStreamingSaves(true)` to write chunk-by-chunk
- Background saves:
  - `provider.EnableBackgroundSaves()` then trigger with `provider.SaveAsync()`
  - Stop with `provider.DisableBackgroundSaves()`
- Introspection:
  - `provider.ChunkCount()`, `provider.DimensionChunkCount(world.Overworld)`, `provider.IsDirty()`, `provider.IsReadOnly()`

## File Layout
World directory (created as needed):
- `overworld.pile` — Overworld data
- `nether.pile` — Nether data (only if present)
- `end.pile` — End data (only if present)

## Notes & Limits
- Whole-world in memory: optimized for small worlds (e.g., lobbies, minigames, Skyblock-style)
- Empty sections are extremely compact and compress well
- Entities/scheduled ticks scale with actual usage
- If you expect very large worlds, consider a chunk-addressable backend instead

## Acknowledgments
This work is based on [hollow-cube/go-polar](https://github.com/hollow-cube/go-polar).
