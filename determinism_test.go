package pile

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/df-mc/dragonfly/server/block"
	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
)

// Tests for the format's determinism guarantee: identical world content must
// produce identical bytes, whatever order the caller supplies collections in.

func TestDeterministicCollectionOrder(t *testing.T) {
	reg := testRegistry(t)
	mk := func(reverse bool) []byte {
		ch := chunk.New(reg, cube.Range{-64, 319})
		ch.SetBlock(0, 0, 0, 0, reg.BlockRuntimeID(block.Stone{}))
		bes := []chunk.BlockEntity{
			{Pos: cube.Pos{1, 1, 1}, Data: map[string]any{"id": "minecraft:chest"}},
			{Pos: cube.Pos{2, 2, 2}, Data: map[string]any{"id": "minecraft:furnace"}},
		}
		ents := []chunk.Entity{
			{ID: 1, Data: map[string]any{"identifier": "minecraft:cow", "UniqueID": int64(1)}},
			{ID: 2, Data: map[string]any{"identifier": "minecraft:pig", "UniqueID": int64(2)}},
		}
		ticks := []chunk.ScheduledBlockUpdate{
			{Pos: cube.Pos{3, 3, 3}, Block: reg.BlockRuntimeID(block.Stone{}), Tick: 5},
			{Pos: cube.Pos{4, 4, 4}, Block: reg.BlockRuntimeID(block.Dirt{}), Tick: 6},
		}
		if reverse {
			bes[0], bes[1] = bes[1], bes[0]
			ents[0], ents[1] = ents[1], ents[0]
			ticks[0], ticks[1] = ticks[1], ticks[0]
		}
		dir := t.TempDir()
		p, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		col := &chunk.Column{Chunk: ch, BlockEntities: bes, Entities: ents, ScheduledBlocks: ticks}
		if err := p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, col); err != nil {
			t.Fatal(err)
		}
		if err := p.Close(); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(dir, "overworld.pile"))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if a, b := mk(false), mk(true); string(a) != string(b) {
		t.Fatalf("collection order changed the output: %d vs %d bytes", len(a), len(b))
	}
}

// TestBuilderEntityIDsUnique: an automatic ID must never repeat one the
// caller supplied explicitly.
