package format

import (
	"bytes"
	"cmp"
	"encoding/hex"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
)

// The wire format is a compatibility surface: files written by one build must
// be readable by another, and a change to the bytes is a change to the format
// whether or not it was intended. These tests pin it.
//
// A deliberate format change means bumping Version and regenerating the
// golden file with `go test ./format -run TestGolden -update`. A test failure
// without that intent means the format moved by accident.
//
// The golden world is built from fixed inputs only (no registry-dependent
// state beyond the vanilla blocks it names), so the bytes depend on the
// format and the Minecraft block version, not on the machine.

var update = flag.Bool("update", false, "rewrite the golden format file")

const goldenPath = "testdata/golden_world.pile"

// goldenWorld builds the reference world: two columns exercising blocks,
// two storage layers, biomes, block entities, entities, scheduled ticks and
// metadata.
func goldenWorld(t *testing.T, reg world.BlockRegistry) *WorldData {
	t.Helper()
	settings, err := marshalNBT(map[string]any{
		"name": "golden", "time": int64(1234), "difficulty": int32(2),
	})
	if err != nil {
		t.Fatal(err)
	}
	d := &WorldData{Settings: settings, UserData: []byte("golden-user-data")}
	for _, pos := range [][2]int32{{0, 0}, {-1, 2}} {
		ch := chunk.New(reg, cube.Range{-64, 319})
		stone, _ := reg.StateToRuntimeID("minecraft:stone", map[string]any{})
		water, _ := reg.StateToRuntimeID("minecraft:water", map[string]any{"liquid_depth": int32(0)})
		for x := uint8(0); x < 16; x++ {
			for z := uint8(0); z < 16; z++ {
				for y := int16(-64); y < -60; y++ {
					ch.SetBlock(x, y, z, 0, stone)
				}
			}
		}
		ch.SetBlock(3, -60, 4, 0, stone)
		ch.SetBlock(3, -60, 4, 1, water)
		d.Columns = append(d.Columns, Column{
			X: pos[0], Z: pos[1],
			Col: &chunk.Column{
				Chunk: ch,
				// Several of each, so the golden bytes also pin the
				// canonical ordering the encoder imposes.
				BlockEntities: []chunk.BlockEntity{{
					Pos: cube.Pos{int(pos[0])*16 + 5, -59, int(pos[1])*16 + 6},
					// x/y/z are stripped on encode and reinjected on decode,
					// so the reference carries them to compare directly.
					Data: map[string]any{
						"id": "minecraft:furnace",
						"x":  int32(pos[0])*16 + 5, "y": int32(-59), "z": int32(pos[1])*16 + 6,
					},
				}, {
					Pos: cube.Pos{int(pos[0])*16 + 1, -60, int(pos[1])*16 + 2},
					Data: map[string]any{
						"id": "minecraft:chest", "CustomName": "golden",
						"x": int32(pos[0])*16 + 1, "y": int32(-60), "z": int32(pos[1])*16 + 2,
					},
				}},
				Entities: []chunk.Entity{{ID: 9, Data: map[string]any{
					"identifier": "minecraft:pig", "UniqueID": int64(9),
					"Pos": []any{float32(3.5), float32(-60), float32(4.5)},
				}}, {ID: 7, Data: map[string]any{
					"identifier": "minecraft:cow", "UniqueID": int64(7),
					"Pos": []any{float32(1.5), float32(-60), float32(2.5)},
				}}},
				ScheduledBlocks: []chunk.ScheduledBlockUpdate{{
					Pos: cube.Pos{int(pos[0])*16 + 2, -59, int(pos[1])*16 + 3}, Block: water, Tick: 120,
				}, {
					Pos: cube.Pos{int(pos[0]) * 16, -60, int(pos[1]) * 16}, Block: stone, Tick: 99,
				}},
				Tick: 4242,
			},
			UserData: []byte("chunk-user-data"),
		})
	}
	return d
}

// TestGoldenFormatStability fails when the encoder's output for fixed content
// changes, which is exactly when the wire format has changed.
func TestGoldenFormatStability(t *testing.T) {
	reg := testRegistry(t)
	var buf bytes.Buffer
	if err := WriteWorld(&buf, goldenWorld(t, reg), reg, Options{Compression: CompressionNone}); err != nil {
		t.Fatal(err)
	}
	got := buf.Bytes()

	if *update {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("golden file rewritten: %d bytes", len(got))
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("%v (regenerate with -update if the format changed deliberately)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("encoder output no longer matches the golden file: the wire format changed.\n"+
			"If deliberate, bump format.Version and re-run with -update.\ngot  %d bytes: %s...\nwant %d bytes: %s...",
			len(got), hex.EncodeToString(got[:min(48, len(got))]),
			len(want), hex.EncodeToString(want[:min(48, len(want))]))
	}
}

// TestGoldenFormatReadable checks that the current decoder still reads the
// stored file: the direction that matters for worlds already on disk.
func TestGoldenFormatReadable(t *testing.T) {
	reg := testRegistry(t)
	file, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Skipf("no golden file yet: %v", err)
	}
	d, err := ReadWorld(file, reg)
	if err != nil {
		t.Fatalf("the current decoder cannot read the golden file: %v", err)
	}
	if len(d.Columns) != 2 {
		t.Fatalf("golden columns = %d, want 2", len(d.Columns))
	}
	if string(d.UserData) != "golden-user-data" {
		t.Fatalf("golden user data = %q", d.UserData)
	}
	want := goldenWorld(t, reg)
	// Decode returns collections in the encoder's canonical order; sort the
	// reference the same way before comparing.
	for _, c := range want.Columns {
		slices.SortStableFunc(c.Col.BlockEntities, func(a, b chunk.BlockEntity) int {
			return comparePos(a.Pos, b.Pos)
		})
		slices.SortStableFunc(c.Col.Entities, func(a, b chunk.Entity) int {
			return cmp.Compare(a.ID, b.ID)
		})
		slices.SortStableFunc(c.Col.ScheduledBlocks, func(a, b chunk.ScheduledBlockUpdate) int {
			if v := comparePos(a.Pos, b.Pos); v != 0 {
				return v
			}
			return cmp.Compare(a.Tick, b.Tick)
		})
	}
	byPos := map[[2]int32]Column{}
	for _, c := range d.Columns {
		byPos[[2]int32{c.X, c.Z}] = c
	}
	for _, w := range want.Columns {
		g, ok := byPos[[2]int32{w.X, w.Z}]
		if !ok {
			t.Fatalf("golden file lost column (%d,%d)", w.X, w.Z)
		}
		compareColumns(t, w, g)
	}
}

// TestContentHashIndependentOfCompression: content identity must not depend
// on compressor version or settings, only on the world's content.
func TestContentHashIndependentOfCompression(t *testing.T) {
	reg := testRegistry(t)
	d := goldenWorld(t, reg)
	var hashes []uint64
	for _, opts := range []Options{
		{Compression: CompressionNone},
		{Compression: CompressionFast},
		{Compression: CompressionBest},
		{Compression: CompressionDefault, FastCompression: true},
	} {
		var buf bytes.Buffer
		if err := WriteWorld(&buf, d, reg, opts); err != nil {
			t.Fatal(err)
		}
		h, err := ContentHash(buf.Bytes(), reg)
		if err != nil {
			t.Fatal(err)
		}
		hashes = append(hashes, h)
	}
	for i, h := range hashes {
		if h != hashes[0] {
			t.Fatalf("content hash varied with compression settings: option %d gave %x, want %x", i, h, hashes[0])
		}
	}
}
