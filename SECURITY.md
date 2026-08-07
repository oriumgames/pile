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

- **The writer paths.** This pass covers decode. `ContentHash` round-trip
  behaviour over hostile-but-legal input is still open, as
  `FREEZE-BLOCKERS.md` records.
- **Filesystem behaviour** — path traversal, symlinks on atomic rename,
  permission bits, temp-file naming. Its own `FREEZE.md` box, not this one.
- **Crash durability.** Its own box.
- **The extended fuzz session.** Its own box. `go test` runs only the seed
  corpora, which are by construction the inputs already known to be safe.
- ~~**The `pile` package's provider surface and the CLI.**~~ Done; see
  "The provider surface and the CLI" below and `HARNESS.md` §8.
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

# The provider surface and the CLI

The pass above drives `format.ReadWorld`, `format.ReadStructure`,
`format.ReadMeta` and `format.OpenIndexed`. This one drives `pile.Open`, every
`world.Provider` method, and every `cmd/pile` subcommand, against hostile files
on disk and hostile command-line arguments. It exists because the layer a caller
actually calls is the layer a hostile file actually meets, and because the
provider adds column caching, the preserved-state sidecar, snapshots, the mover
and template handling on top of an audited decoder.

`hostile_test.go` and `cmd/pile/hostile_test.go` are the subjects, and
`HARNESS.md` §8 records twenty-nine negative controls, all red, including four
that were green when first written.

Every file used here is one a conforming reader **must accept**. That is the
point: broken bytes are what the matrix above already covers, and a legal file
whose content is absurd is what arrives.

## What it found

### 1. `pile render` reserved 2.0 TiB from a 4,269-byte file

`cmd/pile/render.go` sized the image from `int32(maxCX-minCX) + 1`. The
difference of two chunk coordinates does not fit an `int32`: a world holding one
column at X = −2,147,483,648 and one at X = 0 has a true span of 2³¹+1, which as
an `int32` difference is −2,147,483,648, so the width came out at
**−34,359,738,352** — not greater than 8,192, so the "world too large to render"
test passed it. `image.Rect` canonicalises a negative rectangle into a positive
one, so `image.NewRGBA` then asked for four bytes per pixel of a
34,359,738,352 × 16 image.

- **Before:** a **4,269-byte** file holding two chunks grew the heap by
  **2,199,023,190,016 bytes (2.0 TiB)** and then failed inside the PNG encoder
  with "invalid image size". On a machine that cannot reserve two terabytes of
  address space it is a fatal out-of-memory rather than an error.
- **After:** refused before anything is allocated — "world too large to render
  (34359738368x16 blocks)" — with the measured heap growth under 1 GiB, which is
  what the test asserts.
- The span is now computed in `int64` and narrowed only after the ceiling test.
  Nothing about which worlds render changed: a 512-chunk-wide world still
  renders and a 513-chunk-wide one still does not, which the test pins from both
  sides.
- Control `C1`; the "not tightened" half is `C2`.

### 2. `pile extract --max 4294967296,0,0` never returned

`ExtractStructure` computed its size as `int32(hi.X() - lo.X() + 1)`, in `int`
and then truncated. A span of 2³²+1 became a size of **1**:
`format.NewStructureData` accepted the 1×1×1 structure, and the chunk loop
underneath it then walked the *real* span — 268,435,457 positions on one axis,
each one a `LoadColumn`.

- **Before:** did not return. Measured at 40 s and 50 s in two harness runs
  before the timeout fired; the arithmetic says minutes to hours depending on the
  axis chosen, and a caller passing both X and Z waits ~7×10¹⁶ iterations.
- **After:** refused — "region axis 0 spans 4294967297 blocks (0..4294967296),
  which no structure can hold".
- The box is now validated in exact arithmetic (`regionSize`) before anything is
  allocated, an inverted box is named as inverted rather than silently producing
  a negative size, and a box outside the addressable chunk range is refused
  because the loop's keys are `int32`. Every later bound falls out of that: once
  the size is the box it came from, `NewStructureData`'s cell ceiling bounds the
  loop at ~2²⁰ iterations.
- Controls `P12` (library) and `C3` (CLI).

### 3. `Provider.Rollback` destroyed the world when the snapshot was not one

`snapshots/` travels inside a world directory somebody hands you, and `Rollback`
is the operator-facing grief-recovery command. It deleted the world's dimension
files, renamed the snapshot's copies over them, and **only then** tried to read
the result.

- **Before:** a snapshot directory holding `overworld.pile` with 22 bytes of
  text replaced a working world with it. `Rollback` returned an error, the
  provider was left holding no columns, and the *directory* would not open
  again — the world was gone from disk, permanently, with the only copy
  overwritten.
- **After:** the current files are parked as `<dim>.pile.rollbackold` rather
  than deleted, and if the restored world does not load, every rename is undone,
  the provider is reloaded from the originals and the error names the snapshot:
  "rollback shipped: snapshot does not load; the world is unchanged". The test
  compares the world file byte for byte before and after, reopens the directory,
  and requires no staging file to be left behind.
- A second shape went with it: `copyFile` opens the source, and `os.Open` on a
  **FIFO blocks until somebody writes to it**, so a named pipe called
  `trap.pile` in a snapshot directory hung `Rollback` forever. Snapshot entries
  that are not regular files are now skipped. Control `P9` reproduces the hang.
- Controls `P8`, `P9`.

### 4. The decoded-column cache was bounded by count and not by size

`CacheColumns(n)` built an LRU bounded by entry count alone, documented as "its
entries are whole columns and all of a size". That is true of columns a server
wrote and false of a file somebody sent you: §8 permits **1,048,576 entities in
one column**, plus a 16 MiB user data blob and a sidecar with one entry per
block position. The metadata cache beside it had had a byte budget all along.

- **Before:** 64 columns of 1,048,576 entities each were all held — **8,590,219,520
  bytes** of charged weight, unbounded in the constant.
- **After:** one entry, **134,222,180 bytes**, against a 256 MiB budget. (The LRU
  never evicts the entry just stored, so the bound is "the budget, plus one
  entry"; that is deliberate and documented at `lru.Cache.evict`.)
- An ordinary working set is untouched: 64 real columns still all fit, which the
  test asserts as its other half.
- Control `P11`.

### 5. `pile origin --set` reported an anchor it had not set

The paste anchor is three `int32`s on the wire, and `--set` takes three
integers. A value that did not fit was narrowed silently: `--set
4294967296,0,0` rewrote the structure file, set the anchor to 0, and printed
`origin: [0 0 0] -> [0 0 0]`. Now refused, with the file untouched. Control
`C9`.

### 6. `Structure.PasteInto` wrote columns at wrapped coordinates

The same shape one layer down. `PasteInto` builds its chunk keys with
`int32(wx >> 4)`, so a paste position outside the addressable range was narrowed
without comment: the structure landed at coordinates nobody asked for, and two
columns of one structure could collide onto a single key, which is content lost
with no error anywhere. Both the position and the position-plus-origin are now
checked against ±(2³⁵−1), the last block coordinate that maps onto a distinct
`int32` chunk key. Control `C10`.

### 7. `pile compact` called a file canonical without reading it

`pile.FileMode` checks the `PILE` magic and returns the mode byte; it validates
nothing else. `cmdCompact` used it alone, so twelve bytes of `PILE` followed by
zeroes were reported as `solid file, already canonical` — a claim about a file
nobody had read. It now reads the metadata block first. Control `C6`.

### 8. There was no `--max-decoded`

The root `readme.md` tells a reader to reach for `pile.MaxDecodedBytes` if they
open worlds they did not write, and the CLI is the first thing such a world is
pointed at. It had no such dial: `verify`, `stats`, `check`, `inspect`, `render`,
`diff`, `patch`, `apply`, `export`, `import`, `extract`, `paste`, `origin`,
`prune`, `move`, `upgrade`, `mode`, `compact` and `convert` all decoded with the
format's own ceilings and nothing else.

`--max-decoded n` is now on every one of them, and it reaches the reader:
`TestCommandsHonourTheDecodeCeiling` requires eleven commands to refuse a
4,096-column world at a 64 KiB ceiling with `format.ErrDecodeBudget` and *not*
`ErrCorrupt`, and requires the same world to pass with no ceiling. Controls `C7`
and `C7b` — one for the codec options, one for the provider options, because a
single control would have left half the commands unproved.

The library gained the same reach: `LoadWorldFiles` takes `...Option`,
`MoveOptions` has a `MaxDecoded` field, and `LoadStructure` has
`StructureMaxDecodedBytes`. Before this, `MaxDecodedBytes` existed on `Open` and
nowhere else, so every offline tool path bypassed it.

## Found here, fixable only in `format`

**`format.MarshalNBT` produces a blob its own `format.UnmarshalNBT` refuses, for
any string of 32,768 bytes or more.** §8 puts `maxStringLen` at 65,535 and the
marshaller accepts up to that, but the Bedrock NBT encoding underneath writes a
string's length as a *signed* 16-bit value, so at 2¹⁵ the length wraps negative
and the decoder reports "unexpected buffer end during op: 'String'". The
boundary is exact: 32,767 round-trips, 32,768 does not.

Through this package that is **data loss with no error anywhere**, reproduced:

```go
p, _ := pile.Open(dir)
col := /* any column */
col.BlockEntities[0].Data["long"] = strings.Repeat("b", 32768)
p.StoreColumn(pos, world.Overworld, col)   // no error
p.Close()                                   // returns nil: the file is written
_, err := pile.Open(dir)
// "corrupt file: decode nbt: nbt: unexpected buffer end during op: 'String'"
```

The world's settings `Name` and the markers blob reach the same marshaller and
are **not** lost, but only by accident: §7.1 and §7.2 make the writer re-decode
those two blobs to check their schema, so the round trip fails at `Close` with a
confusing message instead of at the next `Open` with a dead world. A block
entity's or an entity's NBT has no such schema and goes to the wire straight
from the marshaller — the same asymmetry item A above found for the container
budget, in a rule item A did not cover.

**It is not fixed here, deliberately**, because the fix is a writer-side change
in `format/nbt.go` — the marshaller must refuse what its own reader will — and
this pass does not edit `format`. It needs no byte to move and no file's
validity to change: every blob it would newly refuse is one no reader ever
accepted. `TestLongNBTStringsRoundTrip` holds the boundary that works, so the
day the limit moves in either direction a test says so; control `P14`.

## What a caller still cannot bound

**This is the most important line in this document for somebody loading files
other people send them.**

`MaxDecodedBytes` charges decoded columns at 1,024 bytes and decoded section
storages at 128. It charges **nothing** for entities, block entities or
scheduled updates. §8 bounds those per chunk — 1,048,576 each — and the column
ceiling multiplies them rather than bounding them, so the product is not bounded
by anything a caller can set.

Measured, through `pile.Open`, on the machine this pass ran on:

| file | ceiling set | charged | result |
|---|---|---|---|
| 2 columns × 1,048,576 entities, **4,764 bytes** | `MaxDecodedBytes(64 KiB)` | 2,048 of 65,536 | accepted; **773,708,104 bytes retained**, 11,806× the ceiling |
| 4 columns × 1,048,576 entities, **9,409 bytes** | `MaxDecodedBytes(64 MiB)` | 4,096 of 67,108,864 | accepted; **1,547,408,096 bytes retained**, ~7 s |

The only ceiling that refuses the second file is one below 4,096 bytes — a world
of three columns — so **there is no setting that both admits a real world and
refuses this one**. Within the rules the shape scales to the 512 MiB body
ceiling: an entity is five bytes on the wire at minimum, so about 10⁸ entities
and tens of gigabytes are reachable from a file that fits in a network packet
once zstd has done its work.

`TestEntityFloodEscapesTheDecodeCeiling` asserts this, as a characterisation
rather than as a guard: it requires the file to be *accepted* and requires the
decode to cost far more than the ceiling permitted. When somebody charges the
per-chunk collections it will go red, and its failure message says so.

**What closing it would take.** `storageBudget` in `format/decode.go` already
has the shape: it charges columns and storages at two constants, and adding
`chargeEntities`, `chargeBlockEntities` and `chargeTicks` at the three
`r.count(maxPerChunk, …)` sites is the same edit three more times. Two things
make it more than a patch, which is why it is reported rather than done:

1. It is in `format`, which this pass does not edit.
2. The **default** ceiling must stay unreachable by a conforming file, or the
   change stops being a policy dial and becomes a validity rule — the thing
   `SECURITY.md` item E was careful not to do. §8 does not bound the product of
   columns and per-chunk collections, so the default would have to be raised to
   cover the worst case the format permits (roughly 4,194,304 × 1,048,576 ×
   whatever an entity is charged), and the useful part of the change is entirely
   in what a *caller* then sets. That is a decision for whoever owns §8, not a
   refactor.

Until then, the honest advice is the one in the recipe below: a caller that
opens foreign worlds must treat a decode as something that can cost tens of
gigabytes, and must bound it from outside the process.

## Loading a file somebody sent you

A recipe, and what it does and does not buy. It has been run: the shapes below
are `hostile_test.go`, and the numbers are measurements rather than estimates.

### The recipe

```go
p, err := pile.Open(dir,
    pile.ReadOnly(),                 // 1
    pile.MaxDecodedBytes(64<<20),    // 2
    pile.CacheColumns(0),            // 3
)
```

**1. `pile.ReadOnly()`.** Not only "do not save": every mutator on the provider
becomes a no-op, `Snapshot`, `Rollback` and `DeleteSnapshot` return
`ErrReadOnly`, and no path writes into the directory. It is what stops a world
you are inspecting from being rewritten in the format this build happens to
produce, and it is what stops `Rollback` from being reachable at all.

**2. `pile.MaxDecodedBytes(n)`, sized to what your own worlds cost.** 1,024
bytes per column is the model, so 64 MiB admits about 65,536 columns — far more
than a lobby and far less than the format's own 4,194,304. A file refused under
it fails with `format.ErrDecodeBudget`, which does **not** wrap
`format.ErrCorrupt`: it is not a claim the file is invalid, and a pipeline that
quarantines corrupt files must not quarantine this one.

**3. `pile.CacheColumns(0)`** — the default — unless you need it. The cache is
bounded by count *and* by 256 MiB of weight now, but zero is zero.

**Do not pass `pile.AppendMode()`** for a file you are only reading. It opens
the dimension file `O_RDWR` and holds it for the provider's lifetime, and a
solid file is refused by it anyway.

**On the command line**, the same dial: `--max-decoded`, on every subcommand
that decodes chunk content.

```sh
pile inspect  suspect/overworld.pile                      # header only, no chunks
pile verify   suspect --max-decoded 67108864              # full decode, bounded
pile stats    suspect --max-decoded 67108864
```

`pile inspect` is the cheapest first look: it reads the header and the metadata
block and decodes no chunks. `pile check` reads the block palette only.

### What `LoadSkip` does and does not do

`pile.LoadSkip(pile.SkipEntities|…)` is **not** a bound and must not be used as
one. It drops categories from the column `LoadColumn` hands back — after the
file has been decoded, with every entity already built and, in solid mode, still
held by the provider for its lifetime. It is a convenience for template worlds
whose entities are spawned by code. It removes nothing from the peak, and it
removes nothing from what is retained.

The same is true of `pile.Skip(…)`, which applies on *store* and so only affects
what a later save writes.

### What the recipe holds

Proved, not assumed:

- A 4,096-column file is refused at a 256 KiB ceiling, with
  `errors.Is(err, format.ErrDecodeBudget)` true and
  `errors.Is(err, format.ErrCorrupt)` false
  (`TestOpenHoldsTheCeilingOnAHostileColumnFlood`, control `P1`).
- A world whose overworld is truncated and whose nether is fine fails as a
  whole, names the file, and returns no provider — no partial world is served
  and no save writes the missing dimension back
  (`TestOpenRefusesAHostileDimensionAndNamesIt`, control `P3`).
- An unreadable dimension file is an error naming it, not an empty world
  (control `P4`).
- A column declaring a vertical range that is not the dimension's is re-based
  before it reaches a caller, so dragonfly's unchecked sub-chunk indexing cannot
  be reached through it (control `P5`).
- Settings and markers that are legal but absurd — a 32,767-byte world name,
  spawn at both `int32` extremes, 20,000 markers, NaN and ±Inf positions — load,
  survive a save, and reload identically (controls `P6`, `P6b`).
- An indexed file rewritten underneath an open provider produces errors from
  every method that touches it, and `Columns` reports through `IterError` rather
  than iterating short — a short iteration is how a backup silently loses chunks
  (control `P7`).
- Concurrent `LoadColumn`, `StoreColumn`, `Columns`, `ChunkUserData` and `Save`
  over a world of large columns, in both modes, under `-race`, and the world
  reopens with every column intact (control `P13c`).
- Every column crossing the provider boundary is a copy, on all three load paths
  (controls `P13`, `P13b`).
- Every subcommand refuses five shapes of garbage rather than reporting success
  (control `C6` found one that did not).

### What it does not hold

- **The per-chunk collections.** See "What a caller still cannot bound" above.
  This is the gap, and no setting closes it.
- **Wall-clock time.** Nothing bounds how long a decode takes. The 9,409-byte
  file above takes about seven seconds. Do not decode foreign files on a
  request path, and do not decode them unbounded in parallel.
- **Transient allocation inside an NBT blob.** `MaxDecodedBytes` does not charge
  it; §8's container budget bounds it at its own ceiling, which `ReadMeta` can
  spend ~82 MiB against from a 132-byte input.
- **Authenticity.** xxHash64 is not a MAC. A checkpoint hash says the file has
  not been damaged, not that it is the file you were sent. If that matters, sign
  it.
- **A second process.** A world directory is assumed to have one owner;
  `FSBEHAVIOUR.md` §5.
- **The operating system.** The only complete bound on a decode of a foreign
  world is an external one — a memory cgroup, `RLIMIT_AS`, a separate process
  you are willing to have killed. For the stated use case, loading maps
  strangers send you, that is the honest recommendation and this document would
  be wrong to imply the library can replace it.
