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

**v2 is frozen.** `format.FrozenVersion` says so declaratively, and while
`format.Version` equals it the byte-locked fixtures cannot be regenerated at
all: `TestGoldenFormatStability`, `TestConformanceVectors` and
`TestConformanceVectorsNegative` refuse `-update` outright, and `-format-change`
— which before the freeze blessed a deliberate change — does not lift the
refusal. Incrementing `format.Version` is the only way to move a byte.

The fixtures in `testdata/` pin the encoder's output for fixed content, and
`TestGoldenFormatStability` fails if it changes. `TestGoldenFormatReadable`
separately checks that the current decoder still reads the stored files, which
is the direction that matters for worlds already on disk;
`TestManifestsAgreeWithVersion` checks that every manifest was generated at the
version this build writes.

Moving to v3, in order:

1. set `format.Version = 3` in `format.go` and leave `FrozenVersion` at 2 —
   that is what lifts the lock, since 3 is not the frozen version;
2. make the change, and regenerate with
   `go test ./format -run 'TestGoldenFormatStability|TestConformanceVectors' -update`
   (while unfrozen, a fixture whose bytes change at an unchanged `Version` still
   needs `-format-change`, which is the old accident guard);
3. update the specification, re-pin it with
   `go test ./format -run TestSpecRulesPinned -update`, and update the
   independent vector walker, which asserts the version it parses
   (`vectorwalk_test.go`);
4. freeze the new version by setting `FrozenVersion = 3`.

`TestSpecRulesPinned` is deliberately outside the lock: adding a `MUST` sentence
that states a rule the implementation already enforces moves no byte and stays
permitted after the freeze. What is forbidden is changing which files a reader
accepts, and that shows up as a moved fixture.

The compatibility statement — what v2 guarantees, what is not frozen, and the
`ContentHash` and threat-model caveats — is in the [root readme](../readme.md).

## Bounding a decode

Every reader — `ReadWorld`, `ReadStructure`, `ReadMeta`, `OpenIndexed` and
`ContentHash` — takes optional `ReadOption`s:

```go
d, err := format.ReadWorld(file, reg, format.MaxDecodedBytes(64<<20))
w, err := format.OpenIndexed(path, reg, true, format.MaxDecodedBytes(64<<20))
```

`MaxDecodedBytes` caps the live decoded state a call may produce, counted over
decoded columns and section storages. §8's own ceilings are set at what the
format can represent rather than at what a deployment wants to spend, so this
is the dial for anything reading files it did not write. Passing nothing, or
`0`, is the format's ceiling and decodes exactly what a reader without the
option decodes. A larger value is clamped back down to it: the ceiling can be
tightened, never raised.

It is **per handle** for `OpenIndexed` — the directory it keeps resident plus
one decoded record — because indexed mode decodes a record at a time and a
per-call number would bound nothing.

A refusal under it is `format.ErrDecodeBudget`, which is the one decode error
that does **not** wrap `ErrCorrupt`. The file may be entirely conforming; it is
just larger than you asked to decode.

## Safety properties

- Decoders never panic on arbitrary input (fuzzed continuously); all
  violations are errors wrapping `format.ErrCorrupt` (or `ErrChecksum`,
  `ErrUnsupportedVersion`, `ErrUnsupportedMode`, `ErrUnknownFlags`), with the
  single exception of `ErrDecodeBudget` above, which reports the caller's own
  limit rather than a defect in the file.
- Files written against older Minecraft block versions upgrade on read, once
  per unique state.
- The codec reaches into dragonfly's chunk internals through layout-asserted
  unsafe mirrors: a dragonfly upgrade that changes those struct layouts
  panics at package init instead of corrupting data. Pin your dragonfly
  version and run tests when bumping it.
