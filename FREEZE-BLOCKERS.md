# Format-affecting work: what landed, and what the pass turned up

Every item here changed which files a conforming reader accepts, which is why
each had to land **before** `format.Version` is frozen: afterwards, closing one
of these gaps rejects files that were valid.

All seven items from the triage pass are closed, together with the two entries
it listed as worth closing while the files were open, and one further gap found
during the work. Each check was proved by disabling it and watching the test
that names it fail; the golden suite stayed green throughout with no `-update`
and no `-format-change`.

## Closed

**1. Unused local palette entries** (§3.3, `5ab4d3d6`). `decodeOneBlob` now
refuses a section blob declaring a palette entry no index names. This was the
one that mattered most, because uniformity is read off the local palette's
length: a palette of `[air, stone]` with all 4096 indices zero is air
everywhere and read as content, so §4.3's "an all-air section MUST be absent"
and its trailing-air-layer rule were both bypassable by padding a palette. They
ran; they did not hold. `TestRejectsUnusedLocalPaletteEntry`.

**2. `blockVersion` must be non-zero** (§2.1, `718f271d`). Both readers now
compare it: `parseFrame` and `loadDirectory`. The prologue copy is not a copy a
reader may take on trust from the header — it is the authority over it — so the
header's check protects nothing there. The rule has its own invariant entry
now, apart from unknown versions and flag bits. `TestRejectsZeroBlockVersion`.

**3. Structure decoder parity** (§4.8 `c5770076`, §3.4 `1e589410` +
`078b2b7d`). `ReadStructure` now resolves cell layers through the same
`tableBlobSource` a world record uses, so unreferenced blob-table entries and
blob ids out of first-use order are refused, and it checks the strict (y, z, x)
ascent of block entities, which carries their uniqueness. The writer refuses
two at one position as well; it used to emit a file it could not read back.
That makes the NBT tie-break §4.8 names for structure block entities
unreachable, since two entries can only tie on position.

`TestDecodersAgreeOnValidity` is the part that matters beyond the four fixes:
one shape rendered into both a chunk record and a structure cell, put through
both decoders, required to get the same answer. Four of the six triaged gaps
were this drift and nothing asserted against it.

**4. Uniform-default biome sections** (§4.7, `7984f33b`). `applyRecord` refuses
a present biome blob uniform on the file's own `defaultBiomeRef`. The rule was
filed as `WriterOnly` under an argument that covers the neighbouring flag
decision (`13221d37`) and not this; the two now have separate entries with
separate labels. `TestRejectsStoredDefaultBiomeSection`.

**5. Empty palette segments** (§5.3, `2ed66e73`). Both segment loops refuse a
segment with no entries. A directory naming no segments stays legal. The
invariant used to name the duplicate-reference test, which passes whatever this
rule does. `TestRejectsEmptyPaletteSegment`.

**6. Directory offset accumulation.** `poff` is range-checked per step. Note
what this is and is not: it does not change which files open, because adopting
a checkpoint reads every record the directory names. It stops
`e.off+int64(e.length)` wrapping past its bounds test, which is what stands
between a hundred-byte file and a four-gigabyte allocation in `verifyRecords`.
Its test therefore asserts *which* check refuses the file.
`TestRejectsWrappingDirectoryOffset`.

The triage also expected a delta chain that wraps onto legal offsets. There is
no such input: each entry's offset was already bounded, and a step that
overflows lands next to the bottom of int64. An explicit overflow test beside
the range check would have no distinguishing input, so there is not one.

**7. Frame content size** (§2.5, `edd065ba`) — **struck, not enforced.** The
recommendation to enforce rested on "our `EncodeAll` always sets the field". It
does not: klauspost omits the frame content size below a few hundred bytes, and
in indexed mode most frames a file holds are smaller than that. Wiring the
check in and running the suite is what showed it — the property tests fell over
on an empty chunk and a 1×1×1 structure, and the golden indexed worlds became
unreadable. Enforcing would have invalidated a large share of everything this
library has written to satisfy a rule its writer cannot keep, and nothing rests
on it: `WithDecoderMaxMemory` bounds a streaming decode by the same ceiling.
§2.5 now says there is no such requirement, and why, so it is not reintroduced
as an improvement.

**Also worth closing.** §5.3 `89ca9097`: `parseSegRefs` now requires strictly
ascending frame offsets, which is what "the order they were written" means for
an append-only file. Its strict ascent also refuses a duplicate reference, so
the seen-set that used to hold that half is gone rather than left where it
could never fail. `FREEZE.md` and the invariant table disagreed about
out-of-box cell padding; the checklist was the wrong one and now says so.

**Found during the work, not on the list.** §5.1 never said a frame ends where
its content ends, and the meta frame and both kinds of palette segment accepted
trailing bytes — a padded frame decoded to exactly what the unpadded one did
while carrying its own length and hash in the directory. The sentence is now in
§5.1 and all five frame kinds are checked and fixtured.
`TestRejectsTrailingBytesInFrames`.

## Still open, and why the number is not zero

A sweep of every pinned rule against the code after this work found no further
sentence the specification states and no code enforces. That is a weaker result
than it sounds, for the reasons the triage gave and which still hold:

1. **Only sentences containing MUST are pinned.** Layout-table range
   annotations were spot-checked by hand against the constants this round and
   agreed, but nothing checks them and nothing will. A later pass found the
   first real instance of the cost: §3.1's *indices strictly ascending* was an
   annotation inside a layout fence, and the extractor strips fences, so no
   invariant had to claim it and the decoder enforced only half of it for as
   long as it existed. It is a normative sentence now. The class is still open,
   and it is the reason this item stays on the list rather than being ticked.
2. **The harness still cannot catch a vacuous claim.** `TestEveryRuleIsClaimed`
   proves a sentence is claimed and `TestEveryInvariantNamesALiveTest` proves
   the named test compiles. Neither is evidence the test reaches the rule. The
   entries touched this round were each proved by disabling the production
   check; the rest of the table has not been, and `STATUS.md` puts the
   remainder at roughly a dozen.
3. **The gap found off-list was not a rule at all** until this round wrote it
   down. Three of five readers enforced it and two did not, and no amount of
   checking the table against the specification would have surfaced that,
   because the specification did not state it. The way that class gets found is
   reading two implementations of one thing side by side —
   `TestDecodersAgreeOnValidity` now does that automatically for the pair that
   has drifted four times.

What remains untouched by any of this is the writer side. Rules marked
`WriterOnly` are checkable only by re-encoding, and `ContentHash` round-trip
coverage over hostile-but-legal input has still not been verified.
