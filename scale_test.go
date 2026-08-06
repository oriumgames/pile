package pile

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/df-mc/dragonfly/server/block"
	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
)

// scaleColumn builds a cheap but non-trivial column.
func scaleColumn(reg world.BlockRegistry, x int32) *chunk.Column {
	ch := chunk.New(reg, cube.Range{-64, 319})
	stone := reg.BlockRuntimeID(block.Stone{})
	dirt := reg.BlockRuntimeID(block.Dirt{})
	for bx := range uint8(16) {
		for bz := range uint8(16) {
			for y := int16(-64); y < -32; y++ {
				ch.SetBlock(bx, y, bz, 0, stone)
			}
			ch.SetBlock(bx, -32, bz, 0, dirt)
		}
	}
	// A per-column variation so not everything dedups away.
	ch.SetBlock(uint8(x&15), -20, 3, 0, dirt)
	return &chunk.Column{Chunk: ch}
}

// TestScaleSolid stores and round-trips 5000 chunks through a solid world.
func TestScaleSolid(t *testing.T) {
	if testing.Short() {
		t.Skip("scale test")
	}
	reg := testRegistry(t)
	dir := t.TempDir()
	p, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	const n = 5000
	for i := range int32(n) {
		if err := p.StoreColumn(world.ChunkPos{i % 80, i / 80}, world.Overworld, scaleColumn(reg, i)); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	saveTime := time.Since(start)

	st, _ := os.Stat(filepath.Join(dir, "overworld.pile"))
	start = time.Now()
	q, err := Open(dir, ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	loadTime := time.Since(start)
	defer q.Close()
	if q.ChunkCount(world.Overworld) != n {
		t.Fatalf("chunk count = %d, want %d", q.ChunkCount(world.Overworld), n)
	}
	col, err := q.LoadColumn(world.ChunkPos{41, 30}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	if col.Chunk.Block(3, -40, 3, 0) != reg.BlockRuntimeID(block.Stone{}) {
		t.Fatal("content wrong at scale")
	}
	t.Logf("solid: %d chunks, file %d bytes (%.1f B/chunk), save %v, load %v",
		n, st.Size(), float64(st.Size())/n, saveTime, loadTime)
	if !raceEnabled && (saveTime > 30*time.Second || loadTime > 30*time.Second) {
		t.Fatalf("scale performance collapsed: save %v load %v", saveTime, loadTime)
	}
}

// TestScaleIndexed stores 3000 chunks across many checkpoints, reopens,
// spot-checks and compacts.
func TestScaleIndexed(t *testing.T) {
	if testing.Short() {
		t.Skip("scale test")
	}
	reg := testRegistry(t)
	dir := t.TempDir()
	p, err := Open(dir, AppendMode())
	if err != nil {
		t.Fatal(err)
	}
	const n = 3000
	for i := range int32(n) {
		if err := p.StoreColumn(world.ChunkPos{i % 60, i / 60}, world.Overworld, scaleColumn(reg, i)); err != nil {
			t.Fatal(err)
		}
		if i%250 == 0 {
			if err := p.Save(); err != nil { // many checkpoints
				t.Fatal(err)
			}
		}
	}
	// Overwrite a slice of them to create garbage.
	for i := range int32(500) {
		if err := p.StoreColumn(world.ChunkPos{i % 60, i / 60}, world.Overworld, scaleColumn(reg, i+7)); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	q, err := Open(dir, AppendMode())
	if err != nil {
		t.Fatal(err)
	}
	openTime := time.Since(start)
	defer q.Close()
	if q.ChunkCount(world.Overworld) != n {
		t.Fatalf("chunk count = %d, want %d", q.ChunkCount(world.Overworld), n)
	}
	start = time.Now()
	if _, err := q.LoadColumn(world.ChunkPos{31, 27}, world.Overworld); err != nil {
		t.Fatal(err)
	}
	colTime := time.Since(start)
	st, _ := os.Stat(filepath.Join(dir, "overworld.pile"))
	t.Logf("indexed: %d chunks, file %d bytes, open %v, column read %v", n, st.Size(), openTime, colTime)
	if !raceEnabled && (openTime > 10*time.Second || colTime > 100*time.Millisecond) {
		t.Fatalf("indexed performance collapsed: open %v column %v", openTime, colTime)
	}
}
