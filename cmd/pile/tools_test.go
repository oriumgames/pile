package main

import (
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/df-mc/dragonfly/server/block"
	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/item"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/oriumgames/pile"
)

// buildWorldA: chunks (0,0), (1,0), (2,0).
func buildWorldA(t *testing.T, dir string) {
	t.Helper()
	reg := testRegistry(t)
	b := pile.NewBuilder(reg, cube.Range{-64, 319})
	b.Fill(cube.Pos{0, 0, 0}, cube.Pos{47, 3, 15}, block.Stone{})
	b.Settings(&world.Settings{Name: "A", TickRange: 6})
	if err := b.Save(dir); err != nil {
		t.Fatal(err)
	}
}

// buildWorldB: (0,0) same as A, (1,0) modified, (2,0) removed, (3,0) added.
func buildWorldB(t *testing.T, dir string) {
	t.Helper()
	reg := testRegistry(t)
	b := pile.NewBuilder(reg, cube.Range{-64, 319})
	b.Fill(cube.Pos{0, 0, 0}, cube.Pos{15, 3, 15}, block.Stone{})  // (0,0) identical
	b.Fill(cube.Pos{16, 0, 0}, cube.Pos{31, 3, 15}, block.Stone{}) // (1,0) base
	b.SetBlock(cube.Pos{20, 10, 5}, block.Dirt{})                  // (1,0) modified
	b.Fill(cube.Pos{48, 0, 0}, cube.Pos{63, 3, 15}, block.Stone{}) // (3,0) added
	b.Settings(&world.Settings{Name: "B", TickRange: 6})
	if err := b.Save(dir); err != nil {
		t.Fatal(err)
	}
}

func TestDiffPatchApply(t *testing.T) {
	reg := testRegistry(t)
	dirA, dirB, dirC := t.TempDir(), t.TempDir(), t.TempDir()
	buildWorldA(t, dirA)
	buildWorldB(t, dirB)
	buildWorldA(t, dirC) // deterministic: identical to dirA

	a, err := pile.LoadWorldFiles(dirA, reg)
	if err != nil {
		t.Fatal(err)
	}
	b, err := pile.LoadWorldFiles(dirB, reg)
	if err != nil {
		t.Fatal(err)
	}
	diffs, err := diffWorlds(a, b, reg)
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 1 {
		t.Fatalf("diff dims = %d, want 1", len(diffs))
	}
	d := diffs[0]
	if len(d.added) != 1 || d.added[0] != [2]int32{3, 0} {
		t.Fatalf("added = %v", d.added)
	}
	if len(d.removed) != 1 || d.removed[0] != [2]int32{2, 0} {
		t.Fatalf("removed = %v", d.removed)
	}
	if len(d.modified) != 1 || d.modified[0] != [2]int32{1, 0} {
		t.Fatalf("modified = %v", d.modified)
	}

	// Patch A -> B, apply onto the copy of A, expect equality with B.
	patchPath := filepath.Join(t.TempDir(), "up.pilepatch")
	if err := cmdPatch([]string{"-o", patchPath, dirA, dirB}); err != nil {
		t.Fatal(err)
	}
	if err := cmdApply([]string{dirC, patchPath}); err != nil {
		t.Fatal(err)
	}
	c, err := pile.LoadWorldFiles(dirC, reg)
	if err != nil {
		t.Fatal(err)
	}
	diffs2, err := diffWorlds(c, b, reg)
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs2) != 0 {
		t.Fatalf("world after apply still differs: %+v", diffs2)
	}
	p, err := pile.Open(dirC, pile.ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if got := p.Settings().Name; got != "B" {
		t.Fatalf("meta not applied: name = %q", got)
	}
}

func TestPruneCLI(t *testing.T) {
	dir := t.TempDir()
	buildWorldA(t, dir) // chunks (0,0),(1,0),(2,0)
	if err := cmdPrune([]string{"--bounds", "0,0,20,15", "--no-backup", dir}); err != nil {
		t.Fatal(err)
	}
	p, err := pile.Open(dir, pile.ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	// Bounds 0..20 intersect chunks (0,0) and (1,0) only.
	if got := p.ChunkCount(world.Overworld); got != 2 {
		t.Fatalf("chunks after prune = %d, want 2", got)
	}
}

func TestRenderCLI(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	b := pile.NewBuilder(reg, cube.Range{-64, 319})
	b.Fill(cube.Pos{0, 0, 0}, cube.Pos{47, 3, 15}, block.Stone{})
	b.SetBlock(cube.Pos{5, 4, 5}, block.Wool{Colour: item.ColourRed()})
	if err := b.Save(dir); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "map.png")
	if err := cmdRender([]string{"-o", out, "--bg", "#102030", dir}); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	// 3 chunks in a row: 48x16 blocks.
	if bnd := img.Bounds(); bnd.Dx() != 48 || bnd.Dy() != 16 {
		t.Fatalf("render size = %dx%d, want 48x16", bnd.Dx(), bnd.Dy())
	}
	// Red wool at (5,5) must render red-dominant via the colour property.
	r, g, bl, _ := img.At(5, 5).RGBA()
	if !(r > g && r > bl) {
		t.Fatalf("red wool pixel not red-dominant: r=%d g=%d b=%d", r>>8, g>>8, bl>>8)
	}
	// World A's chunks fully cover 48x16, so use a stone pixel vs bg sanity:
	// stone at (20,8) must differ from the background color.
	sr, sg, sb, _ := img.At(20, 8).RGBA()
	if sr>>8 == 0x10 && sg>>8 == 0x20 && sb>>8 == 0x30 {
		t.Fatal("stone pixel rendered as background")
	}
}

func TestUpgradeAndCheckCLI(t *testing.T) {
	dir := t.TempDir()
	buildWorldA(t, dir)
	if err := cmdCheck([]string{dir}); err != nil {
		t.Fatal(err)
	}
	if err := cmdUpgrade([]string{dir}); err != nil {
		t.Fatal(err)
	}
	p, err := pile.Open(dir, pile.ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if p.ChunkCount(world.Overworld) != 3 {
		t.Fatal("upgrade lost chunks")
	}
}

// TestApplyBaseVerification: applying a patch to a world that is not the
// patch's base is refused unless forced.
func TestApplyBaseVerification(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	buildWorldA(t, dirA)
	buildWorldB(t, dirB)
	patchPath := filepath.Join(t.TempDir(), "up.pilepatch")
	if err := cmdPatch([]string{"-o", patchPath, dirA, dirB}); err != nil {
		t.Fatal(err)
	}
	// Wrong base: world B is not world A.
	wrong := t.TempDir()
	buildWorldB(t, wrong)
	if err := cmdApply([]string{wrong, patchPath}); err == nil {
		t.Fatal("apply to mismatched base was not refused")
	}
	if err := cmdApply([]string{"--force", "--no-backup", wrong, patchPath}); err != nil {
		t.Fatalf("forced apply failed: %v", err)
	}
	// Correct base: backup is written.
	right := t.TempDir()
	buildWorldA(t, right)
	if err := cmdApply([]string{right, patchPath}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(right, "snapshots", "pre-apply", "overworld.pile")); err != nil {
		t.Fatal("pre-apply backup missing")
	}
}
