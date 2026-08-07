package pile

import (
	"path/filepath"
	"testing"

	"github.com/df-mc/dragonfly/server/world"
)

// TestMCDBRoundTrip drives the conversion both ways through the library API.
//
// It exists because the conversion used to live in package main, where the
// library's own suite could not reach it: the CLI had a test, and the code a
// caller would use had none because there was no code a caller could use.
func TestMCDBRoundTrip(t *testing.T) {
	reg := testRegistry(t)

	// A pile world to start from.
	src := t.TempDir()
	p, err := Open(src, Registry(reg))
	if err != nil {
		t.Fatal(err)
	}
	want := map[world.ChunkPos]bool{}
	for _, pos := range []world.ChunkPos{{0, 0}, {1, 2}, {-3, 5}} {
		if err := p.StoreColumn(pos, world.Overworld, testColumn(t, reg, pos)); err != nil {
			t.Fatal(err)
		}
		want[pos] = true
	}
	if err := p.StoreColumn(world.ChunkPos{0, 0}, world.Nether, testColumn(t, reg, world.ChunkPos{0, 0})); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	// pile -> mcdb -> pile.
	db := filepath.Join(t.TempDir(), "db")
	n, err := ExportMCDB(src, db, Registry(reg))
	if err != nil {
		t.Fatal(err)
	}
	if n != len(want)+1 {
		t.Fatalf("exported %d columns, want %d", n, len(want)+1)
	}
	if !IsMCDB(db) {
		t.Error("the exported directory is not recognised as an mcdb world")
	}

	back := filepath.Join(t.TempDir(), "back")
	m, err := ImportMCDB(db, back, Registry(reg))
	if err != nil {
		t.Fatal(err)
	}
	if m != n {
		t.Fatalf("imported %d columns, exported %d", m, n)
	}
	if !IsPile(back) {
		t.Error("the imported directory is not recognised as a pile world")
	}

	q, err := Open(back, Registry(reg), ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	got := map[world.ChunkPos]bool{}
	for pos := range q.Columns(world.Overworld) {
		got[pos] = true
	}
	if err := q.IterError(); err != nil {
		t.Fatal(err)
	}
	_ = q.Close()
	if len(got) != len(want) {
		t.Fatalf("overworld came back with %d columns, want %d", len(got), len(want))
	}
	for pos := range want {
		if !got[pos] {
			t.Errorf("column %v did not survive the round trip", pos)
		}
	}

	// Both directions refuse to write into an existing world of their own kind,
	// because a conversion that merged two worlds would leave no way to tell
	// which columns came from where.
	if _, err := ExportMCDB(src, db, Registry(reg)); err == nil {
		t.Error("ExportMCDB wrote into an existing mcdb world")
	}
	if _, err := ImportMCDB(db, back, Registry(reg)); err == nil {
		t.Error("ImportMCDB wrote into an existing pile world")
	}
}
