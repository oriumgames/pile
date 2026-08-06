package pile

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/df-mc/dragonfly/server/block"
	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
	"github.com/oriumgames/pile/format"
)

// Tests for the preservation of block states that do not resolve against the
// runtime's registry: they must survive every path that reads and rewrites a
// world, so a server running an older build cannot destroy map content it
// does not understand.

func corruptStateName(t *testing.T, file []byte, from, to string) []byte {
	t.Helper()
	if len(from) != len(to) {
		t.Fatal("replacement must be equal length")
	}
	bad := bytes.Clone(file)
	body := bad[16 : len(bad)-44]
	idx := bytes.Index(body, []byte(from))
	if idx < 0 {
		t.Fatalf("%s not found in body", from)
	}
	copy(body[idx:], to)
	// The footer hash authenticates the header and footer control words too,
	// so recompute it the way the format defines rather than over the body.
	binary.LittleEndian.PutUint64(bad[len(bad)-44:], format.CheckpointHash(bad[:16], body, bad[len(bad)-44+8:]))
	return bad
}

// TestUnknownStatePreservation is the audit's destruction scenario: a world
// containing an unknown block state is opened, a chunk is touched by the
// server (load + store) and the world is saved. The unknown state must
// survive in the saved file instead of being rewritten as a placeholder.

func TestUnknownStatePreservation(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()

	// A world whose (1,1,1) block will become unknown.
	b := NewBuilder(reg, cube.Range{-64, 319})
	b.Fill(cube.Pos{0, 0, 0}, cube.Pos{8, 2, 8}, block.Stone{})
	b.SetBlock(cube.Pos{1, 4, 1}, block.Dirt{})
	p := b.Provider()
	d := &format.WorldData{Columns: nil}
	for pos, col := range p.Columns(world.Overworld) {
		d.Columns = append(d.Columns, format.Column{X: pos[0], Z: pos[1], Col: col})
	}
	_ = p.Close()
	var buf bytes.Buffer
	if err := format.WriteWorld(&buf, d, reg, format.Options{Compression: format.CompressionNone}); err != nil {
		t.Fatal(err)
	}
	bad := corruptStateName(t, buf.Bytes(), "minecraft:dirt", "minecraft:d1rt")
	if err := os.WriteFile(filepath.Join(dir, "overworld.pile"), bad, 0o644); err != nil {
		t.Fatal(err)
	}

	// Open, touch the chunk like a server would, save.
	q, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	col, err := q.LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	// The unknown block decodes as the placeholder.
	placeholder, _ := reg.StateToRuntimeID("minecraft:info_update", map[string]any{})
	if rid := col.Chunk.Block(1, 4, 1, 0); rid != placeholder {
		t.Fatalf("unknown block decoded as rid %d, want placeholder %d", rid, placeholder)
	}
	// Server modifies a DIFFERENT block and stores the chunk back.
	col.Chunk.SetBlock(2, 4, 2, 0, reg.BlockRuntimeID(block.Stone{}))
	if err := q.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, col); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}

	// The saved file must still carry the original unknown state.
	saved, err := os.ReadFile(filepath.Join(dir, "overworld.pile"))
	if err != nil {
		t.Fatal(err)
	}
	unresolved, err := format.UnresolvedStates(saved, reg)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range unresolved {
		if strings.Contains(s, "minecraft:d1rt") {
			found = true
		}
	}
	if !found {
		t.Fatalf("unknown state destroyed by load-save round trip; unresolved = %v", unresolved)
	}
	// And it still occupies the original position (placeholder on load).
	r, err := Open(dir, ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	col2, err := r.LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	if rid := col2.Chunk.Block(1, 4, 1, 0); rid != placeholder {
		t.Fatalf("preserved state not at original position, rid %d", rid)
	}
	// If the server overwrote the unknown block itself, preservation stops.
	col2.Chunk.SetBlock(1, 4, 1, 0, reg.BlockRuntimeID(block.Stone{}))
	dir2 := t.TempDir()
	p2, _ := Open(dir2)
	if err := p2.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, col2); err != nil {
		t.Fatal(err)
	}
	_ = p2.Close()
	saved2, _ := os.ReadFile(filepath.Join(dir2, "overworld.pile"))
	if u, _ := format.UnresolvedStates(saved2, reg); len(u) != 0 {
		t.Fatalf("overwritten unknown block still emitted: %v", u)
	}
}

// TestUnknownStatePreservationAppend runs the same scenario in append mode.

func TestUnknownStatePreservationAppend(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()

	b := NewBuilder(reg, cube.Range{-64, 319})
	b.Fill(cube.Pos{0, 0, 0}, cube.Pos{8, 2, 8}, block.Stone{})
	b.SetBlock(cube.Pos{1, 4, 1}, block.Dirt{})
	p := b.Provider()
	d := &format.WorldData{}
	for pos, col := range p.Columns(world.Overworld) {
		d.Columns = append(d.Columns, format.Column{X: pos[0], Z: pos[1], Col: col})
	}
	_ = p.Close()
	var buf bytes.Buffer
	if err := format.WriteWorld(&buf, d, reg, format.Options{Compression: format.CompressionNone}); err != nil {
		t.Fatal(err)
	}
	bad := corruptStateName(t, buf.Bytes(), "minecraft:dirt", "minecraft:d1rt")
	if err := os.WriteFile(filepath.Join(dir, "overworld.pile"), bad, 0o644); err != nil {
		t.Fatal(err)
	}
	// Convert the solid file to indexed via the world files layer.
	wf, err := LoadWorldFiles(dir, reg)
	if err != nil {
		t.Fatal(err)
	}
	wf.Dims[0].Indexed = true
	if err := wf.Write(dir, reg); err != nil {
		t.Fatal(err)
	}

	q, err := Open(dir, AppendMode())
	if err != nil {
		t.Fatal(err)
	}
	col, err := q.LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	col.Chunk.SetBlock(2, 4, 2, 0, reg.BlockRuntimeID(block.Stone{}))
	if err := q.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, col); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}

	iw, err := format.OpenIndexed(filepath.Join(dir, "overworld.pile"), reg, true)
	if err != nil {
		t.Fatal(err)
	}
	defer iw.Close()
	unresolved, err := iw.UnresolvedStates()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range unresolved {
		if strings.Contains(s, "minecraft:d1rt") {
			found = true
		}
	}
	if !found {
		t.Fatalf("append mode destroyed unknown state; unresolved = %v", unresolved)
	}
}

func scheduledDirt(t *testing.T, reg world.BlockRegistry) chunk.ScheduledBlockUpdate {
	t.Helper()
	return chunk.ScheduledBlockUpdate{
		Pos:   cube.Pos{1, 4, 1},
		Block: reg.BlockRuntimeID(block.Dirt{}),
		Tick:  10,
	}
}

// writeUnknownWorld builds a solid world containing one unresolvable block
// state at (1,4,1) plus a scheduled tick referencing it, and writes it to dir.

func writeUnknownWorld(t *testing.T, dir string) {
	t.Helper()
	reg := testRegistry(t)
	b := NewBuilder(reg, cube.Range{-64, 319})
	b.Fill(cube.Pos{0, 0, 0}, cube.Pos{8, 2, 8}, block.Stone{})
	b.SetBlock(cube.Pos{1, 4, 1}, block.Dirt{})
	p := b.Provider()
	d := &format.WorldData{}
	for pos, col := range p.Columns(world.Overworld) {
		if pos[0] == 0 && pos[1] == 0 {
			col.ScheduledBlocks = append(col.ScheduledBlocks, scheduledDirt(t, reg))
		}
		d.Columns = append(d.Columns, format.Column{X: pos[0], Z: pos[1], Col: col})
	}
	_ = p.Close()
	var buf bytes.Buffer
	if err := format.WriteWorld(&buf, d, reg, format.Options{Compression: format.CompressionNone}); err != nil {
		t.Fatal(err)
	}
	bad := corruptStateName(t, buf.Bytes(), "minecraft:dirt", "minecraft:d1rt")
	if err := os.WriteFile(filepath.Join(dir, "overworld.pile"), bad, 0o644); err != nil {
		t.Fatal(err)
	}
}

func hasUnknown(t *testing.T, dir string) bool {
	t.Helper()
	reg := testRegistry(t)
	data, err := os.ReadFile(filepath.Join(dir, "overworld.pile"))
	if err != nil {
		t.Fatal(err)
	}
	u, err := format.UnresolvedStates(data, reg)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range u {
		if strings.Contains(s, "minecraft:d1rt") {
			return true
		}
	}
	return false
}

// TestUnknownSurvivesSaveAs covers the SaveAs/Columns snapshot path.

func TestUnknownSurvivesSaveAs(t *testing.T) {
	dir := t.TempDir()
	writeUnknownWorld(t, dir)
	p, err := Open(dir, ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := p.SaveAs(out); err != nil {
		t.Fatal(err)
	}
	_ = p.Close()
	if !hasUnknown(t, out) {
		t.Fatal("SaveAs destroyed the preserved unknown state")
	}
}

// TestUnknownSurvivesMove covers both move paths.

func TestUnknownSurvivesMove(t *testing.T) {
	for _, tc := range []struct {
		name   string
		offset cube.Pos
	}{
		{"chunk-aligned", cube.Pos{16, 0, 0}},
		{"block-rewrite", cube.Pos{3, 0, 5}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeUnknownWorld(t, dir)
			if _, err := MoveWorld(dir, MoveOptions{Offset: tc.offset, Backup: false}); err != nil {
				t.Fatal(err)
			}
			if !hasUnknown(t, dir) {
				t.Fatal("move destroyed the preserved unknown state")
			}
		})
	}
}

// TestUnknownSurvivesStructureRoundTrip covers structure load/save.

func TestUnknownSurvivesStructureRoundTrip(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	writeUnknownWorld(t, dir)
	p, err := Open(dir, ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	s, err := ExtractStructure(p, world.Overworld, cube.Pos{0, 0, 0}, cube.Pos{8, 8, 8})
	_ = p.Close()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "s.pile")
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	// Load and re-save: the unknown state must still be there.
	loaded, err := LoadStructure(path)
	if err != nil {
		t.Fatal(err)
	}
	path2 := filepath.Join(t.TempDir(), "s2.pile")
	if err := loaded.Save(path2); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path2)
	u, err := format.UnresolvedStates(data, reg)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, st := range u {
		if strings.Contains(st, "minecraft:d1rt") {
			found = true
		}
	}
	if !found {
		t.Fatalf("structure round trip destroyed the unknown state: %v", u)
	}
}

// TestMovePreservesAllLayers covers the layer-2+ loss in non-aligned moves.

func TestUnknownSurvivesPasteAndImport(t *testing.T) {
	src := t.TempDir()
	writeUnknownWorld(t, src)
	p, err := Open(src, ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	s, err := ExtractStructure(p, world.Overworld, cube.Pos{0, 0, 0}, cube.Pos{8, 8, 8})
	_ = p.Close()
	if err != nil {
		t.Fatal(err)
	}

	dst := t.TempDir()
	q, err := Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PasteInto(q, world.Overworld, cube.Pos{100, 0, 100}); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
	if !hasUnknown(t, dst) {
		t.Fatal("paste destroyed the structure's preserved unknown state")
	}
}

// TestMoveKeepsUnreadableEntities: an entity whose Pos cannot be parsed must
// survive a move rather than being silently dropped.

func TestUnknownSurvivesRangeChange(t *testing.T) {
	dir := t.TempDir()
	writeUnknownWorld(t, dir)
	p, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	col, err := p.LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	// Store the (possibly re-based) column straight back, as a server would.
	if err := p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, col); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if !hasUnknown(t, dir) {
		t.Fatal("a load/store cycle destroyed the preserved state")
	}
}

// TestPasteMergesExistingSidecar: pasting into a chunk that already has
// preserved states must not drop the ones the paste did not touch.

func TestPasteMergesExistingSidecar(t *testing.T) {
	reg := testRegistry(t)
	src := t.TempDir()
	writeUnknownWorld(t, src) // unknown block at (1,4,1)
	p, err := Open(src)
	if err != nil {
		t.Fatal(err)
	}
	// A one-block structure of plain stone, pasted far from (1,4,1) but in
	// the same chunk.
	b := NewBuilder(reg, cube.Range{-64, 319})
	b.Fill(cube.Pos{0, 0, 0}, cube.Pos{0, 0, 0}, block.Stone{})
	sp := b.Provider()
	s, err := ExtractStructure(sp, world.Overworld, cube.Pos{0, 0, 0}, cube.Pos{0, 0, 0})
	_ = sp.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PasteInto(p, world.Overworld, cube.Pos{7, 6, 7}); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if !hasUnknown(t, src) {
		t.Fatal("paste dropped a preserved state it did not overwrite")
	}
}

// TestAppendModeStructureExtraction: extracting from an append-mode provider
// must carry preserved states (columnSidecar must consult the append cache).

func TestAppendModeStructureExtraction(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	writeUnknownWorld(t, dir)
	wf, err := LoadWorldFiles(dir, reg)
	if err != nil {
		t.Fatal(err)
	}
	wf.Dims[0].Indexed = true
	if err := wf.Write(dir, reg); err != nil {
		t.Fatal(err)
	}

	p, err := Open(dir, AppendMode())
	if err != nil {
		t.Fatal(err)
	}
	s, err := ExtractStructure(p, world.Overworld, cube.Pos{0, 0, 0}, cube.Pos{8, 8, 8})
	if err != nil {
		t.Fatal(err)
	}
	_ = p.Close()
	path := filepath.Join(t.TempDir(), "s.pile")
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	u, err := format.UnresolvedStates(data, reg)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, st := range u {
		if strings.Contains(st, "minecraft:d1rt") {
			found = true
		}
	}
	if !found {
		t.Fatalf("append-mode extraction lost preserved states: %v", u)
	}
}

// TestRotationPreservesMetadataAndStates covers user data and sidecars.

func TestRotationPreservesMetadataAndStates(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	writeUnknownWorld(t, dir)
	p, err := Open(dir, ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	s, err := ExtractStructure(p, world.Overworld, cube.Pos{0, 0, 0}, cube.Pos{8, 8, 8})
	_ = p.Close()
	if err != nil {
		t.Fatal(err)
	}
	s.Data().UserData = []byte("keep")

	r := s.Rotate(1)
	if string(r.Data().UserData) != "keep" {
		t.Fatalf("rotation dropped user data: %q", r.Data().UserData)
	}
	path := filepath.Join(t.TempDir(), "r.pile")
	if err := r.Save(path); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	u, err := format.UnresolvedStates(data, reg)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, st := range u {
		if strings.Contains(st, "minecraft:d1rt") {
			found = true
		}
	}
	if !found {
		t.Fatalf("rotation destroyed preserved states: %v", u)
	}
}

// TestCloseIsRetryable: a Close that fails leaves the provider usable.
