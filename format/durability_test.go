package format

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cespare/xxhash/v2"
	"github.com/df-mc/dragonfly/server/block"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
)

// Crash durability for indexed mode.
//
// §5.6: "A torn write therefore loses at most the work since the last
// checkpoint." Stated as an invariant a test can hold: after a crash at any
// point during a checkpoint, the file opens, and what it opens to is either
// the checkpoint that was already durable or the one being written — never a
// mixture of the two and never an unreadable file.
//
// The two crash models live in faultfs_test.go. This file drives them over
// every write position a checkpoint has.

// tinyColumn is a one-block column. Record frames have to be small here
// because the suite walks every byte of every write, and a full test column
// costs a few thousand file rebuilds on its own.
func tinyColumn(t testing.TB, reg world.BlockRegistry, x, z int32, r uint32) Column {
	t.Helper()
	ch := chunk.New(reg, overworldRange)
	ch.SetBlock(0, -64, 0, 0, r)
	return Column{X: x, Z: z, Col: &chunk.Column{Chunk: ch}}
}

// worldState is everything about an opened world a crash could get wrong.
type worldState struct {
	gen                         uint64
	chunks                      map[[2]int32]uint64
	settings, userData, markers string
}

func (s worldState) String() string {
	keys := make([][2]int32, 0, len(s.chunks))
	for k := range s.chunks {
		keys = append(keys, k)
	}
	return fmt.Sprintf("gen=%d chunks=%v meta=%q/%q", s.gen, keys, s.settings, s.userData)
}

func (s worldState) equal(o worldState) bool {
	return s.gen == o.gen && maps.Equal(s.chunks, o.chunks) &&
		s.settings == o.settings && s.userData == o.userData &&
		s.markers == o.markers
}

// captureState opens a file and reads everything out of it. It fails the test
// if the file does not open or any column does not decode: "never something
// unopenable" is half of the claim under test.
func captureState(t *testing.T, path string, reg world.BlockRegistry, what string) worldState {
	t.Helper()
	w, err := OpenIndexed(path, reg, true)
	if err != nil {
		t.Fatalf("%s: file does not open: %v", what, err)
	}
	defer w.Close()
	st := worldState{gen: w.Generation(), chunks: map[[2]int32]uint64{}}
	for _, k := range w.Positions() {
		if _, err := w.Column(k[0], k[1]); err != nil {
			t.Fatalf("%s: column (%d,%d) does not decode: %v", what, k[0], k[1], err)
		}
		e := w.dir[k]
		body, err := w.readFrame(frameRef{off: e.off, length: e.length})
		if err != nil {
			t.Fatalf("%s: record (%d,%d) unreadable: %v", what, k[0], k[1], err)
		}
		st.chunks[k] = xxhash.Sum64(body)
	}
	s, u, m := w.Meta()
	st.settings, st.userData, st.markers = string(s), string(u), string(m)
	return st
}

// crashScenario is one checkpoint to be interrupted at every write position.
type crashScenario struct {
	name string
	opts Options
	// prepare writes the state that is already durable when the crash happens.
	prepare func(t *testing.T, w *IndexedWorld)
	// mutate is the work the crash interrupts. It must end in a Checkpoint.
	mutate func(t *testing.T, w *IndexedWorld) error
}

func crashScenarios(t *testing.T) []crashScenario {
	reg := testRegistry(t)
	stone := rid(t, reg, block.Stone{})
	dirt := rid(t, reg, block.Dirt{})
	meta1, _ := marshalNBT(map[string]any{"name": "before"})
	meta2, _ := marshalNBT(map[string]any{"name": "after-the-crash-window"})
	b, ok := world.BiomeByName("desert")
	if !ok {
		t.Fatal("the biome registry is empty: the crash window would not cover a biome segment")
	}
	desert := uint32(b.EncodeBiome())

	// The interesting checkpoint is one that writes every frame kind: a new
	// record, a block palette segment (a block the old checkpoint never saw), a
	// biome segment, a meta frame, the directory and the footer.
	full := func(t *testing.T, w *IndexedWorld) error {
		c := tinyColumn(t, reg, 1, 0, dirt)
		c.Col.Chunk.SetBiome(0, -64, 0, desert)
		if err := w.Store(c); err != nil {
			return err
		}
		if err := w.SetMeta(meta2, []byte("v2"), nil); err != nil {
			return err
		}
		return w.Checkpoint()
	}
	prepare := func(t *testing.T, w *IndexedWorld) {
		t.Helper()
		if err := w.Store(tinyColumn(t, reg, 0, 0, stone)); err != nil {
			t.Fatal(err)
		}
		if err := w.SetMeta(meta1, []byte("v1"), nil); err != nil {
			t.Fatal(err)
		}
		if err := w.Checkpoint(); err != nil {
			t.Fatal(err)
		}
	}
	return []crashScenario{
		{name: "compressed", opts: Options{Compression: CompressionDefault}, prepare: prepare, mutate: full},
		{name: "uncompressed", opts: Options{Compression: CompressionNone}, prepare: prepare, mutate: full},
		{
			// A checkpoint that only replaces a record: no new palette entry, no
			// metadata change. The directory and footer are the whole of it, and
			// the record it supersedes is still on disk, so a crash here is the
			// case where two complete records compete.
			name: "overwrite-only",
			opts: Options{Compression: CompressionDefault},
			prepare: func(t *testing.T, w *IndexedWorld) {
				t.Helper()
				if err := w.Store(tinyColumn(t, reg, 0, 0, stone)); err != nil {
					t.Fatal(err)
				}
				if err := w.Checkpoint(); err != nil {
					t.Fatal(err)
				}
			},
			mutate: func(t *testing.T, w *IndexedWorld) error {
				if err := w.Store(tinyColumn(t, reg, 0, 0, dirt)); err != nil {
					return err
				}
				return w.Checkpoint()
			},
		},
	}
}

// prepared builds the file image that is durable before the crash window
// opens, and returns it with the state it opens to.
func (sc crashScenario) prepared(t *testing.T, reg world.BlockRegistry) ([]byte, worldState) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pre.pile")
	w, err := CreateIndexed(path, reg, sc.opts)
	if err != nil {
		t.Fatal(err)
	}
	sc.prepare(t, w)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	img, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return img, captureState(t, path, reg, "prepared file")
}

// runMutation restores the prepared image at path, opens it on f (which the
// caller has wrapped), runs the mutation and closes the raw handle. The
// IndexedWorld is deliberately never Closed: a crashed process does not get to
// run a shutdown checkpoint.
func (sc crashScenario) runMutation(t *testing.T, path string, pre []byte, reg world.BlockRegistry, wrap func(*os.File) *crashFile) *crashFile {
	t.Helper()
	if err := os.WriteFile(path, pre, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	cf := wrap(f)
	w, err := openIndexedOn(cf, path, reg, false)
	if err != nil {
		_ = cf.Close()
		t.Fatalf("prepared image did not reopen: %v", err)
	}
	_ = sc.mutate(t, w)
	if err := cf.Close(); err != nil {
		t.Fatal(err)
	}
	return cf
}

// TestCheckpointTornWriteExhaustive crashes the process at every byte position
// of every write a checkpoint issues, and at every fsync and truncate, and
// requires the file that is left to open at either the old checkpoint or the
// new one.
//
// Exhaustive is meant literally: for a write of n bytes it runs n+1 crashes,
// one per prefix length, not a sample. The record frames are one-block columns
// precisely so that this stays affordable.
func TestCheckpointTornWriteExhaustive(t *testing.T) {
	reg := testRegistry(t)
	for _, sc := range crashScenarios(t) {
		t.Run(sc.name, func(t *testing.T) {
			pre, oldState := sc.prepared(t, reg)
			dir := t.TempDir()
			path := filepath.Join(dir, "world.pile")

			// A fault-free run: the operation trace to crash inside, and the
			// state the new checkpoint is supposed to reach.
			rec := sc.runMutation(t, path, pre, reg, recordFile)
			newState := captureState(t, path, reg, "uninterrupted checkpoint")
			if newState.equal(oldState) {
				t.Fatal("the mutation changed nothing: there is no crash window to test")
			}
			if len(rec.ops) == 0 {
				t.Fatal("no filesystem operations recorded")
			}

			positions := 0
			for i, op := range rec.ops {
				// Every prefix length for a write, including the empty one (the
				// write never reached the disk) and the whole of it (the write
				// landed and the process died immediately after). Non-write
				// operations have exactly one crash position.
				for p := 0; p <= op.n(); p++ {
					positions++
					sc.runMutation(t, path, pre, reg, func(f *os.File) *crashFile {
						return newCrashFile(f, i, p)
					})
					got := captureState(t, path, reg,
						fmt.Sprintf("crash at op %d (%s) after %d bytes", i, op, p))
					if !got.equal(oldState) && !got.equal(newState) {
						t.Fatalf("crash at op %d (%s) after %d bytes left a file that is neither checkpoint\n got: %v\n old: %v\n new: %v",
							i, op, p, got, oldState, newState)
					}
				}
			}
			t.Logf("%d crash positions over %d operations: %v", positions, len(rec.ops), opKinds(rec.ops))
		})
	}
}

// TestCheckpointSurvivesUnsyncedWriteLoss is the other half of the crash
// model. A prefix of the byte stream is not the only thing a crash can leave:
// writes issued since the last successful fsync may reach the platter in any
// order, or not at all. This drives every subset of every in-flight group.
//
// It is what makes the "fsync before the footer" ordering of §5.6 testable at
// all — under a prefix model the ordering is unobservable, because a prefix
// containing the footer contains everything written before it.
func TestCheckpointSurvivesUnsyncedWriteLoss(t *testing.T) {
	reg := testRegistry(t)
	for _, sc := range crashScenarios(t) {
		t.Run(sc.name, func(t *testing.T) {
			pre, oldState := sc.prepared(t, reg)
			dir := t.TempDir()
			path := filepath.Join(dir, "world.pile")
			rec := sc.runMutation(t, path, pre, reg, recordFile)
			newState := captureState(t, path, reg, "uninterrupted checkpoint")

			groups := syncGroups(rec.ops)
			cases := 0
			for g, group := range groups {
				// Everything in an earlier group is durable; this group's writes
				// survive in an arbitrary subset; nothing later happened.
				var durable []int
				for _, earlier := range groups[:g] {
					durable = append(durable, earlier...)
				}
				for mask := 0; mask < 1<<len(group); mask++ {
					keep := map[int]bool{}
					for _, i := range durable {
						keep[i] = true
					}
					for bit, i := range group {
						if mask&(1<<bit) != 0 {
							keep[i] = true
						}
					}
					img := replayImage(pre, rec.ops, func(i int) bool { return keep[i] })
					if err := os.WriteFile(path, img, 0o600); err != nil {
						t.Fatal(err)
					}
					cases++
					what := fmt.Sprintf("group %d subset %b", g, mask)
					got := captureState(t, path, reg, what)
					if !got.equal(oldState) && !got.equal(newState) {
						t.Fatalf("%s left a file that is neither checkpoint\n got: %v\n old: %v\n new: %v",
							what, got, oldState, newState)
					}
				}
			}
			t.Logf("%d in-flight loss cases over %d sync groups", cases, len(groups))
		})
	}
}

// TestCrashedCheckpointLeavesAWritableFile: recovering is not enough if the
// recovered file cannot be written to again. After a crash at every write
// position, reopening read-write and checkpointing must succeed and must keep
// whatever the recovery adopted.
func TestCrashedCheckpointLeavesAWritableFile(t *testing.T) {
	reg := testRegistry(t)
	sc := crashScenarios(t)[0]
	pre, _ := sc.prepared(t, reg)
	dir := t.TempDir()
	path := filepath.Join(dir, "world.pile")
	rec := sc.runMutation(t, path, pre, reg, recordFile)

	stone := rid(t, reg, block.Stone{})
	for i, op := range rec.ops {
		for _, p := range []int{0, op.n() / 2, op.n()} {
			if op.n() == 0 && p != 0 {
				continue
			}
			sc.runMutation(t, path, pre, reg, func(f *os.File) *crashFile {
				return newCrashFile(f, i, p)
			})
			before := captureState(t, path, reg, "after crash")
			w, err := OpenIndexed(path, reg, false)
			if err != nil {
				t.Fatalf("crash at op %d/%d bytes: reopen read-write: %v", i, p, err)
			}
			if err := w.Store(tinyColumn(t, reg, 7, 7, stone)); err != nil {
				t.Fatalf("crash at op %d/%d bytes: store after recovery: %v", i, p, err)
			}
			if err := w.Close(); err != nil {
				t.Fatalf("crash at op %d/%d bytes: checkpoint after recovery: %v", i, p, err)
			}
			after := captureState(t, path, reg, "after recovery and a fresh store")
			if len(after.chunks) != len(before.chunks)+1 {
				t.Fatalf("crash at op %d/%d bytes: a store after recovery lost content: %v -> %v",
					i, p, before, after)
			}
			for k, h := range before.chunks {
				if after.chunks[k] != h {
					t.Fatalf("crash at op %d/%d bytes: recovery-then-append changed chunk %v", i, p, k)
				}
			}
		}
	}
}

// TestCheckpointRetriesAfterTransientWriteFailure covers the other kind of
// filesystem failure: the write fails but the process lives on. A full disk
// that is then emptied, or one bad sector, must leave a world that saves
// cleanly on the next attempt — and the file it produces must end at its own
// footer, or reopening it reports a recovery that nothing damaged.
func TestCheckpointRetriesAfterTransientWriteFailure(t *testing.T) {
	reg := testRegistry(t)
	sc := crashScenarios(t)[0]
	pre, _ := sc.prepared(t, reg)
	dir := t.TempDir()
	path := filepath.Join(dir, "world.pile")
	rec := sc.runMutation(t, path, pre, reg, recordFile)
	newState := captureState(t, path, reg, "uninterrupted checkpoint")

	for i, op := range rec.ops {
		// A whole write that still reports failure is the position that matters:
		// it is the one that leaves bytes past the writer's logical end.
		for _, p := range []int{0, op.n() / 2, op.n()} {
			if op.n() == 0 && p != 0 {
				continue
			}
			what := fmt.Sprintf("transient failure at op %d (%s) after %d bytes", i, op, p)
			if err := os.WriteFile(path, pre, 0o600); err != nil {
				t.Fatal(err)
			}
			f, err := os.OpenFile(path, os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			cf := newTransientFile(f, i, p)
			w, err := openIndexedOn(cf, path, reg, false)
			if err != nil {
				t.Fatalf("%s: prepared image did not reopen: %v", what, err)
			}
			_ = sc.mutate(t, w)
			cf.crashAt = -1 // the fault clears; the caller retries
			if err := sc.mutate(t, w); err != nil {
				t.Fatalf("%s: retried save failed: %v", what, err)
			}
			if err := w.Close(); err != nil {
				t.Fatalf("%s: close after retry: %v", what, err)
			}
			r, err := OpenIndexed(path, reg, true)
			if err != nil {
				t.Fatalf("%s: retried file does not open: %v", what, err)
			}
			recovered := r.Recovered()
			_ = r.Close()
			if recovered {
				t.Fatalf("%s: a completed save reports recovery, so it does not end at its own footer", what)
			}
			got := captureState(t, path, reg, what)
			if !maps.Equal(got.chunks, newState.chunks) || got.settings != newState.settings || got.userData != newState.userData {
				t.Fatalf("%s: retried save produced\n got: %v\nwant: %v", what, got, newState)
			}
			if got.gen < newState.gen {
				t.Fatalf("%s: generation went backwards: %d < %d", what, got.gen, newState.gen)
			}
		}
	}
}

func opKinds(ops []fsOp) []string {
	out := make([]string, len(ops))
	for i, op := range ops {
		out[i] = op.String()
	}
	return out
}

// Recovery paths.
//
// §5.6 gives recovery three ways to reach a checkpoint — the footer at EOF,
// the backward scan for `ELIP`, and the `prevFooter` chain — and one rule for
// choosing between them: the newest candidate that validates completely wins,
// and a candidate whose referenced frames fail validation falls back to the
// next older one. These drive each of those in turn.

// TestRecoveryFollowsPrevFooterChain builds a file whose newest checkpoints all
// reference one damaged shared frame, and enough of them that the backward
// scan's 64-candidate cap is spent before it reaches a good one. Only the
// prevFooter chain can get there.
func TestRecoveryFollowsPrevFooterChain(t *testing.T) {
	reg := testRegistry(t)
	path := filepath.Join(t.TempDir(), "world.pile")
	stone := rid(t, reg, block.Stone{})
	meta1, _ := marshalNBT(map[string]any{"name": "reachable-only-by-the-chain"})
	meta2, _ := marshalNBT(map[string]any{"name": "doomed"})

	w, err := CreateIndexed(path, reg, Options{Compression: CompressionDefault})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Store(tinyColumn(t, reg, 0, 0, stone)); err != nil {
		t.Fatal(err)
	}
	if err := w.SetMeta(meta1, []byte("good"), nil); err != nil {
		t.Fatal(err)
	}
	if err := w.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	goodGen := w.Generation()

	// From here on every checkpoint shares one meta frame.
	if err := w.SetMeta(meta2, []byte("doomed"), nil); err != nil {
		t.Fatal(err)
	}
	// One past the scan's candidate cap, so the scan cannot reach the good
	// checkpoint even after every candidate it does find is rejected.
	const doomed = 70
	if doomed <= 64 {
		t.Fatal("the run of doomed checkpoints must exceed footerCandidates' cap")
	}
	for i := range int32(doomed) {
		if err := w.Store(tinyColumn(t, reg, i+1, 0, stone)); err != nil {
			t.Fatal(err)
		}
		if err := w.Checkpoint(); err != nil {
			t.Fatal(err)
		}
	}
	doomedMeta := w.metaRef
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if doomedMeta.length == 0 {
		t.Fatal("no meta frame to damage")
	}
	flipByte(t, path, doomedMeta.off+int64(doomedMeta.length)/2)

	r, err := OpenIndexed(path, reg, true)
	if err != nil {
		t.Fatalf("recovery failed outright: %v", err)
	}
	defer r.Close()
	if !r.Recovered() {
		t.Fatal("recovery not reported")
	}
	if r.Generation() != goodGen {
		t.Fatalf("adopted generation %d, want %d (the last checkpoint before the damaged meta frame)", r.Generation(), goodGen)
	}
	if _, u, _ := r.Meta(); string(u) != "good" {
		t.Fatalf("adopted metadata %q, want %q", u, "good")
	}
	if r.ChunkCount() != 1 {
		t.Fatalf("adopted chunk count %d, want 1", r.ChunkCount())
	}
}

// TestRecoveryRejectsFooterWhoseHashFails: a footer that is structurally
// perfect but whose checkpoint hash does not verify is not a checkpoint.
func TestRecoveryRejectsFooterWhoseHashFails(t *testing.T) {
	reg := testRegistry(t)
	for _, field := range []struct {
		name string
		at   int64 // offset within the footer
	}{
		{"stored hash", 0},
		{"directory offset", 8},
		{"generation", 24},
		{"prevFooter", 32},
	} {
		t.Run(field.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "world.pile")
			stone := rid(t, reg, block.Stone{})
			w, err := CreateIndexed(path, reg, Options{Compression: CompressionDefault})
			if err != nil {
				t.Fatal(err)
			}
			if err := w.Store(tinyColumn(t, reg, 0, 0, stone)); err != nil {
				t.Fatal(err)
			}
			if err := w.Checkpoint(); err != nil {
				t.Fatal(err)
			}
			goodGen := w.Generation()
			if err := w.Store(tinyColumn(t, reg, 1, 0, stone)); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil { // the checkpoint about to be broken
				t.Fatal(err)
			}
			st, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			flipByte(t, path, st.Size()-footerSize+field.at)

			r, err := OpenIndexed(path, reg, true)
			if err != nil {
				t.Fatalf("recovery failed outright: %v", err)
			}
			defer r.Close()
			if !r.Recovered() {
				t.Fatal("a footer that fails its hash was adopted without reporting recovery")
			}
			if r.Generation() != goodGen {
				t.Fatalf("adopted generation %d, want the previous checkpoint's %d", r.Generation(), goodGen)
			}
			if r.ChunkCount() != 1 {
				t.Fatalf("adopted chunk count %d, want 1", r.ChunkCount())
			}
		})
	}
}

// TestRecoveryRejectsDirectoryNamingARecordPastEOF: the directory frame's own
// hash proves only that the directory is the one that was written, not that
// what it says is true. An entry reaching past the end of the file must take
// the whole checkpoint down, not a read.
//
// The two shapes are separate rules and are refused by separate checks, which
// is why they are separate cases: an offset outside the file is caught as the
// offset chain is accumulated, while an offset inside it with a length running
// past the end is caught only afterwards, and it is the one that costs a
// four-gigabyte allocation in verifyRecords if it is not.
func TestRecoveryRejectsDirectoryNamingARecordPastEOF(t *testing.T) {
	reg := testRegistry(t)
	for _, tc := range []struct {
		name    string
		forge   func(w *IndexedWorld, e dirEntry) dirEntry
		wantErr string
	}{
		{"offset past EOF", func(w *IndexedWorld, e dirEntry) dirEntry {
			e.off = w.end + 4096
			return e
		}, "is outside the file"},
		{"length runs past EOF", func(w *IndexedWorld, e dirEntry) dirEntry {
			// The offset is a real one inside the file; only the length lies,
			// and it claims the largest the format can express.
			e.length = maxFrameLen
			return e
		}, "out of file bounds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "world.pile")
			stone := rid(t, reg, block.Stone{})
			w, err := CreateIndexed(path, reg, Options{Compression: CompressionDefault})
			if err != nil {
				t.Fatal(err)
			}
			if err := w.Store(tinyColumn(t, reg, 0, 0, stone)); err != nil {
				t.Fatal(err)
			}
			if err := w.Checkpoint(); err != nil {
				t.Fatal(err)
			}
			goodGen := w.Generation()

			// A real checkpoint — correct hash, correct footer, correct
			// everything — whose directory names a record that is not there.
			if err := w.Store(tinyColumn(t, reg, 1, 0, stone)); err != nil {
				t.Fatal(err)
			}
			key := [2]int32{1, 0}
			w.dir[key] = tc.forge(w, w.dir[key])
			w.recordsDirty = true
			if err := w.Checkpoint(); err != nil {
				t.Fatal(err)
			}
			forged := w.dirRef
			if err := w.closeFile(); err != nil {
				t.Fatal(err)
			}

			r, err := OpenIndexed(path, reg, true)
			if err != nil {
				t.Fatalf("recovery failed outright: %v", err)
			}
			defer r.Close()
			if !r.Recovered() {
				t.Fatal("a directory naming a record past EOF was adopted")
			}
			if r.Generation() != goodGen {
				t.Fatalf("adopted generation %d, want %d", r.Generation(), goodGen)
			}
			if r.ChunkCount() != 1 {
				t.Fatalf("adopted chunk count %d, want 1", r.ChunkCount())
			}
			// Falling back is not the same as refusing it for the right reason.
			// Reading the record would fail at EOF whatever the directory said,
			// so without this the bounds checks could both be deleted and the
			// test would stay green — which is exactly what happened the first
			// time it was written. Drive the directory in on its own and name
			// the check that has to refuse it.
			r.resetLoadedState()
			err = r.loadDirectory(forged)
			if err == nil {
				t.Fatal("the forged directory loaded without complaint")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("refused with %q, want a message containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestRecoveryKeepsNewestCheckpointWhenAnOlderLinkIsCorrupt: the fallback must
// not run backwards. A file whose newest checkpoint is intact opens at it even
// when an older link in the chain is rubble.
func TestRecoveryKeepsNewestCheckpointWhenAnOlderLinkIsCorrupt(t *testing.T) {
	reg := testRegistry(t)
	path := filepath.Join(t.TempDir(), "world.pile")
	stone := rid(t, reg, block.Stone{})
	w, err := CreateIndexed(path, reg, Options{Compression: CompressionDefault})
	if err != nil {
		t.Fatal(err)
	}
	var footers []int64
	for i := range int32(4) {
		if err := w.Store(tinyColumn(t, reg, i, 0, stone)); err != nil {
			t.Fatal(err)
		}
		if err := w.Checkpoint(); err != nil {
			t.Fatal(err)
		}
		footers = append(footers, w.prevFooterOff)
	}
	newestGen, newestCount := w.Generation(), w.ChunkCount()
	if err := w.closeFile(); err != nil {
		t.Fatal(err)
	}
	// Break the middle checkpoint's footer. Its chain link is what the newest
	// one names, so a reader that walked the chain before trusting the newest
	// footer would land on the wrong one.
	flipByte(t, path, footers[1])

	r, err := OpenIndexed(path, reg, true)
	if err != nil {
		t.Fatalf("open failed: %v", err)
	}
	defer r.Close()
	if r.Recovered() {
		t.Fatal("an intact newest checkpoint was reported as recovered")
	}
	if r.Generation() != newestGen || r.ChunkCount() != newestCount {
		t.Fatalf("adopted gen %d/%d chunks, want %d/%d", r.Generation(), r.ChunkCount(), newestGen, newestCount)
	}
}

// flipByte inverts one byte of a file in place.
func flipByte(t *testing.T, path string, off int64) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b := []byte{0}
	if _, err := f.ReadAt(b, off); err != nil {
		t.Fatalf("read at %d: %v", off, err)
	}
	b[0] ^= 0xFF
	if _, err := f.WriteAt(b, off); err != nil {
		t.Fatal(err)
	}
}

// TestFailedStoreLeavesNoTailPastTheFooter: a partial write that the process
// survives leaves bytes past the writer's logical end, and nothing later
// necessarily overwrites them — a Store that fails is not followed by a
// checkpoint at all when nothing else is dirty. The file would then end in
// bytes that are not a footer, and reopening it would report a recovery from
// damage that never happened, telling the operator they lost data they did
// not. appendFrame truncates for that reason.
func TestFailedStoreLeavesNoTailPastTheFooter(t *testing.T) {
	reg := testRegistry(t)
	stone := rid(t, reg, block.Stone{})
	dirt := rid(t, reg, block.Dirt{})
	path := filepath.Join(t.TempDir(), "world.pile")

	w, err := CreateIndexed(path, reg, Options{Compression: CompressionDefault})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Store(tinyColumn(t, reg, 0, 0, stone)); err != nil {
		t.Fatal(err)
	}
	if err := w.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := w.closeFile(); err != nil {
		t.Fatal(err)
	}
	clean, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Fail the first write — the record frame — half way through, and let the
	// process live. Nothing is dirty afterwards, so no checkpoint follows.
	cf := newTransientFile(f, 0, 24)
	w2, err := openIndexedOn(cf, path, reg, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := w2.Store(tinyColumn(t, reg, 1, 0, dirt)); err == nil {
		t.Fatal("the faulted store reported success")
	}
	if err := w2.Close(); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if st.Size() != clean.Size() {
		t.Fatalf("a failed store left the file %d bytes long, want %d: the tail past the footer was not discarded",
			st.Size(), clean.Size())
	}

	r, err := OpenIndexed(path, reg, true)
	if err != nil {
		t.Fatalf("reopen after a failed store: %v", err)
	}
	defer r.Close()
	if r.Recovered() {
		t.Fatal("a failed store made the next open report a recovery, though no checkpoint was damaged")
	}
	if r.ChunkCount() != 1 {
		t.Fatalf("chunk count %d, want 1", r.ChunkCount())
	}
}

// TestRecoveryRejectsCheckpointWhoseSharedFrameHashFails: every frame the
// directory references carries its own xxHash64 (§5.5), so damage to a palette
// segment, the metadata or the dictionary takes the checkpoint down instead of
// silently changing world content.
//
// The file is uncompressed on purpose. In a compressed one zstd's own frame
// checksum refuses the damage first, and the directory's hash — the thing under
// test — is never reached.
func TestRecoveryRejectsCheckpointWhoseSharedFrameHashFails(t *testing.T) {
	reg := testRegistry(t)
	path := filepath.Join(t.TempDir(), "world.pile")
	stone := rid(t, reg, block.Stone{})
	filler := make([]byte, 64)
	for i := range filler {
		filler[i] = 'A'
	}

	w, err := CreateIndexed(path, reg, Options{Compression: CompressionNone})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Store(tinyColumn(t, reg, 0, 0, stone)); err != nil {
		t.Fatal(err)
	}
	if err := w.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	goodGen := w.Generation()
	// A second checkpoint with a meta frame of its own, so the damage below
	// belongs to it alone and the first checkpoint stays whole.
	if err := w.SetMeta(nil, filler, nil); err != nil {
		t.Fatal(err)
	}
	if err := w.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	metaRef, dirRef := w.metaRef, w.dirRef
	if err := w.closeFile(); err != nil {
		t.Fatal(err)
	}

	// Damage a byte of opaque user data: it changes the frame's hash and
	// nothing else. Every structural rule the meta frame has still holds, so
	// the recorded hash is the only thing that can refuse it.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	frame := raw[metaRef.off : metaRef.off+int64(metaRef.length)]
	at := bytes.Index(frame, filler[:8])
	if at < 0 {
		t.Fatal("the filler is not where the meta frame was expected to hold it")
	}
	flipByte(t, path, metaRef.off+int64(at)+4)

	r, err := OpenIndexed(path, reg, true)
	if err != nil {
		t.Fatalf("recovery failed outright: %v", err)
	}
	defer r.Close()
	if !r.Recovered() {
		t.Fatal("a checkpoint whose meta frame fails its hash was adopted")
	}
	if r.Generation() != goodGen {
		t.Fatalf("adopted generation %d, want %d", r.Generation(), goodGen)
	}
	// And refused for the recorded hash, not for some structural accident.
	r.resetLoadedState()
	err = r.loadDirectory(dirRef)
	if !errors.Is(err, ErrChecksum) {
		t.Fatalf("refused with %v, want ErrChecksum", err)
	}
}

// TestCheckpointDoesNotReportSuccessWithAnUnsyncedFooter: a checkpoint is only
// taken once its footer is durable. If the fsync fails, the retry has to
// re-sync — and while the disk is still refusing, the retry has to keep saying
// so.
//
// The case that makes this its own mechanism rather than a consequence of the
// dirty flags is a metadata-only checkpoint. Every other dirty flag is cleared
// as its frame is written, before the footer sync, so a second Checkpoint would
// find nothing to do and return success over a footer that is not on the disk.
func TestCheckpointDoesNotReportSuccessWithAnUnsyncedFooter(t *testing.T) {
	reg := testRegistry(t)
	stone := rid(t, reg, block.Stone{})
	path := filepath.Join(t.TempDir(), "world.pile")
	w, err := CreateIndexed(path, reg, Options{Compression: CompressionDefault})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Store(tinyColumn(t, reg, 0, 0, stone)); err != nil {
		t.Fatal(err)
	}
	if err := w.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := w.closeFile(); err != nil {
		t.Fatal(err)
	}

	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	cf := &crashFile{f: f, crashAt: -1, failSyncs: true}
	w2, err := openIndexedOn(cf, path, reg, false)
	if err != nil {
		t.Fatal(err)
	}
	settings, _ := marshalNBT(map[string]any{"name": "metadata only"})
	if err := w2.SetMeta(settings, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := w2.Checkpoint(); err == nil {
		t.Fatal("a checkpoint whose fsync failed reported success")
	}
	if err := w2.Checkpoint(); err == nil {
		t.Fatal("a retried checkpoint reported success while its footer was still unsynced")
	}
	// The disk comes back: the retry must now make it durable.
	cf.failSyncs = false
	if err := w2.Checkpoint(); err != nil {
		t.Fatalf("checkpoint after the fault cleared: %v", err)
	}
	if err := w2.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := OpenIndexed(path, reg, true)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if s, _, _ := r.Meta(); len(s) == 0 {
		t.Fatal("the metadata the retried checkpoint promised is not in the file")
	}
}

// TestCreateIndexedRefusesAnExistingPath: CreateIndexed stages nothing and
// removes nothing — it creates, and a path that already exists is an error.
// O_EXCL is what makes that true of a symlink as well, which os.Create would
// have followed. Compact removes its staging name before calling this, so the
// flag is the only thing standing behind that removal if it ever races.
func TestCreateIndexedRefusesAnExistingPath(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(outside, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "world.pile")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("this filesystem has no symlinks: %v", err)
	}
	if w, err := CreateIndexed(link, reg, Options{Compression: CompressionDefault}); err == nil {
		_ = w.Close()
		t.Fatal("CreateIndexed created a world over an existing path")
	}
	got, err := os.ReadFile(outside)
	if err != nil || string(got) != "untouched" {
		t.Fatalf("CreateIndexed wrote through the symlink: %q %v", got, err)
	}
}
