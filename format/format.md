# Pile File Format, Version 2

This document is the complete specification of the pile binary format, written
so the format can be implemented in any language without reference to the Go
implementation. The Go implementation in this package is the reference; where
this document and the implementation disagree, the implementation wins and the
document has a bug.

A pile file stores one Minecraft (Bedrock) dimension (named in the header, §2.3) or one structure in a
single file. Design goals: minimal size for small worlds, deterministic output
(identical content ⇒ identical bytes), integrity checking, crash safety, and
an optional append-oriented mode for large worlds.

- Header magic: the four bytes `P I L E`
- Footer magic: the four bytes `E L I P`
- Version: 2
- Compression: Zstandard (see §2.5 for the constraints on a frame)
- Checksums: xxHash64, **seed 0**

---

## 1. Primitives and conventions

All fixed-width integers are **little-endian**.

| name    | encoding |
|---------|----------|
| `u8`, `u16`, `u32`, `u64`, `i32` | fixed-width little-endian |
| `uvarint` | unsigned LEB128 (Go `binary.PutUvarint`): 7 bits per byte, high bit = continuation. MUST be minimal: decoders reject overlong encodings |
| `svarint` | zigzag-encoded LEB128 (Go `binary.PutVarint`): value `v` maps to `uvarint((v << 1) ^ (v >> 63))`. MUST be minimal |
| `string` | `uvarint` byte length + UTF-8 bytes. Decoders MUST reject lengths > 65 535, the largest an NBT string length can express, so one concept has one ceiling, and MUST reject bytes that are not valid UTF-8: strings are compared bytewise when ordering palettes, so arbitrary bytes would order differently under an implementation that decodes before comparing |
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

`TAG_List` of a numeric type and the corresponding array tag carry the same
values but are different tags, and both occur in vanilla content. Decoders
MUST keep them apart, and encoders MUST re-emit each value with the tag it was
decoded from:

- In **opaque** NBT (entity and block-entity blobs), `TAG_Byte_Array`,
  `TAG_Int_Array` and `TAG_Long_Array` round trip as themselves. Collapsing them into lists would be lossy in a way the game
  notices, since Bedrock stores UUIDs and similar fields as int arrays.
  A decoder that lowers every array into the same host-language value as a
  list cannot produce byte-identical output and is not conforming.
- In the **metadata compounds this document specifies** (§7), the tag of each
  field is fixed by that section and MUST be emitted exactly: the world
  border's `min` and `max` are `TAG_Int_Array`, not lists. Writers MUST reject
  a metadata blob whose fields carry the wrong tag, because a reader cannot
  tell afterwards.

World and chunk **user data are not NBT at all**: they are opaque byte strings
that the format never parses, so no rule in this section applies to them and
any byte sequence within the blob length limit is valid.

Byte identity across implementations therefore requires a decoder that retains
tag kinds. That is a smaller demand than lossy normalisation, which would have
to be specified for every tag pair and would still corrupt vanilla data.
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
| 12 | i32 | blockVersion | the Minecraft block-state version the writer encoded against. MUST be non-zero, for the reason §3.1 gives for overrides: zero is the value that means "the palette's own version" and cannot also be a version |

Readers MUST reject unknown versions and unknown flag bits (they may change
payload meaning). Not every `kind`/`mode` pair exists: a structure is always
solid, so `kind = 1` with `mode = 1` MUST be rejected, and so MUST any value
of either field this table does not list.

**There is no forward-compatibility lane, by decision.** A v2 reader refuses
every v3 file outright rather than skipping what it does not recognise. The
alternative — a class of ignorable extensions — cannot coexist with the
canonicality this format is built on: if a reader may ignore a field, then two
files differing only in that field decode identically, and the guarantee that
one content has one encoding is gone. Every rule in §4.8 rests on that.

The cost is real and is accepted: shipping a v3 means every reader must be
upgraded before any writer produces one, and old readers give a clean
`ErrUnsupportedVersion` rather than a partial read. The mechanism for that is
the version field, not silent tolerance. Reserved bits are required to be zero
for the same reason — a bit an old reader ignores is a bit that changes the
bytes without changing the content.

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

### 2.3 Flags

| bit | name | meaning |
|-----|------|---------|
| 0 | StoreLight | chunk records contain baked light arrays (§4.6). Advisory: light is never required for correctness. In **mode 0** it MUST be clear when no section carries any, since a solid file's flag and content are written together and setting it over a light-free world is a second encoding of that world. In **mode 1** it is a layout decision fixed when the file is created and obeyed by every record thereafter, so an indexed file may carry it with no chunks yet; indexed bytes are history-dependent by design (§5) and light is outside content identity (§4.8), so no second encoding of any content arises |
| 1 | Stats | the meta block contains a stats compound (§4.2) |
| 2 | reserved | MUST be zero (indexed dictionary presence is signalled by the directory, §5.5) |
| 3 | DefaultBiome | bits 16–31 of flags hold a global biome palette reference used as the world default biome (§4.7) |
| 4 | Uncompressed | the body is stored without compression |
| 5–7 | dimension | which dimension a world file holds: 0 overworld, 1 nether, 2 end. Values 3–7 are reserved and MUST be rejected. Structures MUST leave these bits zero |
| 8–15 | reserved | MUST be zero; readers MUST reject files with any reserved or unknown bit set, so those bits stay available for future features |
| 16–31 | defaultBiomeRef | only meaningful when bit 3 is set, and MUST be zero when it is clear |

### 2.4 Checkpoint hash

The footer's hash authenticates the file's semantic header as well as its
payload. Hashing the payload alone would leave `blockVersion` and the flags
(which carry the default-biome reference) unprotected, so a single flipped bit
there would change how the payload decodes while every integrity check still
passed.

```
preimage = header image (16 bytes, §2.1)
        || payload            (solid: the stored body; indexed: the stored directory frame)
        || footer bytes 8..44 (the control words and the footer magic)
hash     = xxHash64(preimage, seed = 0)
```

Every xxHash64 in this format uses **seed 0**, here and in §3.4 and §5.5.

The preimage's first component is the **header image**: the 16 bytes §2.1
lays out for this file's semantic fields. On an intact file that is exactly
the 16 bytes on disk. When those bytes are damaged they are not, and §5.5
requires a writer to hash the image rebuilt from the directory prologue
rather than what it found at offset 0 — so the preimage is defined by the
semantic fields, and the physical bytes are one materialisation of it.

Everything but the hash field itself is therefore covered. Indexed files
recompute this per checkpoint, so `generation` and `prevFooter` are
authenticated too.

### 2.5 Zstandard frames

Compressed payloads are ordinary Zstandard frames, with one constraint that
the decompressed-size ceilings of §8 do not cover on their own:

- The **window size** MUST NOT exceed 8 MiB. A ceiling on the decompressed
  size bounds the output but not the memory a decoder must hold to produce it,
  so without this a small frame can still demand an arbitrary window. Readers
  MUST refuse a frame that asks for more.

A frame is **not** required to declare its content size, and this is a
decision rather than an omission. Earlier drafts required it, on the reasoning
that a reader is then free to size the output in one piece and check it
against §8 before allocating anything. Neither half of that survived contact
with a compressor. Zstandard leaves the field optional and the reference
encoder omits it for payloads of a few hundred bytes, which in indexed mode is
most frames a file contains: requiring it would have made a large share of
already-written files invalid to satisfy a rule the writer could not keep.
And nothing rests on it, because the ceilings of §8 bound a decode that
streams exactly as they bound one that preallocates, so a frame that hides its
size costs a reader a growing buffer and buys an attacker nothing.

Neither point constrains which encoder produced the frame: as §4.8 says, the
compressed bytes are not part of the format's identity.

---

## 3. Shared building blocks

These structures appear in both modes and in structure files.

**Who enforces a rule.** Every `MUST` below is one of two kinds, and which one
is stated wherever it is not obvious. Most are **decoder-enforced**: a reader
rejects a file that breaks them, so a strict reader and a lenient one agree on
which files are valid. A few are **writer-only**, because the evidence a reader
would need is not in the file. Palette ordering is the archetype: reference
counts are never stored, so nothing in a file proves its palette was sorted.
Such a rule is verified by re-encoding and comparing (§4.8's content identity),
never by reading, and it is called out where it appears. A rule nobody can
check on read must not look like one that is checked.

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
  indexDelta     uvarint          delta from the previous overridden index;
                                  the first entry deltas from 0, so a first
                                  delta of 0 means index 0. Every later delta
                                  MUST be non-zero, since indices strictly
                                  ascend, and decoders MUST reject a zero one
  version        i32              MUST be non-zero and MUST differ from the
                                  palette's own version: an override that
                                  repeats it says nothing, so it would be a
                                  second encoding of an entry with none
```

The override indices MUST strictly ascend, and decoders MUST reject a table
whose running index does not increase, including one whose accumulated sum
wraps. The wrap is the case worth stating separately: a uvarint can express the
modular representative of a negative step, so a delta of 2⁶⁴−2 after an
override at index 5 lands on index 3, which is a legal index and therefore
invisible to a bounds test. A reader that accepts it has accepted a second
encoding of one palette, which is the thing the canonical form exists to
prevent. Refusing a zero delta is necessary and not sufficient: it enforces the
ascent only for as long as the sum cannot wrap.

Only preserved unresolved states (§9) can carry one: everything else is
resolved against the live registry and is therefore at the palette's own
version. Decoders MUST upgrade each entry from its own version when it has an
override, and from the palette's version otherwise. Indexed palette segments
carry the same table after their entries.

Property keys ascend bytewise and are unique, and decoders MUST reject a
repeated or out-of-order key. The rule is the one NBT compounds already follow
(§1) and exists for the same reason: without it one state has many encodings,
and a repeated key is worse, since the later value silently wins and two
different files decode to the same state.

Bedrock block state properties are exactly these three NBT types, making the
encoding lossless.

In **mode 0** — that is, in a solid world and in a structure alike — writers
MUST sort the palette by (writer-only: reference counts are not stored, so a
reader cannot verify the order and MUST NOT try):

1. **descending reference count**, where the reference count is the number of
   local palettes the state appears in, plus one per scheduled block update
   referencing it (not the number of blocks holding it). A local palette is a
   section's in a world and a cell's in a structure; structures carry no
   scheduled updates, so that term is zero for them. Counting
   happens before the blob table deduplicates anything, so a section blob
   shared by a hundred sections contributes a hundred, not one. Writers MUST
   count occurrences rather than distinct blobs, since the two disagree
   whenever deduplication succeeds. Counting happens **after** the trailing
   all-air layers of §4.3 are dropped: a layer that never reaches the file
   contributes nothing, and writers MUST NOT count it. There is no circularity
   here, unlike the biome case in §3.2, because dropping a trailing air layer
   does not depend on the palette order; the rule is stated because two writers
   counting on either side of it would order the palette differently and change
   every byte downstream;
2. then **ascending bytewise comparison of the entry's own encoded bytes**,
   that is the length-prefixed `name` followed by the encoded property block
   exactly as written above (so the name's length prefix leads, and a shorter
   name sorts before a longer one);
3. then **ascending `version`**, taking the palette's own version for entries
   with no override.

The tie-break is defined on the encoded bytes rather than on any readable
form, so an implementation in any language reproduces the order without first
having to agree on a string representation. It is also the reason the order is
total: two entries that compare equal at every step encode identical bytes at
the same version and are therefore the same state, which writers MUST merge
into one entry whose reference count is the sum. Decoders MUST reject a palette
that carries both: a section could reference either, so the file would be a
second encoding of one world.

In indexed mode the palette is first-seen order across segments.

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

The **reference count is the number of section-local biome palettes the name
appears in**, exactly as for block states in §3.1, not the number of voxels
holding it; the two disagree whenever a rare biome appears in many sections,
and they select different palette orders, section references and default-biome
references. Counts are taken over **every** section of every chunk, before the
default-biome elision of §4.7 removes any of them from the file. That ordering
is not a detail: elision picks the biome with the most uniform sections, which
depends on the palette order, which would depend on the counts, which would
depend on what elision removed. Counting first breaks the loop, and writers
MUST count that way or two of them will disagree on the palette order, on
`defaultBiomeRef` and therefore on every byte downstream.

Writers MUST sort the biome palette by (writer-only, for the same reason §3.1
gives):

1. **descending reference count**, as defined above;
2. then **ascending bytewise comparison of the name**, the same comparison
   §3.1 applies to a block entry's encoded bytes. Names are UTF-8 (§1) and are
   compared as bytes rather than as decoded code points, so no implementation
   needs a collation table to agree.

The order is total because names are unique: one entry per biome, and decoders
MUST reject a repeated name, for the reason §3.1 gives for block states.

Names, not numeric IDs, so entries stay stable across game versions. Names are
**fully qualified**: every one MUST contain a namespace and a colon, and a bare
name such as `plains` is invalid rather than a second spelling of
`minecraft:plains`. Servers whose own registries name vanilla biomes without a
namespace qualify them at the boundary. Unknown biomes decode as
`minecraft:plains` while the palette entry keeps the original name, so a read
and rewrite does not rename them.

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
- `width = 1` requires `paletteN ≤ 256`; `width = 2` requires `paletteN > 256`.
  The narrowest sufficient width is the only valid one, so writers MUST select
  it and decoders MUST reject any other.
- References MUST ascend strictly, so a section has exactly one encoding and
  cannot carry duplicate or unused entries.
- Each index selects a local palette entry; decoders MUST reject an index
  greater than or equal to `paletteN`.
- **Canonical form** (required from writers *and enforced by decoders*, which
  reject non-canonical blobs; this enables dedup and determinism):
  refs sorted ascending; indices remapped accordingly; the palette contains
  only entries actually used by the indices; a storage whose used palette
  collapses to one entry MUST be written uniform (width 0).
- Byte-aligned indices are deliberate: zstd's entropy stage compresses them
  near-optimally and encode/decode becomes a copy + remap.

### 3.4 Section blob table (solid and structure only)

Deduplication container: every unique section blob stored once, referenced by
index. Identical bytes MUST share one entry, and decoders MUST reject a table
that repeats a blob or that carries one no record references: the first is a
second encoding of the same file and the second is content nothing reads.

```
count            uvarint          (≤ 16 777 216)
blob[count]      section blobs, concatenated (self-delimiting)
```

Blob ids are assigned in first-use order over the stream of stored units: the
Morton-sorted records of a solid world, or the ascending cells of a structure.
Within one record the order is the order §4.3 writes the fields in: present
block sections by ascending section index, and within each section its layers
by ascending layer number, then present biome sections by ascending section
index. Within a structure it is the order §6 writes them: present cells by
ascending cell index, and within each cell its layers by ascending layer
number. Structures store no biomes, so no biome blobs arise there. That is deducible from the field order, but the whole table's
identity depends on it, so writers MUST assign ids in exactly that sequence.

Block and biome blobs share the table; a blob's refs are interpreted against
the block or biome palette according to the use site.

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
chunkN           uvarint          (≤ 4 194 304, §8)
record[chunkN]   §4.3, sorted by Morton key of (x, z)
```

Chunk positions are unique and Morton keys therefore **strictly** ascend.
Writers MUST reject a world holding two columns at one position, and readers
MUST reject a file whose record keys do not strictly ascend: a duplicate has no
defined meaning, since nothing tells a reader which of the two records wins,
and a non-ascending file is a second encoding of a world that already has one.
The same rule applies to indexed directory entries (§5.5).

Nothing may follow the last record, and decoders MUST reject a body with bytes
left over once the last record has been read; the same holds for a structure
body (§6) after its last entity. Such bytes change nothing the file decodes to,
so admitting them would give one world many encodings.

**Section span.** `minSection` and `sectionN` describe the chunk's whole
vertical range, section-aligned per §1. Writers MUST NOT trim leading or
trailing empty sections, and an empty chunk carries its dimension's full span
with every presence bit clear. Trimming would give one chunk several encodings
and leave the content outside the span undefined; the span costs two varints.

**Layer count.** A layer is selected by an 8-bit index, so 255 is the last one
that can be addressed: with 256 layers present, no index names the last, and
any length-versus-index comparison done in that same 8-bit type wraps to zero.
The ceiling is therefore 255, and decoders MUST reject a larger `layerN`.
Bedrock encodes the storage count in a byte, so 256 is expressible on the wire
and a file may well claim it; that claim describes a layer nothing can read.

> Concretely, dragonfly grows a sub chunk's storage slice with
> `for uint8(len(storages)) <= layer`, which at 256 storages compares zero
> against every index and appends without end. A file claiming 256 layers is
> not merely unusual there, it wedges the process that opens it. Other
> implementations will fail differently; the rule holds regardless, because
> the 256th layer is unaddressable by construction.

**Layer numbering.** Layer numbers are semantic: layer 0 is the block and layer
1 is Bedrock's waterlogging layer. A layer therefore cannot be dropped for
being all air unless every layer above it is dropped too, because removing an
internal one renumbers the layers above it and turns waterlogging into a solid
liquid. Writers MUST drop **trailing** all-air layers, since a layer past the
last stored one already reads as air, and MUST keep internal ones, encoded as
an ordinary uniform section blob referencing air. A layer holding a preserved
unresolved state MUST NOT be treated as all air, whatever its placeholder
resolves to: dropping it discards the state it was preserving.

### 4.1 Meta block

```
settings         blob             NBT; world settings (§7.1); may be empty
userData         blob             application-defined; may be empty
markers          blob             NBT (§7.2); may be empty
border           blob             NBT (§7.3); may be empty
stats            blob             present iff flag Stats; NBT (§4.2)
```

### 4.2 Stats compound (optional)

NBT compound holding `chunks` (long), `filledSections` (long), `uniqueBlobs`
(long), `blockStates` (long) and `biomes` (long). They are longs because the
format's own ceilings exceed what a 32-bit tag can hold. Tools may read it via
the meta block without decoding chunk data.

The same split as §7 applies, and for the same reason: a writer emits all five,
but a compound missing one is **valid** and a reader MUST NOT reject it, while
a key that is present MUST carry the tag named here. Readers MUST ignore keys
they do not know, so a later version can add one without invalidating this
one's files. Stats are a summary of the payload and carry no information the
payload lacks, which is why absence is tolerable here and a wrong tag is not.

### 4.3 Chunk record

```
dx, dz           svarint          chunk position delta from the previous
                                  record (the first record deltas from (0,0))
minSection       svarint          lowest section index (blockY = section*16);
                                  -2048 ≤ minSection and minSection+sectionN ≤ 2048,
                                  so every block Y is an addressable int16
sectionN         uvarint          section count (1 ≤ sectionN ≤ 4096);
                                  the chunk's full vertical range, never trimmed
blockPresence    bitset(sectionN) bit i set = section i has block data
present sections, ascending i:
  layerN         uvarint          storage layers (1 ≤ layerN ≤ 255);
                                  layer 1 is Bedrock's waterlogging layer;
                                  trailing all-air layers omitted, internal ones kept
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

A section MUST be absent when **every** one of its layers is uniform air, and
MUST be present otherwise. A layer holding a preserved unresolved state (§9)
does not count as air for this test any more than it does for the layer rule
below, so a section whose only content is such a state is present. A section
that has any non-air content is present, and the uniform-air layers below that
content are stored, for the reason the layer numbering rule below gives. A
decoder MUST NOT treat a uniform-air layer 0 as an absent section: in a
waterlogged-only section that layer is the state, and dropping it loses the
water above it.

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

Writers MUST store `UniqueID` as a long and readers MUST take it verbatim.
**Zero is a legal value**, and rewriting it (to avoid collisions in some other
storage layer, say) would mean encode, decode and encode again produce
different bytes. A reader that meets a compound with no `UniqueID`, or one of
the wrong type, may substitute an id of its choosing, since no conforming
writer produces that.

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
global biome palette entry. Sections whose biomes are uniformly that biome MUST be
omitted from `biomePresence`, and decoders MUST fill absent sections with the
default.
Writers MUST set the flag whenever the file holds at least one section whose
biomes are uniform and the chosen reference fits the 16 bits available; they
MUST leave it clear otherwise, including when the reference does not fit. The
flag is not an optimisation a writer may decline, because declining it is a
second encoding of the same world.

Not falling back to a runner-up when the winner's reference exceeds 16 bits is
deliberate, not an oversight. A search for the best biome that still fits would
be a second rule for two writers to disagree about, and the case needs a palette
of more than 65 536 biomes to arise at all; giving up the flag entirely is one
rule with one outcome.

Writers pick the biome with the most uniform sections, breaking ties by the
**lowest global biome palette reference**. Without a tie-break two conforming
writers could set a different `defaultBiomeRef` and clear a different presence
bit for the same world. Without the flag, absent biome sections decode as
`minecraft:plains`, the same fallback §3.2 gives an unresolved name. It is a
name rather than a palette reference because the palette may legitimately be
empty (a file written with biomes skipped), so a reference would be out of
range in exactly the files that need it; and a name rather than a numeric id
because ids are a property of the running game version, and one file MUST NOT
decode to different biomes on two of them.

A default biome may itself be a name no registry resolves. Readers that
preserve unresolved biomes MUST report the elided sections through the same
mechanism they use for stored ones, or a read and rewrite renames the biome to
the reader's fallback.

### 4.8 Determinism

Writers targeting deterministic output MUST: sort records by Morton key; sort
the block palette as §3.1 specifies and the biome palette by reference count as §3.2
defines it (ties: name); emit canonical section blobs (§3.3); sort NBT compound keys; and
sort every per-column collection totally.

The collection orders are:

| collection | order |
|------------|-------|
| block entities | (y, z, x), then the encoded NBT **as written**, with the x/y/z keys already stripped |
| entities | `UniqueID`, then the encoded NBT **as written**, with `UniqueID` already set to that value |
| scheduled updates | (y, z, x), then firing tick, then block palette reference |
| structure block entities | (y, z, x), then the encoded NBT as written |
| structure entities | the encoded NBT alone: a structure entity is a bare compound with no separate ID field, so `UniqueID` is simply one of its keys |

These orders are total only because the keys they sort on are unique, which is
therefore a rule and not an assumption. At most one block entity may occupy a
position and at most one scheduled update may name a given position, firing
tick and block reference; a structure's block entities are subject to the same
rule and MUST additionally lie inside the declared box. Decoders MUST reject a
file that breaks any of these, because two entries a reader cannot tell apart
leave their order decided by nothing.

The tie-break is the bytes that get written, not the caller's value. A block
entity's x/y/z are stripped on encode and an entity's `UniqueID` is replaced
with its authoritative ID, so ordering on the unprojected input would let
discarded values decide the file.

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

A frame's content ends where the structure it holds ends. Bytes past that
point are a second encoding of the same content, since the frame's length and
hash are recorded in the directory and the content is unchanged by them, so
decoders MUST reject a frame of any kind that carries them. The dictionary
frame is the exception the rule does not reach: it holds opaque Zstandard
dictionary bytes with no structure to end.

When a shared dictionary is present (§5.5): record frames, palette segment
frames and meta frames are compressed with the dictionary; the **directory
frame and the dictionary frame itself MUST be compressed without it** (they
are read before the dictionary can be loaded). A dictionary-aware decoder
handles both (zstd frames carry the dictionary id). A dictionary means nothing
to a file whose frames are stored raw, so when flag `Uncompressed` is set the
directory's dictionary reference MUST be absent, and a reader MUST reject a
file that carries both.

### 5.2 Record frames

One frame per stored chunk. Content = a chunk record as in §4.3 **except**:

- no `dx`/`dz` (position lives in the directory),
- section blobs are stored **inline** (the section blob bytes of §3.3 appear
  in place of each `blobRef`); there is no blob table,
- no default-biome elision (flag DefaultBiome is never set),
- light follows the header's StoreLight flag.

Palette references point into the cumulative global palettes (§5.3).
Overwriting a chunk appends a new frame; the old one becomes garbage until
compaction. Deleting a chunk is the same operation without a replacement: the
next checkpoint's directory simply omits its entry, and the frame it named
becomes garbage. There is no tombstone, because a directory names exactly the
chunks that exist.

### 5.3 Palette segments

The global palettes grow append-only as **delta segments**. A block segment
frame is an `i32` Minecraft block version followed by a §3.1-encoded palette;
a biome segment frame is a §3.2-encoded palette. Segments hold only the
entries new since the previous checkpoint, and a segment with no entries is
never written: it is pure garbage that two writers could differ on, and
decoders MUST reject one. The directory MUST list the segments in the
order they were written, since entry indices are cumulative across segments and
reordering the list renumbers every palette reference in the file. Palette
order is first-seen; no frequency sorting.

A directory naming no chunks at all is legal: that is what a freshly created
file has, and it is not the same thing as an empty segment.

Each block segment carries its own version because a file outlives game
upgrades: states written before an upgrade must still be upgraded from the
version they were written at, while states appended afterwards must not be.
Decoders MUST enforce the palette limits **cumulatively** across segments and
MUST reject duplicate segment references (both are allocation amplifiers).

### 5.4 Meta frame

`settings`, `userData`, `markers`, `border` blobs, §4.1 layout without the
stats field. A new meta frame is appended when metadata changes; the directory
points at the latest.

Indexed mode therefore has nowhere to put a stats compound, and no section to
elide a default biome from. Flags `Stats` and `DefaultBiome` MUST be clear in
an indexed file, and readers MUST reject one that sets either: a flag whose
payload the layout cannot hold is a claim the file cannot keep.

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
  dx, dz         svarint          delta from previous entry; the first
                                  entry deltas from (0,0)
  offDelta       svarint          frame offset delta from previous entry;
                                  the first entry deltas from 0
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

Because the prologue is the authority, a writer that has opened a file with a
damaged physical header MUST hash **new** checkpoints against the canonical
header image rebuilt from the semantic fields, never against the damaged bytes
it found on disk. Hashing the damaged bytes would tie the world to its own
corruption: repairing the header, or opening the file with a reader that
trusts the prologue as specified, would invalidate the newest checkpoint and
roll the world back.

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

---

## 6. Structure files (kind = 1, mode = 0)

A structure is a free-standing box of blocks with a paste anchor. The body is
a single unit like solid mode:

```
meta block       §4.1 (settings/markers/border empty; userData usable; no stats)
block palette    §3.1
biome palette    §3.2 with count = 0   (structures store no biomes)
blob table       §3.4
sizeX,Y,Z        uvarint × 3      dimensions in blocks (≥ 1; ≤ 1 048 576 each,
                                  and see the cell ceiling below, which binds first)
originX,Y,Z      svarint × 3      paste anchor offset; each MUST be in int32
                                  range, so that one value has one encoding
cellPresence     bitset(cells)
present cells:
  layerN         uvarint  (1 ≤ layerN ≤ 255)
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

**Cell canonicalisation.** The §4.3 rules for chunk sections apply to cells
unchanged, and are normative here rather than merely inherited:

- A cell whose layers are all air is **absent**: its presence bit is clear.
  Setting the bit and storing a uniform air layer is not an alternative
  spelling of the same cell.
- `layerN` is at least 1 for a present cell. A present cell with no layers
  would be an absent cell written the long way.
- Trailing all-air layers are dropped and internal ones are kept, for the
  reason §4.3 gives: layer numbers are semantic.
- Positions inside a cell but outside the structure's box are **air in every
  layer**. The box need not be a multiple of 16, so edge cells have padding,
  and two structures that differ only in that padding are the same structure
  and MUST encode identically.

The box is covered by 16³ **cells**: `cells{X,Y,Z} = ceil(size/16)`. Readers
MUST compute that rounding and the resulting product in at least 64 bits and
check it against the cell ceiling of §8 **before** allocating anything: each
axis alone may be as large as 1 048 576, so `size + 15` overflows a 32-bit
value near the top of its range and their product overflows one far below it,
and a truncated product turns an impossible structure into a small allocation
that then disagrees with the presence bitset. The cell ceiling binds long
before the per-axis one for any structure that is not a sliver.

The cell at cell-coordinates (cx, cy, cz) has index

```
(cx * cellsZ + cz) * cellsY + cy
```

(x-major, then z, then y). Edge cells are zero-padded with air; positions
inside a cell use the standard section index (§1). Absent cells are all air.

---

## 7. Metadata compounds

Two different things live in this section, and conflating them is how a strict
reader and a lenient one end up disagreeing about what is a valid file:

- **Which fields exist is a convention.** Every field is optional, readers
  ignore ones they do not know, and a compound carrying none of them is valid.
  Nothing here is required to be present.
- **How a field is spelled is a rule.** When one of these fields *is* present
  it MUST carry the tag and shape stated below, and the marker list MUST be
  ordered as stated. Readers MUST enforce this exactly as writers do: an
  unsorted marker list is a second encoding of one marker collection, so a
  reader that accepts what a writer refuses to produce disagrees with it about
  what a valid file is. A writer that emits `time` as an int, or an unsorted
  marker list, produces an invalid file even though the field itself was
  optional.

The reason for the split is that a decoder into a dynamically typed map cannot
recover which tag a value came from, so a wrong tag is not a difference a later
reader can detect or repair. Presence, by contrast, is always detectable.

### 7.1 Settings

`name` (string), `spawnX/spawnY/spawnZ` (int), `time` (long), `timeCycle`
(byte), `rainTime` (long), `raining` (byte), `thunderTime` (long),
`thundering` (byte), `weatherCycle` (byte), `requiredSleepTicks` (long),
`currentTick` (long), `defaultGameMode` (int), `difficulty` (int),
`tickRange` (int).

The tag of each listed field is fixed and writers MUST reject a blob carrying
the wrong one: `time` as an int rather than a long is a different encoding of
the same setting, and no decoder can tell afterwards. Keys not listed here are
preserved verbatim and unconstrained.

### 7.2 Markers

Compound `{markers: [compound]}`; each marker has `name` (string), `kind`
(string), `pos` (list of 3 doubles), plus arbitrary extra keys. The list is
sorted by `name`, **strictly** ascending: names are unique, since two markers
with one name would have no defined order and no way to be told apart.
Writers MUST reject an unsorted list rather than copy it through, because the
same marker collection would otherwise have as many encodings as it has
permutations.

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
| NBT nesting depth | 64 |
| NBT containers per blob (every compound or list nested inside another) | 1 048 576 |
| section storages decoded per file | 4 194 304 |
| checkpoint chain links followed during recovery | 256 |
| directory entries parsed during one recovery, summed over every candidate tried | 16 777 216 |
| string length | 65 535 |
| blob length | 16 MiB |
| columns decoded per file: chunk records in a solid body, entries in an indexed directory | 4 194 304 |
| decompressed size of a solid body | 512 MiB |
| decompressed size of an indexed data frame (record, palette segment, metadata, dictionary) | 64 MiB |
| decompressed size of an indexed directory frame | 512 MiB |
| structure cells | 1 048 576 (binds first: it caps a cubic structure near 160 blocks per axis) |
| structure size per axis, in blocks | 1 048 576 (a ceiling on any one axis; only reachable when the others are small) |
| global palette entries | 1 048 576 |
| blob table entries | 16 777 216 |
| section blob local palette entries | 65 536 (the u16 index width) |
| state properties per palette entry | 64 |
| sections per chunk | 4 096 (the full int16 block-Y domain), placed within section indices -2048..2047 |
| layers per section | 255 (Bedrock encodes the storage count in a byte, but the 256th layer is not addressable, see §4.3) |
| entities / block entities / ticks per chunk | 1 048 576 each |
| stored frame length (indexed) | 4 294 967 295 |

Writers MUST additionally refuse content that their own readers would reject.
This applies to the **aggregate** ceilings, not only the per-field ones: a
solid body assembled from individually legal blobs can still pass 512 MiB, and
an indexed metadata frame built from four legal 16 MiB blobs already exceeds
the 64 MiB frame ceiling. Every completed body and every appended frame is
therefore checked against its own limit before compression, along with the
stored frame length against its own ceiling. That ceiling is a validity rule
in its own right rather than the width of any field: a directory carries a
frame length as a `uvarint` and the footer carries the directory's own as a
`u64`, so nothing on the wire truncates, but 2³²−1 is the largest value a
reader may be asked to hold in the 32-bit counter such a length naturally
fits, and a file naming more is invalid. A record whose
decompressed size exceeds the frame ceiling, or NBT nested deeper than 64, is
data loss even though every individual field is within limits. The nesting
ceiling is a fixed number rather than a reader's choice, so one file is not
readable by one implementation and refused by another. In indexed mode it is worse than data loss: a checkpoint that
cannot be reopened rolls the world back to an older one, discarding every
chunk stored since, so metadata is validated when it is handed to the writer
rather than when it is finally written.

Decoders MUST NOT panic on any input; every violation is a clean error. Sizes
derived from untrusted counts must be validated before allocation.

Bounding a count against the bytes that remain is necessary and not sufficient,
because several of the values a file declares cost far more to decode than to
write. One blob reference is a single byte and becomes a live section storage;
one byte of `TAG_End` inside a list of compounds becomes a whole map; an
eleven-byte chunk record becomes a whole column; a
44-byte footer names a directory frame that may decompress to 512 MiB. Decoders
MUST therefore bound the **result** as well as the input, and the decoded
ceilings above exist for exactly that. A conforming reader rejects a file that
would decode into more section storages, more NBT containers, more columns, a
longer recovery chain or more total recovery work than they allow, even though
every individual field is within its own limit.

Three of those need saying more precisely, because each of them was stated once
and left unimplemented.

**The NBT container budget charges every container nested inside another.**
Decoders MUST charge a compound or a list that is an element of a list, and a
compound or a list that is a field of a compound, and MUST NOT charge the
blob's own root compound, which is the blob rather than something inside it. A
compound nested in a compound costs six bytes on the wire — a tag, a two-byte
name length, a name that cannot repeat a sibling's, and `TAG_End` — and becomes
a map exactly as a list element does. Charging only the list case left the
ceiling above stated and unenforced, at twice over: two million sibling
compounds fit inside the 16 MiB blob limit.

**The column ceiling is one number for both modes.** Decoders MUST refuse a
solid body declaring more chunk records than the ceiling above, and an indexed
directory declaring more entries, and the two are deliberately the same value:
a column costs the same to hold whichever named it. A solid file holds every
column at once by design, so 4 194 304 of them is already about four gigabytes
of live objects — this is a ceiling at the point where a world stops being
decodable rather than one chosen to be comfortable. For scale, a real overworld
of ten thousand chunks is four hundred times below it, and every column holding
a single block already consumes one of the 4 194 304 decoded section storages,
so the only thing this ceiling newly refuses is a world with more than four
million *empty* columns. The number it replaces, 2³²−1, was the width of the
count field and not a bound on anything: an eleven-byte chunk record is legal,
so it permitted forty-eight thousand times as much decoded state as the 512 MiB
body ceiling would ever have to deliver.

**Recovery is bounded by its total work and not by its factors.** A reader
MUST stop recovery, and refuse the file, once the directory entries it has
parsed during one open reach the ceiling above, counted across every checkpoint
candidate it tries rather than per candidate. The chain limit and the directory
limit bound two factors of one product, and it is the product that costs: 256
candidates over a directory at its ceiling is a billion entries, measured at
roughly fourteen minutes from a file of about 20 KB, and forging candidates is
free because a footer's hash is xxHash64 over bytes their author controls.
The value above is four directories at the entry ceiling, which is more than a
torn write needs — a damaged checkpoint's directory frame usually fails its own
hash and costs nothing to skip — and it is also 256 times 65 536, so no world
of 65 536 columns or fewer can ever be refused by it.

**A caller may be given a stricter ceiling, and that is not a validity rule.**
An implementation MAY let its caller set a lower limit than the decoded
ceilings above and refuse a file that would exceed it. Such a refusal MUST be
reported distinguishably from a refusal for invalidity, because the file may be
entirely conforming and another reader, or the same reader with a wider limit,
will accept it. A caller-supplied ceiling MUST NOT raise any limit in this
section; it may only tighten one. A reader that accepted a file this section
requires it to refuse would fork the format exactly as surely as one that
refused a file this section requires it to accept, so an implementation that
offers the limit clamps the number it is handed to the values above rather than
trusting it. This paragraph exists because without it the limit reads as a
validity rule, and a second implementation that took it for one would refuse a
conforming file and report the file as the reason.

Positions are part of this: a record's declared span is validated, so every
block-entity and scheduled-update position it carries MUST lie inside that
span. A reader that accepts one outside it hands its caller a coordinate the
caller's own array cannot address.

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
