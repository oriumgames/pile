# Negative controls for the canonicality table

`format/invariants.go` names, for each rule, the tests that enforce it. Two
harness tests check that every normative sentence is claimed and that every
named test exists. Neither is evidence that a named test *reaches* the rule it
claims, and the difference is not academic: a fixture malformed in a second way
is refused for the second reason, the assertion sees an error, and the test
passes with the rule deleted.

This file records the negative control for every entry. The method, for each
`Enforce: Decoded` entry, is:

1. locate the production check the entry depends on, by line number, and
   confirm the line with `sed`/`awk` before editing (a pattern that occurs three
   times gets replaced at the first occurrence otherwise, and the result proves
   nothing);
2. disable it — `if false && cond {` — never delete it, so the surrounding code
   still compiles and still runs;
3. run the tests the entry names with `-count=1`;
4. require at least one to fail;
5. restore the file exactly and confirm with `git diff`.

A control that leaves the named tests green is recorded here as such, and every
one found that way has either been fixed or is explained below.

For each `Enforce: WriterOnly` entry the deliverable is different: a written
statement of what evidence is missing from the file that makes reader
enforcement impossible. Those live in the entry's `Note` field in
`format/invariants.go`; this file records only that they are there and which
ones were added.

Line numbers are as of this commit. Anchors are quoted so they can be found
again if the file moves.

---

## Summary

The table holds 69 entries: 59 `Decoded`, 10 `WriterOnly`. (It held 63 before
the harness pass; one entry was split, see "Enforce labels" below, and four
more were added by the validity tightenings recorded in `SECURITY.md` under
"Format changes: made".)

`STATUS.md` estimated "roughly 13 entries still have vacuous tests". The real
count, at the level of individual production checks rather than whole entries,
is **17 checks across 11 entries** that no named test reached. Six of those
eleven entries had at least one other check that was properly controlled, which
is why the entry-level estimate was low: the entries were not wholly vacuous,
they were partly vacuous, and the partial cover made them look done.

Twelve of the seventeen were fixed, so the check is now controlled. Five could
not be, and each is explained under "Checks with no distinguishing input" or
"Checks nothing can control" below, and stated in the entry's own `Note`.

Two production edits were made. Neither changes which files a reader accepts:

- `format/blob.go`: the `widthU16` arm's `pn == 1` test was removed. Every
  input reaching it is refused by the `pn <= 256` test on the next line, so it
  had no input of its own and could not fail. Both spellings are now pinned by
  `TestRejectsNonCanonicalBlob`, which asserts the exact message each is refused
  with.
- `format/blob.go`: the `widthU8` arm's `pn > 256` test is kept but annotated.
  It is in the same position — §3.3's used-entry rule refuses every input it
  would — but it refuses before a 4096-byte scan and a `pn`-sized bitmap, so it
  earns its place as a bound rather than as enforcement. Its input is pinned by
  a test either way.

No rule that the specification states was found unenforced. The one candidate
is discussed under "Rules a reader could check and must not".

---

## Fixed: tests that did not reach their rule

Each row is a control that was green before the fix and red after.

### 1. `section blobs are canonical`

The worst entry in the table. Eight production checks; the three tests it named
reached one of them.

`TestRejectsNonCanonicalBlob` built two fixtures — a descending reference pair
and a two-entry palette with 16-bit indices — and asserted only that an error
came back. Both fixtures left a palette entry that no index named, so §3.3's
used-entry rule (`blob.go:298`) refused them before either rule under test ran.
Deleting the ascent check and the width check left the test green.

The test now spreads indices over every entry a fixture declares, so no fixture
is refused by the used-entry rule, and asserts on the message. It also covers
the four checks nothing reached at all, and accepts the canonical spelling of
each shape so a blanket rejection cannot masquerade as enforcement.

| check | anchor | control result |
|---|---|---|
| empty local palette | `format/blob.go:197` `if pn == 0 {` | `TestRejectsNonCanonicalBlob` — "empty palette: refused by ... uniform blob with 0 palette entries" |
| references ascend strictly | `format/blob.go:217` `if j > 0 && uint32(v) <= refs[j-1] {` | `TestRejectsNonCanonicalBlob` — "descending references: accepted", "repeated reference: accepted" |
| width 0 iff paletteN 1 (forward) | `format/blob.go:229` `if pn != 1 {` | `TestRejectsNonCanonicalBlob` — "uniform width, two entries: accepted" |
| width 0 iff paletteN 1 (reverse) | `format/blob.go:233` `if pn == 1 {` | `TestRejectsNonCanonicalBlob`, `TestRejectsNonMinimalUniformWidth` |
| u8 upper bound | `format/blob.go:247` `if pn > 256 {` | `TestRejectsNonCanonicalBlob` — refused by the used-entry rule instead, which the message assertion catches |
| narrowest sufficient width | `format/blob.go:259` `if pn <= 256 {` | `TestRejectsNonCanonicalBlob`, `TestRejectsNonMinimalUniformWidth` |
| undefined width code | `format/blob.go:265` `default:` (replaced with `case 200:`) | `TestRejectsNonCanonicalBlob` — "undefined width code: accepted" |
| every entry is used | `format/blob.go:298` `if unseen > 0 {` | `TestRejectsUnusedLocalPaletteEntry` (was already good) |
| index in palette range, u8 | `format/decode.go:840` `if li >= n {` | `TestRejectsOutOfRangePaletteIndex` (was already good) |
| index in palette range, u16 | `format/decode.go:848` `if li >= n {` | `TestRejectsOutOfRangePaletteIndex` (was already good) |

`TestRejectsNonMinimalUniformWidth` existed and covered two of these, but the
entry did not name it. It does now.

### 2. `biome names are fully qualified`

The reader's check was controlled; the writer's was not, and nothing anywhere
in the repository caught its removal.

| check | anchor | control result |
|---|---|---|
| reader refuses a bare name | `format/palette.go:676` | `TestBiomeNamesAreNamespaced` — "a bare biome name was accepted" |
| writer refuses a bare name | `format/palette.go:645` | **was green.** `TestBiomeNamesAreNamespaced` now drives `biomePaletteBuilder.finalize` with a bare name; the control is red: "the writer emitted a bare biome name" |

### 3. `trailing air layers go, internal ones stay`

The structure writer's trailing-air drop was caught only by
`TestGoldenFormatStability`, which reports "the wire format changed" rather than
which rule went, and which the entry does not name.

| check | anchor | control result |
|---|---|---|
| world reader | `format/decode.go:951`, called at `decode.go:646` | `TestRejectsNonCanonicalSections`, `TestDecodersAgreeOnValidity` |
| structure reader | `format/structure.go:645` | `TestDecodersAgreeOnValidity` |
| world writer drops trailing air | `format/encode.go:874` | `TestTrailingAirLayersDropped` |
| structure writer drops trailing air | `format/structure.go:350` | **was green** (golden only). `TestStructureInternalAirLayerSurvives` now compares a padded cell against a bare one; the control is red: "spare trailing air layers reached a structure cell" |
| a preserved state is not air | `format/decode.go:968` | `TestRejectsNonCanonicalSections` |

### 4. `collections are totally ordered`

`TestCollectionTiesUseWrittenBytes` gave both entities the same `UniqueID` *and*
the same body, so after the writer overwrote `UniqueID` the two marshalled to
identical bytes and the NBT tie-break had nothing to order. Deleting the
comparison changed nothing.

| check | anchor | control result |
|---|---|---|
| reader, block entities | `format/decode.go:749` | `TestReaderEnforcesCollectionOrder`, `TestDecodersAgreeOnValidity` |
| reader, scheduled updates | `format/decode.go:795` | `TestReaderEnforcesCollectionOrder` |
| reader, structure block entities | `format/structure.go:697` | `TestDecodersAgreeOnValidity` |
| writer, tick tie-break on palette reference | `format/encode.go:1327` | `TestTiedTicksAndStructureCollections` |
| writer, x/y/z stripped before ordering | `format/encode.go:742` | `TestCollectionTiesUseWrittenBytes` |
| writer, entity tie-break on NBT | `format/encode.go:781` | **was green.** The fixture now gives two entities one ID and different bodies, and hands them over in both orders; the control is red: "two entities sharing an ID kept the caller's order" |

### 5. `a dictionary needs compression`

The entry named `TestRejectsDirectoryStorageMismatch`, which drives a different
rule, and its `Note` claimed the rule was not independently reachable. Both were
wrong. `TestIndexedRejectsDictionaryWhenUncompressed` existed but pointed the
directory's dictionary reference at one arbitrary byte and left the hash zero,
so the frame checksum refused the file before the rule ran.

The fixture now installs a real trained dictionary into an *uncompressed*
world. The frame is therefore stored raw, the reference resolves and hashes,
and the rule is the only thing left to refuse it.

| check | anchor | control result |
|---|---|---|
| uncompressed file may not name a dictionary | `format/indexed.go:740` | **was green.** Now red: "an uncompressed indexed file referencing a dictionary was accepted" |

The old fixture was kept as `TestRejectsMalformedDictionaryReference`: a
reference whose frame does not hash to what it claims is a different check at a
different point, and folding the two together is what made this one
unfalsifiable.

### 6. `palette limits are cumulative`

`TestRejectsCumulativePaletteOverflow` listed two segment references at the same
offset, so §5.3's strict-ascent rule refused the list before the frame-total
bound was reached.

| check | anchor | control result |
|---|---|---|
| segment references ascend strictly | `format/indexed.go:849` | `TestRejectsDuplicateSegmentReference`, `TestRejectsUnorderedSegmentReferences` |
| segment frames fit the file | `format/indexed.go:854` | **was green for this test** (`TestRejectsDuplicateSegmentReference` covered it). The offsets now ascend and the assertion names the bound; the control is red: "segments totalling more than the file were accepted" |
| cumulative entry ceiling | `format/indexed.go:958`, `:1013` | not reachable by any fixture — see "Checks nothing can control" |

### 7. `positions lie inside the declared span`

`TestRejectsOutOfSpanPositions` drove block entities only. Scheduled updates
carry an unbounded Y of their own and are checked in a separate loop.

| check | anchor | control result |
|---|---|---|
| block-entity Y | `format/decode.go:738` | `TestRejectsOutOfSpanPositions` |
| scheduled-update Y | `format/decode.go:787` | **was green.** The test now drives the tick half and asserts the span message; the control is red on all five out-of-span values |

### 8. `zstd frames are bounded`

`TestRejectsOversizedZstdWindow` covered the window ceiling. The decoded-size
ceilings — the other half of the same entry — had no fixture at all: raising
`WithDecoderMaxMemory` to a terabyte left the suite green.

| check | anchor | control result |
|---|---|---|
| window ceiling | `format/zstdpool.go:117` | `TestRejectsOversizedZstdWindow` |
| decoded-size ceiling | `format/zstdpool.go:118` | **was green.** The test now decodes a frame of zeroes that expands past `maxDecodedFrame` from a few hundred stored bytes; the control is red: "a frame decoding to 67108865 bytes passed the 67108864 byte ceiling" |

### 9. `collection keys are unique`

The reader's structure in-box check was covered; the writer's was covered only
by `TestStructureRejectsOutOfBoundsBlockEntity` in the **root** package, which
the invariant table cannot name — `testNames` in `specrules_test.go` resolves
test names against the `format` directory alone.

| check | anchor | control result |
|---|---|---|
| writer, world block entities | `format/encode.go:703` | `TestRejectsDuplicateCollectionEntries` |
| writer, world scheduled updates | `format/encode.go:716` | `TestRejectsDuplicateCollectionEntries` |
| writer, structure block entities | `format/structure.go:171` | `TestStructureWriterRefusesDuplicateBlockEntities` |
| reader, structure block entity in box | `format/structure.go:684` | `TestStructureRejectsBlockEntityOutsideBox` |
| writer, structure block entity in box | `format/structure.go:167` | **was green in this package.** `TestStructureRejectsBlockEntityOutsideBox` now drives the writer too; the control is red on all six out-of-box positions |

---

## Controls that passed as-is

These entries needed no change: disabling the production check turned a named
test red on the first attempt. One row per check, not per entry, because
several entries have two readers or a reader and a writer and a fixture for one
says nothing about the other.

### Primitives (§1)

| entry | check disabled | test that failed |
|---|---|---|
| varints are minimal | `buffer.go:113` `if n != uvarintLen(v)` | `TestRejectsNonMinimalVarints` |
| varints are minimal | `buffer.go:135` signed form | `TestRejectsNonMinimalVarints` |
| strings are bounded UTF-8 | `buffer.go:187` `if !utf8.Valid(p)` | `TestReaderRejectsBadStrings` |
| strings are bounded UTF-8 | `buffer.go:179` `maxStringLen` → `1<<40` | `TestReaderRejectsBadStrings` (asserts the ceiling, not just an error) |
| strings are bounded UTF-8 | `buffer.go:58` `if !utf8.ValidString(s)` | `TestRejectsNonUTF8Strings` |
| blobs are bounded | `buffer.go:196` `maxBlobLen` → `1<<40` | `TestRejectsOversizedBlob` |
| blobs are bounded | `buffer.go:42` writer ceiling | `TestRejectsOversizedBlob` |
| bitset padding is zero | `buffer.go:160` the helper | `TestRejectsBitsetPadding`, `TestRejectsLightBitsetPadding` (four sites at once) |
| bitset padding is zero | `decode.go:471` block presence | `TestRejectsBitsetPadding` |
| bitset padding is zero | `decode.go:505` biome presence | `TestRejectsBitsetPadding` |
| bitset padding is zero | `decode.go:523` light presence | `TestRejectsLightBitsetPadding` |
| bitset padding is zero | `structure.go:615` cell presence | `TestRejectsBitsetPadding` |
| NBT compounds are canonical | `nbtvalidate.go:247` key ascent | `TestRejectsNonCanonicalNBT` |
| NBT compounds are canonical | `nbtvalidate.go:128` unnamed root | `TestRejectsNonCanonicalNBT` |
| array tags are distinct from lists | `nbt.go:133` `tagIntArray` → `tagList` | `TestOpaqueNBTArraysSurvive` |
| metadata field tags are exact | `encode.go:415` writer schemas | `TestRejectsMetaSchemaViolations`, `TestReaderEnforcesMetadataSchemas` |
| metadata field tags are exact | `decode.go:170` reader schemas | `TestReaderEnforcesMetadataSchemas` |

### Header and container (§2)

| entry | check disabled | test that failed |
|---|---|---|
| unknown versions and flags | `decode.go:50` solid version | `TestRejectsReservedFlags` |
| unknown versions and flags | `decode.go:69` solid flags | `TestRejectsReservedFlags` |
| unknown versions and flags | `indexed.go:346` indexed version | `TestIndexedRejectsUnsupportedVersion` |
| unknown versions and flags | `indexed.go:680` directory flags | `TestIndexedRejectsReservedFlags` |
| the dictionary bit stays reserved | `decode.go:69` (bit 2 is in `knownFlags`'s complement) | `TestRejectsReservedFlags` — "reserved flag bit 0x4 accepted" |
| blockVersion names a version | `decode.go:66` physical header | `TestRejectsZeroBlockVersion` |
| blockVersion names a version | `indexed.go:693` directory prologue | `TestRejectsZeroBlockVersion` |
| the default biome reference needs its flag | `decode.go:75` | `TestRejectsReservedFlags` — "reserved flag bit 0x10000 accepted" |
| kind and mode pairs are enumerated | `decode.go:59` kind | `TestRejectsUndefinedKind` |
| kind and mode pairs are enumerated | `decode.go:83` mode | `TestRejectsIndexedStructureKind` |
| kind and mode pairs are enumerated | `indexed.go:677` directory pair | `TestIndexedRejectsStructureKindInDirectory` |
| the dimension field is enumerated | `decode.go:80` reader | `TestRejectsReservedDimension` |
| the dimension field is enumerated | `encode.go:412` writer | `TestRejectsReservedDimension` |
| structures have no dimension | `structure.go:532` | `TestStructureLeavesDimensionBitsZero` |
| solid footers carry no indexed words | `decode.go:97` | `TestSolidFooterMustBeZero` |

### Palettes and blobs (§3)

| entry | check disabled | test that failed |
|---|---|---|
| state properties are ordered and unique | `palette.go:444` duplicate key | `TestRejectsUnorderedStateProperties` |
| state properties are ordered and unique | `palette.go:447` key ascent | `TestRejectsUnorderedStateProperties` |
| state properties are ordered and unique | `palette.go:354` `slices.Sort` → `slices.Reverse` | `TestWriterSortsStateProperties` |
| palette entries are unique | `palette.go:164` writer merge | `TestPaletteMergesIdenticalEntries` |
| palette entries are unique | `palette.go:520` reader, block | `TestRejectsDuplicatePaletteEntries` |
| biome palette entries are unique | `palette.go:681` reader, biome | `TestRejectsDuplicatePaletteEntries` |
| version overrides mean a different version | `palette.go:502` zero override | `TestRejectsRedundantVersionOverride` |
| version overrides mean a different version | `palette.go:507` redundant override | `TestRejectsRedundantVersionOverride` |
| version overrides mean a different version | `palette.go:98` writer normalisation | `TestVersionZeroRoundTrips` |
| identical blobs share one table entry | `blob.go:164` writer dedup | `TestDedup` |
| identical blobs share one table entry | `blob.go:340` reader duplicate | `TestRejectsBlobTableWaste`, `TestDecodersAgreeOnValidity` |
| identical blobs share one table entry | `decode.go:339` unreferenced, world | `TestRejectsUnreferencedBlob`, `TestDecodersAgreeOnValidity` |
| identical blobs share one table entry | `structure.go:664` unreferenced, structure | `TestDecodersAgreeOnValidity` |
| blob ids follow the field order | `decode.go:375` first-use order | `TestReaderEnforcesBlobFirstUseOrder`, `TestDecodersAgreeOnValidity` |

### Chunk records (§4)

| entry | check disabled | test that failed |
|---|---|---|
| the section span is addressable | `decode.go:461` | `TestRejectsUnaddressableSectionSpan` |
| chunk positions are unique and ascending | `decode.go:303` reader | `TestReaderRejectsUnorderedRecords` |
| chunk positions are unique and ascending | `encode.go:442` writer | `TestRejectsDuplicateChunks` |
| layer counts are addressable | `decode.go:478` records | `TestRejectsLayerCountInRecords` |
| layer counts are addressable | `structure.go:622` cells | `TestRejectsLayerCountInCells` |
| light entries describe something | `decode.go:534` reserved bits | `TestRejectsLightEntryFlags` |
| light entries describe something | `decode.go:537` zero flags | `TestRejectsLightEntryFlags` |
| UniqueID is stored verbatim | `decode.go:767` forced resynthesis | `TestEntityIDZeroRoundTrips` |
| uniform-default biome sections are omitted | `decode.go:698` | `TestRejectsStoredDefaultBiomeSection` |
| the biome fallback is version stable | `palette.go:718` `+ 1` on the resolved id | `TestAbsentBiomeFallbackIsVersionStable` |
| the biome fallback is version stable | `palette.go:717` resolve `minecraft:ocean` instead | `TestAbsentBiomeFallbackIsVersionStable` |
| elided biome sections keep their names | `decode.go:682` | `TestUnknownDefaultBiomePreserved` |
| StoreLight matches its content | `decode.go:328` | `TestRejectsStoreLightWithoutLight` |

### Indexed mode (§5)

| entry | check disabled | test that failed |
|---|---|---|
| absent frame references are all zero | `indexed.go:706` | `TestRejectsMalformedFrameReference` |
| the directory prologue is authoritative | `indexed.go:677` (MUST half) | `TestIndexedRejectsStructureKindInDirectory` |
| the directory prologue is authoritative | `indexed.go:374` `headerDamaged` (SHOULD half) | `TestIndexedRecoversDamagedKindAndMode`, `TestIndexedSurvivesHeaderDamage` |
| the directory prologue is authoritative | `indexed.go:641` prologue image in recovery | `TestIndexedRecoversDamagedKindAndMode` |
| recovered writers hash the rebuilt header | `indexed.go:381` `w.hdrBytes = img` | `TestRecoveredHeaderCheckpointVerifies` |
| the directory and dictionary skip the dictionary | `indexed.go:1847` directory written `plain` | `TestIndexedDictionary` |
| the directory and dictionary skip the dictionary | `indexed.go:1175` the `plain` branch | `TestIndexedDictionary` |
| indexed files claim only what they can hold | `indexed.go:683` | `TestIndexedRejectsSolidOnlyFlags` |
| frames end where their content ends | `indexed.go:780` meta frame | `TestRejectsTrailingBytesInFrames` |
| frames end where their content ends | `indexed.go:961` block segment | `TestRejectsTrailingBytesInFrames` |
| frames end where their content ends | `indexed.go:1016` biome segment | `TestRejectsTrailingBytesInFrames` |
| frames end where their content ends | `indexed.go:930` directory | `TestRejectsTrailingBytesInFrames` |
| frames end where their content ends | `indexed.go:1558` record | `TestRejectsTrailingBytesInFrames` |
| directory offsets accumulate in range | `indexed.go:909` | `TestRejectsWrappingDirectoryOffset` (asserts which check refused it) |
| empty palette segments are not written | `indexed.go:958` block | `TestRejectsEmptyPaletteSegment` |
| empty palette segments are not written | `indexed.go:1013` biome | `TestRejectsEmptyPaletteSegment` |

### Structures (§6)

| entry | check disabled | test that failed |
|---|---|---|
| a structure has one envelope | `structure.go:532` flags | `TestRejectsStructureEnvelopeViolations` |
| a structure has one envelope | `structure.go:546` metadata blobs | `TestRejectsStructureEnvelopeViolations` |
| a structure has one envelope | `structure.go:559` biome palette | `TestRejectsStructureEnvelopeViolations` |
| a structure has one envelope | `structure.go:573` size bound | `TestRejectsStructureEnvelopeViolations` |
| a structure has one envelope | `structure.go:591` origin range | `TestRejectsStructureEnvelopeViolations` |
| structure cells are computed in 64 bits | `structure.go:69` cell ceiling | `TestRejectsStructureCellOverflow` (panics in `make`, which is the point) |
| structure cells are computed in 64 bits | `structure.go:68` product computed in 32 bits | `TestRejectsStructureCellOverflow` |

### Limits and the rest (§8, review rounds)

| entry | check disabled | test that failed |
|---|---|---|
| decoders enforce the limits | `buffer.go:172` the shared `count` bound | `TestRejectsOverLimitCounts` (seven ceilings at once) |
| decoders enforce the limits | `nbtvalidate.go:143` nesting depth | `TestNBTValidatorRejectsHostileLengths` |
| the hash seed is zero | `format.go:182` `xxhash.New()` → `NewWithSeed(1)` | `TestHashSeedIsUsedInProduction` |
| the biome palette order is defined (now WriterOnly) | `palette.go:626` count comparison | `TestBiomePaletteOrder` |
| the biome palette order is defined (now WriterOnly) | `palette.go:632` name tie-break reversed | `TestBiomePaletteOrder` |
| stats fields are optional but typed | `encode.go:521` tag comparison | `TestRejectsStatsSchemaViolations` |
| stats fields are optional but typed | `encode.go:518` a missing key made an error | `TestStatsMissingKeyAccepted` — "a stats compound missing keys was rejected" |
| decoders bound the result | `decode.go:439` storage budget | `TestBoundsDecodedStorages` |
| decoders bound the result | `nbtvalidate.go:31` NBT element budget | `TestBoundsDecodedNBTContainers` |

The four validity tightenings made before the freeze have their own controls,
one per production check rather than one per entry, for the same reason the rest
of this document counts checks. `SECURITY.md`'s "Negative controls" table holds
the results; they are listed here so the two documents do not diverge.

| entry | check disabled | test that failed |
|---|---|---|
| the NBT container budget charges nested compounds | `nbtvalidate.go` the compound field's charge | `TestHostileNBTContainerBudget` |
| the NBT container budget charges nested compounds | `nbt.go` `nbtCompound`'s writer-side charge | `TestNBTWriterHoldsTheContainerBudget` |
| version override indices strictly ascend | `palette.go` the `idx < prev` wrap test | `TestHostileOverrideDeltaWraps` |
| version override indices strictly ascend | `vectorwalk_test.go` the independent walker's own wrap test | `TestConformanceVectorsNegative/override_index_chain_wraps` |
| the column ceiling bounds both modes | `decode.go` `r.count(maxChunks, "chunk")` | `TestHostileDecodedColumnCeiling` |
| recovery is bounded by total work | `indexed.go` `finishDirectory`'s budget charge | `TestRecoveryWorkIsBounded` |

The column ceiling's writer half has no control of its own, deliberately: the
encoder's check is against the same constant the decoder's is, so there is no
edit that disables one without the other. Saying so is the point — a control
that could only be faked would be worse than none.

---

## Checks with no distinguishing input

Rules that cannot fail are worse than no rule, because they read as protection.
Each of these was found by a control that stayed green and then shown to be
covered by a neighbouring rule for every possible input.

**`blob.go`, `widthU16` with `pn == 1` — removed.** The condition sat directly
above `if pn <= 256`, which is true for every `pn` that could reach it. No
input separated them. It is gone; `TestRejectsNonCanonicalBlob` now pins the
input and the message it is refused with, so the acceptance boundary is fixed
even though the code is one condition shorter.

**`blob.go:247`, `widthU8` with `pn > 256` — kept, annotated.** A byte index
cannot name entry 256, so §3.3's used-entry rule refuses every such blob
regardless. It is not enforcement and the comment now says so. It stays because
it refuses before `r.take(4096)` and before the `pn`-sized bitmap that rule
allocates, which is a security property rather than a validity one.
`TestRejectsNonCanonicalBlob` covers the input and accepts either message.

**`palette.go:720`, the `return 1` last resort in `fallbackBiomeID`.** On every
runtime that knows `minecraft:plains`, plains *is* id 1, so the literal and the
resolved value are the same number and nothing separates them. Already stated
in the entry's `Note`; the control confirms it (changing the literal to 2 leaves
the test green, changing the resolved value red).

**`indexed.go:310`, the `plain` argument when the dictionary frame is written.**
`installDict` appends the frame before `setDict` installs the codec, and
`Compact` writes into a freshly created world whose codec is nil, so no
dictionary is ever active at that point and the argument decides nothing. The
comment there already says "no dictionary yet"; the control confirms it.

**The NBT tie-breaks for block entities.** §4.8 orders block entities by
(y, z, x) and then by their written NBT, and structure block entities likewise.
Both writers refuse two entries at one position (`encode.go:703`,
`structure.go:171`) and both readers refuse such a file, so a tie is exactly the
input neither will produce: `encode.go:760` and `structure.go:443` can never
decide anything. They are kept because the specification states them, and this
is now recorded in the `collections are totally ordered` note. Of the five
orders §4.8 lists, only entities (whose IDs need not be unique) and scheduled
updates have a tie-break with legal input, and both now have a control.

---

## Checks nothing can control

Not vacuous tests — checks for which no test of this kind can exist. Each is
stated in the entry's `Note` so it is visible where the claim is made.

**The cumulative palette entry ceiling** (`indexed.go:958`, `:1013`). Reaching
it needs a segment carrying more than a million palette entries, which no
fixture builds; the fuzz targets are what cover it. This was already stated in
the entry. The other half of the same entry — the frame-total bound — is now
controlled, which it was not.

**The checkpoint chain ceiling** (`indexed.go:512`, `:514`). The walk terminates
without it: a seen-set rules out cycles, so the ceiling caps work rather than
deciding validity, and no input separates a file it refuses from one it accepts.
`TestBoundsCheckpointChain` shows only that an ordinary chain is still walked,
which is the half a wrong ceiling would break. Testing the other half would need
a timing assertion, which would be flaky and would not prove anything a reading
of the loop does not.

**The writer half of blob first-use order** (§3.4, `078b2b7d`). An id is
assigned and written by the same expression — `table.add(blob)` inside the
record's blob sink — so there is no edit that makes a writer assign a different
id without also emitting that id. Any writer that deviated would produce a file
its own reader refuses, which the round-trip tests cover.

**`decoders never panic`.** A plain `go test` runs the fuzz seed corpora, which
are the inputs already known to be safe, so disabling a bound leaves them green.
What shows the property is real is that the same edits panic the targeted tests
— deleting the structure cell ceiling makes `TestRejectsStructureCellOverflow`
panic inside `make`. The evidence `FREEZE.md` asks for here is a long fuzzing
session, and that precondition is tracked separately under Security.

**The NBT array and list pre-bounds** (`nbtvalidate.go:175`, `:209`). Removing
either leaves the input rejected: the array bound is followed by `skip`, which
refuses the same lengths (and refuses a wrapped product as negative), and the
list bound is followed by the element budget for lists of compounds and by a
truncated read for everything else. They are bounds that make the rejection
cheap and the message accurate, not validity rules with inputs of their own.
The entry already says the NBT fixtures reach none of §8's ceilings.

---

## WriterOnly entries: what evidence is missing

All ten are stated in `format/invariants.go`. Six were added or rewritten in
this pass; four already carried a reason and were left alone.

| entry | missing evidence | status |
|---|---|---|
| the palette order is defined on encoded bytes | the reference counts. Not a field anywhere; every permutation of a palette is consistent with some assignment of them. §3.1 also forbids readers from trying — see below | rewritten |
| reference counts predate deduplication | the counts again. Both spellings produce a legal permutation of the other and a file records neither the counts nor which rule produced its order | rewritten |
| biome counts predate elision | the elided sections themselves. They are what the count was taken over and the rule's whole point is that they are absent from the file. The strongest case in the table: the evidence is not unrecorded, it is the thing the format leaves out | rewritten |
| the biome palette order is defined | as above for biomes | new entry, see "Enforce labels" |
| dropped layers do not count | the dropped layers. §4.3 requires trailing all-air layers to be absent, so a reader cannot tell one that dropped them before counting from one that dropped them after | rewritten |
| writers refuse what their readers reject | everything. The rule governs files that were never written, and a file that exists says nothing about whether its writer would have refused it | rewritten |
| enforcement is stated for every rule | there is no file. The rule constrains this table and the specification, not any sequence of bytes | rewritten |
| the section span is never trimmed | the span the dimension intended, as against the one the record declares | already stated |
| the default biome flag is not optional | which sections were uniform before the writer chose. A file that declined the flag decodes to the same world | already stated |
| cell padding is air | nothing is *missing*: a reader could check it. The entry says so. Padding is outside the declared box by definition, so a file carrying it decodes to exactly the structure one that cleared it decodes to, and checking would tighten validity for no gain in what a reader learns | already stated |

### Rules a reader could check and must not

Two entries deserve to be read carefully rather than skimmed, because in both
the honest answer is "a reader could, and must not".

**`cell padding is air`.** Already stated in the entry and already reconciled
with `FREEZE.md`. A reader can see out-of-box cell positions and compare them
against air. It must not, because a file carrying padding and one that cleared
it decode to the same structure, so a check would reject files that are valid
by every property the format is about.

**`the palette order is defined on encoded bytes`.** The block palette's
reference counts are *probably* reconstructible from a solid file: the count is
appearances in local palettes, every stored local palette is in the file, and
the rules that decide what is stored (dropped air layers, absent all-air
sections) drop before counting, so a reader walking every section blob's
reference list would see the same occurrences the writer counted. I did not
prove this exhaustively and I did not implement it. It is recorded because the
brief asks for it to be recorded loudly:

> **A reader could plausibly check the solid block palette's order. §3.1 says
> readers MUST NOT try. Do not "fix" this.** Adding the check would make
> validity depend on a reconstruction the specification never defines, and
> after the freeze it would reject files this version wrote. The specification
> anticipated this and closed it with a MUST NOT rather than leaving it to
> judgement.

The biome palette is different and genuinely uncheckable: §4.7 counts a section
before deciding to elide it, so the sections that decided the order are exactly
the ones the file omits.

---

## Enforce labels

One label was wrong.

**`the biome palette order is defined`** was `Decoded` and claimed two rules:
`33a5a48c`, which says "writers MUST sort the biome palette by (writer-only,
for the same reason §3.1 gives)", and `230b214c`, which says decoders must
reject a repeated name. One is a writer's rule and one is a reader's; a single
`Enforce` field cannot state both, and the reader's half was carrying the label
for the pair. The entry is now split:

- `the biome palette order is defined` — `WriterOnly`, rule `33a5a48c`, test
  `TestBiomePaletteOrder`;
- `biome palette entries are unique` — `Decoded`, rule `230b214c`, test
  `TestRejectsDuplicatePaletteEntries`.

Both halves have controls (recorded above). No other label was found wrong: the
`Decoded` entries all have a reader that rejects, and the `WriterOnly` entries
all have a reason.

---

## Observations that are not part of the deliverable

Two things turned up that are neither a vacuous entry nor a missing rule, but
that the next person should know.

**`decode.go:315` — trailing bytes after a solid body.** The check exists, no
test reaches it, and no normative sentence claims it. §5.1's frame rule
(`b108c77f`) is about indexed frames; the solid body has no equivalent
sentence. So this is a validity rule the implementation enforces and the
specification does not state — the mirror image of a freeze blocker, and
harmless in that direction, since a stricter reader than the spec describes
rejects only files this writer never produced. It is left alone: adding the
sentence would mean re-pinning `spec_rules.txt`, and removing the check would
loosen a reader. Worth a line in the conformance vectors.

**`palette.go:728`, `qualifyBiome`'s early return.** It is dead for every
dragonfly registry, because vanilla biome names have no colon, so nothing
separates it from returning the prefixed name unconditionally. It is a
normalisation helper on the write path, not a validity check, and reaching it
needs a custom biome whose own name is namespaced. Left alone and noted.

---

# The rest of the suite: mover, preservation, provider, golden

Everything above covers the tests the invariant table names. This part covers
the rest, which `FREEZE.md`'s third Correctness box asks for and which had
never been controlled. The method is the same — locate the production code the
test exercises, confirm the line with `sed`, disable it with `if false && …`,
run with `-count=1`, require a named test to fail, restore and confirm — and
the format of the tables is the same: one row per production check, because an
entry with three branches and one fixture is three-quarters untested and
counting entries hides that.

**Line numbers below are as of the parent commit of this one**, since a control
that names a line in a file this commit also edits is worthless. Anchors are
quoted; grep for the anchor, not for the number.

## Summary

**82 controls were run against the existing suite. 44 came back green.**

- 41 were vacuous coverage and are fixed: a test now reaches the check, and the
  same control is red. Every fix is listed with the control that was green
  before it and the message it produces now.
- 1 is a guard with no distinguishing input, kept and annotated in the code
  (`SaveAsync`, below).
- 2 are golden **fixture** gaps rather than vacuous tests: the golden worlds do
  not contain the shape, and the rule is held by a named test elsewhere.

Three further controls were run against the two tests written for the
dictionary eviction work; all three are red. `SECURITY.md` carries that
measurement.

The ratio is worse than the invariant-table pass (17 checks across 69 entries)
and the reason is structural: the invariant table at least *names* a test per
rule, so the failure mode there is a fixture that misses. Out here there was no
table at all, so whole mechanisms — the mover's clip counters, every read-only
guard but two, three of the four store-time `SkipMask` bits — had never been
pointed at by anything.

---

## 1. The mover

26 controls, 9 green. `move.go` line numbers.

| # | check disabled | anchor | test | result |
|---|---|---|---|---|
| M1 | fast path re-keys the column | `165` `X: c.X + int32(off.X()>>4), …` | `TestMoveFastPath` | RED — the translated chunk does not exist (`leveldb: not found` at the load, before the block assertion) |
| M2 | spawn translation | `427` `s.Spawn = s.Spawn.Add(off)` | `TestMoveFastPath`, `TestMoveUnaligned` | RED — "spawn not translated" |
| M3 | marker translation | `438` `ms[i].Pos[0] += …` | `TestMoveFastPath` | RED — "marker not translated" |
| M4 | pre-move backup | `96` `if opt.Backup {` | `TestMoveFastPath` | RED — "pre-move backup missing" |
| M5 | fast path translates block entities | `400` the `col.BlockEntities` loop | — | **was green** |
| M5a | fast path translates scheduled updates | `417` `col.ScheduledBlocks[i].Pos = …Add(off)` | — | **was green** |
| M5b | fast path translates entities | `408` `if pos, ok := entityPos(e.Data); ok {` | — | **was green** |
| M6 | slow path applies the X offset | `248` `wx := int(c.X)*16 + int(lx) + off.X()` | `TestMoveUnaligned` | RED (same indirect shape as M1) |
| M7 | clip refusal | `79` `if report.ClippedTotal() > 0 && !opt.Clip {` | `TestMoveClipRefusalAndForce` | RED — "err = <nil>, want ErrWouldClip" |
| M8 | dry run writes nothing | `82` `if opt.DryRun {` | `TestMoveClipRefusalAndForce` | RED — "dry run modified the file" |
| M9 | clipped-block counter | `258` `if ch.Block(…) != air {` | `TestMoveClipRefusalAndForce` | RED — "clip report empty" |
| M10 | layer traversal ceiling | `191` `layerN := max(maxSourceLayers(ch), sidecarLayers(…))` | `TestMovePreservesAllLayers` | RED — "layer 1 lost in move" |
| M11 | the ceiling is `MaxLayers`, not 4 | `117` `min(n, format.MaxLayers)` → `min(n, 4)` | `TestMovePreservesDeepLayers` | RED — "layer 7 lost in the move" |
| M12 | an untranslatable entity is kept | `336` the append in the `!ok` arm | `TestMoveKeepsUnreadableEntities` | RED |
| M13a | fast path moves the tick sidecar key | `160` `ut.Pos[0] + int32(off.X()), …` | `TestMoveFastPathKeepsSidecars` | RED — "the sidecar key did not move with the update" |
| M13b | fast path carries the biome sidecar | `168` `UnknownBiomes: c.UnknownBiomes, …` | `TestMoveFastPathKeepsSidecars` | RED |
| M14 | slow path re-anchors preserved biomes | `270` `if len(c.UnknownBiomes) > 0 {` | `TestMovePreservesUnknownBiomes` | RED |
| M15 | the sidecar is consulted before the air test | `298` `if !ok && rid == air {` → `if rid == air {` | `TestMoveKeepsUnknownBlocksWithAirPlaceholder` | RED |
| M16 | state references are rebased | `307` `State: stateBase(tcol) + state` | `TestMoveKeepsUnknownBlocksWithAirPlaceholder` | RED |
| M17 | whole-storage preserved states | `291` `state, ok = uniform[srcKey{…}]` | — | **was green** |
| M18 | entities clip on Y | `340` `if wy < float64(r0) \|\| wy > float64(r1)+1 {` | — | **was green** |
| M19 | block entities clip on Y | `320` `if pos.Y() < r0 \|\| pos.Y() > r1 {` | — | **was green** |
| M20 | scheduled updates clip on Y | `360` `if pos.Y() < r0 \|\| pos.Y() > r1 {` | — | **was green** |
| M21 | user data follows the chunk | `381` `t.UserData = cloneBytes(c.UserData)` | — | **was green** |
| M22 | the column tick follows the chunk | `386` `t.Col.Tick = c.Col.Tick` | — | **was green** |
| M23 | border translation | `93` `wf.Border = moveBorder(…)` | `TestBorderRoundTripAndMove` | RED |

### What the nine were, and what fixes them

**M5 / M5a / M5b — `moveColumnExtras`, all three halves.** Nothing in the
repository noticed when any of them stopped translating. Two of the three are
load-bearing and one is invisible in the file, and telling them apart was the
work:

- an **entity**'s position is written verbatim into its NBT, so dropping the
  translation moves the entity nowhere and the file says so;
- a **scheduled update**'s absolute position is the key `extractColumnRaw`
  matches its preserved-state sidecar on (`encode.go`, `tickState`), and the
  fast path translates that sidecar at `move.go:157`. Translate one and not the
  other and the preserved state is silently dropped by the next write;
- a **block entity**'s is not visible in the file at all. `projectCollections`
  strips `x`/`y`/`z` and the record stores the position's low nibbles plus its
  Y, none of which a chunk-aligned offset with no vertical component changes,
  and the decoder reinjects `x`/`y`/`z` from the chunk key. So no file-level
  control is possible; the assertion is on the column `translateColumns`
  returns.

`TestMoveFastPathTranslatesEveryPosition` pins all three on the returned
column, and `buildSidecarColumn` now carries a block entity and an entity as
well as the sidecars. All three controls are red:
"scheduled update not translated", "entity not translated: [4.5 -60 5.5]",
"block entity not translated".

**M17 — whole-storage preserved states in the mover.** A preserved state can
cover an entire storage (`Index == WholeStorage`), which is how a uniformly
unresolved section decodes, and the slow path resolves that form from a second
table. No fixture had one. `TestMoveKeepsUniformUnknownBlocks` builds a column
with one and asserts all 4,096 positions of the section carry it at the
destination: "uniform preserved state covered 0 positions, want 4096".

**M18 / M19 / M20 — the three clip paths.** `TestMoveClipRefusalAndForce`
clipped blocks only, so `ClippedEntities`, `ClippedBlockEntities` and
`ClippedTicks` were never non-zero. `TestMoveClipsCollections` puts one of each
at Y 2 and moves down 70, with blocks from Y 6 up surviving so the column still
exists, and asserts each counter. It takes the counts from a `DryRun` so the
control fails on the count rather than on a writer error further down, then
performs the move and checks the content is gone. Controls: "ClippedEntities =
0, want 1", and the same for the other two.

**M21 / M22 — chunk user data and the column tick.** Neither is addressed by a
position, so the slow path, which builds its destination columns from nothing,
has to place them by hand, and nothing checked that it did.
`TestMoveCarriesChunkMetadata`: "chunk user data = "", want "meta"", "column
tick = 0, want 4321".

---

## 2. Preservation

21 controls, 14 green — the worst ratio of the four areas, and the one where
the vacuity was closest to the claim. Two of the tests whose names describe a
check exactly were passing with that check disabled.

| # | check disabled | anchor | test | result |
|---|---|---|---|---|
| P1 | a store inherits the previous column's sidecar | `pile.go:371` | `TestUnknownStatePreservation`, `TestUnknownSurvivesRangeChange` | RED |
| P3 | an append store recovers the sidecar from the record | `append.go:45` `cur = sidecarOf(old)` | — | **was green** |
| P4 | `SaveAs`'s snapshot carries preserved states | `pile.go:724` | `TestUnknownSurvivesSaveAs` | RED |
| P6 | extraction records preserved states | `structure.go:281` `if ok {` | four tests | RED |
| P7 | the paste places preserved states | `structure.go:501` `if ok {` | `TestUnknownSurvivesPasteAndImport` | RED |
| P9 | the paste starts from the column's existing sidecar | `structure.go:400` `unknown: append(…, cur.unknown…)` | — | **was green** |
| P10 | `columnSidecar` reads through to the record | `pile.go:424` `side := sidecarOf(fc)` | — | **was green** |
| P11 | rotation carries preserved states | `rotate.go:88` `if ok {` | `TestRotationPreservesMetadataAndStates` | RED |
| P11b | rotation carries user data | `rotate.go:36` | `TestRotationPreservesMetadataAndStates` | RED |
| P12 | a store inherits the biome sidecar | `pile.go:372` | — | **was green** |
| P12b | `SaveAs` carries the biome sidecar | `pile.go:725` | — | **was green** |
| P13a | extraction resolves whole-storage entries | `structure.go:266` | — | **was green** |
| P13b | the paste resolves whole-storage entries | `structure.go:499` | — | **was green** |
| P13c | rotation resolves whole-storage entries | `rotate.go:86` | — | **was green** |
| P14 | a template instance's first store inherits the base sidecar | `pile.go:382` | — | **was green** |
| P15 | the paste drops entries it overwrote | `structure.go:524` | — | **was green** |
| P16 | extraction reaches sidecar-only layers | `structure.go:253` | — | **was green** |
| P17 | `layerCount` is bumped by the sidecar | `structure.go:165` | `TestPasteKeepsUnknownStatesAboveAllocatedLayers` | RED |
| P18 | the paste carries the column's biome sidecar | `structure.go:403` | — | **was green** |
| P19 | extraction rebases per-column state tables | `structure.go:238` | — | **was green** |
| P20 | the paste's state table keeps the column's prefix | `structure.go:402` | — | **was green** |

### The assertion that was weaker than the claim

Ten of these tests end in `format.UnresolvedStates`, directly or through
`hasUnknown`. **That function reads the file's block-state palette.** It proves
a name is somewhere in the file; it says nothing about whether any entry still
ties that name to a position. A sidecar that lost its entries but kept its
state table leaves the palette untouched, and in indexed mode the palette is
append-only, so the name survives even a record that no longer references it.

That is what hid P3 and P9. Both drop the sidecar *entries* while the *state
table* still reaches the writer, so the palette still names `minecraft:d1rt`
and `hasUnknown` still said yes.

`preservedStateAt` / `preservedStateAtIndexed` decode the file and return the
name of the preserved state anchored at a given world position, or `""`. They
are now the assertion in `TestUnknownStatePreservation`,
`TestUnknownStatePreservationAppend`, `TestUnknownSurvivesSaveAs`,
`TestUnknownSurvivesRangeChange`, `TestPasteMergesExistingSidecar` and the two
new append tests, alongside the palette check rather than instead of it.

### What the fourteen were

**P3 — `TestAppendStoreRecoversSidecarWithoutALoad` (new).** `storeAppend`
reads the previous record when the position is not in the bounded metadata
cache. Every append test loaded the column first, which fills that cache, so
the read-through never ran. The new test loads in one session and stores in a
second, which is what a server restart looks like and what a cache eviction
looks like. Control: `preserved state at (1,4,1) = "", want minecraft:d1rt`.

**P10 — `TestAppendSidecarReadsThroughOnAMetaCacheMiss` (new).** The same shape
one level up. `TestAppendModeStructureExtraction` is named for this path, but
`ExtractStructure` calls `LoadColumn` before `columnSidecar`, and `LoadColumn`
publishes to the metadata cache, so the extraction always hit the cache. The
new test asks for the sidecar with nothing loaded. Control: "read-through
returned no preserved states".

The **cache** half of `columnSidecar` has no correctness input of its own and
is not counted above: in append mode every store writes the record through
immediately, so the cached sidecar and the one on disk are always the same
value. It saves a frame decode and decides nothing.

**P9 / P18 / P20 — `TestPasteMergesExistingSidecar` was vacuous.** It pasted a
structure with **no preserved states of its own**, so `sideFor` — the whole
merge — was never called, the paste handed the provider no sidecar, and the
destination's states survived by the ordinary inheritance path P1 covers. The
fixture now extracts its structure from a second corrupted world named
`minecraft:d2rt`, so the two states can be told apart, and plants a preserved
biome in the destination as well. Controls: "the paste dropped an entry it did
not overwrite" (P9), "paste dropped the column's preserved biome, which it
never touches" (P18), and P20 red on the same assertion because dropping the
`cur.states` prefix shifts every inherited reference.

**P15 — `TestPasteOverwritesTheStateItReplaces` (new).** A paste landing on a
position that already carries a preserved state has to drop the inherited
entry. Leaving it there gives the column two entries for one position, and
`injectUnknown` gives the placeholder slot to the first one written, so the
state the paste replaced wins. Control: `preserved state at (1,4,1) =
"minecraft:d1rt", want minecraft:d2rt`.

**P12 / P12b — the biome sidecar.** Every preservation test used block states.
`TestBiomeSidecarSurvivesStoreAndSaveAs` plants an unresolvable biome (helper
`plantUnknownBiome`, factored out of `TestMovePreservesUnknownBiomes`) and
drives both the store path and the `SaveAs` snapshot, which carry it in
separate fields. Controls: "a load/store cycle renamed the unresolved biome to
the runtime fallback", "SaveAs renamed …".

**P13a / P13b / P13c — whole-storage entries in the structure paths.** The same
gap as M17, in extraction, paste and rotation. `TestPasteCarriesUniformPreservedStates` and `TestRotationCarriesUniformPreservedStates` build a 16-cube of
placeholder blocks carrying one `WholeStorage` entry, and
`TestExtractCarriesUniformAndRebasesStates` gets one the way a real file does —
a section uniformly filled with a block whose name the registry cannot resolve.

**P16 — `TestExtractReachesPreservedLayersTheChunkNeverAllocated` (new).** On a
registry whose placeholder resolves to air, a preserved state on layer 2 has no
storage, so `chunkLayers` alone never visits it. Control: "extraction found 0
preserved states, want 1". This is the extraction half of the defect
`TestMovePreservesDeepLayers` and `TestPasteKeepsUnknownStatesAboveAllocatedLayers` already cover in the other two directions.

**P19 — `TestExtractRebasesPerColumnStateTables` (new).** This one needed
thought. Every column of a *file* shares the file's single state table, so an
extraction straight off disk passes with the rebase deleted: the two copies of
the table are identical and index 1 means the same name in both. The tables
diverge only in memory, because a paste appends its structure's states to the
column it lands in and to no other. The fixture pastes into the second of two
chunks and extracts across both from the same provider, without a save in
between. Control: "the second column's states resolved to
map[minecraft:d1rt:3]: its references were not rebased".

**P14 — `TestTemplateInstanceInheritsPreservedStates` (new).** An instance's
first store of a template column has no previous column of its own, so it takes
the base's sidecar. Control: "a template instance's first store dropped the
base column's preserved states".

---

## 3. The provider

28 controls, 19 green. This is where whole mechanisms turned out to be
unpointed-at rather than under-covered.

| # | check disabled | anchor | test | result |
|---|---|---|---|---|
| V1 | `LoadSkip` entities | `pile.go:290` | `TestLoadSkip` | RED |
| V2 | `LoadSkip` block entities | `pile.go:293` | `TestLoadSkip` | RED |
| V3 | `LoadSkip` scheduled ticks | `pile.go:296` | — | **was green** |
| V4 | a store is refused read-only | `pile.go:341` | `TestReadOnly` | RED |
| V5 | `FilterColumn` | `pile.go:344` | `TestSkipAndFilters` | RED |
| V6 | `Skip(SkipEntities)` | `pile.go:349` | `TestSkipAndFilters` | RED |
| V7 | `FilterEntity` | `pile.go:351` | — | **was green** |
| V8 | `Skip(SkipBlockEntities)` | `pile.go:354` | — | **was green** |
| V9 | `FilterBlockEntity` | `pile.go:356` | `TestSkipAndFilters` | RED |
| V10 | `Skip(SkipScheduledTicks)` | `pile.go:359` | — | **was green** |
| V11 | `Skip(SkipChunkUserData)` | `pile.go:385` | — | **was green** |
| V13 | `StoreLight` reaches the writer | `save.go:296` | — | **was green** |
| V14 | `SkipBiomes` reaches the writer | `save.go:295` | — | **was green** |
| V15 | `Snapshot` refused read-only | `snapshot.go:29` | — | **was green** |
| V16 | `DeleteSnapshot` refused read-only | `snapshot.go:98` | — | **was green** |
| V17 | `Rollback` refused read-only | `snapshot.go:111` | — | **was green** |
| V18 | `SaveAsync` refused read-only | `save.go:36` | — | **green, and no input exists** |
| V19 | `Save` refused read-only | `save.go:20` | `TestReadOnly` | RED |
| V20 | a clean dimension is not rewritten | `save.go:149` | — | **was green** |
| V21 | auto-compact on close | `save.go:381` | `TestAppendAutoCompactOnClose` | RED |
| V22 | `Close` saves | `save.go:359` | four tests | RED |
| V23 | `SetChunkUserData` refused read-only | `metadata.go:73` | — | **was green** |
| V24 | `SetUserData` refused read-only | `metadata.go:19` | — | **was green** |
| V25 | `SetMarker` refused read-only | `metadata.go:126` | — | **was green** |
| V26 | `RemoveMarker` refused read-only | `metadata.go:145` | — | **was green** |
| V27 | `SetBorder` refused read-only | `border.go:70` | — | **was green** |
| V28 | `SavePlayerSpawnPosition` refused read-only | `pile.go:525` | — | **was green** |
| V29 | `SaveSettings` refused read-only | `pile.go:205` | — | **was green** |

### Read-only: eleven guards, one test, two checks

`TestReadOnly` drove `StoreColumn` and `Save` and then compared the dimension
file byte for byte. That is a real check and it caught nothing else, because
every other guard either returns an error the test never asked for or changes
state the test never read. `Snapshot`, `DeleteSnapshot` and `Rollback` write to
disk *directly*, so with their guards removed a read-only provider deletes a
snapshot on request; the file comparison did not see it because it only looked
at `overworld.pile`.

`TestReadOnlyRefusesEveryMutator` drives all eleven, asserts `ErrReadOnly`
where the method returns one and asserts the *observable* where it does not
(`Settings().Name`, `UserData()`, `Markers()`, `ChunkUserData`, `Border()`, the
spawn store's map), and finishes by hashing every file under the world
directory and requiring the map to be unchanged. Nine controls turn it red with
a message naming the method.

**V18, `SaveAsync`, is the exception and stays green.** Every method that could
set a dirty flag refuses a read-only provider first — `ds.dirty` and
`p.metaDirty` are set at eleven places, ten behind a guard and the eleventh in
`insertColumn`, which only `Builder` calls — so a read-only save finds no dirty
dimension, writes nothing, and is indistinguishable from the guarded one. There
is no input that separates them. It is **kept** rather than deleted, unlike the
four checks removed elsewhere in this document, and the difference is worth
stating: those were validity checks whose neighbour refused the same inputs, so
removing them lost nothing; this one is the second line of a safety property
whose first line is ten other guards. `save.go` now says so at the guard.

### Skip, filter and writer options

`TestSkipAndFilters` set `Skip(SkipEntities|SkipScheduledTicks)` on a fixture
(`testColumn`) that carries **no scheduled updates and no chunk user data**, and
used `FilterBlockEntity`, which sits in the `else` arm of `SkipBlockEntities`
and therefore excludes it. Four of the store path's branches had no input.

- `TestStoreSkipMaskCoversEveryCategory` (new) stores a column carrying all four
  categories, first with no skipping — asserting the fixture is not empty to
  begin with, which is what made the old one useless — then with all four bits
  set. Controls: "SkipBlockEntities ignored: 1 block entities",
  "SkipScheduledTicks ignored: 1 scheduled updates", "SkipChunkUserData
  ignored: "ud"".
- `TestFilterEntityDropsOnStore` (new) uses a provider that does not set
  `SkipEntities`, which is the only way to reach `FilterEntity` at all.
- `TestLoadSkip` now stores `tickColumn` and skips scheduled ticks too, and
  asserts the file still holds all three. Control: "LoadSkip ignored: … 1
  scheduled updates".
- `TestStoreLightAndSkipBiomesReachTheWriter` (new) covers the two `Options`
  fields the save path fills in and nothing asserted on: it checks
  `FlagStoreLight` in the header of a world saved with `StoreLight()` (and its
  absence without), and that a biome does not survive `Skip(SkipBiomes)`.
  `StoreLight` only claims light the records actually carry, so the fixture
  fills the column's light first — without that the option is invisible.

### The clean-dimension skip

`save.go:149` refuses to rewrite a dimension that is on disk, unmodified, and
whose world metadata is unmodified. The output is byte-identical either way,
because the world encodes deterministically, so nothing about the file's
*contents* can show the difference. `TestSaveSkipsCleanDimensions` sets the
file's mtime an hour into the past, saves, and requires the mtime not to have
moved — an atomic replace gives a new file and a new mtime — then dirties a
column and requires it to move. Control: "a clean dimension was rewritten:
mtime moved to …".

---

## 4. The golden suite

7 controls, 5 red. The golden tests are the byte lock, so the control is: move
one byte a writer produces and require them to notice.

| # | check disabled | anchor | test | result |
|---|---|---|---|---|
| G1 | the `StoreLight` header flag | `format/encode.go:337` | `TestGoldenFormatStability/world_light`, `/world_all` | RED — "the wire format changed" |
| G3 | the structure writer drops trailing air layers | `format/structure.go:364` | `/structure`, `/structure_zstd` | RED |
| G7 | the indexed directory's Morton order | `format/indexed.go:1966` (swap `ka`, `kb`) | `/indexed`, `/indexed_zstd`, `/indexed_torn` | RED |
| G5 | the decoder reinjects a block entity's `x`/`y`/`z` | `format/decode.go:759` | `TestGoldenFormatReadable` | RED — "column (0,0): block entities:" |
| G6 | worker count must not affect output | `format/encode.go:212` (make `SkipBiomes` depend on `Workers`) | `TestUncoveredOptionPaths` | RED — "Workers=1 produced 493 bytes, parallel produced 504" |
| G2 | the state-property key sort | `format/palette.go:354` (`slices.Sort` then `Reverse`) | goldens **green** | fixture gap, see below |
| G4 | the world writer drops trailing air layers | `format/encode.go:874` | goldens **green** | held by `TestTrailingAirLayersDropped` |

**G2 is a real gap and it cannot be closed here.** Reversing the sort of a
block state's property keys does not move a single golden byte, which means no
block in any golden world has **two or more state properties**. That is a
common shape — any stair, any fence — and the canonical order of its keys is
therefore not byte-locked by the goldens. It is enforced: §3.2's reader check
(`palette.go:444`, `:447`) and `TestWriterSortsStateProperties` both hold it,
and both have controls in the table above. Closing the *golden* gap means
adding a block to a golden world, which regenerates the fixtures, and this pass
is forbidden from running `-update`. **Whoever regenerates the goldens next
should add a two-property block state to `goldenWorld`.** Until then the sort
order is held by a unit test and not by the byte lock.

G4 is the same shape and less interesting: the rule has a named test that is
red under the control, and the golden worlds simply do not have a section with
a spare trailing air layer.

`TestUncoveredOptionPaths`'s second claim — that `FastCompression` does not
change the content hash — has no control of its own, and the reason is
structural rather than an omission: the fast and default paths feed identical
bytes to the same encoder and differ only in zstd's concurrency setting, so
there is no edit that changes one and not the other. What the assertion buys is
a regression guard against the dependency, which is worth having and is not the
same thing as a check with an input.

---

## 5. Two tests written in this pass

Listed here so the document stays the whole record; the measurement is in
`SECURITY.md`.

| # | check disabled | anchor | test | result |
|---|---|---|---|---|
| D1 | both dictionary cache bounds | `format/zstdpool.go` `dictCacheEntries`, `dictCacheBytes` | `TestDictCodecCacheIsBounded` | RED — "64 codecs held after 64 distinct dictionaries, bound is 16" |
| D1b | the same | the same | `TestEvictedDictCodecStaysUsable` | RED — "the codec under test was never evicted; the fixture does not reach the case" |
| D2 | a decoder closed under its reader | `format/zstdpool.go` `decodeAll`, `d.Close()` before `DecodeAll` | `TestEvictedDictCodecStaysUsable` | RED — "decoder used after Close" |

`TestDictCodecCacheIsBounded` asserts against the literals 16 and 16 MiB rather
than against `dictCacheEntries` and `dictCacheBytes`, and separately requires
the constants to equal them. A bound asserted against its own constant moves
with the constant and cannot fail — the same trap `SECURITY.md` records for the
column ceiling's writer half. The first version of this test had it, and D1 was
green until it was fixed.

---

## 6. The caller decode ceiling, and three golden fixture gaps

A third pass. It added `MaxDecodedBytes` (`SECURITY.md`, "Format changes:
made", item E), closed G2 above, and re-ran the golden gap hunt as a systematic
sweep rather than by inspection — which found two more gaps of the same shape
that reading had missed.

### 6.1 G2 closed: the state-property key sort

`goldenWorld` could not gain a two-property block state without moving eleven
existing goldens, and moving an existing golden is what the freeze check exists
to catch. The fixture is therefore **additive**: a new variant `world_props`,
whose column carries two preserved states: one with four property keys across
three property types, so the order is pinned across type boundaries; and one
whose keys are `a`, `aa` and `b`, which pins the order as **bytewise** rather
than length-then-bytes — the two disagree on `aa` against `b`, and keys of
equal length cannot tell them apart.

`-update` was run with **no `-format-change`**, which is itself the proof that
nothing moved: the guard refuses to bless a changed hash while `format.Version`
stays at 2, so a silent regeneration of an existing fixture could not have
succeeded. Verified again by comparing the manifests as sets — sixteen entries
before, seventeen after, **no existing hash changed**.

| # | check disabled | anchor | test | result |
|---|---|---|---|---|
| H1 | the state-property key sort | `format/palette.go:354` (`slices.Sort` then `Reverse`) | `TestGoldenFormatStability` | **RED** — `/world_props`, and **only** `/world_props`: the other sixteen goldens stayed green, which is the gap G2 recorded, measured directly |
| H1b | the same | the same | `TestConformanceVectors` | RED — `/world_preserved`. The vectors never had this gap: `world_preserved` has carried a two-property state all along, so no new vector was needed. Established by the same control rather than by reading the fixture. |
| H1c | bytewise order specifically | `format/palette.go:354` replaced with a length-then-bytes comparator | `TestGoldenFormatStability` | **RED** — `/world_props`. This is why `props_b` holds `a`, `aa` and `b`: keys of equal length cannot separate the two orders, and every other property fixture in the tree has equal-length keys. |
| H2 | the property fixture degrading to one property | `goldenPropsWorld`'s states reduced to one property each, then `-update` | `TestGoldenFormatReadable/props_still_multi` | **RED** — "property golden has 1 states with two or more properties (widest 3)" |

### 6.2 The sweep: every writer-side ordering decision, against the goldens

Fifteen sort sites across `encode.go`, `palette.go`, `blob.go`, `structure.go`,
`nbt.go` and `indexed.go`, each reversed in turn with the golden suite run at
no flags. Eleven were caught. The four that were not:

| site | rule | verdict |
|---|---|---|
| `format/structure.go:447` | structure block entities ascend by (y, z, x) | **golden fixture gap.** `goldenStructure` holds exactly one block entity, so the sort had nothing to order and no golden byte moved. The `structure_full` conformance vector does catch it, so the rule was enforced — just not by the artefact the freeze check consults. **Closed**, see 6.3. |
| `format/structure.go:460` | structure entities ascend by encoded NBT | **golden fixture gap**, same shape, same cause, same vector catching it. **Closed**, see 6.3. |
| `format/encode.go:809` | the solid writer's pre-sort of scheduled ticks | **not a gap.** Uncaught by the whole suite, and uncatchable: `sortTicks` (`encode.go:1321`) imposes a total order on (y, x, z, tick, final palette reference) afterwards, and two ticks that tie on all five encode as identical bytes. The pre-sort is a stable sort whose stability can never be observed on the wire. Annotated at the site. |
| `format/indexed.go:2116` | the order `Compact` rewrites records in | **not a canonical rule.** Uncaught by the whole suite, and correctly so: `FREEZE.md` states that indexed byte layout is history-dependent and not frozen, and `golden_indexed_compact.pile` is deliberately not byte-locked because dictionary training is not reproducible. This decides physical record placement, which is a locality choice. The directory frame's own Morton order is a different site (`indexed.go:2031`) and is caught by three goldens. |

### 6.3 The structure collection fixture

`golden_structure_collections`, additive as above. Four block entities supplied
in exactly the reverse of their canonical order, positioned so that each of the
three comparison rungs (y, then z, then x) decides exactly one adjacent pair —
a sort that dropped any one rung still reorders the rest. Three entities, which
is enough because reversing three distinct elements always changes their order.

| # | check disabled | anchor | test | result |
|---|---|---|---|---|
| H3 | structure block entity order | `format/structure.go:458` (`slices.Reverse(bes)` after the sort) | `TestGoldenFormatStability` | **RED** — `/structure_collections`. Green on every golden before this fixture existed. |
| H4 | structure entity order | `format/structure.go:460` (`slices.Reverse(ents)` after it) | `TestGoldenFormatStability` | **RED** — `/structure_collections`. Green on every golden before. |
| H5 | the fixture degrading | `goldenStructureCollections` reduced to one block entity, then `-update` | `TestGoldenFormatReadable/structure_collections` | **RED** — "collection structure holds 1 block entities and 3 entities" |

A note on method, because the first attempt got it wrong: reversing a sort
means inserting `slices.Reverse` **after the closing `})` of the sort call**,
not inside the comparator. An insertion inside the comparator compiles, mutates
the slice during the sort, and produced a spurious "uncaught by the whole
suite" verdict for H3. Line numbers were confirmed with `sed` before each edit
after that.

### 6.4 The caller decode ceiling

Twelve controls, all red. The subject in each case is a single line or clause.

| # | check disabled | anchor | test | result |
|---|---|---|---|---|
| B1 | the default ceiling being §8's | `format/format.go`, `decodedByteCeiling()` returns 4096 | `TestDecodeBudgetDefaultChangesNothing` | RED — "the default ceiling refused a file as over budget" |
| B2 | the clamp to §8's ceiling | `format/format.go`, drop `\|\| c.maxDecodedBytes > decodedBytesCeiling` | `TestDecodeBudgetClampsUpwardOnly` | RED — "a caller asking for 9223372036854775807 got a ceiling of 9223372036854775807" |
| B3 | the one column of headroom | `format/format.go`, drop `+ columnBytes` from `decodedBytesCeiling` | `TestDecodeBudgetCeilingIsUnreachableByDefault` | RED — "the default ceiling is 4831838208 and §8 permits a decode to reach 4831838208" |
| B4 | `ReadWorld` charging a column per record | `format/decode.go`, `if false` over `chargeColumns(1)` | `TestDecodeBudgetChargesColumnsWithNoStorages` | RED |
| B5 | `ReadStructure` honouring the ceiling | `format/structure.go`, `newStorageBudget(cfg)` → a bare `&storageBudget{}` | `TestDecodeBudgetReachesEveryReader/ReadStructure` | RED — "a one-byte ceiling decoded the whole fixture" |
| B6 | `ContentHash` passing options down | `format/check.go`, drop `opts...` | `TestDecodeBudgetReachesEveryReader/ContentHash` | RED |
| B7 | `OpenIndexed` resolving the ceiling | `format/indexed.go`, drop the assignment to `w.decodedByteCeiling` | `TestDecodeBudgetReachesEveryReader/OpenIndexed` | RED |
| B8 | the directory being charged at open | `format/indexed.go`, `false &&` on the `finishDirectory` check | `TestIndexedDecodeBudgetIsPerHandle` | RED — "a ceiling of 16383 opened a directory of 16 entries costing 16384" |
| B9 | a record paying for the directory | `format/indexed.go`, `recordBudget` ignores `len(w.dir)` | `TestIndexedDecodeBudgetIsPerHandle` | RED — "a column decoded with the whole ceiling already spent on the directory" |
| B10 | the indexed record's own column charge | `format/decode.go`, `if false` over `chargeColumns(1)` in `decodeRecordBody` | `TestIndexedDecodeBudgetChargesTheRecordColumn` | RED |
| B11 | a budget refusal stopping recovery | `format/indexed.go`, `false &&` on the `ErrDecodeBudget` break | `TestIndexedDecodeBudgetDoesNotFallBack` | RED — "the open succeeded with 1 columns: it fell back to an older checkpoint" |
| B12 | the sentinel not wrapping `ErrCorrupt` | `format/format.go`, make `ErrDecodeBudget` wrap it | `TestDecodeBudgetSentinelIsNotCorrupt` | RED |
| B13 | the provider passing the ceiling down | `options.go`, `readOpts` returns nil | `TestProviderMaxDecodedBytes` | RED — "the option is not reaching the reader" |

**Two of these were green on the first attempt and are the reason the fixture
`emptyColumnWorld` exists.** B4 and B10 were written against fixtures carrying
blocks, and a ceiling tight enough to refuse such a file is refused by the
*storage* charge whether columns are charged or not — so both tests stayed
green with the column charge deleted. The column charge can only be isolated by
a file that decodes into columns and no storages at all: 96 bytes, 64 records,
each declaring one section and marking none present, which is the shape
`SECURITY.md` records as the 1,161-byte residual. Both controls are red against
it.

### 6.4b A race the ceiling introduced, and the test that was missing

`recordBudget` derives a record's budget from `len(w.dir)`, and `Column`
releases `w.mu` before decoding — so the first version read the live directory
after the unlock, racing a concurrent `Store`. The surrounding code says
plainly that everything a decode needs is snapshotted under the lock; the
budget was not.

It was found by reading the diff rather than by a test, and that is the finding:
**nothing in the suite drove `Column` against a concurrent `Store`**, so
`go test -race` was green with the race present. The budget is now snapshotted
alongside the palettes, and there is a test.

| # | check disabled | anchor | test | result |
|---|---|---|---|---|
| B15 | snapshotting the record budget under the lock | `format/indexed.go`, move `w.recordBudget()` below `w.mu.Unlock()` | `TestIndexedColumnDecodesOutsideTheLockSafely` (`-race`) | **RED** — `WARNING: DATA RACE`, write by `mapaccess2_fast64` against the reader |

The test guards more than the budget: the palette snapshot has the same shape
and had the same absence of coverage.

### 6.5 The indexed fuzz harness

The stated hypothesis for `FuzzOpenIndexed`'s 168k executions over 20 minutes
(against 11-16 million for the other three targets) was that recovery is
legitimately expensive — the §8 total-work bound measures ~4.2 s at its
ceiling. **Measured, and refuted.** Replaying the target's entire 50-entry
cached corpus through the harness body took **2.6 ms in total**, mean 52 us,
slowest entry 320 us. Nothing in the corpus is expensive.

Where the time actually went, in two measurements:

- `sample(1)` on the fuzz workers: their non-idle time is in `mkdir`, `open`,
  `write`, `openat`, `close`, `fdopendir`/`getdirentries64`, `unlinkat` and
  `rmdir` — the harness's per-execution `t.TempDir()`, `os.WriteFile` and
  cleanup, not the decoder.
- A CPU profile of the decode alone: **85.6% in `syscall.rawsyscalln`**, 63%
  cumulative under `readFrame`, whose body is one `w.f.ReadAt`. Every frame an
  indexed reader touches is a `pread`. `FuzzReadWorld` reads a `[]byte`; that is
  the whole difference between 4.5k/s and 2/s.

Benchmarked per execution on this machine: **3.64 ms** with a fresh temp dir,
**1.43 ms** reusing one path, **0.15 ms** with no file at all.

So the fix is not a decode budget — Job 1's ceiling would have bought nothing
here, because the target was never decode-bound. The harness now opens through
`memFile`, an in-memory `indexedFile`. `openIndexedOn` already takes that
interface (it is how the durability suite injects a file that tears), so every
recovery path is still reached; the only thing skipped is `OpenIndexed`'s own
`os.OpenFile`.

Result, 20 minutes on the same machine, `PASS`, no crashers:

| | before | after |
|---|---|---|
| executions | 168,000 | **37,722,587** |
| rate | ~2/s (0/s for the final 25 s) | **~31,400/s** |
| corpus | 50, still growing | 94 -> 146 |

**Still not saturated, and saying so.** The corpus was still taking new
interesting inputs in the final minutes (the 52nd landed at 19m39s), so the
target has not plateaued the way the other three had. What changed is the
budget it has to find them with: 225x more executions per unit of wall time.
The honest statement is that this target now explores at the same order of
magnitude as the others, not that it is finished.

| # | check disabled | anchor | test | result |
|---|---|---|---|---|
| B14 | the in-memory file model agreeing with a real one | `format/fuzz_test.go`, corrupt the last byte of every `memFile.ReadAt` | `TestMemFileMatchesOsFile` | **RED** — "the two file models disagree on the verdict: disk `<nil>`, mem pile: corrupt file: no valid checkpoint" |

Guards the harness rather than the format: `FuzzOpenIndexed` now reads through
`memFile`, so a `memFile` that behaved differently from `os.File` would be
fuzzing the model rather than the decoder. All four indexed goldens must open,
list and decode identically through both.

A note on H2 and H5, because the first attempt at both was wrong in an
instructive way. Degrading the fixture *builder* and running the guard leaves
it green: the guard reads the checked-in `.pile`, which the builder has not
touched. The real regression is a future pass that degrades the builder **and
regenerates**, so the control has to do both — and the guard is then what
catches a fixture that has quietly stopped covering its rule. Both controls
above run `-update` before the assertion for exactly that reason.
