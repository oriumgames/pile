package pile

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"errors"
	"github.com/df-mc/dragonfly/server/block"
	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
	"github.com/df-mc/goleveldb/leveldb"
	"github.com/oriumgames/pile/format"
)

// buildArena creates an in-memory world with a stone platform, a chest
// (block entity) and an entity, for structure tests.
func buildArena(t *testing.T) *Provider {
	t.Helper()
	reg := testRegistry(t)
	b := NewBuilder(reg, cube.Range{-64, 319})
	b.Fill(cube.Pos{0, 0, 0}, cube.Pos{20, 3, 20}, block.Stone{})
	b.SetBlock(cube.Pos{5, 4, 5}, block.NewChest())
	b.AddEntity(map[string]any{
		"identifier": "minecraft:armor_stand",
		"Pos":        []any{float32(7.5), float32(4), float32(7.5)},
	})
	return b.Provider()
}

func TestStructureExtractSaveLoadPaste(t *testing.T) {
	reg := testRegistry(t)
	stone := reg.BlockRuntimeID(block.Stone{})
	p := buildArena(t)
	defer p.Close()

	s, err := ExtractStructure(p, world.Overworld, cube.Pos{0, 0, 0}, cube.Pos{20, 10, 20})
	if err != nil {
		t.Fatal(err)
	}
	if s.Dimensions() != [3]int{21, 11, 21} {
		t.Fatalf("dimensions = %v", s.Dimensions())
	}

	path := filepath.Join(t.TempDir(), "arena.pile")
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadStructure(path)
	if err != nil {
		t.Fatal(err)
	}

	// Canonical stability: re-saving the loaded structure reproduces the file.
	path2 := filepath.Join(t.TempDir(), "arena2.pile")
	if err := loaded.Save(path2); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(path)
	b, _ := os.ReadFile(path2)
	if !bytes.Equal(a, b) {
		t.Fatalf("structure save→load→save changed bytes: %d vs %d", len(a), len(b))
	}

	// world.Structure surface.
	bl, _ := loaded.At(3, 2, 3, nil)
	if reg.BlockRuntimeID(bl) != stone {
		t.Fatalf("At(3,2,3) = %v, want stone", bl)
	}
	chestBlock, _ := loaded.At(5, 4, 5, nil)
	if _, ok := chestBlock.(block.Chest); !ok {
		t.Fatalf("At(5,4,5) = %T, want block.Chest", chestBlock)
	}
	if len(loaded.Data().Entities) != 1 {
		t.Fatalf("entities = %d, want 1", len(loaded.Data().Entities))
	}

	// Paste at an offset into a fresh world.
	dst := NewMemory()
	defer dst.Close()
	at := cube.Pos{100, 50, -30}
	if err := loaded.PasteInto(dst, world.Overworld, at); err != nil {
		t.Fatal(err)
	}
	col, err := dst.LoadColumn(world.ChunkPos{int32(103 >> 4), int32(-27 >> 4)}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	if rid := col.Chunk.Block(uint8(103&15), 52, uint8(-27&15), 0); rid != stone {
		t.Fatalf("pasted block missing, got rid %d", rid)
	}
	// Block entity moved to the absolute position.
	beCol, err := dst.LoadColumn(world.ChunkPos{int32(105 >> 4), int32(-25 >> 4)}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	foundBE := false
	for _, be := range beCol.BlockEntities {
		if be.Pos == (cube.Pos{105, 54, -25}) {
			foundBE = true
			if be.Data["x"] != int32(105) {
				t.Fatalf("block entity x = %v", be.Data["x"])
			}
		}
	}
	if !foundBE {
		t.Fatal("pasted block entity not found")
	}
	// Entity moved with the paste.
	entCol, err := dst.LoadColumn(world.ChunkPos{int32(107 >> 4), int32(-23 >> 4)}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	if len(entCol.Entities) != 1 {
		t.Fatalf("pasted entities = %d, want 1", len(entCol.Entities))
	}
	pos, _ := entityPos(entCol.Entities[0].Data)
	if pos != [3]float64{107.5, 54, -22.5} {
		t.Fatalf("pasted entity pos = %v", pos)
	}
}

func TestStructureSkipAir(t *testing.T) {
	p := buildArena(t)
	defer p.Close()
	s, err := ExtractStructure(p, world.Overworld, cube.Pos{0, 0, 0}, cube.Pos{8, 8, 8}, SkipAir())
	if err != nil {
		t.Fatal(err)
	}
	if bl, _ := s.At(0, 8, 0, nil); bl != nil {
		t.Fatalf("SkipAir At on air = %v, want nil", bl)
	}
	if bl, _ := s.At(0, 0, 0, nil); bl == nil {
		t.Fatal("At on stone returned nil")
	}
}

func TestTemplateInstancesCOW(t *testing.T) {
	reg := testRegistry(t)
	stone := reg.BlockRuntimeID(block.Stone{})
	dirt := reg.BlockRuntimeID(block.Dirt{})

	dir := t.TempDir()
	src := buildArena(t)
	src.SetMarker(Marker{Name: "spawn", Kind: "spawn", Pos: [3]float64{10, 5, 10}})
	if err := src.SaveAs(dir); err != nil {
		t.Fatal(err)
	}
	_ = src.Close()

	tmpl, err := OpenTemplate(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer tmpl.Close()

	a, b := tmpl.Instance(), tmpl.Instance()
	defer a.Close()
	defer b.Close()

	if got := a.Markers(); len(got) != 1 || got[0].Name != "spawn" {
		t.Fatalf("instance markers = %+v", got)
	}
	baseCount := a.ChunkCount(world.Overworld)
	if baseCount == 0 {
		t.Fatal("instance sees no template chunks")
	}

	// Modify a chunk in instance A only.
	col, err := a.LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	col.Chunk.SetBlock(1, 1, 1, 0, dirt)
	if err := a.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, col); err != nil {
		t.Fatal(err)
	}
	if a.ChunkCount(world.Overworld) != baseCount {
		t.Fatalf("shadowing changed count: %d != %d", a.ChunkCount(world.Overworld), baseCount)
	}

	aCol, _ := a.LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
	if rid := aCol.Chunk.Block(1, 1, 1, 0); rid != dirt {
		t.Fatal("instance A modification lost")
	}
	bCol, _ := b.LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
	if rid := bCol.Chunk.Block(1, 1, 1, 0); rid != stone {
		t.Fatalf("instance B sees A's modification (rid %d)", rid)
	}
	tCol, _ := tmpl.Provider().LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
	if rid := tCol.Chunk.Block(1, 1, 1, 0); rid != stone {
		t.Fatal("template sees instance modification")
	}

	// Persist instance A and check the modification survives independently.
	outDir := t.TempDir()
	if err := a.SaveAs(outDir); err != nil {
		t.Fatal(err)
	}
	saved, err := Open(outDir, ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	defer saved.Close()
	if saved.ChunkCount(world.Overworld) != baseCount {
		t.Fatalf("persisted instance chunk count %d, want %d", saved.ChunkCount(world.Overworld), baseCount)
	}
	sCol, err := saved.LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	if rid := sCol.Chunk.Block(1, 1, 1, 0); rid != dirt {
		t.Fatal("persisted instance lost the modification")
	}
	if got := saved.Markers(); len(got) != 1 || got[0].Name != "spawn" {
		t.Fatalf("persisted markers = %+v", got)
	}
}

func TestBuilderFill(t *testing.T) {
	reg := testRegistry(t)
	stone := reg.BlockRuntimeID(block.Stone{})

	b := NewBuilder(reg, cube.Range{-64, 319})
	b.Fill(cube.Pos{-20, 0, -20}, cube.Pos{20, 5, 20}, block.Stone{})
	b.SetMarker(Marker{Name: "mid", Kind: "poi", Pos: [3]float64{0, 6, 0}})

	dir := t.TempDir()
	if err := b.Save(dir); err != nil {
		t.Fatal(err)
	}
	p, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	// -20..20 spans chunks -2..1 in both axes: 16 columns.
	if got := p.ChunkCount(world.Overworld); got != 16 {
		t.Fatalf("chunk count = %d, want 16", got)
	}
	col, err := p.LoadColumn(world.ChunkPos{-2, -2}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	if rid := col.Chunk.Block(uint8(-20&15), 3, uint8(-20&15), 0); rid != stone {
		t.Fatalf("fill corner missing, rid %d", rid)
	}
	if rid := col.Chunk.Block(uint8(-21&15), 3, uint8(-21&15), 0); rid == stone {
		t.Fatal("fill overshot the box")
	}
	if got := p.Markers(); len(got) != 1 || got[0].Name != "mid" {
		t.Fatalf("markers = %+v", got)
	}
}

func TestPasteAllAirSkipsColumnCreation(t *testing.T) {
	reg := testRegistry(t)
	data, err := format.NewStructureData([3]int32{4, 4, 4})
	if err != nil {
		t.Fatal(err)
	}
	s := newStructure(data, SkipAir(), StructureRegistry(reg))

	p := NewMemory()
	defer p.Close()
	if err := s.PasteInto(p, world.Overworld, cube.Pos{0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.LoadColumn(world.ChunkPos{0, 0}, world.Overworld); !errors.Is(err, leveldb.ErrNotFound) {
		t.Fatalf("all-air paste created a column: err = %v", err)
	}
}

// TestPasteClipsEntitiesToRange: entities outside the dimension range must be
// dropped like out-of-range blocks.

func TestPasteClipsEntitiesToRange(t *testing.T) {
	reg := testRegistry(t)
	data, err := format.NewStructureData([3]int32{1, 1, 1})
	if err != nil {
		t.Fatal(err)
	}
	data.Entities = append(data.Entities, map[string]any{
		"identifier": "minecraft:cow",
		"Pos":        []any{float32(0.5), float32(1000), float32(0.5)},
		"UniqueID":   int64(1),
	})
	s := newStructure(data, StructureRegistry(reg))

	p := NewMemory()
	defer p.Close()
	if err := s.PasteInto(p, world.Overworld, cube.Pos{0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	for _, col := range p.Columns(world.Overworld) {
		if len(col.Entities) != 0 {
			t.Fatalf("entity above the dimension range was pasted at %v", col.Entities[0].Data["Pos"])
		}
	}
}

// TestStructureWriterRejectsOversized mirrors the world writer's limits.

func TestPastePreservesUpperLayers(t *testing.T) {
	reg := testRegistry(t)
	stone := reg.BlockRuntimeID(block.Stone{})
	water := reg.BlockRuntimeID(block.Water{Depth: 8, Still: true})
	data, err := format.NewStructureData([3]int32{1, 1, 1})
	if err != nil {
		t.Fatal(err)
	}
	cell := chunk.NewSubChunk(reg.AirRuntimeID())
	cell.SetBlock(0, 0, 0, 0, stone)
	cell.SetBlock(0, 0, 0, 1, water)
	cell.SetBlock(0, 0, 0, 2, stone)
	data.Cells[0] = cell
	s := newStructure(data, StructureRegistry(reg))

	p := NewMemory()
	defer p.Close()
	if err := s.PasteInto(p, world.Overworld, cube.Pos{0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	col, err := p.LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	for layer, want := range map[uint8]uint32{0: stone, 1: water, 2: stone} {
		if rid := col.Chunk.Block(0, 0, 0, layer); rid != want {
			t.Fatalf("layer %d lost in paste: rid %d, want %d", layer, rid, want)
		}
	}
}

func TestExtractCarriesEntityID(t *testing.T) {
	reg := testRegistry(t)
	p := NewMemory()
	defer p.Close()
	ch := chunk.New(reg, cube.Range{-64, 319})
	ch.SetBlock(0, 0, 0, 0, reg.BlockRuntimeID(block.Stone{}))
	col := &chunk.Column{Chunk: ch, Entities: []chunk.Entity{{
		ID:   42,
		Data: map[string]any{"identifier": "minecraft:cow", "Pos": []any{float32(0.5), float32(0), float32(0.5)}},
	}}}
	if err := p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, col); err != nil {
		t.Fatal(err)
	}
	s, err := ExtractStructure(p, world.Overworld, cube.Pos{0, 0, 0}, cube.Pos{1, 1, 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Data().Entities) != 1 {
		t.Fatalf("entities = %d", len(s.Data().Entities))
	}
	if id, _ := s.Data().Entities[0]["UniqueID"].(int64); id != 42 {
		t.Fatalf("extraction lost the entity ID: got %v", s.Data().Entities[0]["UniqueID"])
	}
}

// TestSaveAsMetadataOnly: a world with metadata but no chunks must still be
// written and reopenable.
