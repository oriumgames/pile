# Freezing the pile v2 format

This document says what freezing means, what has to be true before it happens,
and what changes are permitted afterwards. It exists because a freeze that is
not written down is not a freeze: without it, the next person to change a byte
has nothing to check themselves against.

## What a freeze is

After the freeze, **the bytes a writer produces for given content are fixed**.
Any change to them is a breaking change requiring `format.Version` to be
incremented, and a v2 reader will refuse a v3 file outright — there is no
forward-compatibility lane, by the decision recorded in §2.1 of the
specification.

That makes three things permanent:

- **The layout.** Field order, widths, encodings, and the meaning of every
  flag bit, including the reserved ones.
- **The canonical form.** Which of several possible encodings of the same
  content a writer must choose: palette order, collection order, blob id
  assignment, omission rules. These are not free choices after freeze even
  where a reader cannot check them.
- **The validity rules.** What a conforming reader rejects. Tightening a rule
  after freeze invalidates files already on disk; loosening one means a file
  this version wrote is one an older version refuses.

## What is *not* frozen

- **Compressed bytes.** Zstandard admits many valid encodings of the same
  content. A different compressor, level, or version may produce different
  frames for the same body, and that is not a format change. This is why file
  identity is `format.ContentHash` (decode, re-encode uncompressed) rather
  than a hash of the stored bytes. `ContentHash` identifies the body and not
  the file: the dimension lives in the header and decoding does not resolve it
  into the body, so files holding the same chunks in different dimensions
  share a hash. That is settled deliberately in favour of leaving the value
  where it is — it is frozen along with everything else, and callers spanning
  dimensions key on the dimension separately.
- **Indexed-mode byte layout over time.** Indexed files are history-dependent
  by design: the same content stored in a different order produces different
  files. Their identity is also `ContentHash`.
- **The Go API.** Frozen separately and later; see the checklist below.
- **Performance and memory behaviour.** Optimising is always permitted
  provided the bytes do not move, which the golden suite verifies.

## Preconditions

Nothing below is optional. Each line names its exit criterion.

### Correctness

- [x] Every entry in `format/invariants.go` that claims `Enforce: Decoded` has
      a reader that actually rejects violations. *Exit: disabling the
      production check turns the named test red, for every entry.*
      Recorded in `HARNESS.md`, one row per check rather than per entry,
      because several entries have two readers or a reader and a writer and a
      fixture for one says nothing about the other. Seventeen checks across
      eleven entries turned out to be reached by no named test; twelve were
      fixed and five are explained there. One entry, `decoders never panic`,
      cannot meet this criterion as written: its enforcers are the fuzz
      targets, and a plain `go test` runs only their seed corpora, which are by
      construction the inputs already known to be safe. Its real exit criterion
      is the extended fuzzing session under Security below.
- [x] Every entry claiming `Enforce: WriterOnly` is genuinely uncheckable by a
      reader. *Exit: a written reason per entry saying what evidence is
      missing from the file.*
      All ten entries state theirs in their `Note`. Two are honest exceptions
      to "genuinely uncheckable": out-of-box cell padding (discussed below) and
      the solid block palette's order, which a reader could plausibly
      reconstruct and which §3.1 forbids it from checking. `HARNESS.md` says so
      under "Rules a reader could check and must not". Neither may be turned
      into a reader check: both would reject files this version wrote.
- [x] No test in the suite passes with its subject reverted. *Exit: a
      recorded negative-control result per canonicality test.*
      Done in two passes, both in `HARNESS.md`. The first covered every test
      the invariant table names. The second covered the rest — the mover,
      preservation, provider and golden tests — with **82 controls, of which 44
      came back green**: 41 were vacuous coverage and are fixed, one is a
      read-only guard with no distinguishing input (kept, annotated at the
      guard, and explained), and two are golden *fixture* gaps whose rules are
      held by named tests elsewhere. One of those two is worth carrying
      forward: **no golden world contained a block state with two or more
      properties**, so the canonical order of a state's property keys was not
      byte-locked by the goldens, only by `TestWriterSortsStateProperties` and
      the reader's own check.
      **That is now closed**, along with two more of the same shape found by
      sweeping every writer-side ordering decision rather than by inspection:
      structure block-entity order and structure entity order, which no golden
      could see because every structure fixture held exactly one of each. Two new
      golden fixtures, `world_props` and `structure_collections`, byte-lock all
      three rules, and `HARNESS.md` §6 records the controls. The
      fixtures are additive on purpose: adding the states to `goldenWorld`
      would have moved eleven existing goldens, and moving an existing golden
      is the event this check exists to catch. `-update` ran without
      `-format-change`, which is itself the proof that no existing fixture
      moved. The sweep also found two sites the suite cannot see and should
      not: a redundant pre-sort whose order no input can distinguish, and
      `Compact`'s record placement, which is indexed layout and explicitly not
      frozen. Both are recorded in `HARNESS.md` §6.2 so they are not mistaken
      for holes later.
      Two tests were also found to assert something weaker than their name:
      ten preservation tests ended in `format.UnresolvedStates`, which reads
      the file's *palette* and so passes on a file whose sidecar entries are
      gone but whose state table survived. They now assert on the state
      anchored at the position as well.
- [x] The structure decoder enforces the rules the world decoder does:
      duplicate block-entity positions and their order, unreferenced
      blob-table entries, blob id first-use order, all-air section and
      trailing-air-layer canonicality. *Exit: `TestDecodersAgreeOnValidity`
      renders one shape into both containers and requires both decoders to
      reach the same verdict, so the next divergence fails a test rather than
      waiting for someone to read both loops.*
      Out-of-box cell padding is deliberately **not** in that list. It is the
      one structure rule a reader cannot enforce, because padding lies outside
      the declared box by definition and a file carrying it decodes to exactly
      the same structure as one that cleared it. `format/invariants.go` files
      it as `WriterOnly` for that reason and it is verified by re-encoding;
      this line used to name it as a decoder precondition and the two
      documents disagreed.
- [x] The specification's normative rules are all claimed
      (`TestEveryRuleIsClaimed`) and all claims name live tests
      (`TestEveryInvariantNamesALiveTest`). Both green. Note what this does and
      does not buy: it proves a sentence is claimed and that the named test
      compiles, and nothing more. Whether a named test reaches the rule is the
      first precondition above, and it is what `HARNESS.md` is for.

### Security

- [x] No allocation is sized from unvalidated input. *Exit: a hostile-input
      matrix — truncation at every field boundary, every count at 0, 1, max,
      max+1 — driven through `ReadWorld`, `ReadStructure`, `ReadMeta` and
      `OpenIndexed`.*
      `format/hostile_test.go`, checked in and run with no flags. It found six
      places where a count validated against the bytes that remain still sized
      an allocation far larger than those bytes — the worst being a 1,634-byte
      file that asked for 2.42 GiB — and each fix is proved by disabling it and
      watching a named ceiling test go red. `SECURITY.md` records every finding
      with its measured before and after, and the residual: a legal 1,161-byte
      file still decodes into 1.12 GiB. That residual is deliberate and now has
      a ceiling over it. §8 caps the columns a file decodes into at 4,194,304 in
      both modes, where it used to cap only the width of the count field, so the
      worst case within the rules fell from about forty-eight gigabytes to about
      four; refusing the 1,161-byte file itself would mean a world-size limit
      below 1024x1024 chunks, and that was refused deliberately.
- [x] No integer computation on input-derived values can wrap before its
      bounds check.
      One did, and it is fixed. §3.1's version-override index chain accumulated
      uvarint deltas in a `uint64`, and a delta can express the modular
      representative of a negative step, so the indices descended where §3.1
      says they ascend. It was never memory-unsafe — the bounds test that
      follows catches an index past the palette, and a wrapped index lands on a
      legal one — but it was a second encoding of one palette, which is what the
      canonical form exists to prevent. The chain is now refused when it fails
      to increase, §3.1 states the ascent as a normative sentence rather than as
      an annotation inside a layout fence (which is why nothing pinned it), and
      `TestHostileOverrideDeltaWraps`, the independent vector walker and
      `neg_override_index_chain_wraps.pile` all hold it. Every other
      accumulation in the decode paths was checked and does not have the shape:
      the two signed delta chains land a wrap next to the bottom of `int64`,
      nowhere near a legal value.
- [x] No input can cause an unbounded loop. *Two such hangs have already been
      found in a dependency; the shape is a length narrowed to a smaller type
      before comparison against an index.*
      No loop in the decode paths is unbounded, and the one narrowing of that
      shape that remained — a section blob's palette length compared through a
      `uint16` in `blobIndices` — is now compared as an `int`. It was
      unreachable, and `SECURITY.md` says so rather than claiming a test it
      cannot have. What the matrix did find is a loop bounded at a number large
      enough to be a denial of service: recovery tried up to 256 checkpoint
      candidates and loaded each one's directory in full, and §8 bounded those
      two factors and not their product. §8 now bounds the product — 16,777,216
      directory entries parsed across one open's whole candidate list — which is
      four directories at the entry ceiling and also exactly 256 times 65,536,
      so no world of 65,536 columns or fewer can ever meet it. Measured on one
      machine, 16 forged footers over a directory at the ceiling went from
      17.5 s to 4.2 s and stopped growing with the candidate count.
- [x] Decoders never panic on any input. *Exit: the four fuzz targets run
      clean for an extended session, not the 10s smoke run.*
      Twenty minutes per target, all four `PASS`, no crashers. The counts are
      what the box is really about, and one of them was not what it looked
      like: `FuzzReadWorld` 11.4M, `FuzzReadStructure` 12.5M, `FuzzNBTStability`
      16.6M — and `FuzzOpenIndexed` **168k**, about 2/s, which is not an
      extended session by any measure that matters. The entry point with the
      most machinery behind it was the one barely being exercised.
      The cause was measured rather than guessed, and the obvious suspect was
      wrong: it is not that recovery is expensive (the whole cached corpus
      replays in 2.6 ms) but that the harness wrote every input to a fresh
      `t.TempDir` and the reader then `pread`s every frame — 85.6% of the
      decode's CPU was in syscalls. The target now runs over an in-memory
      `indexedFile`, which `openIndexedOn` already accepted, so every recovery
      path is still reached. Re-run for twenty minutes: **37,722,587
      executions**, ~31,400/s, `PASS`. `HARNESS.md` §6.5 has the measurements.
      One caveat kept in the open: its corpus was still taking new inputs at
      the twenty-minute mark, so this target has more to find than the other
      three have. It is now exploring at their order of magnitude, which is
      what this box asks for; it is not exhausted.
- [x] Crash durability is tested, not merely implemented. *Exit: an injectable
      filesystem that fails or truncates at each write during a checkpoint,
      asserting the result is always either the old checkpoint or the new one.*
      `format/faultfs_test.go` holds two crash models — a torn prefix that
      kills the process after n bytes of a chosen write, and loss of any subset
      of the writes in flight since the last successful fsync — and
      `TestCheckpointTornWriteExhaustive` runs the first at **every byte
      position of every write** a checkpoint issues, 9,073 of them over three
      scenarios covering all five frame kinds, reopening and comparing the
      whole world state each time. The claim holds: no position produced
      anything but the old checkpoint or the new one. Every durability step has
      a negative control, one of which was red when written — a checkpoint that
      failed part way had already cleared the dirty flag of whatever it managed
      to write, so a metadata-only save whose fsync failed reported success on
      the retry without writing a footer. `DURABILITY.md` records the models,
      the control table, the fix, and what the models do not reach.
- [x] The threat model is written down. **xxHash64 is not a cryptographic
      hash.** The format offers integrity against corruption, not against
      tampering: an attacker who can author content and induce truncation can
      forge a checkpoint. Files from untrusted sources are untrusted content.
      `SECURITY.md`, which states all of that, says what a caller may and may
      not conclude from a checkpoint hash or a `ContentHash`, and records what
      resource use a decode of a hostile file can reach within the rules, so a
      caller can size its own limits.
- [x] Filesystem behaviour is deliberate: path traversal, symlinks on atomic
      rename, permission bits, temp-file naming. *Exit: `FSBEHAVIOUR.md` states
      the decision for each, and `fsbehaviour_test.go` pins every one of them —
      including the ones whose answer is "it does this on purpose", since a
      decision recorded only in prose is one that changes silently.* Two gaps
      were found and closed: the command-line tools still staged with
      `os.Create`, so the exclusive staging the library gained never reached
      the three sites that rewrite a world file in place; and an atomic replace
      carried the staging mode rather than the destination's, so a world an
      operator had closed to 0600 became world-readable on its first save. The
      remaining asymmetries are deliberate and named: a solid-mode save
      replaces a symlink at the dimension path while append mode follows it,
      and a fixed staging name plus `O_EXCL` is protection against a
      pre-created path and **not** mutual exclusion between processes — a world
      directory is assumed to have one owner.

### Conformance

- [x] A vector appendix exists: for each of a minimal solid world, an empty
      chunk, a waterlogged-only section, a 256- and a 257-entry palette, a
      structure with edge padding, a torn-write indexed file, a preserved-state
      sidecar, and each dimension — the exact bytes and the expected
      `ContentHash`. *Exit: `format/vectors.md` documents 17 positive vectors
      (the nine named cases plus layer numbering, default-biome elision with a
      tie, blob dedup and Morton order, the per-column collections, light,
      stats and a full structure) and 59 negative ones, each with the rule a
      conforming reader must refuse it for. The appendix also records what it
      does not cover: the palette sort orders and cell padding, which no vector
      can express because nothing in a file proves them, and indexed mode's
      positive shape, whose bytes are history-dependent. Indexed mode's validity
      rules are covered — §5.3, §5.4 and §5.5 each have a negative vector, since
      "this file must be refused, for this rule" says nothing about the order a
      writer happened to append in.*
- [x] The vectors are generated from the implementation and checked into
      `format/testdata`, and a test verifies them. *Exit:
      `format/testdata/vectors/` plus `TestConformanceVectors` and
      `TestConformanceVectorsNegative`. Each positive vector is byte-compared,
      decoded, re-encoded back to the same bytes, and parsed a second time by
      a walker written from the specification rather than from the decoder,
      which accounts for every byte and re-checks the rules independently;
      each negative vector must be rejected, with an error naming the rule, by
      both readers. Regeneration is the golden suite's guard — the same
      `-update` and `-format-change` flags — so locking `-update` out at freeze
      locks the vectors with the goldens.*

*Rationale: the specification concedes that where prose and implementation
disagree, the implementation wins. Vectors are what make that concession safe
— they are the arbiter a second implementation can check itself against.*

### Mechanics

- [x] `format.Version` is 2 and the golden manifest agrees.
      `format/format.go` says 2; `format/testdata/golden_manifest.txt` and both
      vector manifests (`vectors_manifest.txt`, `vectors_negative_manifest.txt`)
      each declare `version 2`. That is now a checked property rather than an
      observation: `TestManifestsAgreeWithVersion` reads all three and requires
      them to equal `format.Version`, so a bump that regenerates only some of
      them fails a test instead of surfacing in whichever suite runs next. Its
      control is a `Version` of 3 against the v2 manifests, which turns it red
      on all three lines. The specification is titled "Pile File Format, Version
      2", `spec_rules.txt` is in step with it (`TestSpecRulesPinned` green), and
      nothing in the tree describes the format as provisional, draft or subject
      to change.
- [x] The golden guard refuses `-update` once frozen. *Today `-update
      -format-change` regenerates goldens; after freeze that combination must
      fail and a version bump must be the only way to move bytes.*
      The frozen version is stated once, declaratively: `format.FrozenVersion`,
      beside `Version` in `format/format.go`, with the path to a v3 in its doc
      comment. `requireUnfrozen` (in `format/golden_test.go`) refuses `-update`
      whenever `Version == FrozenVersion`, and `-format-change` does not lift
      it. It runs **first thing** in each of the three tests that write
      fixtures — `TestGoldenFormatStability`, `TestConformanceVectors`,
      `TestConformanceVectorsNegative` — because the old guard ran in
      `t.Cleanup`, after every fixture had already been rewritten on disk, so a
      refusal used to leave the regeneration behind. The vectors are as much
      the arbiter as the goldens and are locked with them. `HARNESS.md` §7
      records the controls: all three suites refuse `-update -format-change`
      and write nothing, the message names the version bump, and with `Version`
      bumped to 3 the lock lifts and the fixtures regenerate.
- [x] A compatibility statement is in the README.
      In the root `readme.md`, under "Compatibility": what v2 guarantees, what
      is not frozen, the `ContentHash`-identifies-the-body caveat, the
      xxHash64-is-not-cryptographic line with `pile.MaxDecodedBytes` as the
      knob, and where the specification, the vectors and the conformance
      appendix live. `format/readme.md` carries the mechanics instead — the
      lock, the four steps to a v3 — and points at the root statement, since
      that is the file a second implementer opens. `cmd/pile/readme.md` is a
      command reference and gets neither.
- [ ] The release is tagged.

## Validity tightened before the freeze

Four rules were tightened deliberately in the run-up to the freeze. Each of them
**rejects files a v2 reader used to accept**, which after the freeze would
require a version bump; before it, it is the last chance to make them, and that
is the whole reason they were made now. None of them moves a byte any writer
produces, so the goldens were green throughout with no flags.

1. **The §8 NBT container budget charges compounds nested in compounds.** The
   ceiling was stated and only half implemented: list elements were charged and
   compound fields were not, so a 14 MB blob decoded into twice the ceiling.
2. **§3.1's version-override index chain may not wrap.** A uvarint can carry the
   modular representative of a negative step, so the chain could descend onto a
   legal index — a second encoding of one palette. The ascent rule is now a
   normative sentence rather than an annotation inside a layout fence, which is
   why nothing had pinned it.
3. **§8 caps the columns a file decodes into, at 4,194,304 in both modes.** It
   used to cap only the width of the count field. This one picks a maximum world
   size into a frozen format, deliberately: it is the number an indexed
   directory already had, it is 400x a real overworld, and it is where a solid
   file — which holds every column at once by design — stops being openable.
4. **§8 bounds total recovery work at 16,777,216 directory entries** across one
   open's whole candidate list, rather than bounding the chain length and the
   directory size separately and leaving their product free.

`SECURITY.md`, "Format changes: made", gives each one's before-and-after
measurement, what became invalid, why the writer cannot emit the refused shape,
and the negative control.

**A fifth change is in that list and is deliberately not one of these.** A
caller may now set a stricter decode ceiling than §8's
(`format.MaxDecodedBytes`, `pile.MaxDecodedBytes`), because §8's ceilings are
set at what the format can represent and a legal 1,161-byte file still decodes
into 1.12 GiB. It changes no file's validity: the default is §8's own ceiling
and is set one column above the most §8 permits any decode to cost, so it
cannot fire on a conforming file. A refusal under a caller's ceiling reports
`format.ErrDecodeBudget`, which does **not** wrap `ErrCorrupt`, and §8 now
carries a paragraph saying that such a refusal is not a claim the file is
invalid and that a caller-supplied ceiling may only tighten. That paragraph is
the format change, and it is the whole of it: without it a second
implementation reads the limit as a validity rule, refuses conforming files and
blames the file. See `SECURITY.md` item E.

## After the freeze

**Permitted without a version bump:** anything that does not change the bytes a
writer produces for given content. Optimisation, refactoring, better errors,
new API surface, additional validation that rejects only files this version
never wrote.

**Requires a version bump:** any change to layout, canonical form, or validity
rules — including tightening a rule, which invalidates files already written.

**The check:** run the golden suite and the two vector suites with no flags. If
they pass, the change is permitted. If they do not, it is a version bump — and
the suites will not let you bless it any other way, because `-update` is locked
out while `format.Version` is `format.FrozenVersion`. That is the whole rule,
and it is why the goldens exist.

## Moving to v3

The lock is deliberate and it is not meant to be worked around, so the way past
it is written down here as well as in `format.FrozenVersion`'s doc comment and
`format/readme.md`. In order:

1. **Set `format.Version = 3`** in `format/format.go`, and leave
   `FrozenVersion` at 2. That is what lifts the lock: a version that is not the
   frozen one may still move. Every fixture fails at this point, which is
   correct — the version byte is in every header.
2. **Make the change, and regenerate:**
   `go test ./format -run 'TestGoldenFormatStability|TestConformanceVectors' -update`.
   While unfrozen the older guard still applies, so a fixture whose bytes change
   at an unchanged `Version` needs `-format-change`; immediately after a bump it
   does not, because the manifests still name the previous version.
3. **Update the specification** — `format/format.md`'s title and §2.1, the
   normative sentences the change touches — re-pin it with
   `go test ./format -run TestSpecRulesPinned -update`, claim any new rule in
   `format/invariants.go`, and update the independent vector walker in
   `format/vectorwalk_test.go`, which asserts the version it parses and is
   deliberately not derived from `format.Version`.
4. **Freeze v3** by setting `FrozenVersion = 3`.

`TestSpecRulesPinned` is outside the lock on purpose. Adding a `MUST` sentence
that states a rule the implementation already enforces moves no byte and stays
permitted after the freeze — it has already happened once, for the solid body's
trailing-bytes rule — and locking it would make an editorial correction look
like a format change. What is forbidden is changing which files a reader
accepts, and that shows up as a moved fixture, which is locked.

## Known deferred work

These do not block a freeze because they cannot change the bytes, but they are
open and should be tracked:

- Performance: benchmarks exist but nothing gates them, and there are no
  recorded baselines.
- The Go API surface has never been reviewed as a surface.

Memory used to be on this list: the indexed-mode contract is "directory,
palettes, and one record at a time", and several paths held more. They no
longer do, and the goldens were green throughout, which is the whole of what
this section's first sentence claims. `STATUS.md` records the measurements and
the two shapes that must not be optimised back.

---

# Pre-tag summary

Written for someone who was not here. Everything above this line is the
argument; this is the state of it on the day the tag goes on. The tag itself is
the only unticked box, and it is the maintainer's to place.

## What is done

Every Correctness, Security and Conformance precondition above is ticked, and
each one names the artefact that holds its evidence rather than asserting it:

- **The specification is pinned to the implementation.** Every normative `MUST`
  in `format/format.md` is extracted and hashed into
  `format/testdata/spec_rules.txt`, claimed by an entry in
  `format/invariants.go`, and the claim names a test that exists. Editing a rule
  is therefore a visible event.
- **The tests have been shown to reach the rules they name.** Not once but
  three times, by disabling the production check and requiring a named test to
  go red: the invariant table (17 checks across 11 entries were vacuous, 12
  fixed, 5 explained), then the rest of the suite (82 controls, 44 green, 41
  fixed), then a systematic sweep of every writer-side ordering decision against
  the goldens. `HARNESS.md` is the whole record, and it is the most useful file
  in the repository for judging what the suite is worth.
- **Hostile input has been driven through every reader.** `format/hostile_test.go`
  is a truncation-and-boundary matrix; it found six allocations sized from
  counts that were bounded against the remaining bytes and nothing else.
  `SECURITY.md` records each with a measured before and after. Four validity
  rules were tightened as a result — the last moment at which that was possible.
- **Crash durability is exercised, not asserted.** `TestCheckpointTornWriteExhaustive`
  crashes a checkpoint at every byte position of every write it issues (9,073
  positions) and requires the result to be the old checkpoint or the new one.
  `DURABILITY.md`.
- **The four fuzz targets ran twenty minutes each, clean.**
- **A second implementation has an arbiter.** `format/vectors.md` plus 17
  positive and 59 negative vectors in `format/testdata/vectors/`, verified on
  every flagless run, each positive one re-parsed by a walker written from the
  specification rather than from the decoder.
- **The bytes are locked.** `format.FrozenVersion` and the `requireUnfrozen`
  guard, above.

## What is knowingly accepted and unfixed

None of these is an oversight. Each was measured, argued and left, and closing
several of them now would be a format change.

- **A legal 1,161-byte solid file decodes into 1.12 GiB** and takes about a
  second. An 11-byte chunk record is valid — a record must declare a section but
  need not mark one present — and each becomes about a kilobyte of live objects.
  Refusing it means a world-size limit below 1024×1024 chunks, which a real
  server reaches, and that trade was refused. §8's column ceiling caps the worst
  case at 4,194,304 columns, about four gigabytes; it does not bound this file.
  A caller that opens worlds it did not write should set
  `pile.MaxDecodedBytes` / `format.MaxDecodedBytes`. `SECURITY.md`, "What
  remains".
- **4,194,304 columns is now a maximum world size in a frozen format.** Chosen
  deliberately; 400× a real overworld.
- **Recovery can still spend about four seconds of CPU on a 9.5 KB file.**
  Bounded at 16,777,216 directory entries across one open, so it no longer grows
  with the candidate count, but the residual is the price of reaching a fourth
  fallback on a million-column world.
- **`ReadMeta` can allocate ~82 MiB transiently from a 132-byte settings blob.**
  Within the §8 NBT container budget.
- **xxHash64 is not a cryptographic hash.** Integrity against corruption, not
  against tampering; an attacker who can author content and induce truncation
  can forge a checkpoint that verifies.
- **`ContentHash` identifies the body, not the file.** Two files holding the
  same chunks in different dimensions share a hash. Frozen that way on purpose.
- **A world directory is assumed to have one owner.** `O_EXCL` staging is
  protection against a pre-created path, not mutual exclusion between processes.
  `FSBEHAVIOUR.md` §5.
- **`FuzzOpenIndexed` is not saturated.** It now runs at ~31,400 exec/s rather
  than 2/s, but its corpus was still taking new inputs at twenty minutes, so it
  has more to find than the other three targets do.
- ~~**The writer paths have not been audited against hostile-but-legal input**~~
  — **done after the tag**, in `format/hostilewrite_test.go`. It found six
  defects, three of them files a writer emitted that its own reader refuses
  (positions outside the column, a structure grid past §8's cell ceiling, an
  unregistered block runtime ID), one quadratic scan reachable from
  `ContentHash` at 2 m 4 s on a 155-byte file, two panics on nil input, and one
  **specification divergence that needs a version bump and is deliberately
  unfixed**: §1 and §8 put an NBT string at 65,535 bytes and this reader refuses
  anything from 32,768 up, because gophertunnel takes the length as a signed
  int16. `SECURITY.md`, "The writer matrix", has all of it with measurements;
  `HARNESS.md` §9 has the thirteen controls. No file's readability moved and no
  fixture did; the five new writer refusals are API behaviour changes and are
  listed there.
  **The `pile` provider surface and the CLI are still not audited**: the matrix
  drives `format` directly. Neither can invalidate a file already written — a
  writer refusing an input is an API change, not a format change — so neither
  blocks the tag.
  `FREEZE-BLOCKERS.md` records the first; its closing note reconciles what has
  moved since it was written. Its point 1 — only `MUST` sentences are pinned,
  so layout annotations are not — is still live and is how §3.1's ascent rule
  went half-enforced for as long as it did.
- **No performance baselines, and the Go API has never been reviewed as a
  surface.** Both are post-tag work; neither can invalidate a file.
- **One golden is deliberately not byte-locked**: `golden_indexed_compact.pile`,
  because dictionary training is not reproducible. Its structure is checked by
  `TestGoldenFormatReadable`.

## What a maintainer must know on day one

1. **The rule for any change is one command.** Run `go test ./... -count=1`. If
   the goldens and the two vector suites pass with no flags, the change is
   permitted. If they do not, it is a version bump, and "Moving to v3" above is
   the only path — `-update -format-change` will refuse.
2. **Do not "fix" the block palette order check.** A reader could plausibly
   reconstruct the solid block palette's reference counts and verify §3.1's
   order. §3.1 says readers MUST NOT, and after the freeze such a check would
   reject files this version wrote. `HARNESS.md`, "Rules a reader could check
   and must not".
3. **`ContentHash` is the identity mechanism, not the file's bytes.**
   Compressed bytes and indexed-mode layout are not frozen; do not build
   anything that assumes a byte-for-byte comparison of two files means
   different content, in either direction.
4. **Dragonfly is pinned for a reason.** The codec reaches into `chunk`
   internals through layout-asserted unsafe mirrors and panics at package init
   if a dragonfly upgrade moves them. Currently `v0.11.1`. Bumping it means
   running the whole suite.
5. **The method that found nearly everything here was mutation, not reading.**
   Disable the production check, run the named test with `-count=1`, require it
   to fail. Tests written and controlled two rounds earlier still turned out to
   patch bytes inside the wrong structure. Do the same for every new test.
6. **`STATUS.md` is a working note and says so**; delete it once the tag has
   settled. `FREEZE.md`, `SECURITY.md`, `HARNESS.md`, `DURABILITY.md`,
   `FSBEHAVIOUR.md` and `format/vectors.md` are the durable record.
