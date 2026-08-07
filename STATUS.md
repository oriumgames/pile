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

Method: `Enforce` is a required field, so an entry that does not say whether
readers reject violations fails the harness. Benchmarks went from 5 to 28.

## Open

**Harness** — roughly 13 entries still have vacuous tests. This is the only
category still gating a freeze, because a table that lies about what protects
the format is worse than no table.

**Memory** — `snapshotPalettes` is O(n²) per store; `Compact` sampling needs
verifying; `udCache`/`unkCache` have no eviction; `Columns()` materialises a
whole dimension; the shared codec state is oversized.

**Security** — about eleven lower-severity items remain, none of them a
process-killer.

**Conformance vectors** — not started, and they are what makes a freeze mean
anything. The specification concedes that where prose and implementation
disagree the implementation wins; vectors are the arbiter that makes the
concession safe.

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

Harness, then memory, then vectors, then freeze. Security's remainder and the
API surface review can follow the tag, since neither can invalidate a file that
has already been written.
