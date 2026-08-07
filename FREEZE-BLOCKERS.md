# Format-affecting work that must land before the freeze

Every item here changes which files a conforming reader accepts. Each therefore
has to land **before** `format.Version` is frozen: afterwards, closing one of
these gaps rejects files that were valid, which is a breaking change.

Found by a triage pass over the specification against the decoders. Six of the
seven are rules `format/format.md` states and no code enforces. They were found
by hand in a single pass, which is the reason to expect more — see the closing
note.

## 1. Unused local palette entries (§3.3, rule `5ab4d3d6`)

**Do this one first: it is also a bypass for two rules that are enforced.**

The spec says a section blob's palette "contains only entries actually used by
the indices". Nothing checks it — not `decodeOneBlob`, `blobIndices`,
`applyBlockBlob`, `applyBiomeBlob` or `makeStorage`.

The consequence is not merely a wasted entry. `uniformAirBlob`
(`format/decode.go`) returns false whenever `len(b.refs) != 1`, so a blob whose
local palette is `[air, stone]` with all 4096 indices zero is semantically
uniform air and still passes `checkSectionCanonical`. That defeats §4.3
`12bc11b2` (an all-air section MUST be absent) and `a9ee3ac1` (trailing all-air
layers MUST be dropped). A file can carry present all-air sections and trailing
air layers today, each a second encoding of a world that already has one.

Fix: reject a blob with an entry no index references. Note the writer already
produces only used-entry palettes, so no file this library wrote becomes
invalid.

## 2. `blockVersion` must be non-zero (§2.1, rule `718f271d`)

Zero is the value that means "the palette's own version" and cannot also be a
version. `parseFrame` (`format/decode.go`) reads the field and never compares
it to zero; `loadDirectory` (`format/indexed.go`) does the same with the
prologue copy. Both readers accept a file declaring zero. Fix both.

## 3. Structure decoder parity (§4.8 `c5770076`, §3.4 `1e589410` + `078b2b7d`)

Four of the six gaps are the same shape: **the structure decoder is a second
implementation of the world decoder and has drifted.** `ReadStructure` is
missing checks `ReadWorld` has:

- **Block-entity uniqueness.** At most one per position. `validateStructureData`
  bounds positions only; `ReadStructure`'s loop has no `seen` map, unlike
  `applyRecord`'s `seenBE`. Enforced by neither side.
- **Block-entity ordering.** The world reader checks (y, z, x) then written
  NBT; the structure reader does not.
- **Unreferenced blob-table entries** and **blob id first-use order.**
  `ReadWorld` enforces both through `tableBlobSource`'s `used` and `nextID`
  parameters. `ReadStructure` resolves cell references with a bare
  `blobs[ref]` and passes no tracking at all.

While fixing these, add a test that asserts the two decoders enforce the same
rule set, or the next divergence is found the same way this one was.

## 4. Uniform-default biome sections must be omitted (§4.7, rule `7984f33b`)

When `DefaultBiome` is set, a section whose biomes are uniformly the default
must be absent. `applyRecord` never checks it, so a file with the flag set and
a redundantly stored uniform-default section is accepted — a second encoding.

This is also a **mis-classification**: `format/invariants.go` files the rule
under "the default biome flag is not optional" as `WriterOnly`. That
justification covers the flag decision (`13221d37`) only. The omission rule is
fully reader-checkable: the flag is set, `defaultBiomeRef` is known, and a
present blob uniform on that ref is visibly a violation. Correct the label.

## 5. Empty palette segments (§5.3, rule `2ed66e73`)

A segment with no entries must be rejected. `finishDirectory` never checks
`n == 0` or `len(segRids) == 0`. The invariant claiming this names
`TestRejectsDuplicateSegmentReference`, which tests a different rule.

## 6. Directory offset accumulation

`poff += doff` in `finishDirectory` is not range-checked, unlike the `px`/`pz`
beside it. Two consequences: `e.off + int64(e.length)` can wrap past its
`> w.end` test, and a delta chain that wraps int64 onto legal offsets is a
second encoding of the same directory. Range-check per step.

## 7. Frame content size (§2.5, rule `edd065ba`) — a decision, not just a fix

The spec says a compressed frame MUST declare its content size. Nothing
inspects a frame header. There is no safety hole, since `WithDecoderMaxMemory`
bounds streaming output too; it is a conformance gap.

Either enforce it or delete the sentence — **but not neither, and not after the
tag.** After freeze, a second implementation that enforced it would reject
files this one writes, and neither side could be called wrong.

Recommended: enforce. One frame-header parse. Our `EncodeAll` always sets the
field, so no file this library has written becomes invalid; only a frame from a
streaming encoder would be refused.

## Also worth closing while the files are open

- §5.3 `89ca9097`: the directory must list segments in the order written. That
  is reader-checkable as strictly ascending frame offsets; `parseSegRefs` does
  not check it.
- `FREEZE.md` lists out-of-box cell padding as a decoder precondition while
  `format/invariants.go` classes rule `c176f73b` as `WriterOnly` with a written
  argument. The two documents disagree; edit one.

## Why to expect more

Three structural reasons, all worth fixing before trusting any "clean" result:

1. **The harness cannot catch this class.** `TestEveryRuleIsClaimed` proves a
   sentence is claimed; `TestEveryInvariantNamesALiveTest` proves the named test
   compiles. Six claims were false with both green. Until every claim is
   verified by disabling the production check and watching the named test fail,
   the number of unenforced rules is unknown.
2. **Only sentences containing MUST are pinned at all.** Layout tables, range
   annotations like "1 ≤ layerN ≤ 255", and §5.7's bullets are outside the
   pinned set and outside the invariant table, so nothing has ever checked them
   against the code.
3. **The structure path keeps drifting from the world path.** Four of six gaps
   were that. Nothing asserts the two enforce the same rules.

What is *not* covered by any of this: the writer side. Rules marked
`WriterOnly` are checkable only by re-encoding, and `ContentHash` round-trip
coverage over hostile-but-legal input has not been verified.
