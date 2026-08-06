package pile

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/df-mc/dragonfly/server/block"
	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
)

// buildMoveWorld creates a small on-disk world with a platform, block entity,
// entity, marker and spawn for move tests.
func buildMoveWorld(t *testing.T, dir string) {
	t.Helper()
	reg := testRegistry(t)
	b := NewBuilder(reg, cube.Range{-64, 319})
	b.Fill(cube.Pos{0, 0, 0}, cube.Pos{12, 3, 12}, block.Stone{})
	b.AddBlockEntity(cube.Pos{4, 4, 4}, map[string]any{"id": "minecraft:chest"})
	b.AddEntity(map[string]any{
		"identifier": "minecraft:armor_stand",
		"Pos":        []any{float32(6.5), float32(4), float32(6.5)},
	})
	b.SetMarker(Marker{Name: "spawn", Kind: "spawn", Pos: [3]float64{6, 4, 6}})
	b.Settings(&world.Settings{Name: "move-test", Spawn: cube.Pos{6, 4, 6}, TickRange: 6})
	if err := b.Save(dir); err != nil {
		t.Fatal(err)
	}
}

func TestMoveFastPath(t *testing.T) {
	reg := testRegistry(t)
	stone := reg.BlockRuntimeID(block.Stone{})
	dir := t.TempDir()
	buildMoveWorld(t, dir)

	report, err := MoveWorld(dir, MoveOptions{Offset: cube.Pos{32, 0, -16}, Backup: true})
	if err != nil {
		t.Fatal(err)
	}
	if !report.FastPath {
		t.Fatal("expected fast path for chunk-aligned offset")
	}
	if report.ClippedTotal() != 0 {
		t.Fatalf("unexpected clipping: %+v", report)
	}

	p, err := Open(dir, ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	col, err := p.LoadColumn(world.ChunkPos{2, -1}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	if rid := col.Chunk.Block(uint8(35&15), 2, uint8(-13&15), 0); rid != stone {
		t.Fatalf("block not at translated position, rid %d", rid)
	}
	foundBE := false
	for _, be := range col.BlockEntities {
		if be.Pos == (cube.Pos{36, 4, -12}) && be.Data["x"] == int32(36) {
			foundBE = true
		}
	}
	if !foundBE {
		t.Fatalf("block entity not translated: %+v", col.BlockEntities)
	}
	if got := p.Settings().Spawn; got != (cube.Pos{38, 4, -10}) {
		t.Fatalf("spawn not translated: %v", got)
	}
	if ms := p.Markers(); len(ms) != 1 || ms[0].Pos != [3]float64{38, 4, -10} {
		t.Fatalf("marker not translated: %+v", ms)
	}
	// Backup exists and holds the pre-move world.
	if _, err := os.Stat(filepath.Join(dir, "snapshots", "pre-move", "overworld.pile")); err != nil {
		t.Fatal("pre-move backup missing")
	}
}

func TestMoveUnaligned(t *testing.T) {
	reg := testRegistry(t)
	stone := reg.BlockRuntimeID(block.Stone{})
	dir := t.TempDir()
	buildMoveWorld(t, dir)

	report, err := MoveWorld(dir, MoveOptions{Offset: cube.Pos{5, 7, -3}, Backup: false})
	if err != nil {
		t.Fatal(err)
	}
	if report.FastPath {
		t.Fatal("unaligned offset must not take the fast path")
	}
	p, err := Open(dir, ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	// Block (12,3,12) -> (17,10,9), in chunk (1,0).
	col, err := p.LoadColumn(world.ChunkPos{1, 0}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	if rid := col.Chunk.Block(uint8(17&15), 10, uint8(9&15), 0); rid != stone {
		t.Fatalf("block not at translated position, rid %d", rid)
	}
	// Old position must be empty now: (0,0,0) -> air after the move.
	col0, err := p.LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	if rid := col0.Chunk.Block(0, 0, 0, 0); rid == stone {
		t.Fatal("source block still present after move")
	}
	if got := p.Settings().Spawn; got != (cube.Pos{11, 11, 3}) {
		t.Fatalf("spawn not translated: %v", got)
	}
}

func TestMoveClipRefusalAndForce(t *testing.T) {
	dir := t.TempDir()
	buildMoveWorld(t, dir)
	before, _ := os.ReadFile(filepath.Join(dir, "overworld.pile"))

	// Platform sits at Y 0..3; moving down 70 pushes it below -64.
	report, err := MoveWorld(dir, MoveOptions{Offset: cube.Pos{0, -70, 0}})
	if !errors.Is(err, ErrWouldClip) {
		t.Fatalf("err = %v, want ErrWouldClip", err)
	}
	if report.ClippedBlocks == 0 {
		t.Fatal("clip report empty")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "overworld.pile"))
	if !bytes.Equal(before, after) {
		t.Fatal("refused move modified the file")
	}

	// Dry run of a lossless move also leaves the file untouched.
	if _, err := MoveWorld(dir, MoveOptions{Offset: cube.Pos{16, 0, 0}, DryRun: true}); err != nil {
		t.Fatal(err)
	}
	after, _ = os.ReadFile(filepath.Join(dir, "overworld.pile"))
	if !bytes.Equal(before, after) {
		t.Fatal("dry run modified the file")
	}

	// With Clip the move proceeds; y=0..1 rows survive (land at -70.. -69? no:
	// 0-70=-70 clipped, 3-70=-67 clipped; everything below -64 is cut, rows
	// landing at >= -64 survive). Offset -66: y=2,3 -> -64,-63 survive.
	report, err = MoveWorld(dir, MoveOptions{Offset: cube.Pos{0, -66, 0}, Clip: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.ClippedBlocks == 0 {
		t.Fatal("expected clipped blocks")
	}
	p, err := Open(dir, ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	reg := testRegistry(t)
	stone := reg.BlockRuntimeID(block.Stone{})
	col, err := p.LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	if rid := col.Chunk.Block(4, -64, 4, 0); rid != stone {
		t.Fatal("surviving row missing after clipped move")
	}
	if rid := col.Chunk.Block(4, -63, 4, 0); rid != stone {
		t.Fatal("surviving top row missing after clipped move")
	}
}

func TestMovePreservesAllLayers(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	stone := reg.BlockRuntimeID(block.Stone{})
	water := reg.BlockRuntimeID(block.Water{Depth: 8, Still: true})

	b := NewBuilder(reg, cube.Range{-64, 319})
	b.Fill(cube.Pos{0, 0, 0}, cube.Pos{4, 1, 4}, block.Stone{})
	p := b.Provider()
	col, err := p.LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	col.Chunk.SetBlock(2, 1, 2, 1, water) // waterlogging layer
	if err := p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, col); err != nil {
		t.Fatal(err)
	}
	if err := p.SaveAs(dir); err != nil {
		t.Fatal(err)
	}
	_ = p.Close()

	if _, err := MoveWorld(dir, MoveOptions{Offset: cube.Pos{1, 0, 0}, Backup: false}); err != nil {
		t.Fatal(err)
	}
	q, err := Open(dir, ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	moved, err := q.LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	if rid := moved.Chunk.Block(3, 1, 2, 0); rid != stone {
		t.Fatalf("layer 0 lost in move, rid %d", rid)
	}
	if rid := moved.Chunk.Block(3, 1, 2, 1); rid != water {
		t.Fatalf("layer 1 lost in move, rid %d", rid)
	}
}

func TestMoveKeepsUnreadableEntities(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	b := NewBuilder(reg, cube.Range{-64, 319})
	b.Fill(cube.Pos{0, 0, 0}, cube.Pos{4, 1, 4}, block.Stone{})
	p := b.Provider()
	col, err := p.LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	// No usable Pos: the mover cannot translate it.
	col.Entities = append(col.Entities, chunk.Entity{ID: 77, Data: map[string]any{
		"identifier": "minecraft:zombie", "UniqueID": int64(77),
	}})
	if err := p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, col); err != nil {
		t.Fatal(err)
	}
	if err := p.SaveAs(dir); err != nil {
		t.Fatal(err)
	}
	_ = p.Close()

	if _, err := MoveWorld(dir, MoveOptions{Offset: cube.Pos{3, 0, 3}, Backup: false}); err != nil {
		t.Fatal(err)
	}
	q, err := Open(dir, ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	found := false
	for _, c := range q.Columns(world.Overworld) {
		for _, e := range c.Entities {
			if e.ID == 77 {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("move dropped an entity whose Pos could not be parsed")
	}
}
