package main

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/df-mc/dragonfly/server/block"
	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
	"github.com/df-mc/dragonfly/server/world/mcdb"
	"github.com/oriumgames/pile"
)

var regOnce sync.Once

func testRegistry(t testing.TB) world.BlockRegistry {
	t.Helper()
	regOnce.Do(world.DefaultBlockRegistry.Finalize)
	return world.DefaultBlockRegistry
}

func TestConvertBothWays(t *testing.T) {
	reg := testRegistry(t)
	stone := reg.BlockRuntimeID(block.Stone{})

	// Build a source mcdb world.
	srcDir := t.TempDir()
	db, err := mcdb.Open(srcDir)
	if err != nil {
		t.Fatal(err)
	}
	for x := range int32(3) {
		ch := chunk.New(reg, cube.Range{-64, 319})
		for bx := range uint8(16) {
			for bz := range uint8(16) {
				ch.SetBlock(bx, 0, bz, 0, stone)
			}
		}
		col := &chunk.Column{
			Chunk: ch,
			Entities: []chunk.Entity{{ID: int64(100 + x), Data: map[string]any{
				"identifier": "minecraft:cow", "UniqueID": int64(100 + x),
			}}},
			BlockEntities: []chunk.BlockEntity{{
				Pos:  cube.Pos{int(x) * 16, 5, 0},
				Data: map[string]any{"id": "minecraft:chest"},
			}},
		}
		if err := db.StoreColumn(world.ChunkPos{x, 0}, world.Overworld, col); err != nil {
			t.Fatal(err)
		}
	}
	set := db.Settings()
	set.Name = "converted-world"
	db.SaveSettings(set)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// mcdb -> pile.
	pileDir := t.TempDir()
	if err := convertMcdbToPile(srcDir, pileDir); err != nil {
		t.Fatal(err)
	}
	p, err := pile.Open(pileDir, pile.ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	if p.ChunkCount(world.Overworld) != 3 {
		t.Fatalf("pile has %d chunks, want 3", p.ChunkCount(world.Overworld))
	}
	if got := p.Settings().Name; got != "converted-world" {
		t.Fatalf("settings name %q, want converted-world", got)
	}
	col, err := p.LoadColumn(world.ChunkPos{1, 0}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	if rid := col.Chunk.Block(3, 0, 3, 0); rid != stone {
		t.Fatalf("expected stone, got rid %d", rid)
	}
	if len(col.Entities) != 1 || len(col.BlockEntities) != 1 {
		t.Fatalf("entities/block entities lost: %d/%d", len(col.Entities), len(col.BlockEntities))
	}
	_ = p.Close()

	// pile -> mcdb.
	dstDir := filepath.Join(t.TempDir(), "world")
	if err := convertPileToMcdb(pileDir, dstDir); err != nil {
		t.Fatal(err)
	}
	back, err := mcdb.Open(dstDir)
	if err != nil {
		t.Fatal(err)
	}
	defer back.Close()
	if got := back.Settings().Name; got != "converted-world" {
		t.Fatalf("mcdb settings name %q, want converted-world", got)
	}
	bcol, err := back.LoadColumn(world.ChunkPos{2, 0}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	if rid := bcol.Chunk.Block(3, 0, 3, 0); rid != stone {
		t.Fatalf("mcdb round trip: expected stone, got rid %d", rid)
	}
	if len(bcol.Entities) != 1 {
		t.Fatalf("mcdb round trip lost entities: %d", len(bcol.Entities))
	}
}
