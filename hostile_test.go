package pile

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
	"github.com/oriumgames/pile/format"
)

// Hostile input driven through the provider surface: pile.Open and the
// world.Provider methods, against files on disk.
//
// format/hostile_test.go drives the four decoders. Nothing drove the layer a
// caller actually calls, and the provider adds column caching, the
// preserved-state sidecar, snapshots, a mover and template handling on top of
// an audited decoder. Every file built here is a file a conforming reader must
// accept unless it says otherwise: "hostile" means the content is absurd, not
// that the bytes are broken, because a broken file is the case the codec's
// matrix already covers and an absurd one is what actually arrives.

// writeHostileWorld writes a solid world file for a dimension. The columns are
// whatever the caller built, so the file is legal by construction.
func writeHostileWorld(t testing.TB, path string, dim format.Dimension, cols []format.Column) {
	t.Helper()
	reg := testRegistry(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	d := &format.WorldData{Columns: cols, Dimension: dim}
	if err := format.WriteWorld(f, d, reg, format.Options{Compression: format.CompressionBest}); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// emptyColumns builds n columns that hold nothing: the shape SECURITY.md's
// 1,161-byte file is made of.
func emptyColumns(t testing.TB, n int32) []format.Column {
	t.Helper()
	reg := testRegistry(t)
	cols := make([]format.Column, 0, n)
	for x := range n {
		cols = append(cols, format.Column{X: x, Z: 0, Col: &chunk.Column{Chunk: chunk.New(reg, cube.Range{0, 15})}})
	}
	return cols
}

// entityColumns builds cols columns of per entities each. Every entity is
// identical, so the file compresses to almost nothing while the decode is
// forced to build one live map per entity.
func entityColumns(t testing.TB, cols, per int) []format.Column {
	t.Helper()
	reg := testRegistry(t)
	out := make([]format.Column, 0, cols)
	for c := range cols {
		col := &chunk.Column{Chunk: chunk.New(reg, cube.Range{0, 15})}
		col.Entities = make([]chunk.Entity, per)
		for i := range col.Entities {
			col.Entities[i] = chunk.Entity{ID: 0, Data: map[string]any{}}
		}
		out = append(out, format.Column{X: int32(c), Z: 0, Col: col})
	}
	return out
}

// TestOpenHoldsTheCeilingOnAHostileColumnFlood: the shape the caller's ceiling
// exists for, driven through Open rather than through ReadWorld.
//
// A file of empty columns is the cheapest way to buy live objects: eleven bytes
// on the wire become a recRaw, a chunk.Chunk and a Column. The provider must
// refuse it under a ceiling, and must refuse it as policy — ErrDecodeBudget and
// not ErrCorrupt — because a caller that quarantines corrupt files must not
// quarantine this one.
func TestOpenHoldsTheCeilingOnAHostileColumnFlood(t *testing.T) {
	dir := t.TempDir()
	const n = 4096
	writeHostileWorld(t, filepath.Join(dir, "overworld.pile"), format.Overworld, emptyColumns(t, n))

	// Without a ceiling it opens: the file is legal.
	p, err := Open(dir, ReadOnly())
	if err != nil {
		t.Fatalf("the file is legal and must open with no ceiling: %v", err)
	}
	if got := p.ChunkCount(world.Overworld); got != n {
		t.Fatalf("got %d columns, want %d", got, n)
	}
	_ = p.Close()

	// Under a ceiling that admits a few hundred columns it does not.
	_, err = Open(dir, ReadOnly(), MaxDecodedBytes(256<<10))
	if err == nil {
		t.Fatal("a 4096-column file opened under a 256 KiB ceiling")
	}
	if !errors.Is(err, format.ErrDecodeBudget) {
		t.Fatalf("refused, but not as a budget refusal: %v", err)
	}
	if errors.Is(err, format.ErrCorrupt) {
		t.Fatalf("a budget refusal must not claim the file is corrupt: %v", err)
	}
}

// TestEntityFloodEscapesTheDecodeCeiling records a gap, not a guard.
//
// MaxDecodedBytes charges decoded columns at 1,024 bytes and decoded section
// storages at 128, and charges nothing at all for entities, block entities or
// scheduled updates. §8 bounds those per chunk (1,048,576 each) and the column
// ceiling multiplies rather than bounds them, so a file of four columns — 9,409
// bytes on disk — decodes into 4,194,304 live entities and about 1.5 GiB, and
// the only ceiling that refuses it is one below 4,096 bytes, which is a world
// of three columns.
//
// The test therefore asserts what is true today: the file is accepted under a
// ceiling the caller cannot usefully lower, and the decode costs orders of
// magnitude more than the ceiling permitted. When somebody charges the
// per-chunk collections, this test goes red, and that is the intended way to
// find it. SECURITY.md, "What a caller still cannot bound".
func TestEntityFloodEscapesTheDecodeCeiling(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a multi-million entity world")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "overworld.pile")
	const cols, per = 2, 1 << 20
	writeHostileWorld(t, path, format.Overworld, entityColumns(t, cols, per))
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	// A ceiling that admits a 64-column world: 64 KiB. The file names two
	// columns, so it is charged 2,048 bytes of a 65,536-byte budget.
	const ceiling = 64 << 10
	runtime.GC()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	p, err := Open(dir, ReadOnly(), MaxDecodedBytes(ceiling))
	if err != nil {
		t.Fatalf("the ceiling now binds on the entity flood — the gap this test records is closed, "+
			"and SECURITY.md's \"What a caller still cannot bound\" needs updating: %v", err)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	retained := int64(after.HeapAlloc) - int64(before.HeapAlloc)

	n := 0
	for _, c := range p.Columns(world.Overworld) {
		n += len(c.Entities)
	}
	if n != cols*per {
		t.Fatalf("got %d entities, want %d", n, cols*per)
	}
	if retained < 64<<20 {
		// If this is ever small, the fixture stopped reaching the case and the
		// test proves nothing.
		t.Fatalf("the fixture retained only %d bytes; it no longer demonstrates the amplification", retained)
	}
	t.Logf("%d bytes on disk, ceiling %d, %d entities, %d bytes retained (%.0fx the ceiling)",
		fi.Size(), int64(ceiling), n, retained, float64(retained)/float64(ceiling))
	_ = p.Close()
}

// TestOpenRefusesAHostileDimensionAndNamesIt: a world directory whose
// overworld is hostile and whose nether is fine must fail as a whole, name the
// file that failed, and hand back no provider. A partial open would serve a
// world with a dimension silently missing, and the next save would write that
// truncated world back over the file that failed to load.
func TestOpenRefusesAHostileDimensionAndNamesIt(t *testing.T) {
	for _, which := range []string{"overworld.pile", "nether.pile"} {
		t.Run(which, func(t *testing.T) {
			dir := t.TempDir()
			good := emptyColumns(t, 1)
			writeHostileWorld(t, filepath.Join(dir, "overworld.pile"), format.Overworld, good)
			writeHostileWorld(t, filepath.Join(dir, "nether.pile"), format.Nether, good)
			// Truncate one of them: a torn transfer of a world somebody sent.
			raw, err := os.ReadFile(filepath.Join(dir, which))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, which), raw[:len(raw)/2], 0o644); err != nil {
				t.Fatal(err)
			}
			p, err := Open(dir, ReadOnly())
			if err == nil {
				_ = p.Close()
				t.Fatalf("a world with a truncated %s opened", which)
			}
			if p != nil {
				t.Fatal("Open returned both an error and a provider")
			}
			if !strings.Contains(err.Error(), which) {
				t.Fatalf("the error does not name the file that failed: %v", err)
			}
		})
	}
}

// TestOpenRefusesAnUnreadableDimensionFile: a file the process cannot read is
// an error naming it, not an empty world. Serving an empty world here would
// mean the first save wrote an empty file over content that was merely
// unreadable.
func TestOpenRefusesAnUnreadableDimensionFile(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads everything")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "overworld.pile")
	writeHostileWorld(t, path, format.Overworld, emptyColumns(t, 1))
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
	p, err := Open(dir, ReadOnly())
	if err == nil {
		_ = p.Close()
		t.Fatal("an unreadable dimension file opened as a world")
	}
	if !strings.Contains(err.Error(), "overworld.pile") {
		t.Fatalf("the error does not name the file: %v", err)
	}
}

// TestProviderAdaptsAColumnWithAForeignVerticalRange: a file may declare any
// 16-aligned span its record layout can express, which need not be the span the
// dimension has. dragonfly indexes its sub chunk array from the dimension's
// range with no bounds check, so a column handed over unadapted panics the
// server on the first block access.
func TestProviderAdaptsAColumnWithAForeignVerticalRange(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	ch := chunk.New(reg, cube.Range{0, 15})
	writeHostileWorld(t, filepath.Join(dir, "overworld.pile"), format.Overworld,
		[]format.Column{{X: 0, Z: 0, Col: &chunk.Column{Chunk: ch}}})

	p, err := Open(dir, ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	col, err := p.LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	if got := col.Chunk.Range(); got != world.Overworld.Range() {
		t.Fatalf("LoadColumn served a column with range %v; the dimension is %v", got, world.Overworld.Range())
	}
	// Block access across the dimension's whole range must not panic.
	for y := world.Overworld.Range()[0]; y <= world.Overworld.Range()[1]; y += 16 {
		_ = col.Chunk.Block(0, int16(y), 0, 0)
	}
}

// TestProviderSurvivesAbsurdMetadataBlobs: settings and markers are NBT written
// by whoever made the file, and the provider decodes them into Go values it
// then hands to a server.
//
// §7.1 fixes the tag of every settings key, so a wrongly typed one is an
// invalid file the decoder already refuses. What it does not fix is the values,
// and it fixes nothing at all about markers, whose schema is this package's.
// Every field there may be the wrong type or missing, and none of it may panic
// or stop the world from being saved back.
func TestProviderSurvivesAbsurdMetadataBlobs(t *testing.T) {
	reg := testRegistry(t)
	settings, err := format.MarshalNBT(map[string]any{
		"name":            strings.Repeat("n", 1<<15-1), // see TestLongNBTStringsRoundTrip
		"spawnX":          int32(-1 << 31),              // the far corner of the world
		"spawnY":          int32(1 << 30),
		"spawnZ":          int32(1<<31 - 1),
		"defaultGameMode": int32(-1),      // no such game mode
		"difficulty":      int32(1 << 20), // no such difficulty
		"tickRange":       int32(-1),
		"unknownKey":      int64(5), // must survive a load/save round trip
	})
	if err != nil {
		t.Fatal(err)
	}
	// §7.2 fixes the markers schema too, so the interesting hostile marker list
	// is one that satisfies it and is still absurd: non-finite positions, empty
	// strings, and far more markers than any map has.
	const markerN = 20000
	list := make([]map[string]any, 0, markerN)
	list = append(list, map[string]any{"name": "", "kind": "", "pos": []any{0.0, 0.0, 0.0}})
	list = append(list, map[string]any{
		"name": "\x01nan", "kind": "spawn",
		"pos":   []any{math.NaN(), math.Inf(1), math.Inf(-1)},
		"extra": int64(1),
	})
	for i := range markerN - 2 {
		list = append(list, map[string]any{
			"name": fmt.Sprintf("m%08d", i), "kind": "npc",
			"pos": []any{float64(i), 0.0, 0.0},
		})
	}
	markers, err := format.MarshalNBT(map[string]any{"markers": list})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "overworld.pile"))
	if err != nil {
		t.Fatal(err)
	}
	d := &format.WorldData{
		Settings: settings, Markers: markers,
		Columns: emptyColumns(t, 1), Dimension: format.Overworld,
	}
	if err := format.WriteWorld(f, d, reg, format.Options{}); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	p, err := Open(dir)
	if err != nil {
		t.Fatalf("a world with absurd metadata did not open: %v", err)
	}
	s := p.Settings()
	if s.Spawn.X() != -1<<31 || s.Spawn.Z() != 1<<31-1 {
		t.Fatalf("spawn came back as %v", s.Spawn)
	}
	// An unknown game mode or difficulty id keeps the default rather than
	// producing a nil interface the server would dereference.
	if s.DefaultGameMode == nil || s.Difficulty == nil {
		t.Fatalf("an out-of-range game mode or difficulty produced a nil value: %v, %v",
			s.DefaultGameMode, s.Difficulty)
	}
	ms := p.Markers()
	if len(ms) != markerN {
		t.Fatalf("got %d markers, want %d", len(ms), markerN)
	}
	// It must save and reopen: absurd metadata must not become an unwritable
	// world. SaveSettings is what makes the save a real one — a provider whose
	// metadata nobody touched skips every clean dimension, so a bare Save here
	// rewrote nothing and the reopen below was reading the fixture back.
	p.SaveSettings(s)
	if err := p.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	q, err := Open(dir, ReadOnly())
	if err != nil {
		t.Fatalf("the saved world does not reopen: %v", err)
	}
	defer q.Close()
	// The key this build does not know must have survived, or an older server
	// opening a newer world erases its settings.
	if got := q.Settings().Spawn; got != s.Spawn {
		t.Fatalf("spawn changed across the round trip: %v, was %v", got, s.Spawn)
	}
	if got := len(q.Markers()); got != markerN {
		t.Fatalf("got %d markers after the round trip, want %d", got, markerN)
	}
}

// TestIndexedProviderSurvivesTheFileChangingUnderneath: an append-mode provider
// holds the dimension file open for its lifetime and reads records out of it on
// demand. A file replaced or truncated by another process (or by the person who
// sent it, over a network filesystem) must produce errors from the methods that
// touch it, and no panic and no silent wrong answer.
func TestIndexedProviderSurvivesTheFileChangingUnderneath(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	p, err := Open(dir, AppendMode())
	if err != nil {
		t.Fatal(err)
	}
	for x := range int32(32) {
		if err := p.StoreColumn(world.ChunkPos{x, 0}, world.Overworld, testColumn(t, reg)); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	q, err := Open(dir, AppendMode(), ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	path := filepath.Join(dir, "overworld.pile")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Rewrite the body under the open handle: same length, different bytes, so
	// every offset the directory holds still lands inside the file.
	scrambled := make([]byte, len(raw))
	copy(scrambled, raw)
	for i := 64; i < len(scrambled)-64; i++ {
		scrambled[i] ^= 0xFF
	}
	if err := os.WriteFile(path, scrambled, 0o644); err != nil {
		t.Fatal(err)
	}

	errs, ok := 0, 0
	for x := range int32(32) {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("LoadColumn panicked on a file rewritten underneath it: %v", r)
				}
			}()
			if _, err := q.LoadColumn(world.ChunkPos{x, 0}, world.Overworld); err != nil {
				errs++
			} else {
				ok++
			}
		}()
	}
	if errs == 0 {
		t.Fatal("every record still read cleanly after the file was rewritten: the per-record hash is not being checked")
	}
	// The iterator has to report it too, or a backup of this world would be
	// short and nothing would say so.
	for range q.Columns(world.Overworld) {
	}
	if err := q.IterError(); err == nil {
		t.Fatal("Columns iterated a rewritten file without reporting an error")
	}
	t.Logf("%d records refused, %d still readable", errs, ok)
}

// TestRollbackFromAJunkSnapshotLeavesTheWorldIntact: `snapshots/` travels
// inside a world directory somebody hands you, and Rollback is the
// operator-facing recovery command. It used to delete the world's dimension
// files, rename the snapshot's over them, and only then try to read the result:
// a snapshot holding 22 bytes of text destroyed the world, permanently, and the
// directory would not open again.
func TestRollbackFromAJunkSnapshotLeavesTheWorldIntact(t *testing.T) {
	reg := testRegistry(t)
	for _, junk := range []struct {
		name    string
		content []byte
	}{
		{"not a pile file", []byte("not a pile file at all")},
		{"empty", nil},
		{"truncated header", []byte("PILE\x02\x00")},
	} {
		t.Run(junk.name, func(t *testing.T) {
			dir := t.TempDir()
			p, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, testColumn(t, reg)); err != nil {
				t.Fatal(err)
			}
			if err := p.Save(); err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(filepath.Join(dir, "overworld.pile"))
			if err != nil {
				t.Fatal(err)
			}

			snap := filepath.Join(dir, snapshotsDirName, "shipped")
			if err := os.MkdirAll(snap, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(snap, "overworld.pile"), junk.content, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := p.Rollback("shipped"); err == nil {
				t.Fatal("rolling back onto a snapshot that is not a world was reported as success")
			}
			// The provider must still work.
			if got := p.ChunkCount(world.Overworld); got != 1 {
				t.Fatalf("the provider holds %d columns after the refused rollback, want 1", got)
			}
			if _, err := p.LoadColumn(world.ChunkPos{0, 0}, world.Overworld); err != nil {
				t.Fatalf("the column is gone after the refused rollback: %v", err)
			}
			_ = p.Close()

			// And so must the directory.
			got, err := os.ReadFile(filepath.Join(dir, "overworld.pile"))
			if err != nil {
				t.Fatalf("the world file is gone: %v", err)
			}
			if string(got) != string(want) {
				t.Fatalf("the world file changed: %d bytes, was %d", len(got), len(want))
			}
			q, err := Open(dir, ReadOnly())
			if err != nil {
				t.Fatalf("the world no longer opens: %v", err)
			}
			_ = q.Close()
			// No staging litter left behind.
			for _, suffix := range []string{".rollback", ".rollbackold"} {
				m, _ := filepath.Glob(filepath.Join(dir, "*"+suffix))
				if len(m) != 0 {
					t.Fatalf("staging files left behind: %v", m)
				}
			}
		})
	}
}

// TestRollbackSkipsANonRegularSnapshotEntry: copyFile opens the source, and
// os.Open on a FIFO blocks until somebody writes to it. A snapshot directory is
// not necessarily something this process produced.
func TestRollbackSkipsANonRegularSnapshotEntry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no mkfifo")
	}
	reg := testRegistry(t)
	dir := t.TempDir()
	p, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, testColumn(t, reg)); err != nil {
		t.Fatal(err)
	}
	if err := p.Snapshot("good"); err != nil {
		t.Fatal(err)
	}
	snap := filepath.Join(dir, snapshotsDirName, "good")
	if err := mkfifo(filepath.Join(snap, "trap.pile")); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- p.Rollback("good") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("rollback: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Rollback blocked on a FIFO in the snapshot directory")
	}
	_ = p.Close()
}

// TestSnapshotsIgnoresJunkEntries: the snapshots directory is listed, not
// trusted. Files, dangling symlinks and unreadable directories must not turn a
// listing into an error or a panic.
func TestSnapshotsIgnoresJunkEntries(t *testing.T) {
	dir := t.TempDir()
	p, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	snaps := filepath.Join(dir, snapshotsDirName)
	if err := os.MkdirAll(filepath.Join(snaps, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snaps, "a-file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Symlink("/nowhere", filepath.Join(snaps, "dangling"))
	names, err := p.Snapshots()
	if err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	if len(names) != 1 || names[0] != "real" {
		t.Fatalf("got %v, want [real]", names)
	}
}

// TestColumnCacheIsBoundedByWeight: the decoded-column cache used to be bounded
// by entry count alone, on the reasoning that columns are all of a size. One
// legal column may carry 1,048,576 entities, so CacheColumns(n) over a hostile
// world pinned whatever n of those weighed.
func TestColumnCacheIsBoundedByWeight(t *testing.T) {
	reg := testRegistry(t)
	c := newColumnCache(64)
	heavy := &chunk.Column{
		Chunk:    chunk.New(reg, cube.Range{0, 15}),
		Entities: make([]chunk.Entity, 1<<20),
	}
	for i := range int32(64) {
		c.Put([2]int32{i, 0}, cacheEntry{col: heavy})
	}
	if c.Weight() > columnCacheBytes+cacheEntrySize(cacheEntry{col: heavy}) {
		t.Fatalf("the cache holds %d bytes, budget is %d", c.Weight(), columnCacheBytes)
	}
	if c.Len() >= 64 {
		t.Fatalf("all %d oversized entries were kept; the weight budget is not binding", c.Len())
	}
	// The bound is the constant, not whatever the constant happens to be: a
	// budget asserted against itself catches nothing.
	if columnCacheBytes != 256<<20 {
		t.Fatalf("columnCacheBytes is %d, this test was written for %d", columnCacheBytes, 256<<20)
	}
	// And an ordinary working set is untouched by it.
	d := newColumnCache(64)
	light := &chunk.Column{Chunk: chunk.New(reg, cube.Range{0, 15})}
	for i := range int32(64) {
		d.Put([2]int32{i, 0}, cacheEntry{col: light})
	}
	if d.Len() != 64 {
		t.Fatalf("the weight budget evicted %d of 64 ordinary columns", 64-d.Len())
	}
}

// TestExtractStructureRefusesAnUnrepresentableRegion: the region size was
// int32(hi - lo + 1), computed in int and truncated, so a span of 2^32+1 became
// a size of 1 and the chunk loop that follows then walked the real span —
// 268,435,457 positions on one axis. `pile extract --min 0,0,0 --max
// 4294967296,0,0` never returned.
func TestExtractStructureRefusesAnUnrepresentableRegion(t *testing.T) {
	reg := testRegistry(t)
	p := NewMemory(Registry(reg))
	if err := p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, testColumn(t, reg)); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name   string
		lo, hi cube.Pos
	}{
		{"wraps int32", cube.Pos{0, 0, 0}, cube.Pos{1 << 32, 0, 0}},
		{"wraps int32 on Z", cube.Pos{0, 0, 0}, cube.Pos{0, 0, 1 << 32}},
		{"inverted", cube.Pos{10, 0, 10}, cube.Pos{0, 0, 0}},
		{"whole int64 span", cube.Pos{-1 << 62, 0, 0}, cube.Pos{1 << 62, 0, 0}},
	} {
		t.Run(c.name, func(t *testing.T) {
			done := make(chan error, 1)
			go func() {
				_, err := ExtractStructure(p, world.Overworld, c.lo, c.hi)
				done <- err
			}()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("an unrepresentable region was accepted")
				}
			case <-time.After(20 * time.Second):
				t.Fatal("ExtractStructure did not return: the region bound is not being applied before the chunk loop")
			}
		})
	}
	// A region that really is small still works.
	s, err := ExtractStructure(p, world.Overworld, cube.Pos{0, 0, 0}, cube.Pos{15, 15, 15})
	if err != nil {
		t.Fatalf("an ordinary region was refused: %v", err)
	}
	if got := s.Dimensions(); got != [3]int{16, 16, 16} {
		t.Fatalf("got %v, want [16 16 16]", got)
	}
}

// TestConcurrentAccessToAHostileWorld drives LoadColumn, StoreColumn, Columns,
// ChunkUserData and Save at once over a world whose content is absurd, in both
// modes. The point is not throughput: it is that the cache-publish rechecks and
// the sidecar carry-over are exercised against columns that are large and
// numerous rather than against the tidy ones every other test uses.
func TestConcurrentAccessToAHostileWorld(t *testing.T) {
	reg := testRegistry(t)
	for _, mode := range []struct {
		name string
		opts []Option
	}{
		{"solid", []Option{CacheColumns(8)}},
		{"append", []Option{AppendMode(), CacheColumns(8)}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			dir := t.TempDir()
			p, err := Open(dir, mode.opts...)
			if err != nil {
				t.Fatal(err)
			}
			// Seed with columns carrying enough per-chunk content that the
			// caches have something worth holding.
			seed := func(x int32) *chunk.Column {
				col := &chunk.Column{Chunk: chunk.New(reg, cube.Range{-64, 319})}
				col.Entities = make([]chunk.Entity, 64)
				for i := range col.Entities {
					col.Entities[i] = chunk.Entity{ID: int64(i), Data: map[string]any{"identifier": "minecraft:cow"}}
				}
				return col
			}
			for x := range int32(16) {
				if err := p.StoreColumn(world.ChunkPos{x, 0}, world.Overworld, seed(x)); err != nil {
					t.Fatal(err)
				}
			}
			if err := p.Save(); err != nil {
				t.Fatal(err)
			}

			var wg sync.WaitGroup
			stop := make(chan struct{})
			fail := make(chan error, 16)
			for g := range 8 {
				wg.Add(1)
				go func(g int) {
					defer wg.Done()
					for i := 0; ; i++ {
						select {
						case <-stop:
							return
						default:
						}
						x := int32((i + g) % 16)
						switch i % 5 {
						case 0:
							if _, err := p.LoadColumn(world.ChunkPos{x, 0}, world.Overworld); err != nil &&
								!strings.Contains(err.Error(), "leveldb: not found") {
								fail <- err
								return
							}
						case 1:
							if err := p.StoreColumn(world.ChunkPos{x, 0}, world.Overworld, seed(x)); err != nil {
								fail <- err
								return
							}
						case 2:
							for range p.Columns(world.Overworld) {
							}
						case 3:
							_ = p.ChunkUserData(world.ChunkPos{x, 0}, world.Overworld)
						case 4:
							if err := p.Save(); err != nil {
								fail <- err
								return
							}
						}
					}
				}(g)
			}
			time.Sleep(time.Second)
			close(stop)
			wg.Wait()
			select {
			case err := <-fail:
				t.Fatalf("concurrent access failed: %v", err)
			default:
			}
			if err := p.IterError(); err != nil {
				t.Fatalf("an iteration reported an error: %v", err)
			}
			if err := p.Close(); err != nil {
				t.Fatal(err)
			}
			q, err := Open(dir, append([]Option{ReadOnly()}, mode.opts...)...)
			if err != nil {
				t.Fatalf("the world does not reopen after concurrent access: %v", err)
			}
			if got := q.ChunkCount(world.Overworld); got != 16 {
				t.Fatalf("got %d columns after reopening, want 16", got)
			}
			_ = q.Close()
		})
	}
}

// TestLongNBTStringsRoundTrip pins the largest string a world can actually
// carry through a save and a reload.
//
// It is 32,767 bytes, and the reason is a defect rather than a rule: §8 puts
// maxStringLen at 65,535 and format.MarshalNBT accepts up to that, but the
// Bedrock NBT encoding under it writes a string's length as a signed 16-bit
// value, so at 32,768 the length wraps negative and format.UnmarshalNBT refuses
// the blob the marshaller just produced.
//
// The block entity is the case that loses data: settings and markers are
// re-decoded by the writer's own §7.1/§7.2 schema checks, so those fail at
// Close, while a block entity's NBT goes to the wire straight from the
// marshaller — StoreColumn and Close both report success and the world never
// opens again.
//
// This test holds the boundary that works. The half above it is recorded in
// SECURITY.md, "Found here, fixable only in format", and the fix belongs in
// format/nbt.go's marshaller, which must refuse what its own reader will.
func TestLongNBTStringsRoundTrip(t *testing.T) {
	reg := testRegistry(t)
	long := strings.Repeat("m", 1<<15-1)
	dir := t.TempDir()
	p, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	col := testColumn(t, reg)
	col.BlockEntities[0].Data["long"] = long
	if err := p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, col); err != nil {
		t.Fatal(err)
	}
	p.SetMarker(Marker{Name: long, Kind: "spawn"})
	if err := p.Close(); err != nil {
		t.Fatalf("a world holding %d-byte strings did not save: %v", len(long), err)
	}
	q, err := Open(dir, ReadOnly())
	if err != nil {
		t.Fatalf("a world holding %d-byte strings does not reopen: %v", len(long), err)
	}
	defer q.Close()
	ms := q.Markers()
	if len(ms) != 1 || ms[0].Name != long {
		t.Fatalf("the marker did not survive: %d markers", len(ms))
	}
	got, err := q.LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.BlockEntities) != 1 || got.BlockEntities[0].Data["long"] != long {
		t.Fatal("the block entity's long string did not survive")
	}
}

// TestIteratorsHandOutCopies: every column crossing the provider boundary is
// copied, in both directions. dragonfly mutates the columns it is given, and a
// yielded column that aliased the provider's own state would let a consumer —
// a converter, a renderer, a backup — edit the world it is reading.
func TestIteratorsHandOutCopies(t *testing.T) {
	reg := testRegistry(t)
	stone := reg.BlockRuntimeID(nil) + 1
	for _, mode := range []struct {
		name string
		opts []Option
	}{
		{"solid", nil},
		{"append", []Option{AppendMode()}},
		// The cached path is a different one: it hands out a copy of what the
		// cache owns rather than the decode's own result.
		{"append cached", []Option{AppendMode(), CacheColumns(8)}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			dir := t.TempDir()
			p, err := Open(dir, mode.opts...)
			if err != nil {
				t.Fatal(err)
			}
			col := &chunk.Column{Chunk: chunk.New(reg, cube.Range{-64, 319})}
			col.Chunk.SetBlock(0, 0, 0, 0, stone)
			for x := range int32(4) {
				if err := p.StoreColumn(world.ChunkPos{x, 0}, world.Overworld, col); err != nil {
					t.Fatal(err)
				}
			}
			// Mutate everything the iterator hands over.
			for _, c := range p.Columns(world.Overworld) {
				c.Chunk.SetBlock(0, 0, 0, 0, reg.AirRuntimeID())
				c.Entities = append(c.Entities, chunk.Entity{ID: 1, Data: map[string]any{}})
			}
			// And everything LoadColumn hands over.
			for x := range int32(4) {
				c, err := p.LoadColumn(world.ChunkPos{x, 0}, world.Overworld)
				if err != nil {
					t.Fatal(err)
				}
				c.Chunk.SetBlock(0, 0, 0, 0, reg.AirRuntimeID())
			}
			for x := range int32(4) {
				c, err := p.LoadColumn(world.ChunkPos{x, 0}, world.Overworld)
				if err != nil {
					t.Fatal(err)
				}
				if got := c.Chunk.Block(0, 0, 0, 0); got != stone {
					t.Fatalf("column %d was edited through a handed-out copy", x)
				}
				if len(c.Entities) != 0 {
					t.Fatalf("column %d gained %d entities through a handed-out copy", x, len(c.Entities))
				}
			}
			if err := p.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
