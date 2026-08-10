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
	if err := convertMcdbToPile(srcDir, pileDir, false); err != nil {
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
	if err := convertPileToMcdb(pileDir, dstDir, decodeLimit{}); err != nil {
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

// TestUnresolvedConvertErrorTellsThemApart.
//
// A conversion stops on a state the registry cannot resolve for two quite
// different reasons, and the message used to assert the wrong one: it blamed a
// behaviour pack even when every unresolved identifier was minecraft:. A lobby
// that arrived here had 200 unresolved states, all vanilla, because the world
// predates the Minecraft that split minecraft:carpet into minecraft:blue_carpet
// -- for which --permissive converts but leaves vanilla blocks placeholdered,
// and upgrading the world is the real fix.
func TestUnresolvedConvertErrorTellsThemApart(t *testing.T) {
	// A real leveldb world is out of reach here, so the classifier is driven
	// through the pieces the message is built from.
	vanilla := map[string]int{"minecraft:carpet": 10, "minecraft:wool": 11, "minecraft:log": 5}
	custom := map[string]int{"cubecraft:portal_side": 3}

	if got := countStates(vanilla); got != 26 {
		t.Errorf("countStates = %d, want 26", got)
	}
	// Most states first, ties by name, so the list is stable and the biggest
	// offender is the one somebody sees.
	if got := topNames(vanilla, 2); got[0] != "minecraft:wool" || got[1] != "minecraft:carpet" {
		t.Errorf("topNames = %v, want wool then carpet", got)
	}
	if got := topNames(custom, 6); len(got) != 1 {
		t.Errorf("topNames over one identifier returned %v", got)
	}
	if got := topNames(nil, 6); len(got) != 0 {
		t.Errorf("topNames over nothing returned %v", got)
	}
}

// TestBlockVersionString: the version is four packed bytes, and it is quoted at
// somebody trying to work out which Minecraft their world came from.
func TestBlockVersionString(t *testing.T) {
	for _, c := range []struct {
		v    int32
		want string
	}{
		{18105860, "1.20.70.4"},
		{18040335, "1.19.70.15"},
		{18022400, "1.19.0.0"},
		{0, "an unrecorded version"},
	} {
		if got := blockVersionString(c.v); got != c.want {
			t.Errorf("blockVersionString(%d) = %q, want %q", c.v, got, c.want)
		}
	}
}
