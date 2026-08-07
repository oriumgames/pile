# Crash durability: what was tested, and what it proved

§5.6 of the specification claims that an indexed file survives a torn write:
"a torn write therefore loses at most the work since the last checkpoint",
which as an invariant is *the file opens, and what it opens to is either the
checkpoint that was already durable or the one being written — never a mixture
of the two and never something unopenable.*

Before this pass the claim rested on one hand-built fixture
(`TestIndexedTornWriteRecovery`: chop half the footer off, reopen, check the
generation). That is one crash position out of nine thousand.

This document records the injectable filesystem, what the exhaustive run found,
the negative control for every durability step, and what the models do not
cover. Line numbers are as of this commit; anchors are quoted so they can be
found again.

## The injectable filesystem

`format/indexed.go` used `*os.File` directly. It now takes an `indexedFile`
interface — `ReaderAt`, `WriterAt`, `Writer`, `Closer`, `Sync`, `Truncate`,
`Stat` — with `*os.File` the only implementation outside tests, plus two
unexported constructors (`createIndexedOn`, `openIndexedOn`) that take an
already-open file. Nothing else about the writer changed; the goldens and
vectors were green throughout.

`format/faultfs_test.go` holds two crash models. Both matter, and neither
subsumes the other.

**1. Torn prefix (`crashFile`).** A write-through wrapper around a real file.
At a chosen operation index it lets the first *n* bytes land and then refuses
every subsequent write, sync and truncate — including the truncate
`appendFrame` attempts on a short write, because a process that has died does
not get to tidy up. The test then closes the raw handle and reopens the file
from disk. This is a process dying mid-`pwrite`.

`crashFile` has two other modes. `transient` fails the one operation and lets
the process live, which is a full disk or a bad sector rather than a crash;
`failSyncs` takes every byte and refuses every fsync, which is the disk that
will not promise durability.

**2. In-flight write loss (`replayImage` + `syncGroups`).** A prefix is not the
only thing a crash can leave. Writes issued since the last successful fsync may
reach the platter in any subset, or not at all. The fault-free run is recorded
as a trace of `(offset, bytes)`, split at each successful fsync, and every
subset of every in-flight group is replayed into a synthetic file image. A
dropped write is modelled as a hole — a later write at a higher offset extends
the file and the gap reads as zeros — because that is what a filesystem hands
back for a sparse region.

This second model is the only way the "fsync before the footer" ordering of
§5.6 is observable at all. Under a prefix model the ordering cannot be tested,
because a prefix that contains the footer contains everything written before
it.

## What was run

`format/durability_test.go`. Three scenarios, chosen so that between them the
crash window covers every frame kind a checkpoint writes:

| scenario | operations in the crash window |
|---|---|
| compressed | record 51 B, block segment 35 B, biome segment 31 B, meta 54 B, directory 117 B, **fsync**, footer 44 B, **fsync** |
| uncompressed | record 8283 B, block segment 22 B, biome segment 18 B, meta 41 B, directory 106 B, **fsync**, footer 44 B, **fsync** |
| overwrite-only | record 46 B, block segment 35 B, directory 80 B, **fsync**, footer 44 B, **fsync** |

`TestCheckpointTornWriteExhaustive` crashes at **every byte position of every
write**, not a sample: for a write of *n* bytes it runs *n+1* crashes, one per
prefix length, plus one per fsync. That is **9,073 crash positions** (340 +
8,522 + 211), each of which rebuilds the file from the pre-crash image, runs
the mutation under the fault, reopens the file and compares the full state —
generation, the set of chunk positions, a hash of every record body, and all
four metadata blobs — against the old checkpoint and the new one. Every column
is decoded, so "opens" means "reads", not "does not error at `OpenIndexed`".

Exhaustive was affordable because the records are one-block columns. The
uncompressed scenario is there to make sure the result is not an artefact of
small writes: its record frame is 8 KiB and every one of its 8,284 prefixes was
run.

`TestCheckpointSurvivesUnsyncedWriteLoss` runs every subset of every in-flight
group: 81 cases over the three scenarios.

`TestCrashedCheckpointLeavesAWritableFile` reopens read-write after a crash,
stores another column and checkpoints, and requires the result to keep
everything recovery adopted. Recovering into a file that cannot be appended to
is not recovering.

**Result: the claim holds. There is no position at which a torn write produced
anything other than the old checkpoint or the new one.** The list of positions
that failed is empty.

## The defect the suite found

`TestCheckpointDoesNotReportSuccessWithAnUnsyncedFooter` was red when it was
written, and the fix is `w.checkpointPending` in `format/indexed.go`.

`checkpointLocked` clears each dirty flag as it writes the frame that flag
stands for: `pendingBlkN` when the block segment lands, `metaDirty` when the
meta frame lands. `recordsDirty` is the exception — it is cleared last, after
the footer's fsync. So a checkpoint that failed part way had already forgotten
what it owed, and the *next* `Checkpoint()` call took the early return:

```go
if !w.recordsDirty && !w.metaDirty && w.pendingBlkN == 0 && w.pendingBioN == 0 {
    return nil
}
```

For a metadata-only checkpoint — `SaveSettings` followed by `Save` on a
dimension whose chunks have not changed, which is an ordinary provider path —
`recordsDirty` is false. A failing fsync therefore produced: one `Checkpoint`
returning an error, and every subsequent one **returning success without
writing a footer at all**. The metadata was reported saved and was gone at the
next open.

The fix is a flag that is raised before the first frame of a checkpoint is
written and lowered only after the footer is durable, and added to that early
return. It changes no bytes and no validity rule: it only decides whether a
checkpoint is written, never what a written one contains. `checkpointPending`
is also cleared in `resetLoadedState`, beside `footerPending`.

## Negative controls

Method: disable the production step (`if false && …`, never delete, so the
surrounding code still compiles and still runs), run the named tests with
`-count=1`, require at least one to fail, restore, and confirm the restore with
a byte comparison.

| # | step disabled | anchor | named test | result |
|---|---|---|---|---|
| C1 | checkpoint hash in `tryFooter` | `indexed.go` `if checkpointHash(w.hdrBytes, frame, buf[8:]) != wantHash {` | `TestRecoveryRejectsFooterWhoseHashFails` | **red** — "a footer that fails its hash was adopted", subtests *stored hash* and *generation* |
| C2 | `verifyRecords` in `adoptCheckpoint` | `if lerr := w.verifyRecords(); lerr != nil {` | `TestIndexedRecordCorruption`, `TestIndexedRecordCorruptionFallsBack` | **red** |
| C3a | directory entry `off+len` inside the file | `if e.off < headerSize \|\| e.off+int64(e.length) > w.end {` | `TestRecoveryRejectsDirectoryNamingARecordPastEOF/length_runs_past_EOF` | **red** — "the forged directory loaded without complaint" |
| C3b | directory offset-chain bound | `if poff < headerSize \|\| poff > w.end {` | `…/offset_past_EOF` | **red** — refused by the wrong check, message asserted |
| C4 | the `prevFooter` chain walk | `for i := 0; i < len(cands) && len(cands) < maxCheckpointChain; i++ {` | `TestRecoveryFollowsPrevFooterChain` | **red** — "recovery failed outright" |
| C5 | `truncateToEnd` after a short write | `w.truncateToEnd()` in `appendFrameWith` | `TestFailedStoreLeavesNoTailPastTheFooter` | **red** — "left the file 353 bytes long, want 329" |
| C6 | the fsync before the footer | `return fmt.Errorf("pile: sync frames: %w", err)` | everything | **green — see below** |
| C6+C2 | both of the above | | `TestCheckpointSurvivesUnsyncedWriteLoss` | **red** — "group 0 subset 111110: record (1,0) checksum mismatch", all three scenarios |
| C7 | `checkpointPending` (the fix above) | `&& !w.checkpointPending {` | `TestCheckpointDoesNotReportSuccessWithAnUnsyncedFooter` | **red** |
| C8 | per-frame hash in `validRef` | `if xxhash.Sum64(stored) != ref.hash {` | `TestRecoveryRejectsCheckpointWhoseSharedFrameHashFails` | **red** |
| F1b | `O_EXCL` in `CreateIndexed` | `os.OpenFile(path, os.O_RDWR\|os.O_CREATE\|os.O_EXCL, 0o644)` | `TestCreateIndexedRefusesAnExistingPath` | **red** |

### C6, and why it stays green

Removing the fsync between the frames and the footer leaves every test green,
including the in-flight loss model. That is not a gap in the model — the
combined C6+C2 control proves the model reaches the case, and names it: with
the fsync gone, group 0 becomes `[record, block seg, biome seg, meta,
directory, footer]`, and subset `111110` is "everything landed except the
record". The reason the invariant survives is that `verifyRecords` hashes every
record the adopted directory names, so a checkpoint whose record did not reach
the disk is refused and the previous one is adopted: still the old checkpoint,
still not a mixture.

So the ordering and the per-frame hashes are two independent defences of the
same property, and either alone is enough for *old-or-new*. What the fsync buys
that the hashes do not is that the work the caller was told was saved is
actually saved rather than rolled back — a distinction the old-or-new invariant
is not able to express, since losing the newest checkpoint is one of its two
permitted outcomes. It stays for that reason, and the control result is
recorded here rather than being made to look red by weakening the invariant.

### Three tests that had to be repaired before they controlled anything

Worth recording, because the same shape has now been found in four separate
rounds of this project.

- `TestRecoveryRejectsDirectoryNamingARecordPastEOF` passed with **both**
  directory bounds checks deleted. Falling back is not the same as refusing for
  the right reason: reading a record past EOF fails at the read whatever the
  directory said, so the end-to-end assertion could never distinguish them. It
  now drives `loadDirectory` in on its own and asserts the message, and both
  checks control.
- `TestRecoveryFollowsPrevFooterChain` needed 70 doomed checkpoints. Below 65
  the backward scan finds every footer by itself and the chain adds nothing, so
  a smaller fixture would have been green with the chain walk deleted.
- `TestRecoveryRejectsCheckpointWhoseSharedFrameHashFails` has to use an
  uncompressed file. In a compressed one zstd's own frame checksum refuses the
  damage first and the directory's hash — the rule under test — is never
  reached.

## What these models do not cover

- **Sector-level reordering inside one write.** Both models tear a write at a
  byte boundary. A real disk tears at a sector boundary and may commit sectors
  out of order, so a torn write can leave a *later* sector present and an
  earlier one absent. The in-flight model covers exactly this at whole-write
  granularity but not within a single write. Nothing in the format depends on
  intra-write ordering: every frame is validated by a hash over the whole of
  it, and the footer's magic is at its end.
- **Bit rot inside a frame that still hashes.** xxHash64 is not a cryptographic
  hash and the format offers integrity against corruption, not tampering. That
  is FREEZE.md's separate threat-model line.
- **Compaction.** `Compact` writes a whole new file and renames it over the
  original, so its crash behaviour is the rename's, not the checkpoint's.
  `TestStagedNamesAreNeverWrittenThrough/IndexedWorld.Compact` covers the
  staging; a crash mid-compaction leaves the original untouched because nothing
  writes to it until the rename. `IndexedWorld.reopen` exists so a future test
  can crash the post-rename reopen as well; nothing uses it yet.
- **A dictionary file.** The scenarios have no shared dictionary, because one
  is only installed during compaction. A dictionary frame is referenced from
  the directory like any other and validated by `validRef`, which C8 controls.
- **Concurrent writers.** Two processes checkpointing one file is out of scope
  for the same reason it is out of scope for the filesystem audit: the library
  assumes a world directory has one owner. See `FSBEHAVIOUR.md`.
