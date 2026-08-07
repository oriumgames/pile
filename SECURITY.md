# Security

This document says what pile defends against, what it does not, what the
hostile-input matrix covers, what that matrix found, and what remains
unaudited. It exists because a threat model that is not written down is one
every caller has to guess at, and the guesses differ.

The subject is the **decode paths**: `format.ReadWorld`, `format.ReadStructure`,
`format.ReadMeta`, `format.OpenIndexed` and everything they reach. Those four
functions are where bytes from outside the process become objects inside it.

---

## Threat model

### What the format offers

**Integrity against corruption.** Every file carries an xxHash64 checkpoint
hash over its header, its stored payload and its footer control words (§2.4),
and every indexed frame carries its own xxHash64. A disk that flips a bit, a
transfer that truncates, a crash mid-checkpoint: all of these are detected, and
in indexed mode §5.6 recovery falls back to an older checkpoint rather than
failing the open.

**Termination and memory safety on arbitrary input.** A decoder must not
panic, must not hang, and must not be talked into an allocation the input does
not justify. That is a property of this implementation rather than of the
format, and it is what the matrix in `format/hostile_test.go` exists to hold.

### What the format does not offer

**xxHash64 is not a cryptographic hash.** It is not a MAC, it is not
collision-resistant, and it is keyless. Everything a checkpoint hash covers,
an attacker can recompute. Concretely:

- An attacker who can author content and induce truncation **can forge a
  checkpoint**. The recovery scan of §5.6 looks backwards for footer magic and
  accepts the newest footer whose hash verifies; a footer the attacker wrote
  verifies exactly as well as one the writer wrote. Truncating a file to just
  after an attacker-planted footer therefore rolls the world back to whatever
  that footer names.
- A file can be edited in place and re-sealed. `format.CheckpointHash` is
  exported, deliberately, so tooling can do this; so can anyone else.
- Two different files can be made to share a `ContentHash`. It is xxHash64 over
  the canonical body, so it identifies content for caching and comparison, not
  for authentication. Do not use it as a security boundary.

**There is no confidentiality.** Nothing is encrypted. User data blobs, chunk
NBT and settings are stored as given.

**Files from untrusted sources are untrusted content.** A pile file is a
program's input, not its trusted state. Decoding one from an untrusted source
is safe in the sense above — it will not corrupt the process — but its
*contents* are the author's, not yours. A world that arrives over the network
can name any block state, any biome, any entity NBT and any position inside its
own declared span, and the decoder's job is to hand those to you cleanly rather
than to decide they are reasonable.

**Resource use is bounded but not small.** §8's ceilings are set near what the
underlying models can represent, not at what a deployment expects. A legal
solid body is up to 512 MiB decompressed and a legal file of either mode names
up to 4,194,304 columns, and zstd will deliver either from a file of a few
kilobytes. The measured consequences are in "What remains" below. A process
that decodes files it did not write should bound its own concurrency and treat
a decode as an operation that can cost a gigabyte and seconds of CPU, because a
hostile file will make it do exactly that within the rules.

### What this means for a caller

- Treat a decode of a foreign file the way you would treat an image decode:
  memory-safe, bounded, but not free, and not to be run unbounded in parallel.
- Set your own ceiling if you know what your worlds cost.
  `pile.MaxDecodedBytes` on the provider, `format.MaxDecodedBytes` on the
  readers. A file refused under it fails with `format.ErrDecodeBudget`, which
  does not wrap `format.ErrCorrupt`: the file is not being called invalid, and
  must not be quarantined as though it were.
- Do not treat a checkpoint hash, a frame hash or a `ContentHash` as evidence
  that a file came from you. If you need that, sign the file.
- The dragonfly `world.Provider` this library implements is intended for worlds
  the operator controls. Serving arbitrary user-supplied worlds through it is
  outside what has been audited.

---

## What the matrix covers

`format/hostile_test.go`. It runs on every `go test ./...` with no flags, and
under `-race`.

| test | what it drives |
|------|----------------|
| `TestHostileTruncation` | A complete solid world body and a complete structure body, cut at **every byte offset**, each re-sealed with a repaired header and checkpoint hash so the cut is what the reader meets. Driven through `ReadWorld`, `ReadStructure`, `ReadMeta` and `UnresolvedStates`. Cutting at every offset covers every field boundary and also every offset inside a field. Whole-body readers must refuse every proper prefix; `ReadMeta` and `UnresolvedStates` stop early by design (§9), so for them the requirement is a clean return and a monotone accept boundary. |
| `TestHostileIndexedTruncation` | A real two-checkpoint indexed file truncated at every byte offset, through `OpenIndexed`, then every column read, `UnresolvedStates` and `Meta` on whatever opened. |
| `TestHostileCountBoundaries` | Sixteen count fields at **0, 1, max and max+1**: chunk records, blob table, block palette, biome palette, string length, blob length, state properties, sections per chunk, layers per section, block entities, entities, scheduled ticks, section palette, structure axis size, structure block entities, structure entities. Each body is cut off immediately after the count, so what is under test is what the decoder does with the number *before* it has any of the bytes the number promises. Run both uncompressed and zstd-compressed. `max+1` must be refused. |
| `TestHostileIndexedCountBoundaries` | The two counts a directory frame carries — palette segments and directory entries — at the same four values, through `OpenIndexed`, with the frame authenticated so the count is what the reader meets. |
| `TestHostileAllocationCeilings` | Five files of 87–482 bytes, each with a hard ceiling on the bytes its decode may allocate. This is the part that catches what a count check does not. |
| `TestUnresolvedStatesHoldsTheLock` | `UnresolvedStates` against a concurrent `Compact`, and after `Close`. |
| `TestHostileCheckpointReplay` | A file whose every checkpoint candidate fails, so recovery tries all of them. Asserts the open terminates and stays linear in the candidate count. |
| `TestIndexedVerifyReusesOneBuffer` | A directory at 16,384 entries of 4 KiB, all intact, so `verifyRecords` walks the whole thing. |
| `TestHostileNBTContainerBudget` | The §8 container budget at its limit and one past it, by list elements and by sibling compounds alike. |
| `TestNBTWriterHoldsTheContainerBudget` | The writer half of the same rule: the marshaller refuses what the reader refuses. |
| `TestHostileOverrideDeltaWraps` | The one integer computation that wrapped before its bounds check. |
| `TestHostileDecodedColumnCeiling` | The §8 column ceiling, and that a count at the ceiling is refused for its records rather than for the ceiling. |
| `TestRecoveryWorkIsBounded` | The §8 total-work limit on recovery, spent across candidates rather than per candidate. |
| `TestMemFileMatchesOsFile` | That `FuzzOpenIndexed`'s in-memory file model decodes identically to a real file, so the fuzzing behind the "decoders never panic" claim is of the decoder and not of the model. |
| `format/budget_test.go` (ten tests) | The caller's decode ceiling: that the default decodes exactly what a reader without it decodes over every golden and every vector, that a tight ceiling refuses a conforming file as policy rather than as corruption, that every decoding entry point honours it, that it clamps downward only, and that an indexed handle charges its directory once and each record against the remainder. |

Two of those tests used to assert that a hostile input is **accepted**. They
were characterisation tests for the two findings that could not be closed
without a validity change. Both changes have since been made, along with two
more, and the tests are now enforcement tests. See "Format changes: made" below.

### Method

Every fix below was proved the way `HARNESS.md` proves a canonicality check:
disable the production line, run the named test, require it to fail, restore
and confirm with `git diff`. The recorded results are in "Negative controls".

Every allocation finding is proved by a file of a few kilobytes with the
measured allocation before and after. A claimed bound with nothing
demonstrating it is what this project keeps having to redo.

---

## What it found

Nothing in *this* section changes which files a reader accepts. Every fix here
bounds an allocation to what the input can actually justify, or moves work
behind a check the input has to pass anyway. The four validity changes are in
the next section. The golden and vector suites were green throughout, for both
sections, with no `-update` and no `-format-change`.

### Fixed

**1. The blob-table count sized three containers at once.** `decodeBlobTable`
bounded the count against the bytes that remain — a blob is at least three
bytes — and then reserved a `[]decBlob` (56 bytes an entry), a
`map[uint64][]int` and a `[][2]int` from it. Three bytes of input reserved
about a hundred bytes of memory, before a single blob was parsed.

- A **1,634-byte file** declaring the whole 16,777,216-entry limit allocated
  **2.42 GiB**, all of it live.
- The regression case, a 482-byte file declaring 4,194,304: **650,614,336 →
  about 13.4 MB.**
- Fix: cap all three hints at `maxPrealloc` and let append and the map grow.
  One reallocation on a file that really does hold that many blobs.

**2. The block palette count sized `[]parsedState`.** Two bytes of input buy a
32-byte entry plus the 48-byte property map it carries.

- A **158-byte file** declaring 1,048,576 entries: **35,821,032 → about 2.4 MB.**
- Fix: bound the hint, append instead of indexing.

**3. The biome palette count sized two slices and a map.**

- A **159-byte file** declaring 1,048,576 entries: **66,575,952 → about 2.5 MB.**
- Fix: same shape.

**4. The directory's palette-segment count sized `[]frameRef`.** Two bytes of
directory body buy a 24-byte reference, reserved before any reference had been
read, let alone located in the file.

- A **165-byte file** declaring 1,048,576 segments: **27,424,328 → about 2.4 MB.**
- Fix: same shape.

**5. A structure's cell grid was allocated from three uvarints.** Nine bytes
of input decided a `[]*chunk.SubChunk` of up to 1,048,576 entries, allocated
before the cell presence bitset — one bit per cell, and not optional — had been
read.

- An **87-byte file**: **8,393,320 → about 4.7 KB.**
- Fix: `ReadStructure` requires the remaining input to be able to hold the
  presence bitset before it builds the grid. This refuses exactly the files the
  `take` a few instructions later refuses, so the accept boundary does not
  move; `NewStructureData` was split so the count can be learned without the
  allocation.

**6. `verifyRecords` allocated a buffer per record.** Opening any indexed world
cost its own live size in garbage, and recovery repeats the pass for every
checkpoint candidate it tries.

- A **4,226-byte file** naming 16,384 records of 4 KiB: **70,118,216 → about 2.9 MB.**
- Fix: one buffer for the pass, grown when a record needs more.

**7. `UnresolvedStates` read palette segments outside the lock.** It copied
`w.blockSegs` under `w.mu`, released it, and then called `readFrame`, which
reads `w.f`, `w.compressed` and `w.dictCodec`. `Compact` replaces all three
while it rewrites the file, and `Close` closes the handle.

- The race detector reports it directly (`resetLoadedState` writing
  `w.dictCodec` against `readFrame` reading it).
- It is worse than a race. `readFrame` does not re-check a frame's hash — the
  hash was checked when the directory was loaded — so a segment offset taken
  from the *old* directory and read against the *new* file decodes whatever
  happens to sit at that offset, and the caller is told those are the world's
  unresolved states.
- Fix: hold `w.mu` for the whole call, and refuse on a closed handle rather
  than reading through a file descriptor the handle no longer owns. This is the
  use-after-close item; in Go an `*os.File` read after `Close` returns
  `ErrClosed` rather than reaching a reused descriptor, so the consequence was
  wrong data and a race, not memory unsafety.

### Hardened, with no test, and why

**8. `blobIndices` narrowed the palette length to a `uint16`.** §8 puts a
section blob's local palette at 65,536 entries, which is `uint16(65536) == 0`,
and every index would then be compared against zero and rejected. It is
unreachable: §3.3 requires every declared palette entry to be named by one of
the blob's 4096 indices, so a palette larger than 4096 is refused before this
runs. The comparison is now done in an `int` so it does not depend on that
argument holding somewhere else.

There is **no test** for this, and there cannot be one: it has no
distinguishing input, which by this project's own rule (`STATUS.md`, "a check
with no distinguishing input is not enforcement") means it is hardening and not
enforcement. It is recorded here rather than left for the next reader to
rediscover.

### Reviewed and deliberately not changed

**9. `finishDirectory`'s chunk count is the only count in the package not
bounded against the bytes that remain.** Every other count goes through
`reader.count` and then a `remaining()` test. This one is bounded only by
`maxDirEntries`. Adding the bound would change nothing: the directory map is
not preallocated, so the memory already grows with the bytes actually parsed,
and a count larger than the input can back fails a few entries later with
"unexpected end of data". The only effect would be a different error message,
which is a check with no distinguishing effect. It is noted so the asymmetry is
not read as an oversight.

---

## Format changes: made

Four validity rules were tightened, and one thing that is deliberately **not** a
validity rule was added: a caller-supplied decode ceiling (item E).

Four validity rules were tightened. Every one of them **rejects files this
reader used to accept**, which is what makes them format changes rather than
hardening, and they are permitted only because they were made before the
freeze. Two were reported by the previous pass and pinned by characterisation
tests; those tests are now enforcement tests, which is what "inverted" means
here — none of them was deleted.

Each has a normative sentence in §8 or §3.1, an entry in `format/invariants.go`,
a test proved by disabling the production check, and a recorded before and
after. The golden and vector suites were green throughout with no `-update` and
no `-format-change`, because none of these moves a byte a writer produces.

### A. The NBT container budget now charges compounds nested in compounds

§8 stated *NBT containers per blob (compounds and nested lists) — 1 048 576*
and `nbtvalidate.go` implemented half of it. It charged the elements of a list
whose element type is a compound or a list, which is the amplification it was
written for, and it did not charge a compound that is a field of another
compound — although such a compound costs six bytes on the wire (a tag, a
two-byte name length, a name that cannot repeat a sibling's, and `TAG_End`) and
becomes a map exactly as a list element does. The ceiling was stated; the
accounting did not implement it.

- **Before:** a blob of **2,097,152 sibling compounds** — twice the ceiling,
  **14,680,068 bytes**, inside the 16 MiB blob limit — was accepted and decoded
  into 2,097,152 maps at **461 MiB allocated, 265 MiB retained**.
- **After:** refused by `validateNBT` at 3.4 MB allocated, before any of it is
  handed to a decoder.
- The accounting is now one rule: every container nested inside another is
  charged one, and the blob's own root compound is not, because it is the blob
  rather than something inside it. That moves the list boundary by one as well
  — a top-level list of exactly 1,048,576 compounds is 1,048,577 containers and
  is now refused — which is the same rule applied consistently rather than a
  second change.
- **What became invalid:** any file carrying an NBT blob (settings, markers,
  border, stats, a block entity, an entity, or a structure's) that decodes into
  more than 1,048,576 containers.
- **Can the writer emit one?** Not now. The metadata blobs were already
  revalidated with `validateNBT` on write, in `encode.go` and in `indexed.go`,
  so those were covered by accident; a block entity's or an entity's NBT went to
  the wire straight from `marshalNBT` with no container count at all. The
  marshaller now keeps the same count, charged in the same two places, so the
  writer refuses exactly what the reader refuses. §8 already required that
  ("writers MUST additionally refuse content that their own readers would
  reject"); this was the one ceiling it was not done for.
  `TestNBTWriterHoldsTheContainerBudget` is the check, and its negative control
  is below.

### B. The version-override index chain may not wrap

§3.1's sparse version-override table carries index deltas as uvarints, and
`parseStatePalette` accumulated them in a `uint64`:

```go
idx := prev + delta
if idx >= uint64(len(entries)) { ... }
```

The bounds test catches an index past the palette, so nothing here was ever
memory-unsafe. But the sum is modular, and a uvarint can express the modular
representative of a *negative* step. After an override at index 5, a delta of
2⁶⁴−2 lands on index 3 — in range, so the bounds test says nothing — and the
chain has descended where §3.1 says it ascends. The decoder enforced the ascent
only through "every later delta MUST be non-zero", which is sufficient exactly
as long as the sum cannot wrap.

This is the most important of the four, and not because of what it costs: it is
a **second encoding of one palette**, and one content, one encoding is the
doctrine the rest of the format rests on.

- **Before:** accepted.
- **After:** refused, with an error naming the wrap. The check is `idx < prev`,
  which has a distinguishing input of its own: a zero delta is already refused
  above it, and the first delta starts from `prev == 0`, so the only way the sum
  can fail to increase is the wrap.
- **Why it was never caught.** The ascent rule lived as an annotation inside
  §3.1's layout fence — *indices strictly ascending* — and `extractSpecRules`
  strips fences, so there was no sentence for `TestEveryRuleIsClaimed` to
  require an invariant for. It is a normative sentence now, with an entry, and
  the same gap `FREEZE-BLOCKERS.md` names under "Only sentences containing MUST
  are pinned" is one instance smaller.
- **What became invalid:** any file whose block palette (or indexed palette
  segment) has two or more version overrides where a later delta exceeds
  `2^64 − 1 − prev`.
- **Can the writer emit one?** No. `encodeStatePalette` sorts the override
  indices ascending and emits `o.index - prev` between distinct indices bounded
  by 1,048,576, so every delta it writes is a true positive difference. Checked
  by reading the encoder, and by the negative vector, which had to move the
  base vector's first override off index 0 before there was anything to step
  back from.
- Covered by `TestHostileOverrideDeltaWraps`, by the independent walker of the
  vector suite (which had the same wrap and now refuses it), and by
  `neg_override_index_chain_wraps.pile`.

### C. The columns a file decodes into are bounded

§8 bounded decoded section storages, NBT containers and the recovery chain, and
left the column count at 4,294,967,295 — the width of the count field, which is
not a bound on anything. A chunk record must declare at least one section but
need not mark any present, so eleven bytes on the wire become a `recRaw`, a
`chunk.Chunk` and a `Column`.

- **Before:** ceiling 4,294,967,295. In practice the 512 MiB body ceiling bound
  first, at roughly 48.8 million columns and something near 48 GiB of live
  objects. A legal **1,161-byte** file of 1,048,576 eleven-byte records decoded
  into **2.99 GB allocated, 1.12 GiB retained**.
- **After:** ceiling **4,194,304**, in both modes. A file naming more is refused
  before any record byte is read: a 93-byte file declaring 4,194,305 costs
  23,528 bytes to refuse.
- **This does not reduce the 1,161-byte figure, and is not meant to.** That file
  declares 1,048,576 columns, which is a quarter of the new ceiling and still
  legal. What the change replaces is a ceiling that bounded nothing with one
  that bounds the worst case at about four gigabytes instead of about forty-
  eight. Getting the 1.12 GiB case refused would mean a ceiling below 1,048,576
  columns — 1024×1024 chunks — and that is a world size a real server could
  reach. The number was chosen to be far above any plausible world and below the
  point where a solid file stops being openable at all, and the residual is
  recorded under "What remains" rather than papered over.
- **Why this number.** An indexed directory has been capped at 4,194,304 entries
  all along, so the format now states one column ceiling instead of two that
  differed by three orders of magnitude, and neither mode invents a value. It is
  2048×2048 chunks, or 32,768 blocks square, against roughly ten thousand chunks
  for a real overworld. And every column holding a single block already consumes
  one of the 4,194,304 decoded storages of `maxDecodedStorages`, so a world with
  more content-bearing columns than this was already invalid — the only thing
  newly refused is a world with more than four million *empty* columns.
- **What became invalid:** a solid body declaring more than 4,194,304 chunk
  records.
- **Can the writer emit one?** This is the one of the four where the answer is
  "not any more" rather than "it never could". The writer's own check, in
  `encode.go`, is `len(d.Columns) > maxChunks` against the same constant the
  reader uses, so lowering the constant lowered both at once and they cannot
  disagree. Said plainly: this is a **ceiling being lowered**, not a
  canonicality rule being enforced, and a caller who really does hand the writer
  more than four million columns will now be refused where before it would have
  succeeded. That is the cost the ceiling buys, it is why the change belongs
  before the freeze and not after, and no world produced anywhere in this
  repository comes within three orders of magnitude of it.
- The duplicate ceiling check inside `Store` went with it: `maxChunks` and
  `maxDirEntries` are one number now, so the second copy had no input that could
  reach it.

### D. Recovery is bounded by its total work

§8 stated 256 chain links and a 4,194,304-entry directory as separate limits.
The cost is their product: every candidate whose directory parses is loaded in
full — frame decompressed, every entry parsed into the map — and 256 × 4,194,304
is a billion entries. Forging the candidates is free, because the footer hash is
xxHash64 over bytes the attacker wrote.

Measured on the machine this pass ran on, with a directory at the entry ceiling
and every record's hash wrong so no candidate is adopted:

| footers | file | before | after |
|---------|------|--------|-------|
| 1 | 9,205 bytes | 1.14 s | 1.16 s |
| 4 | 9,337 bytes | 4.63 s | 4.32 s |
| 16 | 9,865 bytes | 17.51 s | **4.21 s** |

Linear before, flat past four directories after. The earlier pass recorded
4.1 s / 14.9 s / 53.2 s for the same three cases; the shape is identical and the
constant is not, so the extrapolation to the full 256 candidates is about five
minutes on this machine rather than the quarter of an hour recorded then. Either
way it was a denial of service from a file that fits in a network packet, and
either way it is now bounded by a limit no candidate count can multiply.

- **The limit is 16,777,216 directory entries parsed, summed over every
  candidate an open tries** — the product, not the factors. Bounding the
  candidate count or the directory size alone leaves the other free.
- **Why that value.** It is four directories at the entry ceiling, which is more
  than a legitimate recovery needs: a torn checkpoint's directory frame normally
  fails its own hash and costs nothing to skip, and the crash model in
  `DURABILITY.md` leaves either the old checkpoint or the new one, so one
  fallback is the ordinary case. It is also exactly 256 × 65,536, so **no world
  of 65,536 columns or fewer can ever be refused by it**: such a world keeps
  every one of its 256 candidates. Only a world above that trades candidates for
  size, and only after four full directory loads have already failed.
- **Memoising by directory reference was considered and rejected**, by the
  previous pass and again here. It collapses the case where every footer names
  one directory, and an attacker pays about 200 bytes for a distinct frame to
  evade it, so it buys a number and not a bound. The enforcement test uses a
  file whose every footer names one directory, which is exactly the case the
  memo would have collapsed, and the budget still bounds it.
- **What became invalid:** a file whose valid checkpoint is reachable only after
  more than 16,777,216 directory entries have been parsed.
- **Can the writer emit one?** Not by writing. The writer appends checkpoints;
  it never produces a file with a chain of unusable ones. Reaching the limit
  requires four full-size directories to fail in succession, which needs either
  four stacked crashes on a world of over a million columns or an author who
  forged the footers. Checked by reading `checkpointLocked` and `Compact`: both
  write one footer per checkpoint over the directory they just wrote.

### E. The caller may set a stricter decode ceiling — which is not a validity rule

The one item here that changes no file's validity, and the only reason it sits
under "format changes" at all is that getting it wrong in either direction
would be one.

The residual this pass could not close is item C's: a legal **1,161-byte** file
decodes into **1.12 GiB**, and §8's column ceiling sits four times higher again
at 4,194,304. Refusing that file outright means a world-size ceiling below
1024x1024 chunks, and that trade was refused deliberately. But a lobby server
opening maps it did not write and an operator storing a genuine four-million
column world do not want the same number, and no constant serves both. The
number is therefore the caller's.

- **API.** `format.MaxDecodedBytes(n int64) ReadOption`, accepted as a variadic
  by `ReadWorld`, `ReadStructure`, `ReadMeta`, `OpenIndexed` and `ContentHash`.
  `ContentHash` is in the list because it decodes internally: without it, the
  one way to make a reader do the whole job with no ceiling over it would be to
  ask it to identify the file. The provider re-exports it as
  `pile.MaxDecodedBytes(n int64) Option`.
- **What it charges.** Decoded columns at 1,024 bytes each and decoded section
  storages at 128, the two quantities §8 already bounds by count and the two
  that dominate a decode's live footprint. Both figures come from measurements
  already recorded here: 1,048,576 empty columns at 1.12 GiB retained is about
  1,150 bytes each, and §8 puts a stored layer at "about a hundred bytes". It
  does not charge transient allocation inside an NBT blob, which has its own §8
  ceiling and is released before the decode returns; `ReadMeta` therefore
  accepts the option and documents that it cannot bind there.
- **Per call, except for `OpenIndexed`, which is per handle.** A solid file
  holds every column at once, so one call is the whole decode. An indexed
  handle decodes one record at a time, so a per-call ceiling would let a file
  with four million directory entries spend the caller's whole budget four
  million times over and bound nothing. The handle's ceiling covers the
  directory it loads and retains, charged at open so the file can still be
  declined, plus whatever one record costs on top of it — which is exactly
  indexed mode's stated memory contract.
- **It only tightens.** `n <= 0` selects §8's ceiling; anything else is clamped
  down to it. A reader that accepted what a conforming reader must refuse would
  fork the format as surely as one that refused what it must accept.
- **A refusal is not a claim of invalidity.** `format.ErrDecodeBudget` is a
  distinct sentinel that deliberately does **not** wrap `ErrCorrupt` — the sole
  documented exception to §8's "decoding errors wrap ErrCorrupt unless stated
  otherwise", and §8 now states otherwise. A caller that quarantines or deletes
  files on `ErrCorrupt` must not do either on this one, and a second
  implementation that read the ceiling as a validity rule would refuse
  conforming files and blame the file. §8 carries a paragraph saying so.
- **The default changed nothing, proved two ways.** By sweeping every golden and
  all 76 conformance vectors, accepted and refused alike, through `ContentHash`
  with and without the option and requiring the same verdict, the same error
  *string* and the same hash (`TestDecodeBudgetDefaultChangesNothing`); and by
  arithmetic, since the default ceiling is set one column above the most §8
  permits any decode to cost, so it cannot fire on a conforming file rather
  than merely not firing on the fixtures that exist
  (`TestDecodeBudgetCeilingIsUnreachableByDefault`). All 59 negative vectors are
  still refused for the rule each is named after, which
  `TestConformanceVectorsNegative` asserts by substring rather than by merely
  seeing an error.
- **What it does not do.** It does not lower the worst case a *default* caller
  faces; that is still four gigabytes, and the paragraph below still applies to
  anyone who does not set it. What it adds is a dial, and a name for the
  refusal.
- **Negative controls:** thirteen, in `HARNESS.md` §6.4, all red. Two of them
  were green when first written, and the fixture that fixed them — a 96-byte
  file of 64 columns and no section storages — is recorded there.

## What remains

Everything here is bounded — no unbounded loop, no unbounded allocation — and
several of them are bounded at a number large enough to matter. They are not
defects; they are the cost of the ceilings §8 sets, and lowering any of them
further is a format change, which after the freeze means a version bump. They
are written down because a caller needs them to size its own limits.

### The decompression ratio is the amplifier

zstd will deliver 512 MiB of body from a few hundred bytes, and the §8 ceilings
bound the body, not what the body decodes into. There are ceilings on decoded
section storages (`maxDecodedStorages`), on NBT containers per blob, on columns
per file, on the recovery chain and on total recovery work — §8 says they exist
for exactly this reason — and there is none on block entities, entities or
scheduled updates beyond their per-chunk limits, which the column ceiling now
multiplies rather than leaves free.

Measured:

| input | file | result |
|-------|------|--------|
| A solid world of 1,048,576 records, each 11 bytes and holding nothing | **1,161 bytes** | 2.99 GB allocated, **1.12 GiB retained**, about a second |
| An indexed directory at the 4,194,304-entry ceiling | **9,205 bytes** | 320 MiB retained, 1.1 s |
| A settings blob at the container budget | **132 bytes** | 82 MiB allocated transiently by `ReadMeta` |

The first is the one to know about, and it is **still within the rules after
this pass**. An eleven-byte chunk record is legal — a record must declare at
least one section but need not mark any present — and it becomes a `recRaw`, a
`chunk.Chunk` and a `Column`, about a kilobyte of live objects. The column
ceiling of §8 bounds the worst case at 4,194,304 of them, roughly four
gigabytes; it does not bound this one, which is a quarter of that. Bounding
*this* file means a ceiling below 1,048,576 columns, which is 1024×1024 chunks
and a world size a real server can reach, and that trade was refused
deliberately. See "Format changes: made", item C.

A caller that decodes solid files it did not write should therefore still size
its own limits as though a decode can cost a gigabyte, and should not run such
decodes unbounded in parallel. What changed is the ceiling above it: forty-eight
gigabytes was reachable within the rules before, and four is now.

Or it can set the ceiling itself: `format.MaxDecodedBytes`, and
`pile.MaxDecodedBytes` on the provider, bound the columns and section storages
one decode may produce. See item E above. That is the answer to this row for a
caller that knows what its worlds cost; the row stands as written for one that
does not set it, which is the default.

### Recovery work

Bounded, as of this pass: §8 limits the directory entries parsed across one
open's whole candidate list to 16,777,216, so the product of the chain limit and
the directory limit is no longer free. The measurements and the reasoning are in
"Format changes: made", item D.

What remains is the residual: four full-size directory loads is still about four
seconds of CPU from a file of about 9.5 KB, and that is the price of leaving
recovery able to reach a fourth fallback on a world of over a million columns.
It no longer grows with the candidate count, which was the denial of service.

`TestHostileCheckpointReplay` holds the shape below the bound and
`TestRecoveryWorkIsBounded` holds the bound.

### Process-global caches keyed by attacker-controlled bytes

**Fixed.** `zstdpool.go` memoised `dictCodec`s in a process-wide map keyed by
the dictionary's content and length. A hostile indexed file can carry any
dictionary up to `maxDictLen` (1 MiB), and opening it installed a parsed
decoder for that dictionary which was **never evicted**, so a sequence of files
carrying distinct dictionaries pinned one megabyte each for the life of the
process.

The map was documented as deliberate — it "grows with the number of distinct
dictionaries a process meets (one per compacted file), not with the number of
handles open on them" — and that reasoning is right for files the operator
wrote and wrong for files an attacker supplies, because the key is bytes the
file chooses.

`dictCodecs` is now the bounded LRU the memory pass built for the provider's
column and metadata caches, moved to `internal/lru` so both packages can use
one implementation rather than two. It was moved rather than copied for the
only reason it could not simply be imported: `format` is imported by `pile`, so
a type declared in `pile` is unreachable from `format`, and `internal/` is
where a type both need belongs. It fits without change — it is already bounded
by entry count *and* by a weight budget, which is exactly the shape this needs,
since one entry can be a megabyte on its own.

**The bound is 16 entries and a 16 MiB budget** (`dictCacheEntries`,
`dictCacheBytes`). Sixteen covers a world's three dimensions, the second handle
a compaction opens over each, and headroom; a dictionary this package trains is
at most 16 KiB, so sixteen of those weigh a quarter of a megabyte and the count
is what binds. At `maxDictLen` the two bounds bind at the same point, which is
how the budget was chosen.

Measured on one machine, installing 128 distinct dictionaries of exactly
`maxDictLen` bytes each and reading `HeapAlloc` after two collections:

| | entries held | retained |
|---|---|---|
| before | 128, and unbounded | **134,253,408 bytes (128.03 MiB)**, 1,048,854 per dictionary |
| after | **15** | **15,734,152 bytes (15.01 MiB)** |

Fifteen rather than sixteen because the weight budget bites first at that size:
16 × (1 MiB + 512) is over 16 MiB. The per-dictionary figure is essentially the
dictionary itself; the parsed decoders live in a `sync.Pool` the collector
empties, so they are not part of the standing cost, which is why
`dictCodecWeight` charges the dictionary bytes and a fixed allowance and not a
guess at the decoder.

**Eviction cannot break a live handle, because it does not close anything.**
`lru.Cache.evict` unlinks: it removes the map entry and the list element and
leaves the value alone. An `*IndexedWorld` takes its `dictCodec` at open and
holds it for its lifetime, so a codec evicted mid-read is still reachable
through the reader that is using it and still decodes. Nothing here may close a
codec on eviction — the cache has no way to know when the last handle is done —
and `zstdpool.go` says so at `sharedDictCodec`. `TestEvictedDictCodecStaysUsable`
runs four readers through a handle's codec while another goroutine installs 64
distinct dictionaries, verifies afterwards that the codec really was evicted
(or the fixture proves nothing), and reads through it again. Green under
`-race`; red when `decodeAll` is made to close its decoder before using it
("decoder used after Close").

The cost of the bound is one dictionary parse on the next open of a file whose
codec was evicted. A handle already holding one never re-fetches, and
compaction stays deterministic: a rebuilt encoder is built from the same
dictionary at the same level with concurrency 1.

The same shape, without the attacker control, applies to `encoders` and
`decPools`, which are keyed by a small fixed set of levels and ceilings.

### Not audited

- ~~**The writer paths.**~~ **Closed**: see "The writer matrix" at the end of
  this document. That pass drove `WriteWorld`, `WriteStructure`,
  `CreateIndexed`/`Store`/`Checkpoint`/`Compact`/`Close` and `ContentHash` with
  values a decoder would never produce, found six defects — three of them files
  a writer emitted that its own reader refuses — and one specification
  divergence that needs a version bump and is reported unfixed there.
- **Filesystem behaviour** — path traversal, symlinks on atomic rename,
  permission bits, temp-file naming. Its own `FREEZE.md` box, not this one.
- **Crash durability.** Its own box.
- **The extended fuzz session.** Its own box. `go test` runs only the seed
  corpora, which are by construction the inputs already known to be safe.
- **The `pile` package's provider surface and the CLI.** The matrix drives
  `format` directly. The provider adds caching, sidecars and a mover on top,
  and none of that was driven with hostile input here. The CLI now has
  `--max-decoded`, which is a resource ceiling rather than an audit: it bounds
  what a hostile file costs, and says nothing about whether the commands
  behave correctly when given one.
- **Dragonfly and gophertunnel below the decoder.** `nbtvalidate.go` exists
  because gophertunnel's NBT decoder allocates from declared lengths before
  reading and recurses per nesting level; `maxLayers` is 255 because
  dragonfly's sub chunk grows its storage slice with a comparison that wraps at
  256. Both are guards against a dependency, and both bound what this package
  hands down rather than fixing what is below it. Other paths into dragonfly
  that this package does not gate have not been reviewed.

---

## Negative controls

Each fix was disabled and the named test required to fail. `-count=1`
throughout; the file was restored and confirmed with `git diff` after each.

| # | production line disabled | test | result with it disabled |
|---|--------------------------|------|-------------------------|
| 1 | `format/blob.go`, `hint := min(n, maxPrealloc)` → `hint := n` | `TestHostileAllocationCeilings/solid/blob_table_count` | **FAIL** — 650,614,336 bytes, ceiling 50,331,648 |
| 2 | `format/palette.go`, `make([]parsedState, 0, min(n, maxPrealloc))` → `..., 0, n)` | `TestHostileAllocationCeilings/solid/block_palette_count` | **FAIL** — 35,821,032 bytes, ceiling 16,777,216 |
| 3 | `format/palette.go`, `hint := min(n, maxPrealloc)` → `hint := n` | `TestHostileAllocationCeilings/solid/biome_palette_count` | **FAIL** — 66,575,952 bytes, ceiling 16,777,216 |
| 4 | `format/indexed.go`, `make([]frameRef, 0, min(n, maxPrealloc))` → `..., 0, n)` | `TestHostileAllocationCeilings/indexed/palette_segment_count` | **FAIL** — 27,424,328 bytes, ceiling 8,388,608 |
| 5 | `format/structure.go`, the presence-bitset precheck → `if false && ...` | `TestHostileAllocationCeilings/structure/cell_grid` | **FAIL** — 8,393,320 bytes, ceiling 1,048,576 |
| 6 | `format/indexed.go`, `verifyRecords`'s buffer reuse → `make([]byte, e.length)` per record | `TestIndexedVerifyReusesOneBuffer` | **FAIL** — 70,118,216 bytes, ceiling 8,388,608 |
| 7 | `format/check.go`, `UnresolvedStates`' `defer w.mu.Unlock()` and closed check | `TestUnresolvedStatesHoldsTheLock` under `-race` | **FAIL** — race detected |

Finding 8 (`blobIndices`) has no control, for the reason given above.

The first control taken in that pass was run before the fix rather than after
it, which is the only order that proves the input was ever dangerous: the
2.42 GiB figure for the blob table is a measurement of the shipped code, not a
projection.

And the four validity tightenings, each disabled the same way:

| # | production line disabled | test | result with it disabled |
|---|--------------------------|------|-------------------------|
| A | `format/nbtvalidate.go`, the compound field's `if ct == tagCompound \|\| ct == tagList` charge → `if false && (...)` | `TestHostileNBTContainerBudget` | **FAIL** — a blob of 1,048,577 containers was accepted |
| A′ | `format/nbt.go`, `nbtCompound`'s `n.charge(1)` guard → `if false && (...)` | `TestNBTWriterHoldsTheContainerBudget` | **FAIL** — the writer emitted a value of 1,048,577 containers, which its reader refuses |
| B | `format/palette.go`, `if idx < prev` → `if false && idx < prev` | `TestHostileOverrideDeltaWraps` | **FAIL** — the wrapping chain was accepted |
| B′ | the same, and separately the independent walker's own wrap check in `format/vectorwalk_test.go` | `TestConformanceVectorsNegative/override_index_chain_wraps` | **FAIL** — with B, the reader accepted the vector; with the walker's check disabled, the walker accepted it |
| C | `format/decode.go`, `r.count(maxChunks, "chunk")` → `r.count(1<<32-1, ...)` | `TestHostileDecodedColumnCeiling` | **FAIL** — 4,194,305 columns were accepted by the count and refused later for a different reason |
| D | `format/indexed.go`, `finishDirectory`'s `w.recoveryLeft -= chunkN; w.recoveryLeft < 0` → `; false` | `TestRecoveryWorkIsBounded` | **FAIL** — recovery walked the whole candidate list |

And the dictionary bound:

| # | production line disabled | test | result with it disabled |
|---|--------------------------|------|-------------------------|
| E | `format/zstdpool.go`, `dictCacheEntries` 16 → 4096 and `dictCacheBytes` 16 MiB → 16 GiB | `TestDictCodecCacheIsBounded` | **FAIL** — 64 codecs held after 64 distinct dictionaries, bound is 16 |
| E′ | the same | `TestEvictedDictCodecStaysUsable` | **FAIL** — "the codec under test was never evicted; the fixture does not reach the case" |
| E″ | `format/zstdpool.go`, `decodeAll`'s `d.Close()` inserted before `DecodeAll` | `TestEvictedDictCodecStaysUsable` under `-race` | **FAIL** — "decoder used after Close" |

E and E′ are what a bound asserted against its own constant would not catch:
`TestDictCodecCacheIsBounded` asserts the literals 16 and 16 MiB and separately
requires the constants to equal them, and the eviction test installs an
absolute 64 dictionaries rather than a multiple of the bound. The first draft
of both did neither, and E was green.

The recovery measurement in item D was taken in both directions on one machine
— the bound disabled and then restored — rather than extrapolated, which is why
its table has a before column and an after column rather than a projection.

---

# The writer matrix

`FREEZE.md`'s pre-tag summary named this as **the largest remaining gap and the
one most likely to hold something**: the reader matrix drives bytes, and the
writers had never been driven at all. `format/hostilewrite_test.go` is that
matrix. It runs on every flagless `go test ./...` and under `-race`.

The inputs are Go values rather than bytes, which is what makes it a different
problem: a `WorldData` or a `StructureData` a caller can construct and no
decoder would ever produce. There is no truncation axis and no count field to
walk. What there is instead is §8's requirement that **a writer refuse content
its own reader would reject**, which had been found unimplemented for the NBT
container budget one pass earlier and had no systematic check anywhere else.

## What it covers

| test | what it drives |
|------|----------------|
| `TestHostileWriteShapes` | 36 world shapes through `WriteWorld`: nil and empty everything, duplicate positions, unaligned and out-of-domain vertical ranges, chunk coordinates at both `int32` extremes, 4,096 sections, 255 layers, air layers above and below populated ones, block entities and scheduled updates outside the column on every axis, unregistered runtime IDs, per-chunk collection counts one past their limit, sidecar entries naming states, sections and layers that do not exist, preserved states that collide after version normalisation, malformed metadata blobs. Each must be **refused**, or produce a file its own reader accepts **and** that re-encodes to the same bytes, twice. |
| `TestHostileWriteStructureShapes` | 16 structure shapes through `WriteStructure`: degenerate and inverted boxes, the axis ceiling and one past it, the cell ceiling and one past it, a cell grid disagreeing with the size, a 1×1×1 box whose cell is entirely padding, block entities outside the box and at one position, entities in the caller's order, sidecars on edge cells and on cells that do not exist. |
| `TestHostileWriteRejectsPositionsOutsideTheColumn` | Finding 1, in its three distinguishable consequences, through `WriteWorld` **and** `IndexedWorld.Store`. |
| `TestHostileWriteRejectsUnknownRuntimeID` | Finding 3, through `WriteWorld`, `WriteStructure` and `Store`, including that a refused `Store` leaves an empty world empty. |
| `TestHostileWriteStructureCellCeiling` | Finding 2, at the ceiling (must still write) and one row past it (must refuse). |
| `TestHostileWriteSidecarScanIsLinear` | Finding 4: `ContentHash` over a **155-byte** legal structure file of 1,048,576 cells and 4,096 preserved states, with a wall-clock ceiling. |
| `TestHostileWriteNilInputs` | Finding 5. |
| `TestHostileWriteNBTCeilings` | Each of §8's three NBT ceilings at the last accepted value and one past it, through `marshalNBT`, `validateNBT`, `unmarshalNBT` **and** a real block entity in a real file. Finding 6 lived in the gap between the second and the third. |
| `TestHostileContentHashOverNegativeVectors` | `ContentHash` over all 59 negative conformance vectors: no panic, no hang, and never success on a file the reader refuses. |
| `TestHostileContentHashIsAFixedPointOverFixtures` | `ContentHash` over every golden and positive vector, then over the file re-encoding produces: the two must agree, or a world's identity would depend on how many times it had been rewritten. |
| `TestHostileIndexedWriteSurface` | `Store`, `Checkpoint`, `Compact` and `Close` driven with the same shapes, requiring the file to reopen and every column to read back, and requiring a refused `Store` to leave no palette entry behind — checked through `UnresolvedStates`, because the projected content hash cannot see one. |

## What it found

Six. Every one is a writer defect; **none of them changes which files a reader
accepts**, and the goldens and both vector suites were green throughout with no
flags.

**1. A block entity or scheduled update outside the column it is stored in.**
A record packs a position's x and z into one byte of nibbles, so a position
outside the column's 16×16 footprint has no representation, and `validateColumn`
checked neither that nor the Y against the span the record declares. The gap
produced all three failure modes at once:

- **Silent relocation.** A chest at `(100,0,200)` in chunk `(0,0)` was written,
  and read back, as **`(4,0,8)`** — content changed, success reported. The
  provider's own test fixture was doing this: `testColumn` put a chest at
  `(3,1,5)` and several tests stored that column at chunk `(1,2)`, `(5,0)`,
  `(9,9)` and so on, where it silently became `(19,1,37)` and the rest.
- **A file the reader refuses.** Two block entities 16 apart in X fold onto one
  wire key; `ReadWorld` then refuses with "block entities are out of order or
  repeat a position". 4,290 bytes written, unreadable.
- **The same, on Y.** A block entity at Y 1000 in a chunk spanning 0..15, or a
  scheduled update at Y −500, were both written and both refused on read:
  "block entity at Y 1000 is outside the chunk's span 0..15".

Fixed in `validateColumn`, computed in `int64` because 16 × `MaxInt32` does not
fit an `int32`. `Store` and `Compact` share it.

**2. A structure whose cell grid passes §8's ceiling while every axis is legal.**
`validateStructureData` checked each axis against `maxStructureSize` and never
the product against `maxStructureCells`. `[1048576, 272, 16]` is legal on every
axis and needs **1,114,112 cells**, one row past the ceiling of 1,048,576 — and
only 8.9 MB of pointers, so a caller can hold it. `WriteStructure` produced
**143,477 bytes** and `ReadStructure` refused them outright. Fixed by calling
`structureCellCount`, the same function the decoder uses. A structure *at* the
ceiling still round trips, which the test asserts first.

**3. A block runtime ID the registry does not know.** `blockPaletteBuilder.add`
stored `minecraft:air` for it, with a comment saying it "cannot occur for chunks
produced by the same registry". A caller can put any `uint32` in a
`chunk.SubChunk`, and the consequences went past losing the block:

- The block became air, silently, and success was reported.
- A section holding **nothing else** resolved to uniform air, which §4.3 says an
  absent section is, so `ReadWorld` refused the 116-byte file the writer had
  just produced: "section 0 ends in an all-air layer, so it is either absent or
  shorter".
- Two scheduled updates differing only in an unknown block collapsed onto one
  wire key, which the reader also refuses.

Fixed in both palette builders — the solid one records the first bad ID and
reports it from `finalize`, the indexed one through the existing sticky
`paletteErr`.

This also uncovered a **vacuous test fixture**: `format/property_test.go`'s
`aliasWidthCase` builds its column against an aliasing registry and the boundary
matrix encoded it with the default one, where the second alias is simply an
unknown runtime ID. The alias folding the case is named for was being done by
the unknown-ID fallback. `boundaryCase` now carries the registry a case must be
written with.

**4. The sidecar-layer scan was quadratic, and `ContentHash` is the way in.**
Both writers folded the preserved-state sidecar into a map keyed by
`(section, layer)`, then asked that map — once per section, or once per
structure cell — how many layers that one section reached, by scanning every
key. The cost was cells × keys, and §8 bounds both factors at a million.

`ContentHash` decodes and re-encodes, so a legal file is enough to reach it.
Measured on one machine, on files assembled from bytes:

| file | cells | preserved states | `ContentHash` |
|------|-------|------------------|---------------|
| **159 bytes** | 1,048,576 | 64 | 2.6 s |
| **153 bytes** | 1,048,576 | 256 | 16.7 s |
| **154 bytes** | 1,048,576 | 1,024 | 44.9 s |
| **155 bytes** | 1,048,576 | 4,096 | **2 m 4 s** |

Linear in the key count on a file whose size does not move, which is the shape.
The states are bounded by the decoded-storage ceiling rather than by anything in
these files, so the row above is not the end of the table.

Fixed by folding the keys once into a max-layer-per-section map: same answer,
one pass. The 155-byte file now takes **149 ms**, and
`TestHostileWriteSidecarScanIsLinear` holds a 10-second ceiling over it — three
orders of magnitude under the before, because what is guarded against is
quadratic growth and not ten per cent.

The world path has the same shape and is bounded lower by §8 (4,096 sections
rather than 1,048,576 cells): 2,048 sections and 2,047 preserved states took
303 ms. It is fixed by the same change.

**5. `WriteWorld(out, nil, …)` and `WriteStructure(out, nil, …)` panicked**
with a nil pointer dereference. Both return an error now.

**6. An NBT string of 32,768 bytes or more is written and cannot be read back.**
Found by the matrix, not by inspection. §1 says NBT string lengths are a `u16`
and §8's table says 65,535; `marshalNBT` and `validateNBT` both agree with that.
`unmarshalNBT` does not: gophertunnel's little-endian decoder reads the length
as a **signed int16**, so every length from 32,768 up arrives negative and the
blob is refused with "unexpected buffer end during op: 'String'". Since every
block entity and entity blob is read back through `unmarshalNBT`, a caller with
a 32,768-byte string in a block entity got a file `ReadWorld` refuses — a 69,780
byte blob was enough. Compound **keys** have the same boundary.

The writer now stops at 32,767 (`maxNBTStringWrite`). **The divergence itself is
not fixed and cannot be**: see below.

## What needs a version bump, and is therefore reported and not fixed

**The specification states an NBT string ceiling this implementation's reader
does not honour.** §1 ("string lengths `u16`") and §8's limits table (65,535)
both say 65,535. The reader refuses 32,768..65,535 inside any NBT blob. That is
a **spec/implementation divergence**, and it is exactly the kind the
specification's own concession — where prose and implementation disagree, the
implementation wins — was written to cover, except that no vector exercises it.

Closing it in either direction is a version bump:

- **Widening the reader to 65,535** changes which files are accepted, which
  `FREEZE.md` calls a validity change outright.
- **Narrowing §8's stated ceiling to 32,767** for NBT strings tightens a stated
  rule, moves `spec_rules.txt`, and would make a conforming second
  implementation's files invalid retroactively.

**How a caller reaches it:** put a string of 32,768 bytes or more in a block
entity's or an entity's NBT — a long sign text, a serialised inventory, a
book — and save. Before this pass the file was written and would not load. It is
now refused at write time, so no new file can carry one; a file **written by a
second implementation** that followed §1 still will not load here, and that is
the part only a version bump can change. `TestHostileWriteNBTCeilings` pins the
boundary in all three directions so the divergence cannot move silently.

**What would become invalid** if the ceiling were narrowed to 32,767 in the
specification: any file, from any implementation, carrying an NBT string or
compound key of 32,768..65,535 bytes. No file this implementation has ever
written can be one, because its reader has always refused them.

## API behaviour changes: writers that now refuse input they used to accept

None of these changes any file's readability — every input listed produced
either a file the reader already refused, or content the writer had silently
altered — but each is a behaviour change for a caller.

| surface | now refuses |
|---------|-------------|
| `WriteWorld`, `IndexedWorld.Store`, `Compact` | a block entity or scheduled update whose X or Z lies outside the column, or whose Y lies outside the chunk's range |
| `WriteWorld`, `WriteStructure`, `IndexedWorld.Store` | a block runtime ID the registry does not know |
| `WriteStructure` | a size whose cell grid exceeds `maxStructureCells`, however legal each axis is |
| `WriteWorld`, `WriteStructure` | a nil `*WorldData` / `*StructureData` (previously a panic) |
| every writer, through `marshalNBT` | an NBT string or compound key of 32,768 bytes or more |

`WriteStructure` also now **documents** that it mutates its argument: it clears
the padding of edge cells in place, which `WriteWorld` explicitly does not do to
chunks. That was true before and undocumented, and it means a `StructureData`
shared with another goroutine must not be written concurrently.

## What the writer matrix does not reach

- **A `WorldData` at the 4,194,304-column ceiling.** The count check is in
  `validateWorldData` and the shape one past it is 671 MB of `Column` structs
  before a single chunk exists, so it is not driven. The reader-side ceiling is
  held by `TestHostileDecodedColumnCeiling`.
- **`Compact`'s dictionary training path under hostile input.** It samples
  record bodies the writer produced, so its input is bounded by everything
  above; nothing here drives a hostile dictionary into it.
- **The `pile` provider surface and the CLI**, which remain as `FREEZE.md` says.

## Negative controls

Every check above was disabled and the named test required to fail. `-count=1`
throughout; each file was restored from a copy taken before the run and the tree
confirmed clean afterwards.

| # | production line disabled | test | result with it disabled |
|---|--------------------------|------|-------------------------|
| W1 | `format/encode.go`, `validateColumn`'s `inColumn` → `return nil` first | `TestHostileWriteRejectsPositionsOutsideTheColumn` | **FAIL**, all five cases: 4,259 / 4,260 / 4,234 / 4,246 / 4,235 bytes accepted |
| W2 | the same | `TestHostileWriteShapes` | **FAIL** on the three position shapes |
| W3 | `format/structure.go`, `validateStructureData`'s `structureCellCount` call removed | `TestHostileWriteStructureCellCeiling` | **FAIL** — "the writer emitted 143,477 bytes its own reader refuses: structure size [1048576 272 16] too large (1114112 cells, limit 1048576)" |
| W3′ | the same | `TestHostileWriteStructureShapes/cell_grid_one_row_past_the_ceiling` | **FAIL** — 139,343 bytes accepted |
| W4 | `format/palette.go`, `blockPaletteBuilder.add`'s `b.err` assignment removed | `TestHostileWriteRejectsUnknownRuntimeID` | **FAIL** — world 4,230 bytes accepted; the section-only case 116 bytes accepted, which `ReadWorld` refuses; structure 4,211 bytes accepted |
| W4′ | the same | `TestHostileWriteShapes`, `TestHostileWriteStructureShapes` | **FAIL** on all three unregistered-ID shapes |
| W5 | `format/indexed.go`, `addBlock`'s `noteErr` → `name, props = "minecraft:air", nil` | `TestHostileWriteRejectsUnknownRuntimeID/indexed_Store` | **FAIL** — "Store accepted an unregistered runtime ID" |
| W6 | `format/encode.go`, `sidecarLayerCounts` restored to the per-section scan at both call sites | `TestHostileWriteSidecarScanIsLinear` | **FAIL** — **1 m 12 s** on the 155-byte file, ceiling 10 s (72 s rather than the 124 s first measured because the fix also removed a redundant re-scan; the shape is the same) |
| W7 | `format/encode.go` and `format/structure.go`, both nil guards removed | `TestHostileWriteNilInputs` | **FAIL** — "WriteWorld(nil) panicked: invalid memory address or nil pointer dereference" |
| W8 | `format/nbt.go`, `maxNBTStringWrite` 1<<15−1 → 1<<16−1 | `TestHostileWriteNBTCeilings` | **FAIL** — the 32,768-byte case emitted a blob `unmarshalNBT` refuses; the 65,535-byte case emitted 65,545 bytes the reader refuses |
| W9 | `format/palette.go`, `normaliseStateVersion`'s fold removed | `TestHostileContentHashIsAFixedPointOverFixtures` | **FAIL** — `golden_world_props.pile`: "the re-encoding does not decode: palette version override equals the palette's own version" |
| W10 | `format/encode.go`, `extractColumnRaw`'s trailing-all-air-layer drop removed | `TestHostileWriteShapes` | **FAIL** on two shapes — "the writer emitted a file its own reader refuses: section 0 ends in an all-air layer" |
| W11 | `format/indexed.go`, `Store`'s `restorePalettes` wind-back removed | `TestHostileIndexedWriteSurface` | **FAIL** — "a refused Store left 1 unreferenced palette entries behind: [pile:admitted_first]" |
| W12 | `format/check.go`, `panic("control")` inserted in `ContentHash` | `TestHostileContentHashOverNegativeVectors` | **FAIL** on the three structure vectors — "ContentHash panicked" |

**W11 was green on the first attempt, and the repair is the point.** The test
compared a content hash projected from the world's live columns before and after
the refused stores. A leftover palette entry is not in any column, so the hash
could not see it — and worse, every shape in the list is refused by
`validateColumn`, which runs before a single palette entry is touched, so none
of them could have left anything behind whatever the assertion was. The test now
also stores a column that gets one preserved state admitted and fails on a
second (invalid UTF-8), and asserts on `UnresolvedStates`, which reads the
persisted palette segments. That is the version W11 turns red.

**W12 is the honest one.** `TestHostileContentHashOverNegativeVectors` has no
distinguishing input among the vectors themselves: every negative vector is
refused by a reader before `ContentHash` reaches a writer, so no reader rule one
could disable would let one through into an encoder. It is a no-panic, no-hang
guard over 59 inputs, and W12 proves only that the guard reports a panic rather
than aborting the run. It is recorded here rather than presented as enforcement.
