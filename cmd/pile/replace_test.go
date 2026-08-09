package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/df-mc/dragonfly/server/block"
	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
	"github.com/oriumgames/pile"
	"github.com/oriumgames/pile/format"
)

// replaceWorld writes a world holding a 4x1x4 slab of stone with two dirt
// blocks in it, and returns the directory.
func replaceWorld(t *testing.T) string {
	t.Helper()
	reg := hostileReg(t)
	stone, dirt := reg.BlockRuntimeID(block.Stone{}), reg.BlockRuntimeID(block.Dirt{})
	ch := chunk.New(reg, cube.Range{0, 15})
	for x := range byte(4) {
		for z := range byte(4) {
			ch.SetBlock(x, 1, z, 0, stone)
		}
	}
	ch.SetBlock(0, 1, 0, 0, dirt)
	ch.SetBlock(1, 1, 1, 0, dirt)

	dir := t.TempDir()
	writeWorldAt(t, filepath.Join(dir, "overworld.pile"),
		[]format.Column{{X: 0, Z: 0, Col: &chunk.Column{Chunk: ch}}})
	return dir
}

// countBlocks reports how many blocks of a state a world holds.
func countBlocks(t *testing.T, dir, name string) int {
	t.Helper()
	reg := hostileReg(t)
	want, ok := reg.StateToRuntimeID(name, nil)
	if !ok {
		t.Fatalf("%s is not a state this build knows", name)
	}
	wf, err := pile.LoadWorldFiles(dir, reg)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, df := range wf.Dims {
		for _, c := range df.Columns {
			ch := c.Col.Chunk
			r := ch.Range()
			for x := range byte(16) {
				for z := range byte(16) {
					for y := r[0]; y <= r[1]; y++ {
						if ch.Block(x, int16(y), z, 0) == want {
							n++
						}
					}
				}
			}
		}
	}
	return n
}

// TestReplaceRewritesOnlyWhatItNames.
//
// The command exists for a converted world holding blocks from a behaviour
// pack, where the choice after `pile convert --permissive` is to register them
// in the server or to get rid of them. Getting rid of them must not touch
// anything else.
func TestReplaceRewritesOnlyWhatItNames(t *testing.T) {
	dir := replaceWorld(t)
	if got := countBlocks(t, dir, "minecraft:dirt"); got != 2 {
		t.Fatalf("fixture holds %d dirt, want 2", got)
	}
	if got := countBlocks(t, dir, "minecraft:stone"); got != 14 {
		t.Fatalf("fixture holds %d stone, want 14", got)
	}

	if err := cmdReplace([]string{"--from", "minecraft:dirt", "--to", "minecraft:air", "--no-backup", dir}); err != nil {
		t.Fatal(err)
	}
	if got := countBlocks(t, dir, "minecraft:dirt"); got != 0 {
		t.Errorf("%d dirt survived the replace", got)
	}
	if got := countBlocks(t, dir, "minecraft:stone"); got != 14 {
		t.Errorf("the replace changed the stone too: %d, want 14", got)
	}
}

// TestReplaceKeepsEveryColumn: the world is loaded and written back whole, so
// a bug in the rewrite that dropped columns would look like a successful
// replace. Checked because a converted world's chunk count is the first thing
// that would go unnoticed.
func TestReplaceKeepsEveryColumn(t *testing.T) {
	reg := hostileReg(t)
	dir := t.TempDir()
	var cols []format.Column
	for x := range int32(6) {
		cols = append(cols, solidColumn(t, x, 0))
	}
	// One column holds the block being replaced; the rest must survive
	// untouched, including the ones the rewrite never looks at.
	ch := chunk.New(reg, cube.Range{0, 15})
	ch.SetBlock(0, 0, 0, 0, reg.BlockRuntimeID(block.Dirt{}))
	cols = append(cols, format.Column{X: 6, Z: 0, Col: &chunk.Column{Chunk: ch}})
	writeWorldAt(t, filepath.Join(dir, "overworld.pile"), cols)

	if err := cmdReplace([]string{"--from", "minecraft:dirt", "--to", "minecraft:air", "--no-backup", dir}); err != nil {
		t.Fatal(err)
	}
	wf, err := pile.LoadWorldFiles(dir, reg)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(wf.Dim(world.Overworld).Columns); got != 7 {
		t.Errorf("the world has %d columns after a replace, want 7", got)
	}
}

// TestReplaceTakesASnapshot: a replace is not undoable from the world it wrote,
// so the default has to leave something to go back to.
func TestReplaceTakesASnapshot(t *testing.T) {
	dir := replaceWorld(t)
	if err := cmdReplace([]string{"--from", "minecraft:dirt", "--to", "minecraft:air", dir}); err != nil {
		t.Fatal(err)
	}
	p, err := pile.Open(dir, pile.ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	names, err := p.Snapshots()
	_ = p.Close()
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, n := range names {
		if n == "pre-replace" {
			found = true
		}
	}
	if !found {
		t.Errorf("no pre-replace snapshot; snapshots = %v", names)
	}
}

// TestReplaceDryRunWritesNothing.
func TestReplaceDryRunWritesNothing(t *testing.T) {
	dir := replaceWorld(t)
	path := filepath.Join(dir, "overworld.pile")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmdReplace([]string{"--from", "minecraft:dirt", "--to", "minecraft:air", "--dry-run", dir}); err != nil {
		t.Fatal(err)
	}
	if countBlocks(t, dir, "minecraft:dirt") != 2 {
		t.Error("a dry run changed the world")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("a dry run rewrote the file")
	}
}

// TestReplaceRefusesAnUnknownTarget: putting a block that does not exist into a
// world would either panic or silently write air, and both are worse than a
// sentence naming the flag.
func TestReplaceRefusesAnUnknownTarget(t *testing.T) {
	dir := replaceWorld(t)
	err := cmdReplace([]string{"--from", "minecraft:dirt", "--to", "nosuch:block", "--no-backup", dir})
	if err == nil {
		t.Fatal("a target this build does not know was accepted")
	}
	if !strings.Contains(err.Error(), "not a block this build knows") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
	if countBlocks(t, dir, "minecraft:dirt") != 2 {
		t.Error("the refused replace changed the world anyway")
	}
}

// TestReplaceNeedsAFilter.
func TestReplaceNeedsAFilter(t *testing.T) {
	dir := replaceWorld(t)
	if err := cmdReplace([]string{"--to", "minecraft:air", "--no-backup", dir}); err == nil {
		t.Fatal("replace with neither --from nor --unresolved was accepted")
	}
}

// TestParseStateAndPropertyMatch pins the small parser --from and --to share,
// including that a bare identifier takes every state of a block while one
// written with properties takes only the state matching them.
func TestParseStateAndPropertyMatch(t *testing.T) {
	if n, p := parseState("minecraft:stone"); n != "minecraft:stone" || p != nil {
		t.Errorf("bare identifier parsed as %q %v", n, p)
	}
	n, p := parseState("cubecraft:portal_side[cc:facing=east,cc:extended=1]")
	if n != "cubecraft:portal_side" {
		t.Errorf("name is %q", n)
	}
	if len(p) != 2 || p["cc:facing"] != "east" || p["cc:extended"] != "1" {
		t.Errorf("properties parsed as %v", p)
	}
	// Values arrive as strings whatever the state stores, so the comparison
	// has to be by rendering rather than by type.
	if !sameProps(map[string]any{"cc:facing": "east", "cc:extended": uint8(1)}, p) {
		t.Error("a uint8 property did not match the string it was written as")
	}
	if sameProps(map[string]any{"cc:facing": "west", "cc:extended": uint8(1)}, p) {
		t.Error("a different property value matched")
	}
	if sameProps(map[string]any{"cc:facing": "east"}, p) {
		t.Error("a state with fewer properties matched")
	}

	all := []format.BlockState{
		{Name: "a", Properties: map[string]any{"f": "east"}},
		{Name: "a", Properties: map[string]any{"f": "west"}},
		{Name: "b", Properties: map[string]any{"f": "east"}},
	}
	if got := pickStates(all, nil, "a", false); len(got) != 2 {
		t.Errorf("a bare identifier picked %d states, want both of a", len(got))
	}
	if got := pickStates(all, nil, "a[f=east]", false); len(got) != 1 || got[0].Properties["f"] != "east" {
		t.Errorf("a qualified identifier picked %v", got)
	}
}
