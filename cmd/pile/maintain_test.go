package main

import (
	"testing"

	"github.com/df-mc/dragonfly/server/block"
	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/oriumgames/pile"
)

func TestModeConversionRoundTrip(t *testing.T) {
	reg := testRegistry(t)
	stone := reg.BlockRuntimeID(block.Stone{})

	dir := t.TempDir()
	b := pile.NewBuilder(reg, cube.Range{-64, 319})
	b.Fill(cube.Pos{0, 0, 0}, cube.Pos{20, 4, 20}, block.Stone{})
	b.Settings(&world.Settings{Name: "mode-test", TickRange: 6})
	if err := b.Save(dir); err != nil {
		t.Fatal(err)
	}

	// solid -> indexed: append mode can open it now.
	if err := cmdMode([]string{dir, "indexed"}); err != nil {
		t.Fatal(err)
	}
	p, err := pile.Open(dir, pile.AppendMode())
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Settings().Name; got != "mode-test" {
		t.Fatalf("settings lost in conversion: %q", got)
	}
	col, err := p.LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	if rid := col.Chunk.Block(3, 2, 3, 0); rid != stone {
		t.Fatalf("blocks lost in conversion, rid %d", rid)
	}
	if err := p.StoreColumn(world.ChunkPos{9, 9}, world.Overworld, col); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	// compact works on the indexed file.
	if err := cmdCompact([]string{dir}); err != nil {
		t.Fatal(err)
	}

	// indexed -> solid: normal open works again, content intact.
	if err := cmdMode([]string{dir, "solid"}); err != nil {
		t.Fatal(err)
	}
	q, err := pile.Open(dir, pile.ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if got := q.Settings().Name; got != "mode-test" {
		t.Fatalf("settings lost converting back: %q", got)
	}
	if q.ChunkCount(world.Overworld) != 5 { // 2x2 chunks from the fill + 1 stored
		t.Fatalf("chunk count = %d, want 5", q.ChunkCount(world.Overworld))
	}
	col2, err := q.LoadColumn(world.ChunkPos{9, 9}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	if rid := col2.Chunk.Block(3, 2, 3, 0); rid != stone {
		t.Fatalf("stored chunk lost converting back, rid %d", rid)
	}
}
