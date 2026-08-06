# pile/format

Codec for the pile v2 world format: encoding and decoding of solid worlds,
indexed (append-only) worlds and structures, working directly with dragonfly's
`chunk.Column`. The binary format itself is specified in [format.md](format.md).

Most users want the parent package `github.com/oriumgames/pile`, which wraps
this codec in a `world.Provider`, templates, structures and tooling. Use
`format` directly when building tools that operate on files.

## Solid worlds

```go
import "github.com/oriumgames/pile/format"

// Write: deterministic, deduplicated, one zstd frame.
d := &format.WorldData{
    Settings: settingsNBT, // little-endian NBT blobs (or nil)
    Columns:  []format.Column{{X: 0, Z: 0, Col: col}}, // *chunk.Column
}
f, _ := os.Create("overworld.pile")
err := format.WriteWorld(f, d, world.DefaultBlockRegistry, format.Options{
    Compression: format.CompressionBest,
})

// Read: full decode, checksum-verified, parallel apply.
file, _ := os.ReadFile("overworld.pile")
d, err := format.ReadWorld(file, world.DefaultBlockRegistry)

// Metadata only, no chunk decode:
meta, err := format.ReadMeta(file)
```

`Options`:

| field | effect |
|-------|--------|
| `Compression` | `CompressionNone/Fast/Default/Best` (zstd) |
| `SkipBiomes` | store no biome data |
| `StoreLight` | store baked light nibbles (dragonfly recalculates on load regardless) |
| `Stats` | embed a stats compound readable via `ReadMeta` |
| `FastCompression` | multi-threaded zstd; output no longer byte-deterministic |
| `Workers` | bound the encode/decode goroutine pool (0 = GOMAXPROCS) |

Output is byte-deterministic (identical content ⇒ identical file) unless
`FastCompression` is set.

## Indexed worlds

Append-only files with O(1) column access and directory-plus-palette memory:

```go
w, err := format.CreateIndexed("overworld.pile", reg, format.Options{Compression: format.CompressionDefault})
w, err = format.OpenIndexed("overworld.pile", reg, readOnly) // recovers from torn writes

err = w.Store(format.Column{X: x, Z: z, Col: col}) // appends immediately
col, err := w.Column(x, z)                         // one pread + one frame decode
err = w.Checkpoint()                               // durability point
ratio := w.GarbageRatio()
err = w.Compact()                                  // rewrite garbage-free, atomic; trains a
                                                   // shared zstd dictionary when worthwhile
err = w.Close()                                    // checkpoint + close
```

Every record frame carries an xxHash64; corruption is detected per chunk. A
torn write loses at most the work since the last checkpoint.

## Structures

```go
data, err := format.NewStructureData([3]int32{w, h, l})
data.Cells[format.CellIndex(data.Size, cx, cy, cz)] = sub // *chunk.SubChunk
err = format.WriteStructure(out, data, reg, opts)
data, err = format.ReadStructure(file, reg)
```

The parent package's `pile.Structure` wraps this with a `world.Structure`
implementation, extraction, pasting and rotation.

## Utilities

- `format.UnresolvedStates(file, reg)`: block states in a solid/structure
  file that will not resolve against a registry (would decode as placeholder).
- `(*IndexedWorld).UnresolvedStates()`: the same for indexed files.
- `format.MarshalNBT` / `format.UnmarshalNBT`: deterministic little-endian
  NBT (compound keys sorted on encode; decoder hardened against hostile
  input).

## Wire format stability

`testdata/golden_world.pile` pins the encoder's output for fixed content, and
`TestGoldenFormatStability` fails if the bytes change. A deliberate format
change means bumping `format.Version` and regenerating:

```sh
go test ./format -run TestGolden -update
```

`TestGoldenFormatReadable` separately checks the current decoder still reads
the stored file, which is the direction that matters for worlds already on
disk.

## Safety properties

- Decoders never panic on arbitrary input (fuzzed continuously); all
  violations are errors wrapping `format.ErrCorrupt` (or `ErrChecksum`,
  `ErrUnsupportedVersion`, `ErrUnsupportedMode`, `ErrUnknownFlags`).
- Files written against older Minecraft block versions upgrade on read, once
  per unique state.
- The codec reaches into dragonfly's chunk internals through layout-asserted
  unsafe mirrors: a dragonfly upgrade that changes those struct layouts
  panics at package init instead of corrupting data. Pin your dragonfly
  version and run tests when bumping it.
