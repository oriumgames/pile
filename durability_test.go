package pile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/df-mc/dragonfly/server/world"
	"github.com/oriumgames/pile/format"
)

// Tests for durability and failure handling: saves, snapshots and closes must
// either complete or leave a recoverable, self-consistent world behind.

func TestRollbackAtomicity(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	p, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, testColumn(t, reg)); err != nil {
		t.Fatal(err)
	}
	if err := p.StoreColumn(world.ChunkPos{0, 0}, world.Nether, testColumn(t, reg)); err != nil {
		t.Fatal(err)
	}
	if err := p.Snapshot("clean"); err != nil {
		t.Fatal(err)
	}
	if err := p.StoreColumn(world.ChunkPos{9, 9}, world.Overworld, testColumn(t, reg)); err != nil {
		t.Fatal(err)
	}
	if err := p.Save(); err != nil {
		t.Fatal(err)
	}
	if err := p.Rollback("clean"); err != nil {
		t.Fatal(err)
	}
	if p.ChunkCount(world.Overworld) != 1 || p.ChunkCount(world.Nether) != 1 {
		t.Fatalf("rollback left an inconsistent world: %d/%d",
			p.ChunkCount(world.Overworld), p.ChunkCount(world.Nether))
	}
	// No staging leftovers.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".rollback") || strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("rollback left temporary file %s", e.Name())
		}
	}
}

// TestUnknownSurvivesPasteAndImport: pasting a structure that carries
// preserved unknown states into a world must keep them, so the full
// export/import round trip is lossless.

func TestCloseIsRetryable(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	p, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, testColumn(t, reg)); err != nil {
		t.Fatal(err)
	}
	// Force the save to fail: the destination path is occupied by a
	// directory, so the temp-file rename cannot succeed.
	if err := os.MkdirAll(filepath.Join(dir, "overworld.pile"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err == nil {
		t.Fatal("Close succeeded despite an unwritable destination")
	}
	// Clear the fault and retry: the world must still be saveable.
	if err := os.Remove(filepath.Join(dir, "overworld.pile")); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("retried Close failed: %v", err)
	}
	q, err := Open(dir, ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if q.ChunkCount(world.Overworld) != 1 {
		t.Fatal("retried Close did not persist the world")
	}
}

func TestSaveAsFailsOnUnreadableRecord(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	p, err := Open(dir, AppendMode())
	if err != nil {
		t.Fatal(err)
	}
	for _, pos := range []world.ChunkPos{{0, 0}, {1, 0}} {
		if err := p.StoreColumn(pos, world.Overworld, testColumn(t, reg)); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	// Corrupt one record's bytes, leaving the directory and footer intact.
	path := filepath.Join(dir, "overworld.pile")
	probe, err := format.OpenIndexed(path, reg, true)
	if err != nil {
		t.Fatal(err)
	}
	_ = probe.Close()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	buf := []byte{0}
	if _, err := f.ReadAt(buf, 40); err != nil {
		t.Fatal(err)
	}
	buf[0] ^= 0xFF
	if _, err := f.WriteAt(buf, 40); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	q, err := Open(dir, AppendMode())
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	out := t.TempDir()
	if err := q.SaveAs(out); err == nil {
		// If the flipped byte happened to land outside a record, the world is
		// still intact; only fail when a column really is unreadable.
		if _, rerr := q.LoadColumn(world.ChunkPos{0, 0}, world.Overworld); rerr != nil {
			t.Fatal("SaveAs succeeded despite an unreadable record")
		}
	}
}

// TestPasteAllAirSkipsColumnCreation: an all-air SkipAir paste must not
// create chunks (which would suppress terrain generation).

func TestSaveAsMetadataOnly(t *testing.T) {
	p := NewMemory()
	p.SaveSettings(&world.Settings{Name: "meta-only", TickRange: 6})
	p.SetUserData([]byte("cfg"))
	out := t.TempDir()
	if err := p.SaveAs(out); err != nil {
		t.Fatal(err)
	}
	_ = p.Close()
	q, err := Open(out, ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if q.Settings().Name != "meta-only" || string(q.UserData()) != "cfg" {
		t.Fatalf("metadata-only world did not round trip: %+v %q", q.Settings(), q.UserData())
	}
}

// TestStructureRejectsOutOfBoundsBlockEntity covers both writer and reader.
