package pile

import (
	"fmt"
	"sync"
	"testing"

	"github.com/df-mc/dragonfly/server/block"
	"github.com/df-mc/dragonfly/server/world"
)

// TestConcurrentProviderHammer exercises parallel loads, stores, metadata
// mutation and async saves. Run with -race; its value is data-race detection.
func TestConcurrentProviderHammer(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	p, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Seed some columns.
	for i := range int32(8) {
		if err := p.StoreColumn(world.ChunkPos{i, 0}, world.Overworld, testColumn(t, reg, world.ChunkPos{i, 0})); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := range 50 {
				pos := world.ChunkPos{int32((g*50 + i) % 12), 0}
				switch i % 4 {
				case 0:
					_ = p.StoreColumn(pos, world.Overworld, testColumn(t, reg, pos))
				case 1:
					if col, err := p.LoadColumn(pos, world.Overworld); err == nil {
						_ = col.Chunk.Block(1, 1, 1, 0)
					}
				case 2:
					p.SetMarker(Marker{Name: fmt.Sprintf("m%d", g), Kind: "poi", Pos: &[3]float64{float64(i), 0, 0}})
					_ = p.Markers()
					p.SetUserData([]byte{byte(g), byte(i)})
					_ = p.UserData()
					_ = p.ChunkCount(world.Overworld)
				case 3:
					p.SaveAsync()
				}
			}
		}(g)
	}
	wg.Wait()
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	q, err := Open(dir)
	if err != nil {
		t.Fatalf("world corrupted by concurrent access: %v", err)
	}
	defer q.Close()
	if q.ChunkCount(world.Overworld) == 0 {
		t.Fatal("columns lost")
	}
}

// TestConcurrentAppendHammer does the same against an append-mode provider.
func TestConcurrentAppendHammer(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	p, err := Open(dir, AppendMode())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for g := range 6 {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := range 30 {
				pos := world.ChunkPos{int32((g*30 + i) % 10), 0}
				switch i % 3 {
				case 0:
					_ = p.StoreColumn(pos, world.Overworld, testColumn(t, reg, pos))
				case 1:
					_, _ = p.LoadColumn(pos, world.Overworld)
				case 2:
					p.SaveAsync()
				}
			}
		}(g)
	}
	wg.Wait()
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	q, err := Open(dir, AppendMode())
	if err != nil {
		t.Fatalf("append world corrupted by concurrent access: %v", err)
	}
	defer q.Close()
	if q.ChunkCount(world.Overworld) == 0 {
		t.Fatal("columns lost")
	}
}

// TestConcurrentTemplateInstances runs many instances of one template in
// parallel, each mutating its own copy, and checks full isolation.
func TestConcurrentTemplateInstances(t *testing.T) {
	reg := testRegistry(t)
	stone := reg.BlockRuntimeID(block.Stone{})
	dirt := reg.BlockRuntimeID(block.Dirt{})

	dir := t.TempDir()
	src := buildArena(t)
	if err := src.SaveAs(dir); err != nil {
		t.Fatal(err)
	}
	_ = src.Close()
	tmpl, err := OpenTemplate(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer tmpl.Close()

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for g := range 8 {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			inst := tmpl.Instance()
			defer inst.Close()
			col, err := inst.LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
			if err != nil {
				errs <- err
				return
			}
			// Each instance writes its own goroutine ID as a block position.
			col.Chunk.SetBlock(uint8(g), 50, 0, 0, dirt)
			if err := inst.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, col); err != nil {
				errs <- err
				return
			}
			got, err := inst.LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
			if err != nil {
				errs <- err
				return
			}
			if got.Chunk.Block(uint8(g), 50, 0, 0) != dirt {
				errs <- fmt.Errorf("instance %d lost its own write", g)
				return
			}
			// No other instance's write may be visible.
			for other := range 8 {
				if other != g && got.Chunk.Block(uint8(other), 50, 0, 0) == dirt {
					errs <- fmt.Errorf("instance %d sees instance %d's write", g, other)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	// Template itself untouched.
	tcol, err := tmpl.Provider().LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	for g := range 8 {
		if tcol.Chunk.Block(uint8(g), 50, 0, 0) == dirt {
			t.Fatalf("template mutated by instance %d", g)
		}
	}
	if tcol.Chunk.Block(3, 2, 3, 0) != stone {
		t.Fatal("template content damaged")
	}
}

// TestSaveAsyncCoalesce fires many async saves and verifies a consistent,
// loadable file results.
func TestSaveAsyncCoalesce(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	p, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 20 {
		if err := p.StoreColumn(world.ChunkPos{int32(i), 0}, world.Overworld, testColumn(t, reg, world.ChunkPos{int32(i), 0})); err != nil {
			t.Fatal(err)
		}
		p.SaveAsync()
	}
	if err := p.Close(); err != nil { // waits for the saver, then final save
		t.Fatal(err)
	}
	q, err := Open(dir, ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if q.ChunkCount(world.Overworld) != 20 {
		t.Fatalf("chunk count = %d, want 20", q.ChunkCount(world.Overworld))
	}
}
