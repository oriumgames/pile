# Conformance vectors for pile v2

This is the appendix `FREEZE.md` requires. It exists because the specification
concedes that where prose and implementation disagree, the implementation wins.
That concession is only safe if there is something more precise than prose to
check a second implementation against, and these files are it.

Each vector is a real file, checked into `format/testdata/vectors/`, produced by
the reference writer and verified on every test run. A **positive** vector is a
file a conforming reader must accept, together with what it must conclude from
it. A **negative** vector is a file a conforming reader must reject, together
with the rule it breaks.

If you are writing a reader in another language, the shortest useful path is:

1. decode `world_minimal.pile` — 118 bytes, annotated byte for byte below;
2. decode the rest of the positive vectors and check your `ContentHash` against
   the table;
3. re-encode each one and check you get the same bytes back;
4. run the negative vectors through your reader and check every one is refused.

Section numbers below refer to `format/format.md`.

## What is and is not pinned

Every positive vector is written **uncompressed** (flag `Uncompressed`, bit 4).
That is deliberate. §4.8 says the compressed bytes are not part of the format's
identity: Zstandard admits many valid encodings of one body, so a vector holding
compressed bytes would pin the compressor rather than the format. There is
therefore no compressed vector here. A reader still has to handle compressed
bodies; the golden suite in `format/testdata/` carries compressed fixtures for
that, but they are stability fixtures for this implementation, not conformance
vectors.

The one indexed vector is an exception in the other direction. Indexed files are
history-dependent by design (§5), so their bytes carry no cross-implementation
meaning; what is pinned about `indexed_torn.pile` is the content it recovers to.

## File identity

`format.ContentHash` is the identity of a file's *content*: decode it, re-encode
uncompressed, and hash the body (everything between the 16-byte header and the
44-byte footer). It is stable across compressor changes and across
implementations that follow the canonical rules. The `ContentHash` column below
is what your reader must compute after decoding the vector and re-encoding it.

Two things it deliberately does **not** cover, both visible in the table:

- **Derived and advisory content.** `world_stats` holds a §4.2 stats compound
  and `world_minimal` does not, and both hash to `c62c88c4f7de5206`. Stats are a
  summary of the payload; light is a cache. Neither changes what the world is.
- **The dimension.** `world_minimal`, `world_dim_nether` and `world_dim_end`
  hold identical chunks in three different dimensions and all hash to
  `c62c88c4f7de5206`, because the dimension lives in the header flags and
  `ContentHash` covers only the body. If you use `ContentHash` as a map
  identity, key it by dimension as well; the reference CLI does.

The manifests `vectors_manifest.txt` and `vectors_negative_manifest.txt` record
an `xxhash64` of each file's stored bytes alongside its content hash. Those are
for this repository's own regeneration guard, not for conformance: a second
implementation has no reason to reproduce a file's stored bytes when it
compresses differently.

---

## The minimal world, byte for byte

`world_minimal.pile` is 118 bytes: one column at (0, 0) holding one 16-block
section of solid stone, in the overworld, with no metadata, no entities and no
scheduled updates. Nothing in it is optional.

```
off  bytes                            field                            value
  0  50 49 4c 45                      header.magic                     "PILE"
  4  02 00                            header.version                   2
  6  00                               header.kind                      0 = world
  7  00                               header.mode                      0 = solid
  8  18 00 00 00                      header.flags                     DefaultBiome|Uncompressed
 12  0f 46 13 01                      header.blockVersion              18040335
 16  00                               meta.settings (blob, len 0)      empty
 17  00                               meta.userData (blob, len 0)      empty
 18  00                               meta.markers  (blob, len 0)      empty
 19  00                               meta.border   (blob, len 0)      empty
 20  01                               blockPalette.count               1
 21  0f                               entry[0].name.len                15
 22  6d 69 6e ... 6e 65               entry[0].name                    "minecraft:stone"
 37  00                               entry[0].propN                   0
 38  00                               blockPalette.overrideN           0
 39  01                               biomePalette.count               1
 40  0f                               name[0].len                      15
 41  6d 69 6e ... 61 6e               name[0]                          "minecraft:ocean"
 56  01                               blobTable.count                  1
 57  01                               blob[0].paletteN                 1
 58  00                               blob[0].ref[0]                   0
 59  00                               blob[0].width                    0 = uniform
 60  01                               chunkN                           1
 61  00                               record[0].dx                     0
 62  00                               record[0].dz                     0
 63  07                               record[0].minSection             -4
 64  01                               record[0].sectionN               1
 65  01                               record[0].blockPresence          bitset(1): section 0 present
 66  01                               section[0].layerN                1
 67  00                               section[0].blobRef[0]            0
 68  00                               record[0].biomePresence          bitset(1): none stored
 69  00                               record[0].beN                    0
 70  00                               record[0].entN                   0
 71  00                               record[0].tick                   0
 72  00                               record[0].stN                    0
 73  00                               record[0].userData (blob, len 0) empty
 74  d2 7c 1d bf 99 4a 95 9b          footer.hash                      xxHash64, §2.4
 82  00 × 8                           footer.dirOffset                 0
 90  00 × 8                           footer.dirLength                 0
 98  00 × 8                           footer.generation                0
106  00 × 8                           footer.prevFooter                0
114  45 4c 49 50                      footer.magic                     "ELIP"
```

Things worth noticing, because each is a rule and not a coincidence:

- **Flags are `0x18`.** Bit 4 is `Uncompressed`. Bit 3 is `DefaultBiome`, and
  it is set because the file holds a section whose biomes are uniform: §4.7
  makes that mandatory, not an optimisation the writer may decline. Bits 16-31
  hold the default reference, which is 0 here.
- **The biome section is not stored.** `biomePresence` is all clear, and a
  decoder must fill the section with palette entry 0.
- **`minSection` is a signed zigzag varint.** `0x07` decodes to −4, the section
  containing block Y −64.
- **The section blob is uniform.** `paletteN` is 1, so `width` must be 0 and no
  index array follows. Writing 4096 zero bytes instead is a second encoding of
  the same section and is rejected (`neg_blob_uniform_width_nonzero`).
- **The footer's four control words are zero.** A solid file has no directory
  and no checkpoint chain, and §2.2 requires a reader to reject a non-zero one.

`world_empty_chunk.pile` (100 bytes) is the same file with an empty block
palette, an empty blob table and a record over the overworld's full 24-section
span with every presence bit clear. It is what "this column exists and is air"
looks like, which is a different thing from a column that was never stored.

---

## Positive vectors

| file | bytes | `ContentHash` | what it fixes |
|------|-------|---------------|----------------|
| `world_minimal.pile` | 118 | `c62c88c4f7de5206` | the smallest world with a block in it; uniform section blob (§3.3), mandatory default-biome flag (§4.7), overworld dimension bits |
| `world_empty_chunk.pile` | 100 | `30e4341db2f6a9f3` | an empty chunk carries its dimension's full span with every presence bit clear; leading and trailing empty sections are never trimmed (§4.3) |
| `world_waterlogged.pile` | 4 252 | `1469c8cd54f56ed8` | layer 1 holds water over a **uniform-air layer 0**, which is stored. A reader that treats a uniform-air layer 0 as an absent section loses the water above it (§4.3) |
| `world_palette_256.pile` | 9 446 | `05a71f611f82d3aa` | a section-local palette of exactly 256 entries: `width` 1, 4 096 index bytes. 256 is the widest a u8 index can address (§3.3) |
| `world_palette_257.pile` | 13 563 | `746f16fb4d6b0b43` | 257 entries: `width` 2, 8 192 index bytes. The narrowest sufficient width is the only valid one, so this file may not use width 1 and the previous one may not use width 2 |
| `world_layers.pile` | 8 370 | `9e19d79c2ee3c64d` | layer numbering (§4.3): layer 0 stone, layer 1 **all air and stored**, layer 2 water, nothing above. Internal all-air layers are kept because layer numbers are semantic; trailing ones are dropped |
| `world_default_biome.pile` | 8 354 | `91fa77118171bce3` | §4.7 elision with a **tie**: two biomes each hold two uniform sections, so only the stated tie-break (lowest global biome palette reference) decides `defaultBiomeRef` |
| `world_dedup_morton.pile` | 4 269 | `39f6ab8ab0bc3913` | four identical columns share one blob-table entry (§3.4), and records come out in Morton order, not in the order the columns were handed over (§4) |
| `world_collections.pile` | 4 777 | `806c15590275b24d` | block entities, entities, scheduled updates, a column tick, world and chunk user data, and §7 metadata. Pins the total orders of §4.8 |
| `world_light.pile` | 16 522 | `2f1e597784d4fcdd` | flag `StoreLight` (§4.6). Light presence is independent of block presence: sections with no blocks carry sky light, and the flags byte is never zero for a present entry |
| `world_stats.pile` | 226 | `c62c88c4f7de5206` | flag `Stats` and the §4.2 compound. Note the hash: stats are derived, so they do not change content identity |
| `world_preserved.pile` | 4 302 | `4c57686fe436d2d1` | the preserved-state sidecar (§9). Two unresolved block states at two different block versions, so the §3.1 sparse override table has two entries; a scheduled update naming one of them; a two-property state; and an unresolved **biome** name that becomes the file's default biome |
| `world_dim_nether.pile` | 118 | `c62c88c4f7de5206` | `world_minimal`'s content with dimension bits 5-7 = 1 |
| `world_dim_end.pile` | 118 | `c62c88c4f7de5206` | the same with dimension bits 5-7 = 2 |
| `structure_edge_padding.pile` | 12 431 | `f55d6c695c07baab` | a 17×3×18 structure: the box is not a multiple of 16, so its four cells have padding outside it. Padding is air in every layer (§6) |
| `structure_full.pile` | 12 684 | `5e1f1c878ca0ef99` | a negative origin, an internal all-air cell layer, block entities, entities and user data; settings, markers and border empty and the biome palette at zero entries, as §6 requires |
| `indexed_torn.pile` | 400 | `c62c88c4f7de5206` | an indexed file (§5) whose newest footer is destroyed. Opening it must fall back through `prevFooter` to the previous checkpoint; the content that survives is the one column stored before it |

The three dimension vectors are byte-identical apart from the header's flags
word at offsets 8-11 and the checkpoint hash at offset 74, which covers it. A
test asserts exactly that, so you can diff them to locate the field.

`structure_edge_padding.pile` also carries the one §6 rule a reader cannot
check. Padding lies outside the declared box by definition, so a file carrying
data there decodes to the same structure as one that cleared it; nothing in the
file proves which happened. It is verified the only way it can be, by encoding
the same structure twice — once with blocks written into the padding and once
without — and requiring identical bytes.

---

## Negative vectors

Every file below must be **rejected**. Each is a named mutation of a positive
vector with its §2.4 checkpoint hash recomputed, so it fails for the rule it is
named after and not for an integrity check — with one deliberate exception,
`neg_checkpoint_hash.pile`, which is what a stale hash looks like.

A note on how they are used here: this repository requires each one to be
rejected by the production reader *and* by an independent walker written from
the specification, so that a vector cannot pass by accident on an
implementation detail. A handful of rules the walker does not model are marked
**(reader only)** with the reason.

### Header and footer (§2.1, §2.2, §2.3, §2.4)

| file | must be rejected because |
|------|--------------------------|
| `neg_header_magic` | the first four bytes are not `PILE` |
| `neg_header_version` | version 3. There is no forward-compatibility lane: a v2 reader refuses a v3 file outright rather than skipping what it does not recognise, because a field a reader may ignore breaks the one-content-one-encoding guarantee everything else rests on |
| `neg_header_kind` | kind 2 is not defined. Values the §2.1 table does not list are rejected, not ignored |
| `neg_header_mode` | mode 2 is not defined |
| `neg_header_structure_indexed` | kind 1 with mode 1. A structure is always solid, so this pair does not exist |
| `neg_header_block_version_zero` | `blockVersion` is 0, which is the value an override uses to mean "the palette's own version". A field cannot mean both |
| `neg_flag_reserved_bit2` | flag bit 2 is reserved and must be zero. Indexed dictionary presence is signalled by the directory, so the bit carries no meaning; accepting it would spend it |
| `neg_flag_reserved_bit8` | flag bits 8-15 are reserved and must be zero |
| `neg_flag_dimension_reserved` | dimension value 3. Values 3-7 are reserved and rejected rather than ignored, so the encoding stays available |
| `neg_flag_default_biome_ref_without_flag` | flag bits 16-31 are non-zero while bit 3 is clear. The reference is only meaningful with its flag, and requiring zero otherwise keeps one encoding per header |
| `neg_footer_magic` | the last four bytes are not `ELIP` |
| `neg_footer_generation_nonzero` | a solid file's `generation` word is non-zero. The directory, generation and previous-footer words carry no information in a solid file and must be zero |
| `neg_checkpoint_hash` | one payload byte was flipped and the footer hash left stale. The hash covers the header, the stored payload and the footer's own control words |

### Primitives (§1)

| file | must be rejected because |
|------|--------------------------|
| `neg_uvarint_overlong` | a uvarint encoded in two bytes where one suffices. Minimality is required, or one number has many encodings |
| `neg_bitset_padding_bits` | a padding bit above the bitset's declared length is set |
| `neg_string_not_utf8` | a palette name contains a byte sequence that is not valid UTF-8. Palettes are ordered by byte comparison, so an implementation that decodes before comparing must be able to |

### Global palettes (§3.1, §3.2)

| file | must be rejected because |
|------|--------------------------|
| `neg_palette_duplicate_entry` | two block palette entries encode identically. A section could reference either, so the file is a second encoding of one world |
| `neg_palette_property_order` | a state's property keys are not in strictly ascending bytewise order. Without the rule one state has many encodings, and a repeated key is worse: the later value silently wins |
| `neg_override_zero_delta` | a version override past the first has a zero index delta. Override indices strictly ascend |
| `neg_override_index_chain_wraps` | a version override whose index delta is 2⁶⁴ minus the previous index, so the running sum wraps and the chain descends onto index 0. The indices strictly ascend, and a bounds test alone cannot see this: the index it lands on is a legal one. Refusing a zero delta enforces the ascent only for as long as the sum cannot wrap, and a uvarint can carry the modular representative of a negative step |
| `neg_override_zero_version` | an override's version is 0, which is the value that means "no override" |
| `neg_override_same_version` | an override repeats the palette's own version, which says nothing and is a second encoding of an entry with no override |
| `neg_biome_bare_name` | a biome name has no namespace. `plains` is invalid rather than a second spelling of `minecraft:plains` |
| `neg_biome_duplicate_name` | the biome palette repeats a name |

### Section blobs and the blob table (§3.3, §3.4)

| file | must be rejected because |
|------|--------------------------|
| `neg_blob_uniform_width_nonzero` | a one-entry local palette stored with `width` 1 and 4 096 zero indices. `width` is 0 if and only if `paletteN` is 1 |
| `neg_blob_width_not_minimal` | a two-entry palette stored with `width` 2. The narrowest sufficient width is the only valid one |
| `neg_blob_refs_not_ascending` | local palette references are out of order |
| `neg_blob_refs_duplicate` | a local palette reference repeats. References ascend *strictly*, so a section has exactly one encoding |
| `neg_blob_unused_palette_entry` | a local palette entry no index uses. This is not merely dead weight: every rule that turns on uniformity — the all-air section rule, the trailing-air-layer rule, the default-biome elision — reads the palette length to decide, so an unused entry bypasses all of them |
| `neg_blob_index_out_of_range` | an index at or past `paletteN` selects nothing |
| `neg_blob_table_duplicate` | the table stores the same blob bytes twice. The table exists to store identical bytes once |
| `neg_blob_table_unreferenced` | the table carries a blob no record references |
| `neg_blob_id_first_use_order` | a record references blob 1 before blob 0. Ids are assigned in first-use order over the stream of stored units, which is checkable on the wire: the first reference is 0 and each later one either repeats a seen id or is the next unseen one |

### Records and sections (§4, §4.3, §4.6, §8)

| file | must be rejected because |
|------|--------------------------|
| `neg_record_keys_not_ascending` | two records whose Morton keys descend. A non-ascending file is a second encoding of a world that already has one |
| `neg_record_duplicate_position` | two records at one chunk position. Nothing tells a reader which one wins |
| `neg_record_trailing_bytes` | one byte follows the last record |
| `neg_section_present_with_no_layers` | a present section declaring `layerN` 0, which is an absent section written the long way |
| `neg_layer_count_over_max` | `layerN` 256. A layer is selected by an 8-bit index, so the 256th is unaddressable, and a length-versus-index comparison in that same type wraps to zero |
| `neg_section_all_air` | a present section whose only layer is uniform air. Such a section must be absent |
| `neg_section_trailing_air_layer` | a stored layer above the last non-air one. A layer past the last stored one already reads as air |
| `neg_light_flags_zero` | a present light entry whose flags byte is 0, which is a second encoding of an absent entry |
| `neg_light_flags_reserved_bits` | a light flags byte with bits 2-7 set |
| `neg_block_entity_outside_span` | a block entity whose Y lies outside the record's declared span. A reader that accepts one hands its caller a coordinate the caller's own array cannot address |

`neg_section_all_air` and `neg_section_trailing_air_layer` name two distinct
rules but this implementation enforces them with one check, deliberately: an
all-air section has an all-air last layer, so no input separates them, and a
check with no distinguishing input is not enforcement. Both files are here
because both shapes are ones a reader must refuse, however it decides to.

Both are **(reader only)** here: telling air from any other block means
resolving a palette entry against a block registry, which the independent
walker deliberately does not do — it models the wire, not the game.

### Collection order and uniqueness (§4.8)

| file | must be rejected because |
|------|--------------------------|
| `neg_block_entity_duplicate_position` | two block entities at one position. At most one may occupy a position, which is what makes the order total |
| `neg_block_entity_out_of_order` | block entities not in (y, z, x) order |
| `neg_scheduled_update_out_of_order` | scheduled updates not in (y, z, x), then firing tick, then block reference order |

### Metadata (§1, §4.2, §7)

| file | must be rejected because |
|------|--------------------------|
| `neg_nbt_keys_not_ascending` | an NBT compound whose keys are not in strictly ascending bytewise order |
| `neg_nbt_duplicate_keys` | an NBT compound with a repeated key |
| `neg_nbt_named_root` | the root compound carries a name. One canonical envelope: the root is unnamed |
| `neg_settings_wrong_tag` **(reader only)** | `time` stored as an int rather than a long. Which §7 fields exist is a convention; how a field is spelled is a rule, because a decoder into a dynamically typed map cannot recover which tag a value came from. The walker checks NBT structure, and a wrong tag is structurally well-formed |
| `neg_markers_not_sorted` **(reader only)** | the marker list is not sorted by name. The same marker collection would otherwise have as many encodings as it has permutations. Structurally well-formed, so only the §7.2 schema catches it |
| `neg_border_wrong_tag` **(reader only)** | the border's `min` is a list of ints rather than an int array. Same reason |
| `neg_stats_wrong_tag` **(reader only)** | a stats counter stored as an int rather than a long. A missing counter is valid; a mistyped one is not |

### Structures (§6)

| file | must be rejected because |
|------|--------------------------|
| `neg_structure_flag_set` | a structure header setting `Stats`. A structure sets no flag other than `Uncompressed`, so one structure has exactly one valid envelope |
| `neg_structure_biome_palette_nonempty` | a structure declaring a biome palette entry. Structures store no biomes |
| `neg_structure_block_entity_outside_box` | a structure block entity at a coordinate outside the declared box |

### Indexed mode (§5.3, §5.4, §5.5)

All three are **(reader only)**: the independent walker models a solid body,
and an indexed file has none — its content is a set of frames located by a
directory.

| file | must be rejected because |
|------|--------------------------|
| `neg_indexed_prologue_stats_flag` | the directory prologue sets `Stats`. Indexed mode has no stats field, so the flag is a claim the layout cannot keep |
| `neg_indexed_prologue_block_version_zero` | the prologue's `blockVersion` is 0, the value an override uses to mean "the palette's own version" |
| `neg_indexed_empty_palette_segment` | a block palette segment frame holding no entries. A directory naming no segments at all stays legal; a segment that adds nothing is garbage two writers could differ on |

Two things about these differ from every other negative vector, and a second
implementation will meet both.

**The mutation is in the prologue, never in the header.** §5.5 makes the
prologue the authority, so a rule about what an indexed file may contain has to
be enforced on the prologue's copy for it to be enforced at all. Each of these
files keeps a valid physical header, so a reader that answers from the header
accepts all three. That is what makes them vectors for §5.5 as much as for the
rule each is named after.

**Each file holds exactly one checkpoint.** §5.6 says a reader that cannot
adopt the newest checkpoint falls back to an older one, so a file whose newest
checkpoint breaks a rule and whose previous one does not is a file that *opens*,
at the previous generation. It is not a file a reader rejects. The reference
writer takes a checkpoint when it creates a file and another when it closes it,
so these vectors are built from a two-checkpoint file with the creation-time
footer blanked. If your reader opens one of these successfully at an earlier
generation rather than refusing it, check that it is judging the checkpoint you
think it is.

---

## Rules no vector here exercises

Stated plainly, so the appendix is not read as more complete than it is.

- **The palette sort orders (§3.1, §3.2).** Reference counts are never stored,
  so nothing in a file proves its palette was sorted by them. The rule is
  writer-only by construction and is verified by re-encoding and comparing —
  which every positive vector does — never by reading. A vector cannot express
  "this palette is in the wrong order" as something to reject.

  This one is a closed door rather than an omission, and worth stating because
  a second implementation will find the same temptation. A reader *could*
  approximate the block palette's reference counts, by walking every stored
  local palette and counting. §3.1 says it MUST NOT try. Doing so would reject
  files this version wrote — the count is taken over material the file does not
  keep, including sections §4.7 elided and the trailing air layers §4.3 dropped
  — and would make validity depend on a reconstruction the specification never
  defined.
- **Cell padding in structures (§6)**, for the same reason: it lies outside the
  declared box, so a file carrying it decodes identically to one that cleared
  it. It is covered by an encode-twice comparison instead of by a vector.
- **Most of indexed mode (§5).** No *positive* vector pins an indexed file's
  bytes beyond `indexed_torn`, and none should: an indexed file's bytes are
  history-dependent, so a byte-pinned positive vector would assert facts about
  the order this implementation happens to write in rather than about the
  format. That objection does not reach the negative vectors above, which say
  only that a file must be refused and name the rule refusing it, so §5.3, §5.4
  and §5.5 are now exercised. What is still uncovered is the meta frame, the
  dictionary frame, compaction, and every §5 rule that decides what a *valid*
  indexed file looks like rather than which ones are invalid.
- **The §8 ceilings**, apart from the layer count. A vector for a 512 MiB body
  or a 1 048 576-entry palette would be a vector nobody can check into a
  repository. They are covered by the limit tests instead.

  Three of them are worth naming, because each is a validity rule a second
  implementation has to get right and none of them can have a vector here. The
  **NBT container budget** needs a blob past 1 048 576 containers, and a
  container costs about five bytes however it is nested, so the smallest file
  that exercises it is over five megabytes. The **column ceiling** needs
  4 194 305 chunk records at eight bytes each, so about thirty-three megabytes;
  a small file declaring more columns than it holds is refused either way, for
  running out of records, so it would be a vector that passes against a reader
  with no ceiling at all. The **total recovery work** limit needs 16 777 216
  directory entries parsed, which is larger again and only in a mode whose
  positive bytes are not pinned anyway. All three are covered by the limit and
  hostile-input tests, each with a recorded negative control.
- **Zstandard's window ceiling (§2.5).** Every positive vector is uncompressed,
  so no frame here asks for a window at all.
