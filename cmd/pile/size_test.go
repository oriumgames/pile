package main

import (
	"io/fs"
	"path/filepath"
	"testing"
	"time"

	"github.com/df-mc/dragonfly/server/block"
	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
	"github.com/df-mc/dragonfly/server/world/mcdb"
)

// TestSizeComparison converts a synthetic 256-chunk lobby-like world and
// reports mcdb vs pile sizes. It guards the proposal's headline claim with a
// loose bound: pile must be at least 3x smaller than mcdb.
func TestSizeComparison(t *testing.T) {
	reg := testRegistry(t)
	stone := reg.BlockRuntimeID(block.Stone{})
	dirt := reg.BlockRuntimeID(block.Dirt{})
	grass := reg.BlockRuntimeID(block.Grass{})
	planks := reg.BlockRuntimeID(block.Planks{})

	srcDir := t.TempDir()
	db, err := mcdb.Open(srcDir)
	if err != nil {
		t.Fatal(err)
	}
	for x := range int32(16) {
		for z := range int32(16) {
			ch := chunk.New(reg, cube.Range{-64, 319})
			for bx := range uint8(16) {
				for bz := range uint8(16) {
					for y := int16(-64); y < 60; y++ {
						ch.SetBlock(bx, y, bz, 0, stone)
					}
					for y := int16(60); y < 64; y++ {
						ch.SetBlock(bx, y, bz, 0, dirt)
					}
					ch.SetBlock(bx, 64, bz, 0, grass)
				}
			}
			// Sparse structure so not everything dedups into one section.
			if (x+z)%3 == 0 {
				for bx := uint8(2); bx < 8; bx++ {
					ch.SetBlock(bx, 65+int16(x%4), 4, 0, planks)
				}
			}
			if err := db.StoreColumn(world.ChunkPos{x, z}, world.Overworld, &chunk.Column{Chunk: ch}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	pileDir := t.TempDir()
	start := time.Now()
	if err := convertMcdbToPile(srcDir, pileDir); err != nil {
		t.Fatal(err)
	}
	convertTime := time.Since(start)

	mcdbSize := dirSize(t, srcDir)
	pileSize := dirSize(t, pileDir)
	t.Logf("256 chunks: mcdb %d bytes, pile %d bytes (%.1fx smaller), convert %v",
		mcdbSize, pileSize, float64(mcdbSize)/float64(pileSize), convertTime)
	if pileSize*3 > mcdbSize {
		t.Errorf("pile (%d) is not at least 3x smaller than mcdb (%d)", pileSize, mcdbSize)
	}
}

func dirSize(t *testing.T, dir string) int64 {
	t.Helper()
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if info, err := d.Info(); err == nil && !d.IsDir() {
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return total
}
