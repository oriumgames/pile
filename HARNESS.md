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

The table holds 64 entries: 54 `Decoded`, 10 `WriterOnly`. (It held 63 before
this pass; one entry was split, see "Enforce labels" below.)

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
