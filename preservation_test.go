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
	return corruptStateNames(t, file, [][2]string{{from, to}})
}

func corruptStateNames(t *testing.T, file []byte, pairs [][2]string) []byte {
	t.Helper()
	bad := bytes.Clone(file)
	body := bad[16 : len(bad)-44]
	for _, p := range pairs {
		from, to := p[0], p[1]
		if len(from) != len(to) {
			t.Fatal("replacement must be equal length")
		}
		idx := bytes.Index(body, []byte(from))
		if idx < 0 {
			t.Fatalf("%s not found in body", from)
		}
		copy(body[idx:], to)
	}
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
	// And an entry still ties it to its position: the palette check above
	// passes on a file whose sidecar entries are gone but whose state table
	// survived, which is not preservation.
	if got := preservedStateAt(t, dir, reg, cube.Pos{1, 4, 1}); got != "minecraft:d1rt" {
		t.Fatalf("preserved state at (1,4,1) = %q, want minecraft:d1rt", got)
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
	_ = iw.Close()
	if got := preservedStateAtIndexed(t, dir, reg, cube.Pos{1, 4, 1}); got != "minecraft:d1rt" {
		t.Fatalf("preserved state at (1,4,1) = %q, want minecraft:d1rt: an indexed palette keeps the name whether or not anything still references it", got)
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
	writeUnknownWorldNamed(t, dir, "minecraft:d1rt")
}

func writeUnknownWorldNamed(t *testing.T, dir, to string) {
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
	bad := corruptStateName(t, buf.Bytes(), "minecraft:dirt", to)
	if err := os.WriteFile(filepath.Join(dir, "overworld.pile"), bad, 0o644); err != nil {
		t.Fatal(err)
	}
}

// toIndexed rewrites a world's dimension files in indexed mode.
func toIndexed(t *testing.T, dir string, reg world.BlockRegistry) {
	t.Helper()
	wf, err := LoadWorldFiles(dir, reg)
	if err != nil {
		t.Fatal(err)
	}
	for i := range wf.Dims {
		wf.Dims[i].Indexed = true
	}
	if err := wf.Write(dir, reg); err != nil {
		t.Fatal(err)
	}
}

func hasUnknown(t *testing.T, dir string) bool {
	t.Helper()
	return hasUnknownNamed(t, dir, "minecraft:d1rt")
}

func hasUnknownNamed(t *testing.T, dir, name string) bool {
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
		if strings.Contains(s, name) {
			return true
		}
	}
	return false
}

// preservedStateIn returns the name of the preserved block state anchored at a
// world position in a decoded column, or "" if none is.
//
// UnresolvedStates, which most of these tests use, reads the file's state
// palette: it proves a name is somewhere in the file and says nothing about
// whether an entry still ties it to a position. A sidecar that lost its
// entries but kept its state table leaves the palette untouched, so the two
// checks are not the same claim and the weaker one hid a live control.
func preservedStateIn(c format.Column, pos cube.Pos) string {
	sec := int32(pos.Y() >> 4)
	idx := uint16(pos.X()&15)<<8 | uint16(pos.Z()&15)<<4 | uint16(pos.Y()&15)
	for _, u := range c.Unknown {
		if u.Section != sec || u.Layer != 0 {
			continue
		}
		if u.Index != idx && u.Index != format.WholeStorage {
			continue
		}
		if int(u.State) < len(c.UnknownStates) {
			return c.UnknownStates[u.State].Name
		}
	}
	return ""
}

func preservedStateAt(t *testing.T, dir string, reg world.BlockRegistry, pos cube.Pos) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "overworld.pile"))
	if err != nil {
		t.Fatal(err)
	}
	d, err := format.ReadWorld(data, reg)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range d.Columns {
		if c.X == int32(pos.X()>>4) && c.Z == int32(pos.Z()>>4) {
			return preservedStateIn(c, pos)
		}
	}
	return ""
}

func preservedStateAtIndexed(t *testing.T, dir string, reg world.BlockRegistry, pos cube.Pos) string {
	t.Helper()
	iw, err := format.OpenIndexed(filepath.Join(dir, "overworld.pile"), reg, true)
	if err != nil {
		t.Fatal(err)
	}
	defer iw.Close()
	c, err := iw.Column(int32(pos.X()>>4), int32(pos.Z()>>4))
	if err != nil {
		t.Fatal(err)
	}
	return preservedStateIn(c, pos)
}

// statesContain reports whether a preserved-state table names a block.
func statesContain(states []format.BlockState, name string) bool {
	for _, s := range states {
		if s.Name == name {
			return true
		}
	}
	return false
}

// plantUnknownBiome puts a preserved, unresolvable biome name into the first
// column of a world's overworld file. The provider has no API for minting one:
// a preserved biome only ever arrives from a file whose name the registry
// cannot resolve.
func plantUnknownBiome(t *testing.T, dir string, reg world.BlockRegistry, name string) {
	t.Helper()
	wf, err := LoadWorldFiles(dir, reg)
	if err != nil {
		t.Fatal(err)
	}
	df := wf.Dim(world.Overworld)
	if df == nil || len(df.Columns) == 0 {
		t.Fatal("no columns loaded")
	}
	// Preservation only re-emits where the runtime fallback is still in place,
	// so the section has to hold it.
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
	df.Columns[0].UnknownBiomeNames = []string{name}
	df.Columns[0].UnknownBiomes = []format.UnknownBlock{
		{Section: -4, Index: format.WholeStorage, State: 0},
	}
	if err := wf.Write(dir, reg); err != nil {
		t.Fatal(err)
	}
}

// hasUnknownBiome reports whether a world still carries a preserved biome name.
func hasUnknownBiome(t *testing.T, dir string, reg world.BlockRegistry, name string) bool {
	t.Helper()
	wf, err := LoadWorldFiles(dir, reg)
	if err != nil {
		t.Fatal(err)
	}
	df := wf.Dim(world.Overworld)
	if df == nil {
		return false
	}
	for _, c := range df.Columns {
		for _, n := range c.UnknownBiomeNames {
			if n == name {
				return true
			}
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
	if got := preservedStateAt(t, out, testRegistry(t), cube.Pos{1, 4, 1}); got != "minecraft:d1rt" {
		t.Fatalf("preserved state at (1,4,1) = %q, want minecraft:d1rt", got)
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
	if got := preservedStateAt(t, dir, testRegistry(t), cube.Pos{1, 4, 1}); got != "minecraft:d1rt" {
		t.Fatalf("preserved state at (1,4,1) = %q, want minecraft:d1rt", got)
	}
}

// TestPasteMergesExistingSidecar: pasting into a chunk that already has
// preserved states must not drop the ones the paste did not touch.

func TestPasteMergesExistingSidecar(t *testing.T) {
	reg := testRegistry(t)
	dst := t.TempDir()
	writeUnknownWorld(t, dst) // unknown block at (1,4,1), named minecraft:d1rt
	plantUnknownBiome(t, dst, reg, "audit:unknown")

	// The pasted structure has to carry a preserved state of its own, or the
	// merge is never reached: with nothing to place, the paste hands the
	// provider no sidecar at all and the destination's own is inherited by a
	// different path entirely. Its state is named differently from the
	// destination's so the two can be told apart in the result.
	other := t.TempDir()
	writeUnknownWorldNamed(t, other, "minecraft:d2rt")
	sp, err := Open(other, ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	s, err := ExtractStructure(sp, world.Overworld, cube.Pos{1, 4, 1}, cube.Pos{1, 4, 1})
	_ = sp.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !statesContain(s.Data().UnknownStates, "minecraft:d2rt") {
		t.Fatalf("the fixture structure carries no preserved state: %+v", s.Data().UnknownStates)
	}

	p, err := Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	// Same chunk as (1,4,1), a different position.
	if err := s.PasteInto(p, world.Overworld, cube.Pos{7, 6, 7}); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if !hasUnknownNamed(t, dst, "minecraft:d2rt") {
		t.Fatal("paste lost the preserved state it brought with it")
	}
	if got := preservedStateAt(t, dst, reg, cube.Pos{1, 4, 1}); got != "minecraft:d1rt" {
		t.Fatalf("preserved state at (1,4,1) = %q, want minecraft:d1rt: the paste dropped an entry it did not overwrite", got)
	}
	if got := preservedStateAt(t, dst, reg, cube.Pos{7, 6, 7}); got != "minecraft:d2rt" {
		t.Fatalf("preserved state at (7,6,7) = %q, want minecraft:d2rt", got)
	}
	if !hasUnknownBiome(t, dst, reg, "audit:unknown") {
		t.Fatal("paste dropped the column's preserved biome, which it never touches")
	}
}

// TestPasteOverwritesTheStateItReplaces: a paste landing on a position that
// already carries a preserved state has to drop the inherited entry. Leaving
// it there gives the column two entries for one position, and the first one
// written claims the placeholder slot, so the state the paste replaced wins.
func TestPasteOverwritesTheStateItReplaces(t *testing.T) {
	reg := testRegistry(t)
	dst := t.TempDir()
	writeUnknownWorld(t, dst)

	other := t.TempDir()
	writeUnknownWorldNamed(t, other, "minecraft:d2rt")
	sp, err := Open(other, ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	s, err := ExtractStructure(sp, world.Overworld, cube.Pos{1, 4, 1}, cube.Pos{1, 4, 1})
	_ = sp.Close()
	if err != nil {
		t.Fatal(err)
	}

	p, err := Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PasteInto(p, world.Overworld, cube.Pos{1, 4, 1}); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if got := preservedStateAt(t, dst, reg, cube.Pos{1, 4, 1}); got != "minecraft:d2rt" {
		t.Fatalf("preserved state at (1,4,1) = %q, want minecraft:d2rt: the paste kept the entry it replaced", got)
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

// TestAppendStoreRecoversSidecarWithoutALoad: the provider's metadata cache is
// bounded and empty at open, so an overwrite of a position it has not seen
// this session has to read the previous record to find the preserved states it
// must carry. Every other append test loads the column first, which fills that
// cache, so the read-through was reached by nothing.
func TestAppendStoreRecoversSidecarWithoutALoad(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	writeUnknownWorld(t, dir)
	toIndexed(t, dir, reg)

	// Session one: read the column and put it aside.
	p, err := Open(dir, AppendMode())
	if err != nil {
		t.Fatal(err)
	}
	col, err := p.LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	// Session two: store it straight back, with no load to fill the cache.
	q, err := Open(dir, AppendMode())
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
	u, err := iw.UnresolvedStates()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range u {
		if strings.Contains(s, "minecraft:d1rt") {
			found = true
		}
	}
	if !found {
		t.Fatalf("an append store that never loaded the column dropped its preserved states: %v", u)
	}
	_ = iw.Close()
	if got := preservedStateAtIndexed(t, dir, reg, cube.Pos{1, 4, 1}); got != "minecraft:d1rt" {
		t.Fatalf("preserved state at (1,4,1) = %q, want minecraft:d1rt: an append store that never loaded the column dropped the entry anchoring it", got)
	}
}

// TestAppendSidecarReadsThroughOnAMetaCacheMiss: columnSidecar answers from the
// bounded metadata cache when it can and from the record on disk when it
// cannot. Every caller in the tree loads the column first, which fills the
// cache, so the read-through half was reached by nothing — including by the
// test named for it.
func TestAppendSidecarReadsThroughOnAMetaCacheMiss(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	writeUnknownWorld(t, dir)
	toIndexed(t, dir, reg)

	p, err := Open(dir, AppendMode())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	// Nothing has been loaded, so the cache is empty and the answer has to
	// come off disk.
	side := p.columnSidecar(world.ChunkPos{0, 0}, world.Overworld)
	if !statesContain(side.states, "minecraft:d1rt") {
		t.Fatalf("read-through returned no preserved states: %+v", side.states)
	}
	if len(side.unknown) == 0 {
		t.Fatal("read-through returned a state table with no entries referencing it")
	}
}

// TestExtractCarriesUniformAndRebasesStates: a column's preserved states can
// cover a whole storage rather than a single index — which is exactly how a
// uniformly unresolved section decodes — and every column after the first has
// its state references rebased onto the growing table. Two columns unresolved
// in different blocks separate the two rules: with no rebasing, the second
// column's positions resolve to the first column's name.
func TestExtractCarriesUniformAndRebasesStates(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()

	b := NewBuilder(reg, cube.Range{-64, 319})
	b.Fill(cube.Pos{0, 0, 0}, cube.Pos{15, 15, 15}, block.Dirt{})
	b.Fill(cube.Pos{16, 0, 0}, cube.Pos{31, 15, 15}, block.Grass{})
	bp := b.Provider()
	d := &format.WorldData{}
	for pos, col := range bp.Columns(world.Overworld) {
		d.Columns = append(d.Columns, format.Column{X: pos[0], Z: pos[1], Col: col})
	}
	_ = bp.Close()
	var buf bytes.Buffer
	if err := format.WriteWorld(&buf, d, reg, format.Options{Compression: format.CompressionNone}); err != nil {
		t.Fatal(err)
	}
	bad := corruptStateNames(t, buf.Bytes(), [][2]string{
		{"minecraft:dirt", "minecraft:d1rt"},
		{"minecraft:grass_block", "minecraft:g1ass_block"},
	})
	if err := os.WriteFile(filepath.Join(dir, "overworld.pile"), bad, 0o644); err != nil {
		t.Fatal(err)
	}

	p, err := Open(dir, ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	s, err := ExtractStructure(p, world.Overworld, cube.Pos{0, 0, 0}, cube.Pos{31, 15, 15})
	_ = p.Close()
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]int{}
	for _, u := range s.Data().Unknown {
		if int(u.State) >= len(s.Data().UnknownStates) {
			t.Fatalf("state reference %d past the %d-entry table", u.State, len(s.Data().UnknownStates))
		}
		names[s.Data().UnknownStates[u.State].Name]++
	}
	if names["minecraft:d1rt"] == 0 {
		t.Fatalf("extraction dropped the whole-storage preserved state: %v", names)
	}
	if names["minecraft:g1ass_block"] == 0 {
		t.Fatalf("the second column's positions resolved to %v: its state references were not rebased", names)
	}
}

// TestExtractRebasesPerColumnStateTables: two columns of one provider can hold
// different preserved-state tables, because a paste appends its structure's
// states to the column it lands in and to no other. An extraction spanning
// both has to rebase the second column's references onto the growing table.
// No file can show this: writing one out gives every column the file's single
// table again, which is why an extraction straight off disk passes either way.
func TestExtractRebasesPerColumnStateTables(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()

	b := NewBuilder(reg, cube.Range{-64, 319})
	b.Fill(cube.Pos{0, 0, 0}, cube.Pos{20, 2, 8}, block.Stone{})
	b.SetBlock(cube.Pos{1, 4, 1}, block.Dirt{})
	b.SetBlock(cube.Pos{17, 4, 1}, block.Dirt{})
	bp := b.Provider()
	d := &format.WorldData{}
	for pos, col := range bp.Columns(world.Overworld) {
		d.Columns = append(d.Columns, format.Column{X: pos[0], Z: pos[1], Col: col})
	}
	_ = bp.Close()
	var buf bytes.Buffer
	if err := format.WriteWorld(&buf, d, reg, format.Options{Compression: format.CompressionNone}); err != nil {
		t.Fatal(err)
	}
	bad := corruptStateName(t, buf.Bytes(), "minecraft:dirt", "minecraft:d1rt")
	if err := os.WriteFile(filepath.Join(dir, "overworld.pile"), bad, 0o644); err != nil {
		t.Fatal(err)
	}

	// A structure carrying a state of its own, pasted into the second chunk
	// only, so that column's table is one entry longer than the first's.
	other := t.TempDir()
	writeUnknownWorldNamed(t, other, "minecraft:d2rt")
	sp, err := Open(other, ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	s2, err := ExtractStructure(sp, world.Overworld, cube.Pos{1, 4, 1}, cube.Pos{1, 4, 1})
	_ = sp.Close()
	if err != nil {
		t.Fatal(err)
	}

	p, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s2.PasteInto(p, world.Overworld, cube.Pos{18, 6, 1}); err != nil {
		t.Fatal(err)
	}
	s, err := ExtractStructure(p, world.Overworld, cube.Pos{0, 0, 0}, cube.Pos{31, 8, 8})
	if err != nil {
		t.Fatal(err)
	}
	_ = p.Close()

	names := map[string]int{}
	for _, u := range s.Data().Unknown {
		if int(u.State) >= len(s.Data().UnknownStates) {
			t.Fatalf("state reference %d past the %d-entry table", u.State, len(s.Data().UnknownStates))
		}
		names[s.Data().UnknownStates[u.State].Name]++
	}
	if names["minecraft:d1rt"] == 0 {
		t.Fatalf("extraction lost the first column's preserved state: %v", names)
	}
	if names["minecraft:d2rt"] == 0 {
		t.Fatalf("the second column's states resolved to %v: its references were not rebased onto the growing table", names)
	}
}

// TestExtractReachesPreservedLayersTheChunkNeverAllocated: on a registry whose
// placeholder resolves to air, a preserved state on a high layer has no
// storage of its own, so the traversal ceiling has to come from the sidecar
// rather than from what the chunk allocated.
func TestExtractReachesPreservedLayersTheChunkNeverAllocated(t *testing.T) {
	reg := airPlaceholderRegistry{testRegistry(t)}
	data, err := format.NewStructureData([3]int32{16, 16, 16})
	if err != nil {
		t.Fatal(err)
	}
	out := newStructure(data, StructureRegistry(reg))
	col := &chunk.Column{Chunk: chunk.New(reg, cube.Range{-64, 319})}
	extractChunkRegion(out, col, 0, 0, cube.Pos{0, 0, 0}, cube.Pos{15, 15, 15},
		[]format.UnknownBlock{{Section: 0, Layer: 2, Index: 0, State: 0}},
		[]format.BlockState{{Name: "audit:deep", Version: 1}})
	if len(out.data.Unknown) != 1 {
		t.Fatalf("extraction found %d preserved states, want 1: a state on layer 2 was never visited",
			len(out.data.Unknown))
	}
}

// uniformStructure builds a 16-cube of placeholder blocks carrying one
// whole-storage preserved state, the shape a uniformly unresolved cell decodes
// into.
func uniformStructure(t *testing.T, reg world.BlockRegistry) *Structure {
	t.Helper()
	placeholder, ok := reg.StateToRuntimeID("minecraft:info_update", map[string]any{})
	if !ok {
		t.Fatal("the registry does not know the placeholder block")
	}
	ch := chunk.New(reg, cube.Range{-64, 319})
	for x := range uint8(16) {
		for z := range uint8(16) {
			for y := int16(0); y < 16; y++ {
				ch.SetBlock(x, y, z, 0, placeholder)
			}
		}
	}
	p := NewMemory(Registry(reg))
	if err := p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, &chunk.Column{Chunk: ch}); err != nil {
		t.Fatal(err)
	}
	s, err := ExtractStructure(p, world.Overworld, cube.Pos{0, 0, 0}, cube.Pos{15, 15, 15},
		StructureRegistry(reg))
	_ = p.Close()
	if err != nil {
		t.Fatal(err)
	}
	s.Data().UnknownStates = []format.BlockState{{Name: "audit:uniform", Version: 1}}
	s.Data().Unknown = []format.UnknownBlock{
		{Section: 0, Layer: 0, Index: format.WholeStorage, State: 0},
	}
	return s
}

// TestPasteCarriesUniformPreservedStates: the paste resolves whole-storage
// entries from a table of their own, and a paste that consults only the
// per-index table writes the placeholder with nothing to say what it stands
// for.
func TestPasteCarriesUniformPreservedStates(t *testing.T) {
	reg := testRegistry(t)
	s := uniformStructure(t, reg)
	dst := t.TempDir()
	q, err := Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PasteInto(q, world.Overworld, cube.Pos{0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
	if !hasUnknownNamed(t, dst, "audit:uniform") {
		t.Fatal("the paste dropped a whole-storage preserved state")
	}
}

// TestRotationCarriesUniformPreservedStates: the same table, in the rotator.
func TestRotationCarriesUniformPreservedStates(t *testing.T) {
	reg := testRegistry(t)
	r := uniformStructure(t, reg).Rotate(1)
	n := 0
	for _, u := range r.Data().Unknown {
		if int(u.State) >= len(r.Data().UnknownStates) {
			t.Fatalf("state reference %d past the %d-entry table", u.State, len(r.Data().UnknownStates))
		}
		if r.Data().UnknownStates[u.State].Name != "audit:uniform" {
			t.Fatalf("preserved state resolved to %q", r.Data().UnknownStates[u.State].Name)
		}
		n++
	}
	if n != 16*16*16 {
		t.Fatalf("rotation carried %d preserved positions, want %d: the whole-storage table was not consulted",
			n, 16*16*16)
	}
}

// TestBiomeSidecarSurvivesStoreAndSaveAs: an unresolved biome is preserved by
// the same sidecar machinery as an unresolved block, and both the store path
// and the SaveAs snapshot carry it in fields of their own.
func TestBiomeSidecarSurvivesStoreAndSaveAs(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	writeUnknownWorld(t, dir)
	plantUnknownBiome(t, dir, reg, "audit:unknown")

	p, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	col, err := p.LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	// The server touches a block elsewhere in the column and stores it back.
	col.Chunk.SetBlock(2, 4, 2, 0, reg.BlockRuntimeID(block.Stone{}))
	if err := p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, col); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := p.SaveAs(out); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if !hasUnknownBiome(t, dir, reg, "audit:unknown") {
		t.Fatal("a load/store cycle renamed the unresolved biome to the runtime fallback")
	}
	if !hasUnknownBiome(t, out, reg, "audit:unknown") {
		t.Fatal("SaveAs renamed the unresolved biome to the runtime fallback")
	}
}

// TestTemplateInstanceInheritsPreservedStates: an instance's first store of a
// template column has no previous column of its own to inherit from, so it has
// to take the base's sidecar. Nothing else puts it there.
func TestTemplateInstanceInheritsPreservedStates(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	writeUnknownWorld(t, dir)

	tpl, err := OpenTemplate(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer tpl.Close()
	inst := tpl.Instance()
	col, err := inst.LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	col.Chunk.SetBlock(2, 4, 2, 0, reg.BlockRuntimeID(block.Stone{}))
	if err := inst.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, col); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := inst.SaveAs(out); err != nil {
		t.Fatal(err)
	}
	_ = inst.Close()
	if !hasUnknown(t, out) {
		t.Fatal("a template instance's first store dropped the base column's preserved states")
	}
}

// TestCloseIsRetryable: a Close that fails leaves the provider usable.
