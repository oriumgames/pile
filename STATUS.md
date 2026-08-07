# Where the freeze work stands

A working note, not documentation. `FREEZE.md` says what freezing means and
what must be true first; this says how far along that is and what the open
work actually costs. Delete it when the freeze ships.

## The short version

The wire format is done. Twenty-five adversarial review rounds found no defect
in its layout or semantics, and the one real format hole found since
(`c8d30f0`) came from mutation-testing the harness rather than from reading the
code. What is left is the code around the format, the evidence that the code is
right, and two things that had never been audited at all: security against
hostile input, and memory.

## What the audits found

Four parallel audits ran against the four axes. Each found real work, and the
ratio is the interesting part:

- **Format**: nothing. Consistent with the five review rounds before it.
- **Security**: 26 findings, seven of which kill the process from a file of a
  few kilobytes. Never audited before as a category.
- **Harness**: 22 of 50 entries claiming decoder enforcement were false — seven
  named a rule no reader checked, the rest named a test that stayed green when
  the check was deleted.
- **Memory**: three breaks of the indexed-mode contract, plus 113–146 MiB of
  fixed codec state against a "lightweight for small worlds" goal.

The lesson worth keeping: **the bounds discipline was real but bounded the
wrong thing.** Every count was checked against the bytes that remain, and
several values a file declares cost far more to decode than to write — one blob
reference is a byte and a live storage; one `TAG_End` in a list of compounds is
a byte and a whole map. That is now a stated rule in §8, because it generalises
past the six instances that were fixed.

## Done

Security: the seven process-killers, the two write-path loops that never
returned (reachable from `ContentHash`, so from ordinary tooling), a dictionary
ceiling the window bound does not cover, bounded dictionary sampling, and
exclusive file staging so a predictable `.tmp` name cannot be a symlink.

Correctness: the uniform-width format hole; structures brought inside the
palette rules they depend on; collection order, blob first-use order and the
§7 metadata schemas enforced on read as well as write; enforcement labels
corrected on four entries.

One more, found by the harness pass and closed after the vectors landed: both
solid readers reject a body with bytes after the last record, and §4 said so
without a MUST, so the extractor never pinned the sentence and no invariant
claimed it. It is a rule now, with an entry, a fixture per reader and a negative
vector. No file's validity moved — the code already did this — which is why it
was a specification edit and a `TestSpecRulesPinned` re-pin rather than a format
change.

Method: `Enforce` is a required field, so an entry that does not say whether
readers reject violations fails the harness. Benchmarks went from 5 to 28.

## Memory: what landed

The six memory commits are on main, rebased over the format-affecting work
rather than merged, so the history stays linear and each fix is still a commit
on its own. All eight findings are closed. Measured before and after, with the
golden suite green and no update flags, so no writer produces different bytes:

- `Store` into a 17,499-entry palette: 1,680,870 → 218,342 B/op, now identical
  to storing into an empty one. The wholesale map copy became a per-`Store`
  journal of newly inserted keys.
- `Columns()` breaking after one of 64: 883,665 → 14,058 B/op, 16,888 → 266
  allocs. Both modes are lazy; `snapshotDim` survives only for `SaveAs`, which
  genuinely needs the whole dimension.
- Shared zstd decoders under 8 concurrent decodes: 194.6 MiB retained → 0.
  Concurrency 1 plus pools, and the body and directory decoders collapse since
  their ceilings match. The frame decoder deliberately stays separate: merging
  it into the 512 MiB pool would let a hostile record allocate that much before
  a length check rejected it.
- Dictionary codecs with 8 writing handles on one dictionary: 88 MiB → 0,
  shared process-wide by (level, dictionary hash).
- `udCache`/`unkCache` folded into one bounded LRU, capped both by entry count
  and by a 64 MiB weight budget, because a single user-data blob may be 16 MiB.
- Reading a world whose sections almost all differ: 513,604 → 360,876 B/op. The
  blob table's duplicate check hashes each blob's span and compares on a hit,
  instead of copying every 4 KiB or 8 KiB blob into a map key.

Two judgement calls that both look like something to improve later, and are
not. Encoders are not pooled: pooling cost +0.9–1.4 MB/op and roughly 3x wall
time on `WriteWorld`, so they are built lazily instead and a read-only process
constructs none. And dictionary sampling is strided rather than a prefix,
because directory keys are in Morton order and a prefix samples one corner of
the world; each sample is also truncated, so the byte budget spreads over the
world instead of being spent on whichever records the walk reaches first.

## Durability and filesystem behaviour: what landed

The two `FREEZE.md` security boxes about crash durability and filesystem
behaviour are ticked. `DURABILITY.md` and `FSBEHAVIOUR.md` carry the evidence;
the short version:

- `format/indexed.go` takes an `indexedFile` interface instead of `*os.File`,
  so a test can fail or truncate a chosen write. Two crash models: a torn
  prefix, and loss of any subset of the writes in flight since the last fsync.
  The second is the only way the "fsync before the footer" ordering is
  observable at all.
- `TestCheckpointTornWriteExhaustive` runs **every byte position of every
  write** — 9,073 crash positions over three scenarios covering all five frame
  kinds — and the durability claim holds at every one of them.
- One real defect: a checkpoint that failed part way had already cleared the
  dirty flag of whatever it managed to write, so a **metadata-only** save whose
  fsync failed reported success on the retry with no footer written at all. The
  metadata was reported saved and gone at the next open. Fixed with
  `checkpointPending`, proved by disabling it.
- Two filesystem gaps: `cmd/pile` still staged with `os.Create` at three sites
  that rewrite a world file in place, and an atomic replace widened the
  destination's permission bits (0600 → 0644 on the first save).
- One control stays green and is recorded as such rather than dressed up: the
  fsync before the footer is redundant with `verifyRecords` for the
  *old-or-new* invariant. The combined control proves the model reaches the
  case and that the record hashes are what save it. The fsync earns its place
  by keeping acknowledged work rather than rolling it back, which old-or-new
  cannot express.

## Open

**Harness** — done, and `HARNESS.md` is the record. Every `Decoded` entry now
has a control that turns a named test red, every `WriterOnly` entry states what
evidence is missing, and the exceptions to both are written down where the
claim is made rather than left for the next reader to discover.

The estimate above was thirteen entries. The real number was seventeen checks
across eleven entries, and the reason the entry count read low is worth
keeping: most of those entries were not wholly vacuous. They had two or three
readers, one of which was controlled, and the partial cover made the entry look
finished. Counting entries hides that; counting checks does not. Twelve were
fixed. Five cannot be controlled and say so.

The recurring shape, again, was a fixture malformed in a second way: the
section-blob tests built palettes with an entry no index named, so §3.3 refused
them before the ascent and width rules ran, and the assertions only asked
whether an error came back. Assert on which error.

**Security** — the hostile-input pass is done and `SECURITY.md` is the record:
the threat model, what the matrix in `format/hostile_test.go` covers, what it
found, and what remains. Three of the seven `FREEZE.md` boxes are ticked by it,
plus the threat model.

The "about eleven lower-severity items" this section used to estimate were
re-derived rather than read off a list, and the count was about right: fourteen
items, of which seven are fixed, one is hardening with no test it could have,
one was reviewed and deliberately left, and five are residuals or format
changes. The estimate was wrong about the severity, though. Two of the seven
are not lower-severity at all by the standard the first pass used — a
1,634-byte file allocating 2.42 GiB, and a 20 KB file making `OpenIndexed`
run for a quarter of an hour — and both were missed the first time for the same
reason the memory pass gives: the count was checked against the bytes that
remain, so it looked bounded.

**Four validity rules were then tightened**, which is the last thing that can
happen before a freeze and could not have happened after it. Two of them were
the findings the security pass had reported and pinned rather than fixed — the
§8 NBT container budget not charging compounds nested inside compounds, and
§3.1's version-override index chain wrapping in `uint64` before its bounds check
— and their characterisation tests are now enforcement tests rather than
deletions. The other two bound what §8 had left unbounded: the columns a file
decodes into, and the total work recovery may spend.

Each has a normative sentence, a table entry, a control that turns a named test
red, and a measured before and after in `SECURITY.md` under "Format changes:
made". Only one of the four could have a negative conformance vector — the
override wrap, which is small. The other three need a file of five, thirty-three
and eighty megabytes respectively to express, so they join the other §8 ceilings
under "Rules no vector here exercises", where `format/vectors.md` now says so
explicitly rather than leaving a second implementation to wonder.

The column ceiling is the one with a real cost and it is worth stating flatly:
4,194,304 columns is now a maximum world size in a frozen format. It was chosen
because an indexed directory already had that ceiling, because it is four
hundred times a real overworld, and because a solid file holds every column at
once, so at four million the file is already about four gigabytes of live
objects and stops being openable for reasons no ceiling can help with. It does
**not** refuse the 1,161-byte file that decodes into 1.12 GiB; refusing that one
means a limit below 1024x1024 chunks, which a real server can reach.

**Conformance vectors** — done. `format/vectors.md` is the appendix, 17 positive
and 59 negative vectors in `format/testdata/vectors/`, verified on every run
with no flags. Each positive vector is also parsed by a second reader written
from the specification, and each negative one must be refused by both with an
error naming its rule.

Two things the vector work left behind. The appendix says plainly what no vector
can express — the palette sort orders and cell padding, because nothing in a
file proves them — which is the part a second implementation most needs told.
And a negative vector for an indexed file is a different claim from a negative
vector for a solid one: §5.6 recovery means a reader that refuses the newest
checkpoint falls back to an older one, so a file with a valid earlier checkpoint
*opens* instead of being rejected. The §5 vectors are built with exactly one
checkpoint for that reason, and `format/vectors.md` says so where a reader of
those vectors will meet it.

**Cross-process safety** — a world directory is assumed to have one owner and
nothing enforces it. `FSBEHAVIOUR.md` §5 says why `O_EXCL` is not a lock.

## Two method notes, both learned the hard way

**Mutation-test every canonicality test, not just new ones.** Disable the
production check, run the named test, require it to fail. A negative control
that passes proves the test fails when *that line* changes, not that it reaches
the rule it claims. Tests written and controlled two rounds earlier still
turned out to patch bytes inside the wrong structure entirely.

**Verify the anchor before editing.** A pattern that appears three times in a
file will be replaced at the first occurrence, and the resulting control tells
you nothing. Grep by line number, confirm with `sed -n`, and re-read the
function afterwards. Always `-count=1`: cached results lie.

**A check with no distinguishing input is not enforcement.** One was written
and removed the same day: rejecting an all-air section separately from a
trailing all-air layer has no input that separates them, because an all-air
section has an all-air last layer. Delete such a check rather than leave
something that cannot fail.

## Sequence from here

Freeze. What still gates it is under Security in `FREEZE.md`, none of which can
change a byte: the extended fuzzing session, the hostile-input matrix, crash
durability, the written threat model. The API surface review can follow the tag,
since it cannot invalidate a file already written.

The closed door the harness pass turned up is now carried by the vectors as
well as by `HARNESS.md`: a reader could plausibly reconstruct the solid block
palette's reference counts and check its order, and §3.1 says readers MUST NOT.
`format/vectors.md` says so under "Rules no vector here exercises", which is
where a second implementation will be looking when it wonders why the order is
not checkable.
