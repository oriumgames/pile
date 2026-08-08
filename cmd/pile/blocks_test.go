package main

import (
	"strings"
	"testing"

	"github.com/df-mc/dragonfly/server/block"
	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
	"github.com/df-mc/dragonfly/server/world/mcdb"
)

// TestScanPalettesReadsARealWorld drives the palette walker against bytes
// dragonfly wrote, rather than against a fixture built by the same
// understanding of the layout that the walker has. A hand-built sub-chunk would
// pass a walker that had the format wrong in the same way.
func TestScanPalettesReadsARealWorld(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	db, err := mcdb.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	stone := reg.BlockRuntimeID(block.Stone{})
	dirt := reg.BlockRuntimeID(block.Dirt{})
	ch := chunk.New(reg, cube.Range{-64, 319})
	for x := range uint8(16) {
		for z := range uint8(16) {
			ch.SetBlock(x, 0, z, 0, stone)
			ch.SetBlock(x, 1, z, 0, dirt)
		}
	}
	if err := db.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, &chunk.Column{Chunk: ch}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	states, err := scanPalettes(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"minecraft:stone", "minecraft:dirt", "minecraft:air"} {
		if _, ok := states[want]; !ok {
			t.Errorf("%s is in the world and not in the scan; got %d identifiers", want, len(states))
		}
	}
	// Every identifier found must be vanilla: nothing here registered anything
	// else, so a non-minecraft: name would mean the walker is reading garbage
	// and calling it a block.
	for name := range states {
		if !strings.HasPrefix(name, "minecraft:") {
			t.Errorf("scan reported %q, which nothing in this world uses", name)
		}
	}
}

// TestScanPalettesNeedsNoRegistry is the property the command exists for: it
// reads worlds whose blocks the running binary cannot resolve, which is exactly
// when you need to know what they are.
//
// It is asserted structurally rather than by fixture, because building a world
// with unregisterable blocks would mean registering them.
func TestScanPalettesNeedsNoRegistry(t *testing.T) {
	dir := t.TempDir()
	db, err := mcdb.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// An empty world has no palettes at all, and that is an error rather than
	// an empty answer: silence would look the same as a world of nothing but
	// vanilla blocks.
	if _, err := scanPalettes(dir); err == nil {
		t.Error("scanning a world with no chunks returned no error")
	}
}

func TestSchemaOf(t *testing.T) {
	for _, c := range []struct {
		name   string
		states []map[string]any
		want   string
	}{
		{"no properties", []map[string]any{{}}, ""},
		{"one value", []map[string]any{{"custom:facing_direction": int32(0)}}, "custom:facing_direction=0"},
		{
			"several values, deduplicated and ordered",
			[]map[string]any{
				{"custom:facing_direction": int32(2)},
				{"custom:facing_direction": int32(0)},
				{"custom:facing_direction": int32(2)},
			},
			"custom:facing_direction=0,2",
		},
		{
			"two properties",
			[]map[string]any{
				{"a": int32(1), "b": "x"},
				{"a": int32(2), "b": "x"},
			},
			"a=1,2  b=x",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := schemaOf(c.states); got != c.want {
				t.Errorf("schemaOf = %q, want %q", got, c.want)
			}
		})
	}
}
