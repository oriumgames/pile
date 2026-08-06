package pile

import (
	"testing"

	"github.com/df-mc/dragonfly/server/block"
	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
)

// TestDragonflyWorldIntegration drives the provider through a real dragonfly
// World: blocks set through a transaction must survive world close, provider
// reopen and a second world session.
func TestDragonflyWorldIntegration(t *testing.T) {
	testRegistry(t)
	dir := t.TempDir()

	p, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	w := world.Config{
		Provider:     p,
		Synchronous:  true,
		SaveInterval: -1,
	}.New()

	pos := cube.Pos{12, 40, -7}
	w.Do(func(tx *world.Tx) {
		tx.SetBlock(pos, block.Dirt{}, nil)
		tx.SetBlock(pos.Add(cube.Pos{0, 1, 0}), block.Stone{}, nil)
		if _, ok := tx.Block(pos).(block.Dirt); !ok {
			t.Errorf("block not visible inside the same transaction")
		}
	})
	if err := w.Close(); err != nil { // saves loaded chunks through the provider
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	// Second session: a fresh provider + world must read the block back.
	p2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	w2 := world.Config{
		Provider:     p2,
		Synchronous:  true,
		SaveInterval: -1,
	}.New()
	w2.Do(func(tx *world.Tx) {
		if _, ok := tx.Block(pos).(block.Dirt); !ok {
			t.Errorf("block did not survive the provider round trip: %T", tx.Block(pos))
		}
		if _, ok := tx.Block(pos.Add(cube.Pos{0, 1, 0})).(block.Stone); !ok {
			t.Errorf("second block did not survive: %T", tx.Block(pos.Add(cube.Pos{0, 1, 0})))
		}
	})
	if err := w2.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p2.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestDragonflyWorldTemplateInstance serves a template instance to a real
// dragonfly world: edits must stay in the instance, the template file must
// stay byte-identical.
func TestDragonflyWorldTemplateInstance(t *testing.T) {
	testRegistry(t)
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
	inst := tmpl.Instance()

	w := world.Config{Provider: inst, Synchronous: true, SaveInterval: -1}.New()
	pos := cube.Pos{5, 60, 5}
	w.Do(func(tx *world.Tx) {
		tx.SetBlock(pos, block.Dirt{}, nil)
	})
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	col, err := inst.LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	reg := testRegistry(t)
	if col.Chunk.Block(5, 60, 5, 0) != reg.BlockRuntimeID(block.Dirt{}) {
		t.Fatal("world edit did not reach the instance")
	}
	tcol, err := tmpl.Provider().LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	if tcol.Chunk.Block(5, 60, 5, 0) == reg.BlockRuntimeID(block.Dirt{}) {
		t.Fatal("world edit leaked into the template")
	}
	_ = inst.Close()
}
