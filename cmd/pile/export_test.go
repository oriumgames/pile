package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/df-mc/dragonfly/server/block"
	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/oriumgames/pile"
)

// jsonEqual compares two JSON documents semantically: exporting reformats
// inline user data (MarshalIndent), so byte equality is not expected.
func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return false
	}
	aj, _ := json.Marshal(av)
	bj, _ := json.Marshal(bv)
	return string(aj) == string(bj)
}

func TestExportImportRoundTrip(t *testing.T) {
	reg := testRegistry(t)
	stone := reg.BlockRuntimeID(block.Stone{})

	// Source world with negative coordinates, settings, marker, JSON user data.
	srcDir := t.TempDir()
	b := pile.NewBuilder(reg, cube.Range{-64, 319})
	b.Fill(cube.Pos{-24, -10, -24}, cube.Pos{24, 6, 24}, block.Stone{})
	b.Settings(&world.Settings{
		Name: "export-test", Time: 1234, TickRange: 8,
		DefaultGameMode: world.GameModeAdventure, Difficulty: world.DifficultyHard,
		Spawn: cube.Pos{1, 7, 2},
	})
	b.SetMarker(pile.Marker{Name: "spawn", Kind: "spawn", Pos: &[3]float64{1, 7, 2}})
	b.SetUserData([]byte(`{"game":"bedwars"}`))
	if err := b.Save(srcDir); err != nil {
		t.Fatal(err)
	}

	// Export.
	outDir := t.TempDir()
	if err := cmdExport([]string{srcDir, outDir}); err != nil {
		t.Fatal(err)
	}
	blob, err := os.ReadFile(filepath.Join(outDir, "data.json"))
	if err != nil {
		t.Fatal(err)
	}
	var data exportData
	if err := json.Unmarshal(blob, &data); err != nil {
		t.Fatal(err)
	}
	if data.Settings.Name != "export-test" || data.Dimension != "overworld" {
		t.Fatalf("data.json settings wrong: %+v", data)
	}
	if !jsonEqual(t, data.UserData, []byte(`{"game":"bedwars"}`)) {
		t.Fatalf("user data not inlined as JSON: %q", data.UserData)
	}
	// Bounds are chunk-granular; -24 lives in chunk -2 (block -32).
	if data.Origin[0] != -32 || data.Origin[2] != -32 {
		t.Fatalf("origin = %v, want chunk-aligned -32", data.Origin)
	}

	// Import into a fresh world.
	dstDir := filepath.Join(t.TempDir(), "world")
	if err := cmdImport([]string{outDir, dstDir}); err != nil {
		t.Fatal(err)
	}
	p, err := pile.Open(dstDir, pile.ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if got := p.Settings(); got.Name != "export-test" || got.Time != 1234 ||
		got.TickRange != 8 || got.Spawn != (cube.Pos{1, 7, 2}) {
		t.Fatalf("imported settings wrong: %+v", got)
	}
	if ms := p.Markers(); len(ms) != 1 || ms[0].Name != "spawn" {
		t.Fatalf("imported markers wrong: %+v", ms)
	}
	if !jsonEqual(t, p.UserData(), []byte(`{"game":"bedwars"}`)) {
		t.Fatalf("imported user data wrong: %q", p.UserData())
	}
	// Blocks restored at original absolute positions.
	col, err := p.LoadColumn(world.ChunkPos{-2, -2}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	if rid := col.Chunk.Block(uint8(-24&15), -5, uint8(-24&15), 0); rid != stone {
		t.Fatalf("imported block missing at (-24,-5,-24), rid %d", rid)
	}
	col2, err := p.LoadColumn(world.ChunkPos{1, 1}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	if rid := col2.Chunk.Block(uint8(24&15), 6, uint8(24&15), 0); rid != stone {
		t.Fatalf("imported block missing at (24,6,24), rid %d", rid)
	}

	// Import refuses an existing pile world.
	if err := cmdImport([]string{outDir, dstDir}); err == nil {
		t.Fatal("import into existing world did not fail")
	}
}
