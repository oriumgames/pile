package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/df-mc/dragonfly/server/block"
	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/oriumgames/pile"
	"github.com/oriumgames/pile/format"
)

func TestMoveSpawnToCLI(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	b := pile.NewBuilder(reg, cube.Range{-64, 319})
	b.Fill(cube.Pos{100, 60, 100}, cube.Pos{110, 64, 110}, block.Stone{})
	b.Settings(&world.Settings{Name: "spawnto", Spawn: cube.Pos{105, 65, 105}, TickRange: 6})
	if err := b.Save(dir); err != nil {
		t.Fatal(err)
	}

	if err := cmdMove([]string{"--spawn-to", "0,65,0", "--no-backup", dir}); err != nil {
		t.Fatal(err)
	}
	p, err := pile.Open(dir, pile.ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if got := p.Settings().Spawn; got != (cube.Pos{0, 65, 0}) {
		t.Fatalf("spawn = %v, want 0,65,0", got)
	}
	stone := reg.BlockRuntimeID(block.Stone{})
	// Block 105,60,105 moved to 0,60,0.
	col, err := p.LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	if rid := col.Chunk.Block(0, 60, 0, 0); rid != stone {
		t.Fatalf("block under spawn missing, rid %d", rid)
	}
}

func TestOriginCLI(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	b := pile.NewBuilder(reg, cube.Range{-64, 319})
	b.Fill(cube.Pos{0, 0, 0}, cube.Pos{9, 2, 9}, block.Stone{})
	worldDir := filepath.Join(dir, "world")
	if err := b.Save(worldDir); err != nil {
		t.Fatal(err)
	}
	structPath := filepath.Join(dir, "s.pile")
	if err := cmdExtract([]string{"--min", "0,0,0", "--max", "9,2,9", worldDir, structPath}); err != nil {
		t.Fatal(err)
	}

	if err := cmdOrigin([]string{"--center", structPath}); err != nil {
		t.Fatal(err)
	}
	data, err := format.ReadStructure(mustRead(t, structPath), reg)
	if err != nil {
		t.Fatal(err)
	}
	if data.Origin != [3]int32{-5, 0, -5} {
		t.Fatalf("origin = %v, want (-5,0,-5)", data.Origin)
	}
	if err := cmdOrigin([]string{"--set", "1,2,3", structPath}); err != nil {
		t.Fatal(err)
	}
	data, _ = format.ReadStructure(mustRead(t, structPath), reg)
	if data.Origin != [3]int32{1, 2, 3} {
		t.Fatalf("origin = %v, want (1,2,3)", data.Origin)
	}
	// Content intact after origin edits.
	s, err := pile.LoadStructure(structPath)
	if err != nil {
		t.Fatal(err)
	}
	bl, _ := s.At(4, 1, 4, nil)
	if reg.BlockRuntimeID(bl) != reg.BlockRuntimeID(block.Stone{}) {
		t.Fatal("structure content lost after origin edit")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestSnapToChunk pins the rounding --center applies.
func TestSnapToChunk(t *testing.T) {
	for _, c := range []struct{ in, want int }{
		{0, 0}, {7, 0}, {8, 16}, {15, 16}, {16, 16}, {23, 16}, {24, 32},
		{-7, 0}, {-8, -16}, {-15, -16}, {-16, -16}, {-19999, -20000},
		{-16023, -16016},
	} {
		if got := snapToChunk(c.in); got != c.want {
			t.Errorf("snapToChunk(%d) = %d, want %d", c.in, got, c.want)
		}
		if got := snapToChunk(c.in); got%16 != 0 {
			t.Errorf("snapToChunk(%d) = %d, which is not on the chunk grid", c.in, got)
		}
	}
}

// TestMoveCenterKeepsTheChunkGrid.
//
// --center used to centre to the block, which for a map far from the origin
// means an offset like (0,0,-19999): not a multiple of 16, so every chunk is
// cut across a boundary and rewritten. A 66-column arena came back as 80, and a
// build laid out on chunk boundaries stopped being on them.
//
// Rounding to the grid costs at most eight blocks of centring and keeps both
// the fast path and the column count. --by is still there for an exact offset.
func TestMoveCenterKeepsTheChunkGrid(t *testing.T) {
	reg := hostileReg(t)
	dir := t.TempDir()
	var cols []format.Column
	for x := int32(1000); x <= 1002; x++ {
		cols = append(cols, solidColumn(t, x, 0))
	}
	writeWorldAt(t, filepath.Join(dir, "overworld.pile"), cols)

	if err := cmdMove([]string{"--center", "--no-backup", dir}); err != nil {
		t.Fatal(err)
	}
	wf, err := pile.LoadWorldFiles(dir, reg)
	if err != nil {
		t.Fatal(err)
	}
	got := wf.Dim(world.Overworld).Columns
	if len(got) != 3 {
		t.Fatalf("the move turned 3 columns into %d: the offset was not chunk-aligned", len(got))
	}
	// Centred to within half a chunk of the origin, and still on the grid.
	for _, c := range got {
		if c.X < -2 || c.X > 2 {
			t.Errorf("column landed at chunk X %d, which is not near the origin", c.X)
		}
	}
}
