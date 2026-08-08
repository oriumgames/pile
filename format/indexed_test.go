package format

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func createIndexedWorld(t *testing.T, path string, cols ...Column) {
	t.Helper()
	reg := testRegistry(t)
	w, err := CreateIndexed(path, reg, Options{Compression: CompressionDefault})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cols {
		if err := w.Store(c); err != nil {
			t.Fatal(err)
		}
	}
	settings, _ := marshalNBT(map[string]any{"name": "indexed-test"})
	w.SetMeta(settings, []byte("meta"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestIndexedRoundTrip(t *testing.T) {
	reg := testRegistry(t)
	path := filepath.Join(t.TempDir(), "overworld.pile")
	want := []Column{
		buildTestColumn(t, reg, 0, 0),
		buildTestColumn(t, reg, 3, -2),
		buildTestColumn(t, reg, -7, 9),
	}
	createIndexedWorld(t, path, want...)

	w, err := OpenIndexed(path, reg, true)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if w.ChunkCount() != 3 {
		t.Fatalf("chunk count = %d, want 3", w.ChunkCount())
	}
	settings, userData := w.Meta()
	m, err := unmarshalNBT(settings)
	if err != nil || m["name"] != "indexed-test" {
		t.Fatalf("meta settings = %v (%v)", m, err)
	}
	if !bytes.Equal(userData, []byte("meta")) {
		t.Fatal("meta user data mismatch")
	}
	for _, wc := range want {
		got, err := w.Column(wc.X, wc.Z)
		if err != nil {
			t.Fatalf("Column(%d,%d): %v", wc.X, wc.Z, err)
		}
		compareColumns(t, wc, got)
	}
	if _, err := w.Column(99, 99); !errors.Is(err, ErrNoColumn) {
		t.Fatalf("missing column error = %v", err)
	}
}

func TestIndexedAppendReopen(t *testing.T) {
	reg := testRegistry(t)
	path := filepath.Join(t.TempDir(), "overworld.pile")
	createIndexedWorld(t, path, buildTestColumn(t, reg, 0, 0))

	// Reopen read-write and append another column.
	w, err := OpenIndexed(path, reg, false)
	if err != nil {
		t.Fatal(err)
	}
	c2 := buildTestColumn(t, reg, 5, 5)
	if err := w.Store(c2); err != nil {
		t.Fatal(err)
	}
	gen := w.Generation()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	w2, err := OpenIndexed(path, reg, true)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()
	if w2.ChunkCount() != 2 {
		t.Fatalf("after reopen: chunk count = %d, want 2", w2.ChunkCount())
	}
	if w2.Generation() <= gen {
		t.Fatalf("generation did not advance: %d", w2.Generation())
	}
	got, err := w2.Column(5, 5)
	if err != nil {
		t.Fatal(err)
	}
	compareColumns(t, c2, got)
}

func TestIndexedGarbageAndCompact(t *testing.T) {
	reg := testRegistry(t)
	path := filepath.Join(t.TempDir(), "overworld.pile")
	w, err := CreateIndexed(path, reg, Options{Compression: CompressionDefault})
	if err != nil {
		t.Fatal(err)
	}
	// Overwrite the same column many times to accumulate garbage.
	for range 10 {
		if err := w.Store(buildTestColumn(t, reg, 0, 0)); err != nil {
			t.Fatal(err)
		}
		if err := w.Checkpoint(); err != nil {
			t.Fatal(err)
		}
	}
	if ratio := w.GarbageRatio(); ratio < 0.5 {
		t.Fatalf("expected heavy garbage, ratio %.2f", ratio)
	}
	stBefore, _ := os.Stat(path)

	if err := w.Compact(); err != nil {
		t.Fatal(err)
	}
	// A freshly compacted tiny file still carries its creation checkpoint
	// (~100 bytes) as dead weight, so allow a little slack.
	if ratio := w.GarbageRatio(); ratio > 0.2 {
		t.Fatalf("garbage after compact: %.2f", ratio)
	}
	stAfter, _ := os.Stat(path)
	if stAfter.Size() >= stBefore.Size() {
		t.Fatalf("compact did not shrink file: %d -> %d", stBefore.Size(), stAfter.Size())
	}
	// Content survives compaction and further appends work.
	want := buildTestColumn(t, reg, 0, 0)
	got, err := w.Column(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	compareColumns(t, want, got)
	if err := w.Store(buildTestColumn(t, reg, 1, 1)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	w2, err := OpenIndexed(path, reg, true)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()
	if w2.ChunkCount() != 2 {
		t.Fatalf("chunk count after compact+append = %d, want 2", w2.ChunkCount())
	}
}

func TestIndexedTornWriteRecovery(t *testing.T) {
	reg := testRegistry(t)
	path := filepath.Join(t.TempDir(), "overworld.pile")
	w, err := CreateIndexed(path, reg, Options{Compression: CompressionDefault})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Store(buildTestColumn(t, reg, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if err := w.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	genGood := w.Generation()
	if err := w.Store(buildTestColumn(t, reg, 1, 0)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil { // second checkpoint
		t.Fatal(err)
	}

	// Tear the final checkpoint: chop half the footer off.
	st, _ := os.Stat(path)
	if err := os.Truncate(path, st.Size()-footerSize/2); err != nil {
		t.Fatal(err)
	}

	rw, err := OpenIndexed(path, reg, true)
	if err != nil {
		t.Fatalf("recovery failed: %v", err)
	}
	defer rw.Close()
	if rw.Generation() != genGood {
		t.Fatalf("recovered generation %d, want %d", rw.Generation(), genGood)
	}
	if rw.ChunkCount() != 1 {
		t.Fatalf("recovered chunk count %d, want 1 (pre-torn state)", rw.ChunkCount())
	}
	if _, err := rw.Column(0, 0); err != nil {
		t.Fatalf("recovered column unreadable: %v", err)
	}
}

func TestIndexedRecordCorruption(t *testing.T) {
	reg := testRegistry(t)
	path := filepath.Join(t.TempDir(), "overworld.pile")
	createIndexedWorld(t, path, buildTestColumn(t, reg, 0, 0))

	// Find the record frame offset via the directory, then flip a byte in it.
	probe, err := OpenIndexed(path, reg, true)
	if err != nil {
		t.Fatal(err)
	}
	entry := probe.dir[[2]int32{0, 0}]
	_ = probe.Close()
	if entry.length == 0 {
		t.Fatal("no directory entry for (0,0)")
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	buf := []byte{0}
	if _, err := f.ReadAt(buf, entry.off+int64(entry.length)/2); err != nil {
		t.Fatal(err)
	}
	buf[0] ^= 0xFF
	if _, err := f.WriteAt(buf, entry.off+int64(entry.length)/2); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	// A checkpoint referencing a damaged record is not adopted: recovery
	// falls back to an older one rather than serving corruption, and says so.
	w, err := OpenIndexed(path, reg, true)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if !w.Recovered() {
		t.Fatal("damaged checkpoint adopted without reporting recovery")
	}
	if _, err := w.Column(0, 0); !errors.Is(err, ErrNoColumn) && !errors.Is(err, ErrChecksum) {
		t.Fatalf("corrupted record error = %v, want ErrNoColumn (rolled back) or ErrChecksum", err)
	}
}

// TestIndexedRecordCorruptionFallsBack: when an older checkpoint holds an
// intact copy of the damaged record, recovery must reach it.
func TestIndexedRecordCorruptionFallsBack(t *testing.T) {
	reg := testRegistry(t)
	path := filepath.Join(t.TempDir(), "overworld.pile")
	w, err := CreateIndexed(path, reg, Options{Compression: CompressionNone})
	if err != nil {
		t.Fatal(err)
	}
	// Checkpoint 2 holds a good copy; checkpoint 3 replaces it with one we
	// then damage.
	if err := w.Store(buildTestColumn(t, reg, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if err := w.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	good := w.dir[[2]int32{0, 0}]
	if err := w.Store(buildTestColumn(t, reg, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	newer := good
	{
		probe, err := OpenIndexed(path, reg, true)
		if err != nil {
			t.Fatal(err)
		}
		newer = probe.dir[[2]int32{0, 0}]
		_ = probe.Close()
	}
	if newer.off == good.off {
		t.Fatal("expected the second store to append a new record frame")
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	buf := []byte{0}
	if _, err := f.ReadAt(buf, newer.off+int64(newer.length)/2); err != nil {
		t.Fatal(err)
	}
	buf[0] ^= 0xFF
	if _, err := f.WriteAt(buf, newer.off+int64(newer.length)/2); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	rw, err := OpenIndexed(path, reg, true)
	if err != nil {
		t.Fatal(err)
	}
	defer rw.Close()
	if !rw.Recovered() {
		t.Fatal("recovery not reported")
	}
	got, err := rw.Column(0, 0)
	if err != nil {
		t.Fatalf("older intact copy not recovered: %v", err)
	}
	compareColumns(t, buildTestColumn(t, reg, 0, 0), got)
}

// TestIndexedDictionary verifies that compaction trains a shared dictionary
// once there is enough material, and that dictionary files reopen and read
// correctly.
func TestIndexedDictionary(t *testing.T) {
	reg := testRegistry(t)
	path := filepath.Join(t.TempDir(), "overworld.pile")
	w, err := CreateIndexed(path, reg, Options{Compression: CompressionDefault})
	if err != nil {
		t.Fatal(err)
	}
	// Enough varied columns to clear the training thresholds.
	for i := range int32(40) {
		if err := w.Store(buildTestColumn(t, reg, i, i%5)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Compact(); err != nil {
		t.Fatal(err)
	}
	if !w.HasDict() {
		t.Fatal("compaction did not train a dictionary")
	}
	// Reads through the dictionary work, and appends after it too.
	want := buildTestColumn(t, reg, 3, 3)
	got, err := w.Column(3, 3)
	if err != nil {
		t.Fatal(err)
	}
	compareColumns(t, want, got)
	if err := w.Store(buildTestColumn(t, reg, 99, 0)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen: dictionary is loaded from the directory and everything reads.
	rw, err := OpenIndexed(path, reg, true)
	if err != nil {
		t.Fatal(err)
	}
	defer rw.Close()
	if !rw.HasDict() {
		t.Fatal("reopened file lost its dictionary")
	}
	if rw.ChunkCount() != 41 {
		t.Fatalf("chunk count = %d, want 41", rw.ChunkCount())
	}
	got2, err := rw.Column(99, 0)
	if err != nil {
		t.Fatal(err)
	}
	compareColumns(t, buildTestColumn(t, reg, 99, 0), got2)
}

// TestDictSampleStride: training a dictionary must never hold more than
// dictMaxSamples record bodies, and the ones it does hold have to be spread
// across the world rather than taken from the front of Morton order, or the
// dictionary describes one corner of the map and nothing else.
func TestDictSampleStride(t *testing.T) {
	for _, n := range []int{1, 16, 255, 256, 257, 1000, 1 << 20} {
		stride := dictSampleStride(n)
		if stride < 1 {
			t.Fatalf("n=%d: stride %d", n, stride)
		}
		taken, last := 0, -1
		for i := 0; i < n; i += stride {
			taken++
			last = i
		}
		if taken > dictMaxSamples {
			t.Fatalf("n=%d: %d samples exceeds the %d cap", n, taken, dictMaxSamples)
		}
		if n >= dictMinSamples && taken < dictMinSamples {
			t.Fatalf("n=%d: only %d samples, below the %d training minimum", n, taken, dictMinSamples)
		}
		// The last sample sits within one stride of the end, so the far side of
		// the world is represented too.
		if n > 0 && last < n-stride {
			t.Fatalf("n=%d stride=%d: sampling stopped at %d", n, stride, last)
		}
	}
}

// TestIndexedHostileFooter covers the overflow in the footer's directory
// bounds check: a tiny file must not drive a huge allocation.
func TestIndexedHostileFooter(t *testing.T) {
	reg := testRegistry(t)
	path := filepath.Join(t.TempDir(), "hostile.pile")

	hdr := &writer{}
	hdr.raw(headerMagic[:])
	hdr.u16(Version)
	hdr.u8(KindWorld)
	hdr.u8(ModeIndexed)
	hdr.u32(FlagUncompressed)
	hdr.i32(0)
	ftr := &writer{}
	ftr.u64(0)          // dir hash
	ftr.u64(headerSize) // dir offset
	ftr.u64(1<<63 - 1)  // dir length: overflows a naive bounds check
	ftr.u64(1)          // generation
	ftr.u64(0)          // prev footer
	ftr.raw(footerMagic[:])
	if err := os.WriteFile(path, append(hdr.bytes(), ftr.bytes()...), 0o644); err != nil {
		t.Fatal(err)
	}
	// Must be rejected cleanly, not panic or allocate gigabytes.
	if w, err := OpenIndexed(path, reg, true); err == nil {
		_ = w.Close()
		t.Fatal("hostile footer accepted")
	}
}

// TestIndexedSegmentVersioning: palette segments record the block version
// they were written at, so states survive a game-version bump.
func TestIndexedSegmentVersioning(t *testing.T) {
	reg := testRegistry(t)
	path := filepath.Join(t.TempDir(), "seg.pile")
	w, err := CreateIndexed(path, reg, Options{Compression: CompressionDefault})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Store(buildTestColumn(t, reg, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	// Reopening must resolve every state (no placeholder fallbacks).
	r, err := OpenIndexed(path, reg, true)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	unresolved, err := r.UnresolvedStates()
	if err != nil {
		t.Fatal(err)
	}
	if len(unresolved) != 0 {
		t.Fatalf("states failed to resolve after reopen: %v", unresolved)
	}
	got, err := r.Column(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	compareColumns(t, buildTestColumn(t, reg, 0, 0), got)
}

// TestIndexedRejectsOversizedMeta: metadata a reader would refuse must be
// rejected at SetMeta, not written into a checkpoint that then rolls back.
func TestIndexedRejectsOversizedMeta(t *testing.T) {
	reg := testRegistry(t)
	path := filepath.Join(t.TempDir(), "meta.pile")
	w, err := CreateIndexed(path, reg, Options{Compression: CompressionDefault})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Store(buildTestColumn(t, reg, 1, 0)); err != nil {
		t.Fatal(err)
	}
	if err := w.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := w.Store(buildTestColumn(t, reg, 2, 0)); err != nil {
		t.Fatal(err)
	}
	if err := w.SetMeta(nil, make([]byte, (16<<20)+1)); err == nil {
		t.Fatal("oversized metadata accepted")
	}
	if err := w.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	// Both chunks must survive: the rejected metadata never entered a frame.
	r, err := OpenIndexed(path, reg, true)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.ChunkCount() != 2 {
		t.Fatalf("chunk count = %d, want 2 (checkpoint rolled back)", r.ChunkCount())
	}
}

// TestIndexedCompactReadOnly: compaction must refuse a read-only world
// rather than rewriting the file underneath it.
func TestIndexedCompactReadOnly(t *testing.T) {
	reg := testRegistry(t)
	path := filepath.Join(t.TempDir(), "ro.pile")
	createIndexedWorld(t, path, buildTestColumn(t, reg, 0, 0))
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	w, err := OpenIndexed(path, reg, true)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := w.Compact(); !errors.Is(err, ErrReadOnlyFile) {
		t.Fatalf("Compact on a read-only world = %v, want ErrReadOnlyFile", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("read-only Compact modified the file")
	}
}

// TestIndexedStoreValidatesColumn: a record the decoder would reject must be
// refused at Store, not written and discovered on read.
func TestIndexedStoreValidatesColumn(t *testing.T) {
	reg := testRegistry(t)
	path := filepath.Join(t.TempDir(), "v.pile")
	w, err := CreateIndexed(path, reg, Options{Compression: CompressionNone})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	c := buildTestColumn(t, reg, 0, 0)
	c.UserData = make([]byte, (16<<20)+1)
	if err := w.Store(c); err == nil {
		t.Fatal("oversized chunk user data accepted")
	}
	if w.ChunkCount() != 0 {
		t.Fatal("rejected column was stored anyway")
	}
}

// TestCompactSurvivesDictionaryFailure: dictionary training is best effort
// and must never take down a compaction, whatever the samples look like.
func TestCompactSurvivesDictionaryFailure(t *testing.T) {
	reg := testRegistry(t)
	path := filepath.Join(t.TempDir(), "dictfail.pile")
	w, err := CreateIndexed(path, reg, Options{Compression: CompressionDefault})
	if err != nil {
		t.Fatal(err)
	}
	// Highly repetitive records: the upstream dictionary builder has been
	// observed to panic on sample sets like this.
	for i := range int32(24) {
		c := buildTestColumn(t, reg, i, 0)
		if err := w.Store(c); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Compact(); err != nil {
		t.Fatalf("compaction failed: %v", err)
	}
	if w.ChunkCount() != 24 {
		t.Fatalf("chunks after compaction = %d, want 24", w.ChunkCount())
	}
	for _, k := range w.Positions() {
		if _, err := w.Column(k[0], k[1]); err != nil {
			t.Fatalf("column (%d,%d) unreadable after compaction: %v", k[0], k[1], err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestIndexedSurvivesHeaderDamage: the directory carries the semantic header
// fields, so damage confined to the 16 physical header bytes must not
// invalidate every checkpoint.
func TestIndexedSurvivesHeaderDamage(t *testing.T) {
	reg := testRegistry(t)
	path := filepath.Join(t.TempDir(), "hdr.pile")
	want := buildTestColumn(t, reg, 3, -2)
	createIndexedWorld(t, path, want)

	// Corrupt the block version inside the header only.
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	b := []byte{0}
	if _, err := f.ReadAt(b, 12); err != nil {
		t.Fatal(err)
	}
	b[0] ^= 0xFF
	if _, err := f.WriteAt(b, 12); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	w, err := OpenIndexed(path, reg, true)
	if err != nil {
		t.Fatalf("header damage made the world unopenable: %v", err)
	}
	defer w.Close()
	if !w.HeaderDamaged() {
		t.Fatal("header damage was not reported")
	}
	if w.ChunkCount() != 1 {
		t.Fatalf("chunk count = %d, want 1", w.ChunkCount())
	}
	got, err := w.Column(3, -2)
	if err != nil {
		t.Fatal(err)
	}
	compareColumns(t, want, got)
}

// TestIndexedDirectoryPrologueAuthoritative: a directory whose prologue does
// not describe an indexed world is rejected, and a damaged newest directory
// falls back to the previous checkpoint rather than being trusted.
func TestIndexedDirectoryPrologueAuthoritative(t *testing.T) {
	reg := testRegistry(t)
	path := filepath.Join(t.TempDir(), "prologue.pile")
	w, err := CreateIndexed(path, reg, Options{Compression: CompressionNone})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Store(buildTestColumn(t, reg, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if err := w.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := w.Store(buildTestColumn(t, reg, 1, 0)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	probe, err := OpenIndexed(path, reg, true)
	if err != nil {
		t.Fatal(err)
	}
	dirOff := probe.dirRef.off
	_ = probe.Close()

	// Rewrite the newest directory's prologue to claim it is a structure.
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte{KindStructure}, dirOff); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	rw, err := OpenIndexed(path, reg, true)
	if err != nil {
		t.Fatalf("recovery to the previous checkpoint failed: %v", err)
	}
	defer rw.Close()
	if !rw.Recovered() {
		t.Fatal("falling back past the damaged checkpoint was not reported")
	}
	if rw.ChunkCount() != 1 {
		t.Fatalf("chunk count = %d, want 1 (the pre-damage checkpoint)", rw.ChunkCount())
	}
}

// TestSetMetaRefusesReadOnlyAndClosed: reporting success on a world that can
// never persist the change leaves Meta answering with something no reader will
// ever see.
func TestSetMetaRefusesReadOnlyAndClosed(t *testing.T) {
	reg := testRegistry(t)
	path := filepath.Join(t.TempDir(), "w.pile")
	w, err := CreateIndexed(path, reg, Options{Compression: CompressionNone})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Store(buildTestColumn(t, reg, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.SetMeta([]byte{0x0a, 0, 0, 0x00}, nil); err == nil {
		t.Error("SetMeta succeeded on a closed world")
	}

	ro, err := OpenIndexed(path, reg, true)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	before, _ := ro.Meta()
	if err := ro.SetMeta([]byte{0x0a, 0, 0, 0x00}, nil); !errors.Is(err, ErrReadOnlyFile) {
		t.Errorf("SetMeta on a read-only world: err = %v, want ErrReadOnlyFile", err)
	}
	if after, _ := ro.Meta(); !bytes.Equal(before, after) {
		t.Error("a refused SetMeta changed what Meta reports")
	}
}

// TestDictCodecShared: the codecs for a shared dictionary belong to the
// dictionary, not to the handle that installed it. Building a private encoder
// and decoder per open file cost around 11 MiB for every handle that wrote a
// frame, so a world with several dimensions open paid for the same dictionary
// several times over.
func TestDictCodecShared(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "overworld.pile")
	w, err := CreateIndexed(path, reg, Options{Compression: CompressionDefault})
	if err != nil {
		t.Fatal(err)
	}
	for i := range int32(40) {
		if err := w.Store(buildTestColumn(t, reg, i, i%5)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Compact(); err != nil {
		t.Fatal(err)
	}
	if !w.HasDict() {
		t.Fatal("compaction did not train a dictionary")
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(dir, "nether.pile")
	if err := os.WriteFile(copyPath, src, 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := OpenIndexed(path, reg, true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := OpenIndexed(copyPath, reg, true)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if a.dictCodec == nil || a.dictCodec != b.dictCodec {
		t.Fatalf("two handles on the same dictionary hold different codecs: %p vs %p", a.dictCodec, b.dictCodec)
	}
	// Both still read through it.
	for _, h := range []*IndexedWorld{a, b} {
		got, err := h.Column(3, 3)
		if err != nil {
			t.Fatal(err)
		}
		compareColumns(t, buildTestColumn(t, reg, 3, 3), got)
	}
}
