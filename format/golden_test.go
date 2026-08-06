package format

import (
	"bytes"
	"cmp"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/cespare/xxhash/v2"

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

var (
	update = flag.Bool("update", false, "rewrite the golden format files")
	// formatChange acknowledges that rewriting the goldens changes the wire
	// format on purpose. Without it, -update refuses to bless changed bytes
	// while format.Version stays the same, so a format change cannot be
	// committed by accident.
	formatChange = flag.Bool("format-change", false, "confirm an intentional wire format change when updating goldens")
)

const manifestPath = goldenDir + "/golden_manifest.txt"

// goldenManifest records the format version the fixtures were generated at
// and a hash per fixture, so -update can tell a regeneration from a change.
func readManifest(t *testing.T) (version int, hashes map[string]uint64) {
	t.Helper()
	hashes = map[string]uint64{}
	b, err := os.ReadFile(manifestPath)
	if err != nil {
		return 0, hashes
	}
	for line := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		if f[0] == "version" {
			version, _ = strconv.Atoi(f[1])
			continue
		}
		h, _ := strconv.ParseUint(f[1], 16, 64)
		hashes[f[0]] = h
	}
	return version, hashes
}

func writeManifest(t *testing.T, hashes map[string]uint64) {
	t.Helper()
	names := make([]string, 0, len(hashes))
	for n := range hashes {
		names = append(names, n)
	}
	sort.Strings(names)
	var sb strings.Builder
	fmt.Fprintf(&sb, "version %d\n", Version)
	for _, n := range names {
		fmt.Fprintf(&sb, "%s %016x\n", n, hashes[n])
	}
	if err := os.WriteFile(manifestPath, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

const goldenDir = "testdata"

// goldenVariants pins one file per distinct encoding path. Anything not
// listed here can change its bytes without a test noticing, so a new format
// option or mode belongs in this table on the day it lands.
var goldenVariants = []struct {
	name string
	opts Options
}{
	{"world_plain", Options{Compression: CompressionNone}},
	{"world_zstd", Options{Compression: CompressionBest}},
	{"world_zstd_fast", Options{Compression: CompressionFast}},
	{"world_light", Options{Compression: CompressionNone, StoreLight: true}},
	{"world_stats", Options{Compression: CompressionNone, Stats: true}},
	{"world_nobiomes", Options{Compression: CompressionNone, SkipBiomes: true}},
	{"world_all", Options{Compression: CompressionBest, StoreLight: true, Stats: true}},
}

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
	prevVersion, prevHashes := readManifest(t)
	newHashes := map[string]uint64{}
	t.Cleanup(func() {
		if !*update {
			return
		}
		var changed []string
		for name, h := range newHashes {
			if old, ok := prevHashes[name]; ok && old != h {
				changed = append(changed, name)
			}
		}
		sort.Strings(changed)
		if len(changed) > 0 && prevVersion == Version && !*formatChange {
			t.Fatalf("refusing to bless a wire format change: %v changed while format.Version is still %d.\n"+
				"Bump format.Version for a released format, or re-run with -format-change if v%d is not frozen yet.",
				changed, Version, Version)
		}
		writeManifest(t, newHashes)
	})
	for _, v := range goldenVariants {
		t.Run(v.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteWorld(&buf, goldenWorld(t, reg), reg, v.opts); err != nil {
				t.Fatal(err)
			}
			newHashes[v.name] = xxhash.Sum64(buf.Bytes())
			compareGolden(t, filepath.Join(goldenDir, "golden_"+v.name+".pile"), buf.Bytes())
		})
	}

	t.Run("structure", func(t *testing.T) {
		var buf bytes.Buffer
		if err := WriteStructure(&buf, goldenStructure(t, reg), reg, Options{Compression: CompressionNone}); err != nil {
			t.Fatal(err)
		}
		newHashes["structure"] = xxhash.Sum64(buf.Bytes())
		compareGolden(t, filepath.Join(goldenDir, "golden_structure.pile"), buf.Bytes())
	})

	t.Run("indexed", func(t *testing.T) {
		b := buildGoldenIndexed(t, reg)
		newHashes["indexed"] = xxhash.Sum64(b)
		compareGolden(t, filepath.Join(goldenDir, "golden_indexed.pile"), b)
	})
}

// compareGolden checks bytes against a stored file, or rewrites it under
// -update.
func compareGolden(t *testing.T, path string, got []byte) {
	t.Helper()
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("%s rewritten: %d bytes", path, len(got))
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (regenerate with -update if the format changed deliberately)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("encoder output no longer matches %s: the wire format changed.\n"+
			"If deliberate, bump format.Version and re-run with -update.\ngot  %d bytes: %s...\nwant %d bytes: %s...",
			path, len(got), hex.EncodeToString(got[:min(48, len(got))]),
			len(want), hex.EncodeToString(want[:min(48, len(want))]))
	}
}

// buildGoldenIndexed writes a fixed indexed world (records, palette segments,
// metadata, directory, footer chain) and returns its bytes. Compaction runs so
// the dictionary path is exercised too.
func buildGoldenIndexed(t *testing.T, reg world.BlockRegistry) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "indexed.pile")
	w, err := CreateIndexed(path, reg, Options{Compression: CompressionNone})
	if err != nil {
		t.Fatal(err)
	}
	d := goldenWorld(t, reg)
	for _, c := range d.Columns {
		if err := w.Store(c); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	// A second generation, so the footer chain and directory deltas are
	// covered as well.
	if err := w.Store(d.Columns[0]); err != nil {
		t.Fatal(err)
	}
	if err := w.SetMeta(d.Settings, d.UserData, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// goldenStructure is the reference structure: two cells, two layers, a block
// entity and an entity.
func goldenStructure(t *testing.T, reg world.BlockRegistry) *StructureData {
	t.Helper()
	data, err := NewStructureData([3]int32{20, 5, 18})
	if err != nil {
		t.Fatal(err)
	}
	stone, _ := reg.StateToRuntimeID("minecraft:stone", map[string]any{})
	water, _ := reg.StateToRuntimeID("minecraft:water", map[string]any{"liquid_depth": int32(0)})
	air := reg.AirRuntimeID()
	for _, cell := range []struct{ cx, cy, cz int32 }{{0, 0, 0}, {1, 0, 1}} {
		sub := chunk.NewSubChunk(air)
		sub.SetBlock(1, 2, 3, 0, stone)
		sub.SetBlock(1, 2, 3, 1, water)
		sub.SetBlock(4, 0, 5, 0, stone)
		data.Cells[CellIndex(data.Size, cell.cx, cell.cy, cell.cz)] = sub
	}
	data.UserData = []byte("golden-structure")
	data.BlockEntities = append(data.BlockEntities, StructureBlockEntity{
		Pos:  [3]int32{1, 2, 3},
		Data: map[string]any{"id": "minecraft:chest", "CustomName": "golden"},
	})
	data.Entities = append(data.Entities, map[string]any{
		"identifier": "minecraft:cow", "UniqueID": int64(11),
		"Pos": []any{float32(1.5), float32(2), float32(3.5)},
	})
	return data
}

// TestGoldenFormatReadable checks that the current decoder still reads the
// stored files: the direction that matters for worlds already on disk.
func TestGoldenFormatReadable(t *testing.T) {
	reg := testRegistry(t)
	for _, v := range goldenVariants {
		t.Run(v.name, func(t *testing.T) {
			path := filepath.Join(goldenDir, "golden_"+v.name+".pile")
			file, err := os.ReadFile(path)
			if err != nil {
				t.Skipf("no golden file yet: %v", err)
			}
			d, err := ReadWorld(file, reg)
			if err != nil {
				t.Fatalf("the current decoder cannot read %s: %v", path, err)
			}
			if len(d.Columns) != 2 {
				t.Fatalf("golden columns = %d, want 2", len(d.Columns))
			}
			if string(d.UserData) != "golden-user-data" {
				t.Fatalf("golden user data = %q", d.UserData)
			}
			if v.opts.SkipBiomes {
				return // biome comparison would fail by design
			}
			want := goldenWorld(t, reg)
			canonicaliseColumns(want)
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
		})
	}

	t.Run("structure", func(t *testing.T) {
		file, err := os.ReadFile(filepath.Join(goldenDir, "golden_structure.pile"))
		if err != nil {
			t.Skipf("no golden structure yet: %v", err)
		}
		s, err := ReadStructure(file, reg)
		if err != nil {
			t.Fatalf("the current decoder cannot read the golden structure: %v", err)
		}
		if s.Size != [3]int32{20, 5, 18} || string(s.UserData) != "golden-structure" {
			t.Fatalf("golden structure changed: %v %q", s.Size, s.UserData)
		}
		if len(s.BlockEntities) != 1 || len(s.Entities) != 1 {
			t.Fatalf("golden structure contents: %d block entities, %d entities",
				len(s.BlockEntities), len(s.Entities))
		}
	})

	t.Run("indexed", func(t *testing.T) {
		src := filepath.Join(goldenDir, "golden_indexed.pile")
		file, err := os.ReadFile(src)
		if err != nil {
			t.Skipf("no golden indexed world yet: %v", err)
		}
		path := filepath.Join(t.TempDir(), "indexed.pile")
		if err := os.WriteFile(path, file, 0o644); err != nil {
			t.Fatal(err)
		}
		w, err := OpenIndexed(path, reg, true)
		if err != nil {
			t.Fatalf("the current decoder cannot read the golden indexed world: %v", err)
		}
		defer w.Close()
		if w.Recovered() {
			t.Fatal("golden indexed world reports recovery: its newest checkpoint is not intact")
		}
		if w.ChunkCount() != 2 {
			t.Fatalf("golden indexed chunks = %d, want 2", w.ChunkCount())
		}
		want := goldenWorld(t, reg)
		canonicaliseColumns(want)
		for _, c := range want.Columns {
			got, err := w.Column(c.X, c.Z)
			if err != nil {
				t.Fatalf("golden indexed column (%d,%d): %v", c.X, c.Z, err)
			}
			compareColumns(t, c, got)
		}
	})
}

// canonicaliseColumns applies the encoder's collection ordering to reference
// content so it can be compared with decoded output.
func canonicaliseColumns(d *WorldData) {
	for _, c := range d.Columns {
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
}
