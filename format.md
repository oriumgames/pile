# Pile World File Format (v1)

This document describes the binary file format used by Pile, a compact single-file world format based on Polar, with several structural and behavioral differences. Pile stores one file per dimension:
- overworld: overworld.pile
- nether: nether.pile
- end: end.pile

The format is designed for fast load/store of small worlds and supports streaming, compression, and paletted section storage.

Status:
- Magic number: 0x50696C65 ("Pile")
- Version: 1
- Endianness: Big-endian for fixed-size integers; variable-length integers are signed LEB128 (Go encoding/binary Varint)
- Compression: Zstandard (optional)
- Streaming saves supported (uncompressed length header may be a placeholder)

---

## Binary conventions

- uint8/uint16/uint32/int16/int32/int64: Big-endian
- varint: Signed LEB128 using Go’s `encoding/binary` Varint. Where counts/lengths are encoded with varint, they must be non-negative (the decoder rejects negative values).
- string: varint length (bytes), followed by UTF-8 bytes. Maximum length: 1 MiB.
- bytes: varint length, followed by that many bytes. Maximum length: 16 MiB.
- bool: encoded as a single byte 0 or 1 (only used internally inside the NBT metadata; the file format itself uses explicit integer and varint fields).

Indexing inside 16x16x16 sections uses a linear index `i` in [0, 4095] mapped as:
- x = i & 0xF
- z = (i >> 4) & 0xF
- y = (i >> 8) & 0xF

In other words, x increases fastest, then z, then y.

---

## Top-level file layout

File = Header + Data

Header (always uncompressed):
- uint32 magic = 0x50696C65
- int16 version = 1
- uint8 compression:
  - 0 = none
  - 1 = zstd
- varint data_length
  - Intended to be the uncompressed length of the world data (for non-streaming writers).
  - Readers MUST NOT rely on this value (streaming writers may write 0 as a placeholder). It is safe to ignore.

Data:
- If compression == 1: the remainder of the file is a zstd stream that contains the "World data" payload below.
- If compression == 0: the remainder is the "World data" payload uncompressed.

---

## World data payload

World:
- int32 min_section
- int32 max_section
  - Sections are addressed in the half-open range [min_section, max_section). The number of sections is `max_section - min_section`.
  - `min_section` and `max_section` are derived from the dimension Y-range: `min_section = minY >> 4`, `max_section = maxY >> 4`.
- bytes world_user_data
  - Arbitrary world metadata. In Pile this is used to store world settings as an NBT compound (see “World settings metadata”).
- varint chunk_count (0..1_000_000)
- chunk[chunk_count]

Notes:
- The order of chunks is not specified and should not be relied upon by readers.

---

## Chunk record

Chunk:
- int32 x
- int32 z
- section[(max_section - min_section)]
  - Each section is encoded in full; empty sections use a compact “empty section” encoding (see below).
- varint block_entity_count
- block_entity[block_entity_count]
- varint entity_count
- entity[entity_count]
- varint scheduled_tick_count
- scheduled_tick[scheduled_tick_count]
- bytes heightmaps
  - Reserved for future use. May be empty.
- bytes chunk_user_data
  - Reserved for future use. May be empty.

---

## Section encoding

A section is a 16x16x16 cube of blocks and per-block biomes. Data is stored paletted for compactness.

Section:
- Block palette:
  - varint block_palette_size = N
  - string block_name[N] (e.g., "minecraft:stone")
  - varint block_data_len = Lb
  - int64 block_data[Lb] (paletted indices, bit-packed)
- Biome palette:
  - varint biome_palette_size = M
  - string biome_name[M] (e.g., "minecraft:plains")
  - varint biome_data_len = Lm
  - int64 biome_data[Lm] (paletted indices, bit-packed)

Empty section encoding (canonical):
- Block palette: size = 1, entry = "minecraft:air", block_data_len = 0
- Biome palette: size = 1, entry = "minecraft:plains", biome_data_len = 0

### Paletted int64 packing

- Bits per entry `b = ceil(log2(palette_size))`. If `palette_size <= 1`, `b = 0`, and no data words are written (all values are index 0).
- Values are packed into 64-bit words, least-significant-bits first, with:
  - values_per_long = floor(64 / b) (if b == 0, array is empty)
  - For index `i`, the destination word is `long_idx = i / values_per_long`
  - Bit offset within that word is `bit_offset = (i % values_per_long) * b`
  - The `b` bits for the palette index are written at that offset in the word’s least significant bits.
- The linear index `i` uses the (x, z, y) ordering described in “Binary conventions”.

---

## Block entities

- varint block_entity_count
- block_entity[block_entity_count]

block_entity:
- uint8 packed_xz
  - x: bits 0..3 (0..15), z: bits 4..7 (0..15)
  - Only 4 bits are used for each of x and z (local within the 16x16 chunk).
- int32 y (absolute Y)
- string id (e.g., "minecraft:chest")
- bytes data (NBT compound, uninterpreted by the format)

---

## Entities

- varint entity_count
- entity[entity_count]

entity:
- string identifier (e.g., "minecraft:zombie")
- string uuid (RFC4122 textual form, e.g., "123e4567-e89b-12d3-a456-426614174000")
- bytes data (NBT compound, optional)
  - The payload may include position, rotation, velocity, and other fields. Pile does not enforce a schema; consumers may infer from their runtime.

Notes:
- The identifier and UUID are stored explicitly for fast indexing and stable identity. The NBT payload may or may not duplicate this data.

---

## Scheduled ticks

- varint scheduled_tick_count
- scheduled_tick[scheduled_tick_count]

scheduled_tick:
- uint8 packed_xz (same 4-bit-per-axis packing as block entities)
- int32 y (absolute Y)
- string block (block identifier who owns the tick, e.g., "minecraft:oak_sapling")
- varint tick (int64; absolute tick time)

---

## Heightmaps (reserved)

- bytes heightmaps
  - Currently unused. If present, the interpretation is application-defined. May be empty.

---

## Chunk user data (reserved)

- bytes chunk_user_data
  - Reserved for application-defined metadata. May be empty.

---

## World settings metadata (stored in world_user_data)

Pile stores world settings inside `world_user_data` as an NBT compound. Keys and types:

- name: string
- spawnX: int32
- spawnY: int32
- spawnZ: int32
- time: int64
- timeCycle: bool
- rainTime: int64
- raining: bool
- thunderTime: int64
- thundering: bool
- weatherCycle: bool
- currentTick: int64
- defaultGameMode: int32 (engine-specific enum ID)
- difficulty: int32 (engine-specific enum ID)

Readers should treat this blob as optional; files may omit or leave it empty.

---

## Compression

- compression == 0 (none): The world data payload follows uncompressed.
- compression == 1 (zstd): The world data payload follows as a Zstandard stream. Encoders may choose different compression levels; readers must accept any valid zstd stream.

Encoders:
- Non-streaming encoders typically compute and write the uncompressed payload into memory, optionally compress, write header (with `data_length` = length of uncompressed payload), then write the payload.
- Streaming encoders write the header and then stream the world data chunk-by-chunk (possibly through a streaming zstd encoder). In this case, `data_length` may be 0 or a placeholder and should be ignored by readers.

Readers:
- MUST ignore `data_length` and read until EOF of the stream.
- MUST support both compressed and uncompressed payloads.

---

## Limits and validation

- Strings: length <= 1 MiB (decoder rejects larger lengths).
- Byte arrays: length <= 16 MiB (decoder rejects larger lengths).
- Counts: chunk_count, block_entity_count, entity_count, scheduled_tick_count must be >= 0. `chunk_count` additionally must be reasonable (the reference decoder rejects > 1,000,000).
- Paletted arrays:
  - If `palette_size <= 1`, the corresponding data array length is 0 and all values are the first palette entry.
  - If packed data is shorter than required, out-of-range indices are treated as 0 (first palette entry) by tolerant consumers.
- Chunk order is unspecified.
- Unknown or extra metadata fields should be ignored by consumers.

---

## Versioning

- File header contains a version (int16). The current and maximum supported version is 1.
- Readers should reject files with a version greater than supported.
- Backward-compatible additions should be done by extending reserved/user data sections or by adding fields that can be safely skipped by older readers.

---

## Implementation notes

- Section indexing across Y: The i-th section in a chunk corresponds to Y-section index `(min_section + i)`. Within a section, block Y is the relative 0..15 value described under “Binary conventions.”
- Local X/Z are always 0..15 and packed into `packed_xz` with 4 bits per axis.
- Lighting data is not stored in the format. Consumers should recalculate lighting when loading worlds.
- When writing empty sections, prefer the canonical empty-section encoding described above.
