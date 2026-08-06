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
