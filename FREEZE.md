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

- [ ] Every entry in `format/invariants.go` that claims `Enforce: Decoded` has
      a reader that actually rejects violations. *Exit: disabling the
      production check turns the named test red, for every entry.*
- [ ] Every entry claiming `Enforce: WriterOnly` is genuinely uncheckable by a
      reader. *Exit: a written reason per entry saying what evidence is
      missing from the file.*
- [ ] No test in the suite passes with its subject reverted. *Exit: a
      recorded negative-control result per canonicality test.*
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
- [ ] The specification's normative rules are all claimed
      (`TestEveryRuleIsClaimed`) and all claims name live tests
      (`TestEveryInvariantNamesALiveTest`).

### Security

- [ ] No allocation is sized from unvalidated input. *Exit: a hostile-input
      matrix — truncation at every field boundary, every count at 0, 1, max,
      max+1 — driven through `ReadWorld`, `ReadStructure`, `ReadMeta` and
      `OpenIndexed`.*
- [ ] No integer computation on input-derived values can wrap before its
      bounds check.
- [ ] No input can cause an unbounded loop. *Two such hangs have already been
      found in a dependency; the shape is a length narrowed to a smaller type
      before comparison against an index.*
- [ ] Decoders never panic on any input. *Exit: the four fuzz targets run
      clean for an extended session, not the 10s smoke run.*
- [ ] Crash durability is tested, not merely implemented. *Exit: an injectable
      filesystem that fails or truncates at each write during a checkpoint,
      asserting the result is always either the old checkpoint or the new one.*
- [ ] The threat model is written down. **xxHash64 is not a cryptographic
      hash.** The format offers integrity against corruption, not against
      tampering: an attacker who can author content and induce truncation can
      forge a checkpoint. Files from untrusted sources are untrusted content.
- [ ] Filesystem behaviour is deliberate: path traversal, symlinks on atomic
      rename, permission bits, temp-file naming.

### Conformance

- [x] A vector appendix exists: for each of a minimal solid world, an empty
      chunk, a waterlogged-only section, a 256- and a 257-entry palette, a
      structure with edge padding, a torn-write indexed file, a preserved-state
      sidecar, and each dimension — the exact bytes and the expected
      `ContentHash`. *Exit: `format/vectors.md` documents 17 positive vectors
      (the nine named cases plus layer numbering, default-biome elision with a
      tie, blob dedup and Morton order, the per-column collections, light,
      stats and a full structure) and 39 negative ones, each with the rule a
      conforming reader must refuse it for. The appendix also records what it
      does not cover: the palette sort orders and cell padding, which no vector
      can express because nothing in a file proves them, and most of indexed
      mode, whose bytes are history-dependent.*
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

- [ ] `format.Version` is 2 and the golden manifest agrees.
- [ ] The golden guard refuses `-update` once frozen. *Today `-update
      -format-change` regenerates goldens; after freeze that combination must
      fail and a version bump must be the only way to move bytes.*
- [ ] A compatibility statement is in the README.
- [ ] The release is tagged.

## After the freeze

**Permitted without a version bump:** anything that does not change the bytes a
writer produces for given content. Optimisation, refactoring, better errors,
new API surface, additional validation that rejects only files this version
never wrote.

**Requires a version bump:** any change to layout, canonical form, or validity
rules — including tightening a rule, which invalidates files already written.

**The check:** run the golden suite. If it passes without `-format-change`, the
change is permitted. If it does not, it is a version bump. That is the whole
rule, and it is why the goldens exist.

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
