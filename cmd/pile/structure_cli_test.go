package main

import (
	"path/filepath"
	"testing"

	"github.com/df-mc/dragonfly/server/block"
	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/oriumgames/pile"
)

func TestExtractPasteCLI(t *testing.T) {
	reg := testRegistry(t)
	stone := reg.BlockRuntimeID(block.Stone{})

	// Build a source world on disk.
	srcDir := t.TempDir()
	b := pile.NewBuilder(reg, cube.Range{-64, 319})
	b.Fill(cube.Pos{0, 0, 0}, cube.Pos{10, 4, 10}, block.Stone{})
	if err := b.Save(srcDir); err != nil {
		t.Fatal(err)
	}

	// Extract a region into a structure file.
	structPath := filepath.Join(t.TempDir(), "cut.pile")
	if err := cmdExtract([]string{"--min", "0,0,0", "--max", "10,4,10", srcDir, structPath}); err != nil {
		t.Fatal(err)
	}

	// Paste it into a fresh world at an offset.
	dstDir := t.TempDir()
	nb := pile.NewBuilder(reg, cube.Range{-64, 319})
	nb.SetBlock(cube.Pos{500, 0, 500}, block.Stone{}) // seed so the world exists
	if err := nb.Save(dstDir); err != nil {
		t.Fatal(err)
	}
	if err := cmdPaste([]string{"--at", "32,10,32", structPath, dstDir}); err != nil {
		t.Fatal(err)
	}

	p, err := pile.Open(dstDir, pile.ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	col, err := p.LoadColumn(world.ChunkPos{2, 2}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	if rid := col.Chunk.Block(uint8(37&15), 12, uint8(37&15), 0); rid != stone {
		t.Fatalf("pasted block missing at (37,12,37), rid %d", rid)
	}
}
