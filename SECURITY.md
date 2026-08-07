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

`zstdpool.go` memoises `dictCodec`s in a process-wide map keyed by the
dictionary's content and length. A hostile indexed file can carry any
dictionary up to `maxDictLen` (1 MiB), and opening it installs a parsed
decoder for that dictionary which is **never evicted**. Opening a thousand
files with a thousand distinct dictionaries pins a thousand of them for the
life of the process.

This is documented in `zstdpool.go` as deliberate — the map "grows with the
number of distinct dictionaries a process meets (one per compacted file), not
with the number of handles open on them" — and that reasoning is right for
files the operator wrote. It is not right for files an attacker supplies. The
fix is an LRU on `dictCodecs`, and it is safe (evicting only means the next
open rebuilds; a handle still using a codec keeps it alive through its own
reference). It was not made in this pass because it is a change to shared codec
lifetime rather than to a decode path, and it belongs with whoever owns the
memory work.

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
- **The `pile` package's provider surface and the CLI.** The matrix drives
  `format` directly. The provider adds caching, sidecars and a mover on top,
  and none of that was driven with hostile input here.
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

The recovery measurement in item D was taken in both directions on one machine
— the bound disabled and then restored — rather than extrapolated, which is why
its table has a before column and an after column rather than a projection.
