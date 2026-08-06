# Pile File Format, Version 2

This document is the complete specification of the pile binary format, written
so the format can be implemented in any language without reference to the Go
implementation. The Go implementation in this package is the reference; where
this document and the implementation disagree, the implementation wins and the
document has a bug.

A pile file stores one Minecraft (Bedrock) dimension (or one structure) in a
single file. Design goals: minimal size for small worlds, deterministic output
(identical content ⇒ identical bytes), integrity checking, crash safety, and
an optional append-oriented mode for large worlds.

- Header magic: the four bytes `P I L E`
- Footer magic: the four bytes `E L I P`
- Version: 2
- Compression: Zstandard
- Checksums: xxHash64

---

## 1. Primitives and conventions

All fixed-width integers are **little-endian**.

| name    | encoding |
|---------|----------|
| `u8`, `u16`, `u32`, `u64`, `i32` | fixed-width little-endian |
| `uvarint` | unsigned LEB128 (Go `binary.PutUvarint`): 7 bits per byte, high bit = continuation. MUST be minimal: decoders reject overlong encodings |
| `svarint` | zigzag-encoded LEB128 (Go `binary.PutVarint`): value `v` maps to `uvarint((v << 1) ^ (v >> 63))`. MUST be minimal |
| `string` | `uvarint` byte length + UTF-8 bytes. Decoders MUST reject lengths > 65 536 (64 KiB) |
| `blob`   | `uvarint` byte length + raw bytes. Decoders MUST reject lengths > 16 777 216 (16 MiB) |
| `bitset(n)` | `ceil(n/8)` bytes; bit `i` is bit `i%8` of byte `i/8` (LSB-first). Padding bits above `n` MUST be zero |

**Section-local block index.** A section is a 16×16×16 cube. The linear index
of local position (x, y, z), each in [0, 15], is:

```
i = (x << 8) | (z << 4) | y        // i in [0, 4095]
```

y varies fastest, then z, then x. (This matches dragonfly's storage order.)

**Light nibble arrays.** A light array holds 4096 4-bit values in 2048 bytes.
Value `i` (same linear index as blocks) lives in byte `i >> 1`; even `i` in the
low nibble, odd `i` in the high nibble.

**Morton (Z-order) key.** Chunk ordering uses a 64-bit Morton key over chunk
coordinates. Each coordinate is first mapped into unsigned space by XOR with
`0x80000000`, then the 32 bits of x are spread to even bit positions and z to
odd bit positions:

```
key(x, z) = spread(u32(x) ^ 0x80000000) | spread(u32(z) ^ 0x80000000) << 1
```

where `spread` distributes bit `i` of the input to bit `2i` of the output.

**NBT.** All NBT blobs are little-endian Bedrock NBT (numbers little-endian,
string lengths `u16`, array lengths `i32`), a single compound with an empty
root name. Compound keys MUST be unique and in strictly ascending bytewise
order, and the root compound MUST be unnamed: these are validity rules, not
just writer conventions, because duplicate keys are ambiguous and a fixed
order is what lets independent writers produce identical bytes for identical
content. Decoders MUST reject blobs that violate them.

Decoding NBT into a dynamically typed map cannot distinguish `TAG_List` of a
numeric type from the corresponding array tag: both become the same
host-language value. Because two writers must produce identical bytes for
identical content, the normalisation is normative rather than optional:

- In **opaque** NBT (entity and block-entity blobs, chunk and world user
  data), `TAG_Byte_Array`, `TAG_Int_Array` and `TAG_Long_Array` are valid on
  input, but a value obtained by decoding one MUST re-encode as `TAG_List` of
  the element type. The array tag therefore does not survive a round trip,
  and implementations MUST NOT rely on it doing so. Since every conforming
  implementation normalises identically, re-encoding a given file yields the
  same bytes everywhere.
- In the **metadata compounds this document specifies** (§7), the tag of each
  field is fixed by that section and MUST be emitted exactly: the world
  border's `min` and `max` are `TAG_Int_Array`, not lists.

This keeps byte identity achievable across implementations without requiring
a decoder that retains tag kinds.
Tag types used: byte(1), short(2), int(3), long(4), float(5), double(6),
byte_array(7), string(8), list(9), compound(10), int_array(11),
long_array(12).

---

## 2. Container layout

```
+--------------------------+
| Header      (16 bytes)   |  never compressed
+--------------------------+
| Body                     |  solid: a single unit (§4)
|                          |  indexed: a sequence of frames (§5)
+--------------------------+
| Footer      (44 bytes)   |  never compressed
+--------------------------+
```

### 2.1 Header (16 bytes)

| offset | type | field | value |
|--------|------|-------|-------|
| 0  | 4 bytes | magic | `PILE` |
| 4  | u16 | version | 2 |
| 6  | u8  | kind | 0 = world, 1 = structure |
| 7  | u8  | mode | 0 = solid, 1 = indexed |
| 8  | u32 | flags | §2.3 |
| 12 | i32 | blockVersion | the Minecraft block-state version the writer encoded against |

Readers MUST reject unknown versions and unknown flag bits (they may change
payload meaning).

### 2.2 Footer (44 bytes, at end of file)

| offset | type | field |
|--------|------|-------|
| 0  | u64 | checkpoint hash, §2.4 |
| 8  | u64 | dirOffset. Indexed: file offset of the directory frame; solid: 0 |
| 16 | u64 | dirLength. Indexed: stored length of the directory frame; solid: 0 |
| 24 | u64 | generation. Indexed: checkpoint counter; solid: 0 |
| 32 | u64 | prevFooter. Indexed: file offset of the previous checkpoint's footer (0 for the first); solid: 0. Must be older than this footer's own offset |
| 40 | 4 bytes | magic | `ELIP` |

The footer magic differs from the header magic so the indexed-mode backward
recovery scan (§5.6) can never mistake a header for a footer.

In a **solid** file the directory, generation and previous-footer words carry
no information and MUST be zero; readers MUST reject a non-zero value. This
keeps one valid encoding per file.

### 2.4 Checkpoint hash

The footer's hash authenticates the file's semantic header as well as its
payload. Hashing the payload alone would leave `blockVersion` and the flags
(which carry the default-biome reference) unprotected, so a single flipped bit
there would change how the payload decodes while every integrity check still
passed.

```
preimage = header (16 bytes)
        || payload            (solid: the stored body; indexed: the stored directory frame)
        || footer bytes 8..44 (the control words and the footer magic)
hash     = xxHash64(preimage)
```

Everything but the hash field itself is therefore covered. Indexed files
recompute this per checkpoint, so `generation` and `prevFooter` are
authenticated too.

### 2.3 Flags

| bit | name | meaning |
|-----|------|---------|
| 0 | StoreLight | chunk records contain baked light arrays (§4.6). Advisory: light is never required for correctness |
| 1 | Stats | the meta block contains a stats compound (§4.2) |
| 2 | reserved | MUST be zero (indexed dictionary presence is signalled by the directory, §5.5) |
| 3 | DefaultBiome | bits 16–31 of flags hold a global biome palette reference used as the world default biome (§4.7) |
| 4 | Uncompressed | the body is stored without compression |
| 5–15 | reserved | MUST be zero; readers MUST reject files with any reserved or unknown bit set, so those bits stay available for future features |
| 16–31 | defaultBiomeRef | only meaningful when bit 3 is set, and MUST be zero when it is clear |

---

## 3. Shared building blocks

These structures appear in both modes and in structure files.

### 3.1 Global block state palette

Every unique block state referenced anywhere in the file, encoded once.

```
count            uvarint          (≤ 1 048 576)
entry[count]:
  name           string           e.g. "minecraft:stone"
  propN          uvarint          (≤ 64)
  prop[propN]:                    sorted by key, ascending bytewise
    key          string
    type         u8               0 = byte, 1 = int32, 2 = string
    value        u8 | i32 | string
```

After the entries, a **sparse version-override table** records entries whose
states are expressed at a different Minecraft block version than the palette
itself:

```
overrideN        uvarint          (<= count; usually 0)
override[overrideN]:             indices strictly ascending
  indexDelta     uvarint          delta from the previous overridden index
  version        i32              must be non-zero
```

Only preserved unresolved states (§9) can carry one: everything else is
resolved against the live registry and is therefore at the palette's own
version. Decoders MUST upgrade each entry from its own version when it has an
override, and from the palette's version otherwise. Indexed palette segments
carry the same table after their entries.

Bedrock block state properties are exactly these three NBT types, making the
encoding lossless. In solid mode the palette is sorted by descending reference count, where the
**reference count is the number of section-local palettes the state appears
in, plus one per scheduled block update referencing it** (not the number of
blocks holding it). Ties break by ascending canonical state string
(`name[key=value,...]`, keys sorted), and any remaining tie by the state's
length-prefixed identity, so the order is total. In indexed mode the palette
is first-seen order across segments.

Boolean properties, which a custom block registry may present instead of the
byte form, are encoded as `type = 0` with value 0 or 1.

Decoding: upgrade each state from the header's `blockVersion` to the runtime's
block version (Bedrock block state upgrade schema), then resolve it in the
block registry. Unresolvable states decode as a placeholder block
(`minecraft:info_update`), falling back to air; the palette entry itself is
preserved so tooling can report it.

### 3.2 Global biome palette

```
count            uvarint
name[count]      string           e.g. "minecraft:plains"
```

Names, not numeric IDs, so entries stay stable across game versions. Unknown biomes decode as
`minecraft:plains`.

### 3.3 Section blob

The canonical encoding of one 16³ storage (a block layer or a section's
biomes). Self-delimiting.

```
paletteN         uvarint          (1 ≤ paletteN ≤ 65 536)
ref[paletteN]    uvarint          references into the global palette
width            u8               0 = uniform, 1 = u8, 2 = u16 (little-endian)
indices          4096 * width bytes   absent when width = 0
```

Rules:

- `width` MUST be 0 iff `paletteN == 1`; then every position holds `ref[0]`.
- `width = 1` requires `paletteN ≤ 256`; `width = 2` requires `paletteN > 256`
  (the narrowest sufficient width is the only valid one).
- References MUST ascend strictly, so a section has exactly one encoding and
  cannot carry duplicate or unused entries.
- Each index selects a local palette entry; indices ≥ `paletteN` are invalid.
- **Canonical form** (required from writers *and enforced by decoders*, which
  reject non-canonical blobs; this enables dedup and determinism):
  refs sorted ascending; indices remapped accordingly; the palette contains
  only entries actually used by the indices; a storage whose used palette
  collapses to one entry MUST be written uniform (width 0).
- Byte-aligned indices are deliberate: zstd's entropy stage compresses them
  near-optimally and encode/decode becomes a copy + remap.

### 3.4 Section blob table (solid and structure only)

Deduplication container: every unique section blob stored once, referenced by
index. Identical bytes MUST share one entry.

```
count            uvarint          (≤ 16 777 216)
blob[count]      section blobs, concatenated (self-delimiting)
```

Blob ids are assigned in first-use order over the (Morton-sorted) record
stream. Block and biome blobs share the table; a blob's refs are interpreted
against the block or biome palette according to the use site.

---

## 4. Solid mode (mode = 0, kind = world)

The body is a single unit: compressed as one Zstandard frame unless flag
`Uncompressed` is set. The footer carries the checkpoint hash of §2.4, which
covers the header, the stored (compressed) body and the footer's own control
words.

Body content, in order:

```
meta block       §4.1–4.2
block palette    §3.1
biome palette    §3.2
blob table       §3.4
chunkN           uvarint          (≤ 4 294 967 296, §8)
record[chunkN]   §4.3, sorted by Morton key of (x, z)
```

Nothing may follow the last record.

### 4.1 Meta block

```
settings         blob             NBT; world settings (§7.1); may be empty
userData         blob             application-defined; may be empty
markers          blob             NBT (§7.2); may be empty
border           blob             NBT (§7.3); may be empty
stats            blob             present iff flag Stats; NBT (§4.2)
```

### 4.2 Stats compound (optional)

NBT compound with at least: `chunks` (long), `filledSections` (long),
`uniqueBlobs` (long), `blockStates` (long), `biomes` (long). They are longs
because the format's own ceilings exceed what a 32-bit tag can hold. Tools may read it
via the meta block without decoding chunk data. Readers MUST ignore unknown
keys.

### 4.3 Chunk record

```
dx, dz           svarint          chunk position delta from the previous
                                  record (the first record deltas from (0,0))
minSection       svarint          lowest section index (blockY = section*16)
sectionN         uvarint          section count (1 ≤ sectionN ≤ 4096)
blockPresence    bitset(sectionN) bit i set = section i has block data
present sections, ascending i:
  layerN         uvarint          storage layers (1 ≤ layerN ≤ 256);
                                  layer 1 is Bedrock's waterlogging layer
  blobRef[layerN] uvarint         blob table references
biomePresence    bitset(sectionN)
present biome sections, ascending i:
  blobRef        uvarint
light            §4.6, present iff flag StoreLight
beN              uvarint          (≤ 1 048 576)
be[beN]          §4.4
entN             uvarint          (≤ 1 048 576)
ent[entN]:
  nbt            blob             §4.5
tick             svarint          the column's current tick
stN              uvarint          (≤ 1 048 576)
st[stN]:                          scheduled block updates
  packedXZ       u8               x = bits 0–3, z = bits 4–7 (chunk-local)
  y              svarint          absolute block Y
  blockRef       uvarint          global block palette reference
  at             svarint          absolute tick of the update
userData         blob             application-defined chunk metadata
```

An air-only section is absent (one presence bit); a fully empty chunk record
(all bits clear, zero counts) costs ~10 bytes and means "exists, is air", which is
distinct from a chunk that was never stored.

Writers MUST NOT emit a uniform-air block layer; a section is either absent or
contains at least one non-air-only layer. Decoders treat a uniform-air layer 0
defensively as absent.

### 4.4 Block entity

```
packedXZ         u8               x = bits 0–3, z = bits 4–7 (chunk-local)
y                svarint          absolute block Y
nbt              blob             NBT compound
```

The `x`, `y`, `z` keys are stripped from the NBT on write and reinjected (as
int tags, absolute coordinates) on read; the identifier stays inside the NBT
(`id` key by Bedrock convention).

### 4.5 Entity

One NBT compound per entity, stored whole: `identifier`, `Pos` (list of 3
floats), `Rotation`/`Yaw`/`Pitch`, `Motion`, and a `UniqueID` (long) which the
reference implementation surfaces as the entity's stable id. The format does
not interpret entity NBT beyond `UniqueID`.

### 4.6 Light (flag StoreLight)

```
lightPresence    bitset(sectionN)   bit i set = section i carries light
per set bit, ascending:
  flags          u8                 bit 0 = block light present,
                                    bit 1 = sky light present,
                                    bits 2-7 MUST be zero
  blockLight     2048 bytes         present iff bit 0
  skyLight       2048 bytes         present iff bit 1
```

Light presence is **independent of block presence**: a section with no blocks
still carries light (a fully open column has full sky light throughout), so
tying the two would make valid states unrepresentable. `flags` MUST NOT be
zero for a present section, since an entry with no arrays is a second
encoding of an absent one.

Nibble layout per §1. Light is a cache: readers are free to ignore it, and
consumers that recompute lighting (dragonfly does, unconditionally) gain
nothing from it.

Structures do not carry light in any form. They are pasted into worlds, which
relight the affected columns, so storing it would be dead weight with no
consumer.

### 4.7 Default biome

When flag `DefaultBiome` is set, `defaultBiomeRef` (flags bits 16–31) names a
global biome palette entry. Sections whose biomes are uniformly that biome are
omitted from `biomePresence`; decoders fill absent sections with the default.
Writers pick the biome with the most uniform sections. Without the flag,
absent biome sections decode as biome id 0.

### 4.8 Determinism

Writers targeting deterministic output MUST: sort records by Morton key; sort
the block palette by reference count (ties: canonical state string, then the
length-prefixed identity) and the biome palette by reference count (ties:
name); emit canonical section blobs (§3.3); sort NBT compound keys; and sort
every per-column collection totally — block entities by (y, z, x) then
encoded NBT, entities by id then encoded NBT, scheduled updates by position,
tick and block reference. Structure block entities and entities follow the
same rule.

Two caveats on file identity:

- The **compressed** bytes are not specified. Zstandard admits many valid
  encodings of the same content, so a different compressor implementation,
  version or level produces a different file for identical content. The
  footer's checkpoint hash (§2.4) covers the stored bytes and is an integrity
  check, not a content identity.
- For content identity, hash the **uncompressed body** instead (the reference
  implementation exposes this as `format.ContentHash`). That value depends
  only on the world's content and this specification, so it is stable across
  compressor upgrades and across implementations that follow the canonical
  rules above. (The reference implementation's "fast compression" option trades
this away.)

---

## 5. Indexed mode (mode = 1, kind = world)

An append-only file for large worlds: random access to single chunks with
only a directory and the palettes resident in memory. Indexed files are
history-dependent and therefore not deterministic.

### 5.1 Frames

All body content lives in **frames** appended after the header. A frame is a
byte range `[offset, offset+length)` recorded wherever it is referenced (the
directory, or the footer for the directory itself). Unless flag
`Uncompressed` is set, each frame is an independent Zstandard frame.

When a shared dictionary is present (§5.5): record frames, palette segment
frames and meta frames are compressed with the dictionary; the **directory
frame and the dictionary frame itself MUST be compressed without it** (they
are read before the dictionary can be loaded). A dictionary-aware decoder
handles both (zstd frames carry the dictionary id).

### 5.2 Record frames

One frame per stored chunk. Content = a chunk record as in §4.3 **except**:

- no `dx`/`dz` (position lives in the directory),
- section blobs are stored **inline** (the section blob bytes of §3.3 appear
  in place of each `blobRef`); there is no blob table,
- no default-biome elision (flag DefaultBiome is never set),
- light follows the header's StoreLight flag.

Palette references point into the cumulative global palettes (§5.3).
Overwriting a chunk appends a new frame; the old one becomes garbage until
compaction.

### 5.3 Palette segments

The global palettes grow append-only as **delta segments**. A block segment
frame is an `i32` Minecraft block version followed by a §3.1-encoded palette;
a biome segment frame is a §3.2-encoded palette. Segments hold only the
entries new since the previous checkpoint, and the directory lists them in
order; entry indices are cumulative across segments. Palette order is
first-seen; no frequency sorting.

Each block segment carries its own version because a file outlives game
upgrades: states written before an upgrade must still be upgraded from the
version they were written at, while states appended afterwards must not be.
Decoders MUST enforce the palette limits **cumulatively** across segments and
MUST reject duplicate segment references (both are allocation amplifiers).

### 5.4 Meta frame

`settings`, `userData`, `markers`, `border` blobs, §4.1 layout without the
stats field. A new meta frame is appended when metadata changes; the directory
points at the latest.

### 5.5 Directory frame

Written at every checkpoint (always compressed without the dictionary):

Every frame the directory references carries its own xxHash64, so corruption
in a palette segment, the metadata or the dictionary is detected instead of
silently changing world content.

```
kind             u8               1 = structure, 0 = world; MUST be 0
mode             u8               MUST be 1 (indexed)
flags            u32              as in the header (§2.3)
blockVersion     i32              as in the header
frameRef         = off uvarint, len uvarint, hash u64   (len 0 = absent)
meta             frameRef
dict             frameRef
blockSegN        uvarint
blockSeg[blockSegN]: frameRef
biomeSegN        uvarint
biomeSeg[biomeSegN]: frameRef
chunkN           uvarint          (≤ 4 194 304, §8)
chunk[chunkN]:                    sorted by Morton key of (x, z)
  dx, dz         svarint          delta from previous entry
  offDelta       svarint          frame offset delta from previous entry
  len            uvarint          stored frame length
  hash           u64              xxHash64 of the stored frame bytes
```

The four prologue fields repeat the header's semantic content. They are the
authority: a reader takes `flags` and `blockVersion` from the directory, and
they are also the header preimage of the checkpoint hash (§2.4), so a
checkpoint stays verifiable when the 16 physical header bytes are damaged.
Readers MUST reject a directory whose prologue disagrees with the constraints
of §5.7, and SHOULD report a mismatch against an otherwise intact physical
header (the reference implementation exposes `HeaderDamaged`; rewriting the
file repairs it). Only the magic and version in the physical header are
required to be intact for a file to be recognised at all.

Per-record hashes localise corruption to single chunks. The dictionary frame
contains raw Zstandard dictionary bytes (with an embedded dictionary id); the
reference implementation trains one during compaction when there are at least
16 records totalling at least 64 KiB.

### 5.6 Checkpoints and recovery

A **checkpoint** appends, in order: pending palette segment frames, a meta
frame (if metadata changed) and the directory frame; the file is then
**fsynced before the footer is written**, so a footer can never refer to
frames that are not durable. The footer (§2.2) carries the checkpoint hash of
§2.4 (whose payload is the directory frame's stored bytes), an incremented
`generation` and the previous footer's offset, and is followed by a second
fsync. There is exactly one hash formula in this format; earlier drafts
described a payload-only hash and are superseded by §2.4.

Adopting a checkpoint means validating everything it references: the
directory's hash, then each referenced frame's hash. A checkpoint whose
shared frames do not validate is rejected in favour of an older one, reached
either by the backward scan or by following `prevFooter`, so a file remains
recoverable as long as any complete checkpoint survives.

The footer at EOF is authoritative. If it is invalid (torn write), readers
scan backwards for the footer magic `ELIP` and validate each candidate:
structural fields in bounds, `prevFooter` older than the candidate, directory
hash matching, and the full directory contents (palette segments, metadata,
dictionary) loading successfully. The newest candidate that validates
completely is adopted; a candidate whose referenced frames fail validation
falls back to the next older one. Everything after the adopted checkpoint is
garbage; appending simply continues at EOF. A torn write therefore loses at
most the work since the last checkpoint.

Recovery trusts in-band data: an attacker who both authors world content and
can induce truncation could embed a forged checkpoint inside record bytes.
Treat files from untrusted sources as untrusted content.

Compaction rewrites the live records (Morton order) into a fresh file and
atomically renames it over the original.

---

### 5.7 Canonical rules for indexed files

- An absent frame reference MUST have offset, length and hash all zero; a
  non-zero offset or hash with a zero length is invalid.
- The directory begins with the semantic header fields (kind, mode, flags, block
  version) so that a checkpoint remains self-describing: recovery validates
  the header against the newest intact checkpoint instead of being defeated
  by damage to the 16 header bytes it is hashed with.
- The directory is a single frame, so the number of entries a conforming
  implementation can address is bounded by its directory decode limit rather
  than by the chunk ceiling of §8. The reference implementation accepts
  4,194,304 directory entries.

## 6. Structure files (kind = 1, mode = 0)

A structure is a free-standing box of blocks with a paste anchor. The body is
a single unit like solid mode:

```
meta block       §4.1 (settings/markers/border empty; userData usable; no stats)
block palette    §3.1
biome palette    §3.2 with count = 0   (structures store no biomes)
blob table       §3.4
sizeX,Y,Z        uvarint × 3      dimensions in blocks (≥ 1; ≤ 1 048 576)
originX,Y,Z      svarint × 3      paste anchor offset
cellPresence     bitset(cells)
present cells:
  layerN         uvarint  (≤ 256)
  blobRef[layerN] uvarint
beN              uvarint
be[beN]:
  x, y, z        uvarint × 3      structure-local position
  nbt            blob             x/y/z stripped as in §4.4
entN             uvarint
ent[entN]:
  nbt            blob             Pos is structure-local
```

A structure header MUST set no flags other than `Uncompressed`, its settings,
markers and border blobs MUST be empty, and its biome palette MUST have zero
entries. Decoders MUST reject files that violate this, so one structure has
exactly one valid envelope.

The box is covered by 16³ **cells**: `cells{X,Y,Z} = ceil(size/16)`. The cell
at cell-coordinates (cx, cy, cz) has index

```
(cx * cellsZ + cz) * cellsY + cy
```

(x-major, then z, then y). Edge cells are zero-padded with air; positions
inside a cell use the standard section index (§1). Absent cells are all air.

---

## 7. Metadata compounds

These NBT schemas are conventions of the reference implementation; readers
should treat all fields as optional.

### 7.1 Settings

`name` (string), `spawnX/spawnY/spawnZ` (int), `time` (long), `timeCycle`
(byte), `rainTime` (long), `raining` (byte), `thunderTime` (long),
`thundering` (byte), `weatherCycle` (byte), `requiredSleepTicks` (long),
`currentTick` (long), `defaultGameMode` (int), `difficulty` (int),
`tickRange` (int).

### 7.2 Markers

Compound `{markers: [compound]}`; each marker has `name` (string), `kind`
(string), `pos` (list of 3 doubles), plus arbitrary extra keys. Sorted by
name.

### 7.3 Border

Compound `{min: int_array[2], max: int_array[2]}`: the inclusive XZ block
bounds of the playable area. Advisory.

---

## 8. Limits

These are the values a conforming decoder MUST enforce. The count limits are
allocation guards: the decompressed-payload ceilings bind first in practice,
so a legal file cannot approach the largest counts. Where a layout table and
this table disagree, this table is normative.

Decoders MUST enforce (reference values):

These are **validity rules**: a file exceeding them is invalid, so raising one
later would make new files unreadable to existing readers. They are set at
what the underlying models can represent, not at what a deployment expects to
use. Allocation safety comes from bounding every count by the input bytes that
remain, not from these ceilings.

| item | limit |
|------|-------|
| string length | 64 KiB |
| blob length | 16 MiB |
| chunk records in a solid body | 4 294 967 296 |
| entries in an indexed directory | 4 194 304 |
| decompressed size of a solid body | 512 MiB |
| decompressed size of an indexed data frame (record, palette segment, metadata, dictionary) | 64 MiB |
| decompressed size of an indexed directory frame | 512 MiB |
| structure cells | 1 048 576 |
| structure size per axis, in blocks | 1 048 576 |
| global palette entries | 1 048 576 |
| blob table entries | 16 777 216 |
| section blob local palette entries | 65 536 (the u16 index width) |
| state properties per palette entry | 64 |
| sections per chunk | 4 096 (the full int16 block-Y domain) |
| layers per section | 256 (Bedrock encodes the storage count in a byte) |
| entities / block entities / ticks per chunk | 1 048 576 each |
| stored frame length (indexed) | 4 294 967 295 |

Writers MUST additionally refuse content that their own readers would reject:
a record whose decompressed size exceeds the reader's frame ceiling, or NBT
nested deeper than the reader accepts, is data loss even though every
individual field is within limits.

Decoders MUST NOT panic on any input; every violation is a clean error. Sizes
derived from untrusted counts must be validated before allocation.

---

## 9. Implementation guidance

- **Reading a solid file**: validate header and footer, verify the checkpoint
  hash (§2.4), decompress, then parse the body sequentially. Trailing bytes after the last
  record are an error.
- **Metadata-only access** needs only the meta block at the start of the body
  (stream-decompress and stop).
- **Unknown block states**: keep the palette entry, substitute a placeholder
  at runtime, and re-emit the original entry (at every position still holding
  the placeholder) when writing back: a load/save round trip must not destroy
  data the runtime doesn't understand. The reference implementation carries a
  per-column sidecar of (section, layer, index, state), where the section is
  an **absolute** section Y so the entry stays valid if the column is re-based
  onto a different vertical range; scheduled block updates carry an equivalent
  sidecar.
- **Bounded decompression**: a compressed frame declares its decompressed
  size and decompressors typically preallocate it, so cap the accepted
  decompressed size per payload type (the reference implementation uses
  512 MiB for a solid body, 64 MiB for an indexed data frame and 512 MiB for
  an indexed directory) rather than
  relying on the input being small.
- **Writing**: always write to a temporary file and atomically rename; fsync
  before renaming.
- The dedup + canonical-blob machinery is where solid mode's size wins come
  from: flat or repetitive worlds collapse to a handful of unique blobs.
