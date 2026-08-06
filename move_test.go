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
	"github.com/oriumgames/pile/format"
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

// TestMoveFastPathKeepsSidecars: the fast path only re-keys columns, so it has
// to carry the preservation sidecars across itself. Unknown ticks are keyed by
// absolute position and must move with the updates they describe; unknown
// biomes are keyed by section and survive an offset with no vertical
// component untouched, but only if they are carried at all.
func TestMoveFastPathKeepsSidecars(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	p, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	col := buildSidecarColumn(t, reg)
	if err := p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, col.Col); err != nil {
		t.Fatal(err)
	}
	if err := p.SaveAs(dir); err != nil {
		t.Fatal(err)
	}
	_ = p.Close()

	moved, err := translateColumns([]format.Column{col}, reg, cube.Pos{16, 0, 0}, &MoveReport{})
	if err != nil {
		t.Fatal(err)
	}
	got := moved[0]
	if len(got.UnknownBiomes) != len(col.UnknownBiomes) || len(got.UnknownBiomeNames) == 0 {
		t.Fatalf("move dropped the unknown biome sidecar: %d entries, %d names",
			len(got.UnknownBiomes), len(got.UnknownBiomeNames))
	}
	if len(got.UnknownTicks) != 1 {
		t.Fatalf("move dropped the unknown tick sidecar: %d entries", len(got.UnknownTicks))
	}
	want := col.UnknownTicks[0].Pos
	want[0] += 16
	if got.UnknownTicks[0].Pos != want {
		t.Fatalf("unknown tick position = %v, want %v: the sidecar key did not move with the update",
			got.UnknownTicks[0].Pos, want)
	}
}

// buildSidecarColumn makes a column carrying every preservation sidecar.
func buildSidecarColumn(t *testing.T, reg world.BlockRegistry) format.Column {
	t.Helper()
	ch := chunk.New(reg, cube.Range{-64, 319})
	stone := reg.BlockRuntimeID(block.Stone{})
	ch.SetBlock(0, -64, 0, 0, stone)
	return format.Column{
		X: 0, Z: 0,
		Col: &chunk.Column{Chunk: ch, ScheduledBlocks: []chunk.ScheduledBlockUpdate{
			{Pos: cube.Pos{2, -60, 3}, Block: stone, Tick: 40},
		}},
		UnknownStates:     []format.BlockState{{Name: "audit:missing", Version: 1}},
		UnknownTicks:      []format.UnknownTick{{Pos: [3]int32{2, -60, 3}, At: 40, State: 0}},
		UnknownBiomeNames: []string{"audit:biome"},
		UnknownBiomes:     []format.UnknownBlock{{Section: -4, Index: format.WholeStorage, State: 0}},
	}
}

// TestMovePreservesDeepLayers: the format carries 255 layers and a move reads
// them one at a time, so a cap below that silently drops everything above it.
func TestMovePreservesDeepLayers(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	stone := reg.BlockRuntimeID(block.Stone{})
	dirt := reg.BlockRuntimeID(block.Dirt{})

	b := NewBuilder(reg, cube.Range{-64, 319})
	b.Fill(cube.Pos{0, 0, 0}, cube.Pos{2, 0, 2}, block.Stone{})
	p := b.Provider()
	col, err := p.LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	col.Chunk.SetBlock(1, 0, 1, 7, dirt) // layer 7, well past the old cap of 4
	if err := p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, col); err != nil {
		t.Fatal(err)
	}
	if err := p.SaveAs(dir); err != nil {
		t.Fatal(err)
	}
	_ = p.Close()

	// An unaligned offset forces the slow path, which walks layers explicitly.
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
	if rid := moved.Chunk.Block(2, 0, 1, 0); rid != stone {
		t.Fatalf("layer 0 lost in the move, rid %d", rid)
	}
	if rid := moved.Chunk.Block(2, 0, 1, 7); rid != dirt {
		t.Fatalf("layer 7 lost in the move, rid %d: deep layers are being dropped", rid)
	}
}

// TestMovePreservesUnknownBiomes: a move translates content, it does not
// rename it. Without re-anchoring the biome sidecar, an unresolved biome came
// out the other side as the runtime's fallback.
func TestMovePreservesUnknownBiomes(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	b := NewBuilder(reg, cube.Range{-64, 319})
	b.Fill(cube.Pos{0, 0, 0}, cube.Pos{2, 0, 2}, block.Stone{})
	p := b.Provider()
	if err := p.SaveAs(dir); err != nil {
		t.Fatal(err)
	}
	_ = p.Close()

	// Put a preserved biome into the file directly: the provider has no API
	// for minting one, since it only ever arrives from a file it cannot
	// resolve.
	wf, err := LoadWorldFiles(dir, reg)
	if err != nil {
		t.Fatal(err)
	}
	df := wf.Dim(world.Overworld)
	if df == nil || len(df.Columns) == 0 {
		t.Fatal("no columns loaded")
	}
	// Preservation only re-emits where the runtime fallback is still in
	// place, so the section has to hold it.
	plains := uint32(1)
	if b, ok := world.BiomeByName("plains"); ok {
		plains = uint32(b.EncodeBiome())
	}
	pr := df.Columns[0].Col.Chunk.Range()
	for x := range uint8(16) {
		for z := range uint8(16) {
			for y := int16(pr[0]); y <= int16(pr[1]); y++ {
				df.Columns[0].Col.Chunk.SetBiome(x, y, z, plains)
			}
		}
	}
	df.Columns[0].UnknownBiomeNames = []string{"audit:unknown"}
	df.Columns[0].UnknownBiomes = []format.UnknownBlock{
		{Section: -4, Index: format.WholeStorage, State: 0},
	}
	if err := wf.Write(dir, reg); err != nil {
		t.Fatal(err)
	}

	if _, err := MoveWorld(dir, MoveOptions{Offset: cube.Pos{1, 0, 0}, Backup: false}); err != nil {
		t.Fatal(err)
	}
	after, err := LoadWorldFiles(dir, reg)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range after.Dim(world.Overworld).Columns {
		for _, n := range c.UnknownBiomeNames {
			if n == "audit:unknown" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("the move renamed an unresolved biome to the runtime fallback")
	}
}

// airPlaceholderRegistry hides minecraft:info_update, so an unresolved block
// state decodes as air with only its sidecar entry to mark it. Any code that
// tests for air before consulting that sidecar loses the state, and this is the
// registry where the two cases are indistinguishable.
type airPlaceholderRegistry struct {
	world.BlockRegistry
}

func (r airPlaceholderRegistry) StateToRuntimeID(name string, props map[string]any) (uint32, bool) {
	if name == "minecraft:info_update" {
		return 0, false
	}
	return r.BlockRegistry.StateToRuntimeID(name, props)
}

// TestMoveKeepsUnknownBlocksWithAirPlaceholder: the mover reads a position,
// finds air, and would move on. The sidecar is the only record that the
// position holds a preserved state, so it has to be consulted first.
func TestMoveKeepsUnknownBlocksWithAirPlaceholder(t *testing.T) {
	base := testRegistry(t)
	reg := airPlaceholderRegistry{base}
	dir := t.TempDir()

	b := NewBuilder(base, cube.Range{-64, 319})
	b.Fill(cube.Pos{0, 0, 0}, cube.Pos{2, 0, 2}, block.Stone{})
	p := b.Provider()
	if err := p.SaveAs(dir); err != nil {
		t.Fatal(err)
	}
	_ = p.Close()

	wf, err := LoadWorldFiles(dir, reg)
	if err != nil {
		t.Fatal(err)
	}
	df := wf.Dim(world.Overworld)
	df.Columns[0].UnknownStates = []format.BlockState{{Name: "audit:unknown", Version: 1}}
	df.Columns[0].Unknown = []format.UnknownBlock{{
		Section: -4, Layer: 0,
		Index: uint16(3)<<8 | uint16(3)<<4 | 0, State: 0,
	}}
	if err := wf.Write(dir, reg); err != nil {
		t.Fatal(err)
	}

	if _, err := MoveWorld(dir, MoveOptions{
		Offset: cube.Pos{1, 0, 0}, Backup: false, Registry: reg,
	}); err != nil {
		t.Fatal(err)
	}
	after, err := LoadWorldFiles(dir, reg)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range after.Dim(world.Overworld).Columns {
		for _, s := range c.UnknownStates {
			if s.Name == "audit:unknown" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("the move dropped an unresolved block whose placeholder is air")
	}
}
