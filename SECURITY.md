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

**Resource use is bounded but not small.** §8's ceilings are set at what the
underlying models can represent, not at what a deployment expects. A legal
solid body is up to 512 MiB decompressed and a legal indexed directory names up
to 4,194,304 chunks, and zstd will deliver either from a file of a few
kilobytes. The measured consequences are in "What remains" below. A process
that decodes files it did not write should bound its own concurrency and treat
a decode as an operation that can cost hundreds of megabytes and seconds of
CPU, because a hostile file will make it do exactly that within the rules.

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
| `TestHostileNBTContainerBudget` | The §8 container budget at its limit and one past it, and the case it does not cover. |
| `TestHostileOverrideDeltaWraps` | The one integer computation that wraps before its bounds check. |

Two of those tests assert that a hostile input is **accepted**. They are
characterisation tests for findings recorded below as format changes: if they
start failing, someone has decided to make the change, and this document is out
of date.

### Method

Every fix below was proved the way `HARNESS.md` proves a canonicality check:
disable the production line, run the named test, require it to fail, restore
and confirm with `git diff`. The recorded results are in "Negative controls".

Every allocation finding is proved by a file of a few kilobytes with the
measured allocation before and after. A claimed bound with nothing
demonstrating it is what this project keeps having to redo.

---

## What it found

Nothing below changes which files a reader accepts. Every fix bounds an
allocation to what the input can actually justify, or moves work behind a check
the input has to pass anyway. The golden and vector suites were green
throughout with no `-update` and no `-format-change`.

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

## Format changes: found, not fixed

Two findings cannot be fixed without rejecting a file this reader accepts
today. Under the freeze that is a validity change and therefore a format
change, so both are reported rather than made. Each says precisely which files
would become invalid.

### A. The NBT container budget does not charge compounds nested in compounds

§8 states: *NBT containers per blob (compounds and nested lists) — 1 048 576*,
and *decoders MUST therefore bound the result as well as the input*.

`nbtvalidate.go` charges the budget for the elements of a list whose element
type is a compound or a list, which is the amplification it was written for: an
empty compound inside a list is one byte of `TAG_End` and a whole Go map. It
does **not** charge a compound that is a field of another compound. Such a
compound costs six bytes on the wire — tag, a two-byte name length, a
distinct name (keys must strictly ascend, so they cannot repeat), and
`TAG_End` — and also becomes a map.

Measured: a blob of **2,097,152 sibling compounds**, twice the stated ceiling,
is accepted. It is 14,680,068 bytes, within the 16 MiB blob limit, and decodes
into 2,097,152 maps at a cost of **461 MiB allocated, 265 MiB retained**. The
list-shaped equivalent is refused at 1,048,577.

- **What would become invalid:** any file carrying an NBT blob — settings,
  markers, border, stats, a block entity, an entity, or a structure's — that
  decodes into more than 1,048,576 containers where the excess comes from
  compounds nested inside compounds rather than from list elements.
- **Could this version have written one?** Only from a runtime value that
  already had that shape: the writer checks nesting depth but not container
  count, so `WriteWorld` would emit such a blob if handed one. No file this
  project has produced in testing has more than a handful.
- **Note** that `FREEZE.md`'s own "After the freeze" section permits
  "additional validation that rejects only files this version never wrote"
  without a version bump. Whether this qualifies turns on whether a writer that
  *can* emit such a file counts as one that wrote one. That is a decision for
  whoever owns the freeze, not for this pass.
- Pinned by `TestHostileNBTContainerBudget`, which fails if the gap is closed.

### B. The version-override delta chain wraps before its bounds check

§3.1's sparse version-override table carries index deltas as uvarints, and
`parseStatePalette` accumulates them in a `uint64`:

```go
idx := prev + delta
if idx >= uint64(len(entries)) { ... }
```

The bounds test catches an index past the palette, so nothing here is
memory-unsafe. But the sum is modular, and a uvarint can express the modular
representative of a *negative* step. After an override at index 5, a delta of
2^64−2 lands on index 3. §3.1 says the indices strictly ascend; the decoder
enforces that only through "every later delta MUST be non-zero", which is
sufficient exactly as long as the sum cannot wrap.

The consequence is a second encoding of one palette — the thing the canonical
form exists to prevent — reachable from a legal-looking file, and it is the one
place in the decode paths where an integer computation on an input-derived
value wraps before its bounds check.

The other two delta chains in the format do not have this shape. A record's
chunk position and a directory entry's frame offset both accumulate in an
`int64` from signed varints and range-check every step; there the modular
representative of a wrap lands next to the bottom of `int64`, nowhere near a
legal value, which is what `FREEZE-BLOCKERS.md` item 6 records.

- **What would become invalid:** any file whose block palette (or indexed
  palette segment) has two or more version overrides where a later delta
  exceeds `2^64 − 1 − prev`, i.e. where the running index wraps. No conforming
  file has one: the deltas are differences between ascending indices bounded by
  1,048,576.
- **Could this version have written one?** No. The writer emits deltas between
  sorted indices.
- The rule is already in the specification as a layout-table annotation
  ("indices strictly ascending") rather than as a MUST sentence, which is why
  `TestEveryRuleIsClaimed` never pinned it — the exact gap `FREEZE-BLOCKERS.md`
  names under "Only sentences containing MUST are pinned".
- Pinned by `TestHostileOverrideDeltaWraps`, which fails if the gap is closed.

---

## What remains

Everything here is bounded — no unbounded loop, no unbounded allocation — and
several of them are bounded at a number large enough to matter. They are not
defects in the sense of the three `FREEZE.md` boxes this pass owns; they are
the cost of the ceilings §8 sets, and lowering any of them is a format change.
They are written down because a caller needs them to size its own limits.

### The decompression ratio is the amplifier

zstd will deliver 512 MiB of body from a few hundred bytes, and the §8 ceilings
bound the body, not what the body decodes into. There is a ceiling on decoded
section storages (`maxDecodedStorages`), on NBT containers per blob, and on the
recovery chain — §8 says those three exist for exactly this reason — and there
is none on decoded chunk records, block entities, entities or scheduled
updates.

Measured, after the fixes above:

| input | file | result |
|-------|------|--------|
| A solid world of 1,048,576 records, each 11 bytes and holding nothing | **1,161 bytes** | 2.79 GiB allocated, **1.04 GiB retained**, 1.2 s |
| An indexed directory at the 4,194,304-entry ceiling | **9,206 bytes** | 693 MiB allocated, **320 MiB retained**, 6.2 s |
| A settings blob of 1,048,576 NBT compounds, exactly at the budget | **132 bytes** | 82 MiB allocated transiently by `ReadMeta` |

The first is the one to know about: an eleven-byte chunk record is legal — a
record must declare at least one section but need not mark any present — and it
becomes a `recRaw`, a `chunk.Chunk` and a `Column`, about a kilobyte of live
objects. Bounding it needs a ceiling on decoded columns, which §8 sets at
4,294,967,295, and lowering that is a format change.

### Recovery work is the product of two limits

`adoptCheckpoint` collects up to 64 footer candidates by scanning backwards and
then follows each candidate's `prev` chain to a total of
`maxCheckpointChain = 256`, and every candidate whose directory parses is
loaded in full: the directory frame is decompressed, every entry is parsed into
the map, and records are verified until one fails. §8 bounds the chain at 256
and a directory at 4,194,304 entries, separately. Their product is not bounded.

Measured, with a directory at the entry ceiling and every record's hash wrong
so no candidate is adopted:

| footers | file | elapsed | allocated |
|---------|------|---------|-----------|
| 1 | 9,205 bytes | 4.1 s | 726 MB |
| 4 | 9,337 bytes | 14.9 s | 2.9 GB |
| 16 | 9,865 bytes | 53.2 s | 11.6 GB |

Linear in the candidate count. Extrapolated to the full 256, a file of about
**20 KB makes `OpenIndexed` run for roughly fourteen minutes** and churn
something near 186 GB — transient, so it is CPU and collector pressure rather
than a heap that cannot be satisfied, but it is a denial of service from a file
that fits in a network packet.

Forging the candidates is free: the footer hash is xxHash64 over bytes the
attacker controls, which is the threat model's first paragraph made concrete.

Bounding it means either trying fewer candidates — which refuses files that
open today, because a file whose 200th candidate is the good one currently
opens — or bounding the total work across candidates, which is the same thing
with a different shape. Both are validity changes. Memoising by directory
reference was considered and rejected: it collapses the case where every footer
names one directory, and an attacker pays about 200 bytes per distinct
directory frame to evade it, so it would buy a number rather than a bound.

`TestHostileCheckpointReplay` runs a scaled-down version and asserts the shape
stays linear and the open terminates.

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

The first control taken in this pass was run before the fix rather than after
it, which is the only order that proves the input was ever dangerous: the
2.42 GiB figure for the blob table is a measurement of the shipped code, not a
projection.
