package format

import (
	"bytes"
	"cmp"
	"encoding/hex"
	"flag"
	"fmt"
	"math"
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

// requireUnfrozen is the freeze lockout. While format.Version is the frozen
// version, no fixture in the byte-locked suites may be regenerated at all —
// not with -update, and not with -update -format-change, which before the
// freeze was the escape hatch for a deliberate change. Incrementing
// format.Version is the only way to move a byte, which is the whole content of
// the freeze.
//
// It is called first thing in every test that writes a fixture, so a refusal
// leaves nothing behind: the goldens and vectors are rewritten as they are
// compared, and a guard that fired at the end would refuse a regeneration that
// had already happened on disk.
//
// TestSpecRulesPinned is deliberately *not* locked. Its -update re-pins the
// specification's normative sentences, and adding a MUST that states a rule the
// implementation already enforces moves no byte and stays permitted after the
// freeze. What is forbidden after the freeze is changing what a reader
// accepts, and that shows up as a moved fixture here.
func requireUnfrozen(t *testing.T) {
	t.Helper()
	if !*update || Version != FrozenVersion {
		return
	}
	t.Fatalf("refusing to regenerate: the pile v%d wire format is frozen "+
		"(format.FrozenVersion = %d), so no fixture may move and -format-change does not lift this.\n"+
		"To change the bytes: set format.Version = %d in format/format.go, which lifts this lock; "+
		"re-run with -update; then freeze the new version by setting format.FrozenVersion = %d. "+
		"Then update vecSpecVersion in vectorwalk_test.go and the title of format.md by hand: "+
		"both state the version deliberately rather than deriving it.",
		Version, FrozenVersion, Version+1, Version+1)
}

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

// TestManifestsAgreeWithVersion pins the obvious thing: the version the code
// writes is the version every byte-locked manifest was
// generated at. Each manifest is written by its own -update path, so a
// regeneration that ran at a different version — or a Version bump that
// regenerated only some of them — shows up here rather than in whichever suite
// is run next.
func TestManifestsAgreeWithVersion(t *testing.T) {
	if FrozenVersion > Version {
		t.Errorf("format.FrozenVersion is %d, which is ahead of format.Version %d: "+
			"a version that was never written cannot be frozen", FrozenVersion, Version)
	}
	gv, _ := readManifest(t)
	if gv != Version {
		t.Errorf("%s declares version %d, format.Version is %d", manifestPath, gv, Version)
	}
	for _, path := range []string{vectorManifest, negManifest} {
		if v, _ := readVectorManifest(t, path); v != Version {
			t.Errorf("%s declares version %d, format.Version is %d", path, v, Version)
		}
	}
	// The specification's title is the fourth place the version is written and
	// the one with no code path to catch it: a v3 implementation documented as
	// v2 misleads the only reader who cannot check it against anything.
	spec, err := os.ReadFile("format.md")
	if err != nil {
		t.Fatalf("read format.md: %v", err)
	}
	title, _, _ := strings.Cut(string(spec), "\n")
	if want := fmt.Sprintf("Version %d", Version); !strings.Contains(title, want) {
		t.Errorf("format.md is titled %q, which does not say %q", title, want)
	}
}

// TestWalkerVersionMatchesFormat couples the independent walker's version to
// format.Version without letting it derive one from the other.
//
// vectorwalk_test.go restates the specification's constants instead of
// importing them, which is the only reason a vector it accepts is evidence of
// anything: a reader built from the decoder cannot disagree with the decoder.
// The cost is a constant that a version bump has to reach by hand, and
// A version bump therefore has to reach it by hand, and a step written down
// somewhere is not enforcement — this project has been bitten three times by a
// rule that was recorded and pinned by nothing. So the coupling is checked here
// and the walker stays independent.
func TestWalkerVersionMatchesFormat(t *testing.T) {
	if vecSpecVersion != Version {
		t.Fatalf("the independent vector walker parses version %d and format.Version is %d.\n"+
			"Edit vecSpecVersion in format/vectorwalk_test.go by hand — it is deliberately not\n"+
			"derived from format.Version, because a walker that follows the decoder cannot\n"+
			"contradict it.",
			vecSpecVersion, Version)
	}
}

const goldenDir = "testdata"

// goldenVariants pins one file per distinct encoding path. Anything not
// listed here can change its bytes without a test noticing, so a new format
// option or mode belongs in this table on the day it lands.
var goldenVariants = []struct {
	name  string
	opts  Options
	world func(*testing.T, world.BlockRegistry) *WorldData // nil = goldenWorld
}{
	{name: "world_plain", opts: Options{Compression: CompressionNone}},
	{name: "world_zstd", opts: Options{Compression: CompressionBest}},
	{name: "world_zstd_fast", opts: Options{Compression: CompressionFast}},
	{name: "world_light", opts: Options{Compression: CompressionNone, StoreLight: true}, world: goldenLitWorld},
	{name: "world_stats", opts: Options{Compression: CompressionNone, Stats: true}},
	{name: "world_nobiomes", opts: Options{Compression: CompressionNone, SkipBiomes: true}},
	{name: "world_all", opts: Options{Compression: CompressionBest, StoreLight: true, Stats: true}, world: goldenLitWorld},
	// Preserved unresolved block and biome states travel as ordinary palette
	// entries, so their encoding needs its own lock.
	{name: "world_unknown", opts: Options{Compression: CompressionNone}, world: goldenUnknownWorld},
	// States carrying two or more properties, which nothing else here has.
	{name: "world_props", opts: Options{Compression: CompressionNone}, world: goldenPropsWorld},
	// Wide palettes, deep layer stacks, a full-height range and coordinates at
	// the int32 extremes: the encoding paths the small reference world is too
	// small to reach.
	{name: "world_dense", opts: Options{Compression: CompressionNone}, world: goldenDenseWorld},
	{name: "world_dense_zstd", opts: Options{Compression: CompressionBest}, world: goldenDenseWorld},
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
	// Ascending by name, as §7.2 requires.
	markers, err := marshalNBT(map[string]any{"markers": []map[string]any{
		{"name": "arena", "kind": "region", "pos": []any{0.0, 0.0, 0.0}, "radius": int32(12)},
		{"name": "spawn", "kind": "spawn", "pos": []any{1.5, 65.0, -2.5}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	border, err := marshalNBT(map[string]any{
		"min": [2]int32{-512, -512}, "max": [2]int32{512, 512},
	})
	if err != nil {
		t.Fatal(err)
	}
	d := &WorldData{
		Settings: settings, UserData: []byte("golden-user-data"),
		Markers: markers, Border: border,
	}
	for _, pos := range [][2]int32{{0, 0}, {-1, 2}} {
		ch := chunk.New(reg, cube.Range{-64, 319})
		stone, _ := reg.StateToRuntimeID("minecraft:stone", map[string]any{})
		water, _ := reg.StateToRuntimeID("minecraft:water", map[string]any{"liquid_depth": int32(0)})
		for x := range uint8(16) {
			for z := range uint8(16) {
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

// goldenLitWorld is the reference world with light baked in. StoreLight
// claims the records carry light, so a fixture that set the flag over a world
// with none was pinning a file the reader now refuses: the flag and the
// content have to agree.
func goldenLitWorld(t *testing.T, reg world.BlockRegistry) *WorldData {
	t.Helper()
	d := goldenWorld(t, reg)
	for _, c := range d.Columns {
		chunk.LightArea([]*chunk.Chunk{c.Col.Chunk}, int(c.X), int(c.Z)).Fill()
	}
	return d
}

// goldenUnknownWorld builds a world whose column carries preserved block
// states that no registry resolves, exercising the sidecar re-emission path.
func goldenUnknownWorld(t *testing.T, reg world.BlockRegistry) *WorldData {
	t.Helper()
	d := goldenWorld(t, reg)
	c := &d.Columns[0]
	placeholder := placeholderRid(reg)
	// Put the placeholder where the sidecar says an unresolved state lives.
	c.Col.Chunk.SetBlock(1, -64, 1, 0, placeholder)
	c.UnknownStates = []BlockState{
		{Name: "audit:missing", Properties: map[string]any{"variant": "a"}, Version: 17825806},
		{Name: "audit:other", Properties: map[string]any{"n": int32(3)}, Version: 18040335},
	}
	c.Unknown = []UnknownBlock{{
		Section: -4, Layer: 0,
		Index: uint16(1)<<8 | uint16(1)<<4, State: 0,
	}}
	c.UnknownTicks = []UnknownTick{{
		Pos: [3]int32{0, -60, 0}, At: 99, State: 1,
	}}
	return d
}

// goldenPropsWorld byte-locks §3.1's property block: a state's property keys
// ascend bytewise, and each key carries its own type byte.
//
// Every other golden world holds states with at most one property, so the sort
// in encodeProps moved no golden byte — reversing it left the whole golden
// suite green, and the rule was held only by TestWriterSortsStateProperties
// and the reader's own check. After the freeze the goldens are the arbiter, so
// a canonical rule they cannot see is a rule the freeze check does not protect.
// This fixture is that byte lock, and it is additive: it changes no existing
// golden.
//
// Two states, for two different reasons. props_a carries four keys of three
// property types, so the order is pinned across type boundaries rather than
// within one. props_b pins the comparison as **bytewise**, which is a real
// choice and not the only plausible one: its keys are "a", "aa" and "b", and a
// length-then-bytes order — the shape a fixed-width or length-prefixed
// comparison falls into — puts "b" before "aa" where a bytewise one does not.
// Keys of equal length cannot tell the two apart, so nothing else here would.
func goldenPropsWorld(t *testing.T, reg world.BlockRegistry) *WorldData {
	t.Helper()
	d := goldenWorld(t, reg)
	c := &d.Columns[0]
	placeholder := placeholderRid(reg)
	c.Col.Chunk.SetBlock(1, -64, 1, 0, placeholder)
	c.Col.Chunk.SetBlock(2, -64, 2, 0, placeholder)
	// Listed out of sorted order on purpose: the map hides it, and the point is
	// that the writer imposes the order rather than inheriting it.
	c.UnknownStates = []BlockState{
		{Name: "audit:props_a", Properties: map[string]any{
			"gamma": uint8(3), "alpha": int32(1), "delta": int32(4), "beta": "two",
		}, Version: 17825806},
		{Name: "audit:props_b", Properties: map[string]any{
			"b": "x", "a": int32(0), "aa": int32(1),
		}, Version: 18040335},
	}
	c.Unknown = []UnknownBlock{
		{Section: -4, Layer: 0, Index: uint16(1)<<8 | uint16(1)<<4, State: 0},
		{Section: -4, Layer: 0, Index: uint16(2)<<8 | uint16(2)<<4, State: 1},
	}
	return d
}

// denseRange is the widest vertical range the dense fixture uses: 128
// sections, well past the 24 a vanilla overworld has, so section bitsets span
// several words and the section loop is exercised past one byte.
var denseRange = cube.Range{-1024, 1023}

// denseStates is the number of distinct block states the dense fixture puts in
// a single section. Past 256 the encoder must widen palette indices from one
// byte to two, and that switch is otherwise untested by a golden.
const denseStates = 300

// goldenDenseWorld pins the encoding paths goldenWorld is too small to reach:
// a section palette wider than a byte, more than two storage layers, a
// full-height vertical range, chunk coordinates at both int32 extremes, and a
// biome palette whose entry counts tie (so the frequency sort has to fall back
// to its tiebreak to stay deterministic).
//
// The wide palette is built from synthetic preserved states rather than real
// runtime IDs on purpose: a registry-independent fixture does not churn every
// time dragonfly renumbers its block table, and the index-width path it
// exercises is the same either way.
func goldenDenseWorld(t *testing.T, reg world.BlockRegistry) *WorldData {
	t.Helper()
	settings, err := marshalNBT(map[string]any{"name": "golden-dense", "time": int64(-1)})
	if err != nil {
		t.Fatal(err)
	}
	d := &WorldData{Settings: settings, UserData: []byte("golden-user-data")}

	states := make([]BlockState, denseStates)
	for i := range states {
		states[i] = BlockState{
			Name:       fmt.Sprintf("audit:dense_%03d", i),
			Properties: map[string]any{"n": int32(i)},
			// Alternating versions, so per-entry version overrides are covered
			// at width too, not just in the two-entry unknown fixture.
			Version: 17825806 + int32(i%2),
		}
	}

	placeholder := placeholderRid(reg)
	stone, _ := reg.StateToRuntimeID("minecraft:stone", map[string]any{})
	water, _ := reg.StateToRuntimeID("minecraft:water", map[string]any{"liquid_depth": int32(0)})
	dirt, _ := reg.StateToRuntimeID("minecraft:dirt", map[string]any{})
	glass, _ := reg.StateToRuntimeID("minecraft:glass", map[string]any{})
	layers := [4]uint32{stone, water, dirt, glass}

	// Both int32 extremes, so the Morton key and the zigzag coordinate deltas
	// are pinned at the ends of their domain rather than near zero.
	for _, pos := range [][2]int32{{math.MaxInt32, math.MinInt32}, {math.MinInt32, math.MaxInt32}} {
		ch := chunk.New(reg, denseRange)
		unknown := make([]UnknownBlock, 0, denseStates)
		for i := range denseStates {
			// x and z alone wrap after 256 blocks, so y carries the overflow
			// and every state lands on a distinct cell of the same section.
			x, z := uint8(i%16), uint8((i/16)%16)
			y := int16(denseRange[0]) + int16(i/256)
			ch.SetBlock(x, y, z, 0, placeholder)
			unknown = append(unknown, UnknownBlock{
				Section: int32(denseRange[0] >> 4), Layer: 0,
				Index: uint16(x)<<8 | uint16(z)<<4 | uint16(y&15), State: uint32(i),
			})
		}
		// A four-deep layer stack. Layers are dense, so writing layer 3 forces
		// 0..2 into existence as well.
		for l, rid := range layers {
			ch.SetBlock(3, 0, 3, uint8(l), rid)
		}
		// Blocks at the very top and bottom of the range, so the section span
		// the encoder stores is the widest one it can.
		ch.SetBlock(15, int16(denseRange[1]), 15, 0, stone)

		// Two biomes covering exactly half the column each: the counts tie, and
		// only the tiebreak keeps the palette order stable.
		for x := range uint8(16) {
			for z := range uint8(16) {
				for y := int16(denseRange[0]); y <= int16(denseRange[1]); y++ {
					if y < 0 {
						ch.SetBiome(x, y, z, 1)
					} else {
						ch.SetBiome(x, y, z, 2)
					}
				}
			}
		}

		d.Columns = append(d.Columns, Column{
			X: pos[0], Z: pos[1],
			// A column tick at the far end of the domain, so the varint that
			// carries it is pinned at its widest.
			// No collections: this fixture is about palettes, layers, ranges
			// and coordinates, all of which goldenWorld is too small to reach.
			Col:           &chunk.Column{Chunk: ch, Tick: math.MaxInt64},
			UnknownStates: states,
			Unknown:       unknown,
		})
	}
	return d
}

// TestGoldenFormatStability fails when the encoder's output for fixed content
// changes, which is exactly when the wire format has changed.
func TestGoldenFormatStability(t *testing.T) {
	requireUnfrozen(t)
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
				"v%d is not the frozen version (format.FrozenVersion = %d), so re-run with -format-change "+
				"if the change is deliberate, or bump format.Version if v%d has been released.",
				changed, Version, Version, FrozenVersion, Version)
		}
		writeManifest(t, newHashes)
	})
	for _, v := range goldenVariants {
		t.Run(v.name, func(t *testing.T) {
			var buf bytes.Buffer
			build := v.world
			if build == nil {
				build = goldenWorld
			}
			if err := WriteWorld(&buf, build(t, reg), reg, v.opts); err != nil {
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

	t.Run("structure_zstd", func(t *testing.T) {
		var buf bytes.Buffer
		if err := WriteStructure(&buf, goldenStructure(t, reg), reg, Options{Compression: CompressionBest}); err != nil {
			t.Fatal(err)
		}
		newHashes["structure_zstd"] = xxhash.Sum64(buf.Bytes())
		compareGolden(t, filepath.Join(goldenDir, "golden_structure_zstd.pile"), buf.Bytes())
	})

	t.Run("structure_collections", func(t *testing.T) {
		var buf bytes.Buffer
		if err := WriteStructure(&buf, goldenStructureCollections(t, reg), reg, Options{Compression: CompressionNone}); err != nil {
			t.Fatal(err)
		}
		newHashes["structure_collections"] = xxhash.Sum64(buf.Bytes())
		compareGolden(t, filepath.Join(goldenDir, "golden_structure_collections.pile"), buf.Bytes())
	})

	t.Run("indexed", func(t *testing.T) {
		b := buildGoldenIndexed(t, reg)
		newHashes["indexed"] = xxhash.Sum64(b)
		compareGolden(t, filepath.Join(goldenDir, "golden_indexed.pile"), b)
	})

	// Single-threaded zstd is deterministic, so compressed data and directory
	// frames can be byte-locked as long as compaction (which trains a
	// dictionary) stays out of it.
	t.Run("indexed_zstd", func(t *testing.T) {
		b := buildGoldenIndexedZstd(t, reg)
		newHashes["indexed_zstd"] = xxhash.Sum64(b)
		compareGolden(t, filepath.Join(goldenDir, "golden_indexed_zstd.pile"), b)
	})

	t.Run("indexed_torn", func(t *testing.T) {
		b := buildGoldenIndexedTorn(t, reg)
		newHashes["indexed_torn"] = xxhash.Sum64(b)
		compareGolden(t, filepath.Join(goldenDir, "golden_indexed_torn.pile"), b)
	})

	// A compacted indexed file is deliberately not byte-locked: compaction
	// trains a shared dictionary, and the trainer is not reproducible, so
	// these files legitimately differ run to run. Indexed mode is documented
	// as history-dependent for exactly this reason, and ContentHash is the
	// identity mechanism there. The fixture is still regenerated under
	// -update and its structure is locked by TestGoldenFormatReadable.
	t.Run("indexed_compact", func(t *testing.T) {
		if !*update {
			t.Skip("compacted indexed files are not byte-reproducible; structure is checked in TestGoldenFormatReadable")
		}
		b := buildGoldenIndexedCompact(t, reg)
		if err := os.WriteFile(filepath.Join(goldenDir, "golden_indexed_compact.pile"), b, 0o644); err != nil {
			t.Fatal(err)
		}
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
// buildGoldenIndexedCompact pins the paths the uncompressed fixture cannot
// reach: compressed frames, dictionary training and compaction.
func buildGoldenIndexedCompact(t *testing.T, reg world.BlockRegistry) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "compact.pile")
	w, err := CreateIndexed(path, reg, Options{Compression: CompressionDefault})
	if err != nil {
		t.Fatal(err)
	}
	d := goldenWorld(t, reg)
	// Enough varied records to make compaction meaningful and to give the
	// dictionary trainer material it can actually work with: identical
	// samples make training decline (or, upstream, panic).
	stone, _ := reg.StateToRuntimeID("minecraft:stone", map[string]any{})
	dirt, _ := reg.StateToRuntimeID("minecraft:dirt", map[string]any{})
	for i := range int32(24) {
		c := d.Columns[int(i)%len(d.Columns)]
		c.X, c.Z = i, i%3
		// Re-anchor the block entities on the column's new position. A
		// block entity's X and Z are folded into one packed nibble pair, so
		// one carried over from the column this was cloned from is outside
		// the span the record declares -- a file the writer refuses and, before
		// it did, one it wrote and could not read back.
		c.Col.BlockEntities = reanchorBEs(c.Col.BlockEntities, i, i%3)
		c.Col.ScheduledBlocks = reanchorTicks(c.Col.ScheduledBlocks, i, i%3)
		ch := c.Col.Chunk.Clone()
		for y := int16(-64); y < -64+int16(i%12); y++ {
			ch.SetBlock(uint8(i%16), y, uint8((i*7)%16), 0, dirt)
		}
		ch.SetBlock(uint8((i*3)%16), -58, uint8(i%16), 0, stone)
		c.Col = &chunk.Column{
			Chunk: ch, BlockEntities: c.Col.BlockEntities,
			Entities: c.Col.Entities, ScheduledBlocks: c.Col.ScheduledBlocks, Tick: int64(i),
		}
		if err := w.Store(c); err != nil {
			t.Fatal(err)
		}
	}
	for i := range int32(8) {
		c := d.Columns[0]
		c.X, c.Z = i, i%3
		c.Col.BlockEntities = reanchorBEs(c.Col.BlockEntities, i, i%3)
		c.Col.ScheduledBlocks = reanchorTicks(c.Col.ScheduledBlocks, i, i%3)
		if err := w.Store(c); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.SetMeta(d.Settings, d.UserData, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := w.Compact(); err != nil {
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

func buildGoldenIndexed(t *testing.T, reg world.BlockRegistry) []byte {
	return buildGoldenIndexedWith(t, reg, Options{Compression: CompressionNone})
}

// buildGoldenIndexedZstd pins compressed data and directory frames, which the
// uncompressed fixture never produces and the compacted one cannot lock.
func buildGoldenIndexedZstd(t *testing.T, reg world.BlockRegistry) []byte {
	return buildGoldenIndexedWith(t, reg, Options{Compression: CompressionBest})
}

func buildGoldenIndexedWith(t *testing.T, reg world.BlockRegistry, opts Options) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "indexed.pile")
	w, err := CreateIndexed(path, reg, opts)
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
	if err := w.SetMeta(d.Settings, d.UserData, d.Markers, d.Border); err != nil {
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

// buildGoldenIndexedTorn produces an indexed file whose newest checkpoint is
// destroyed, so opening it has to walk the previous-footer chain back to an
// intact generation. Recovery is the whole reason the chain exists, and
// nothing else in the fixture set exercises it.
func buildGoldenIndexedTorn(t *testing.T, reg world.BlockRegistry) []byte {
	t.Helper()
	b := bytes.Clone(buildGoldenIndexed(t, reg))
	// A torn write leaves a plausible-looking tail behind. Corrupting the
	// final footer models the crash that pile is meant to survive: the
	// checkpoint hash no longer matches, so the newest generation is
	// unusable and the previous one must take over.
	tail := b[len(b)-footerSize:]
	for i := range tail {
		tail[i] ^= 0xff
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
	// A non-zero origin, so the svarint paste anchor is byte-locked too.
	data.Origin = [3]int32{-3, 1, 7}
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

// goldenStructureCollections byte-locks §6's collection ordering: a structure's
// block entities ascend by y, then z, then x, and its entities ascend by their
// encoded NBT.
//
// goldenStructure carries exactly one of each, so neither sort had anything to
// order and reversing either moved no golden byte. Both rules were held only by
// the structure_full conformance vector. That is real coverage and it is not
// the coverage the freeze relies on: the post-freeze check is "run the golden
// suite, and if it passes the change is permitted", so a canonical-form rule no
// golden can see is one that check does not protect.
//
// The block entities are supplied in exactly the reverse of their canonical
// order, so the fixture fails if the writer stops imposing one rather than
// merely if it imposes a different one. The entity order is decided by a
// bytewise compare of encoded NBT, which is not predictable by eye; three
// distinct entities are enough, since reversing any three distinct elements
// changes their order.
func goldenStructureCollections(t *testing.T, reg world.BlockRegistry) *StructureData {
	t.Helper()
	data := goldenStructure(t, reg)
	data.UserData = []byte("golden-structure-collections")
	// Pos is [x, y, z] and the order is (y, z, x), so each of the three
	// comparison rungs decides exactly one adjacent pair here: y separates the
	// first from the second, z the second from the third, x the third from the
	// fourth. A sort that dropped any one rung would still reorder the rest.
	data.BlockEntities = []StructureBlockEntity{
		{Pos: [3]int32{4, 2, 5}, Data: map[string]any{"id": "minecraft:hopper"}},
		{Pos: [3]int32{1, 2, 5}, Data: map[string]any{"id": "minecraft:barrel"}},
		{Pos: [3]int32{1, 2, 3}, Data: map[string]any{"id": "minecraft:chest", "CustomName": "golden"}},
		{Pos: [3]int32{1, 0, 3}, Data: map[string]any{"id": "minecraft:furnace"}},
	}
	data.Entities = []map[string]any{
		{"identifier": "minecraft:zombie", "UniqueID": int64(13),
			"Pos": []any{float32(9.5), float32(1), float32(2.5)}},
		{"identifier": "minecraft:pig", "UniqueID": int64(12),
			"Pos": []any{float32(5.5), float32(0), float32(6.5)}},
		{"identifier": "minecraft:cow", "UniqueID": int64(11),
			"Pos": []any{float32(1.5), float32(2), float32(3.5)}},
	}
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
			build := v.world
			if build == nil {
				build = goldenWorld
			}
			want := build(t, reg)
			if !bytes.Equal(d.Markers, want.Markers) || !bytes.Equal(d.Border, want.Border) {
				t.Fatalf("golden metadata changed: markers %d bytes (want %d), border %d bytes (want %d)",
					len(d.Markers), len(want.Markers), len(d.Border), len(want.Border))
			}
			if v.opts.SkipBiomes {
				return // biome comparison would fail by design
			}
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

	// The dense fixture only earns its place if it still reaches the paths it
	// was built for: a fixture that silently degrades to an ordinary world
	// would keep passing while covering nothing.
	t.Run("dense_still_dense", func(t *testing.T) {
		file, err := os.ReadFile(filepath.Join(goldenDir, "golden_world_dense.pile"))
		if err != nil {
			t.Skipf("no dense golden yet: %v", err)
		}
		d, err := ReadWorld(file, reg)
		if err != nil {
			t.Fatal(err)
		}
		c := d.Columns[0]
		if got := c.Col.Chunk.Range(); got != denseRange {
			t.Fatalf("dense range = %v, want %v", got, denseRange)
		}
		if len(c.UnknownStates) != denseStates {
			t.Fatalf("dense preserved states = %d, want %d", len(c.UnknownStates), denseStates)
		}
		wide, deep := 0, 0
		for _, sub := range c.Col.Chunk.Sub() {
			for _, st := range sub.Layers() {
				if st.Palette().Len() > 256 {
					wide++
				}
			}
			if len(sub.Layers()) > 2 {
				deep++
			}
		}
		if wide == 0 {
			t.Fatal("dense golden no longer has a section palette past 256 entries: the two-byte index path is uncovered")
		}
		if deep == 0 {
			t.Fatal("dense golden no longer has a section past two layers")
		}
	})

	// Same reasoning for the property fixture: it is the only golden that puts
	// two properties on one state, so if it ever degrades to one the sort in
	// encodeProps goes back to moving no golden byte and nobody finds out.
	t.Run("props_still_multi", func(t *testing.T) {
		file, err := os.ReadFile(filepath.Join(goldenDir, "golden_world_props.pile"))
		if err != nil {
			t.Skipf("no property golden yet: %v", err)
		}
		d, err := ReadWorld(file, reg)
		if err != nil {
			t.Fatal(err)
		}
		multi, widest := 0, 0
		for _, c := range d.Columns {
			for _, st := range c.UnknownStates {
				if len(st.Properties) >= 2 {
					multi++
				}
				widest = max(widest, len(st.Properties))
			}
		}
		if multi < 2 || widest < 4 {
			t.Fatalf("property golden has %d states with two or more properties (widest %d): "+
				"the canonical order of property keys is no longer byte-locked by any golden",
				multi, widest)
		}
	})

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
		if s.Origin != [3]int32{-3, 1, 7} {
			t.Fatalf("golden structure origin = %v", s.Origin)
		}
		if len(s.BlockEntities) != 1 || len(s.Entities) != 1 {
			t.Fatalf("golden structure contents: %d block entities, %d entities",
				len(s.BlockEntities), len(s.Entities))
		}
	})

	// The collection fixture earns its place only while it still holds enough
	// of each to have an order at all: at one element per collection it goes
	// back to locking nothing, which is the state it was added to fix.
	t.Run("structure_collections", func(t *testing.T) {
		file, err := os.ReadFile(filepath.Join(goldenDir, "golden_structure_collections.pile"))
		if err != nil {
			t.Skipf("no collection structure golden yet: %v", err)
		}
		s, err := ReadStructure(file, reg)
		if err != nil {
			t.Fatalf("the current decoder cannot read the collection structure: %v", err)
		}
		if len(s.BlockEntities) < 4 || len(s.Entities) < 3 {
			t.Fatalf("collection structure holds %d block entities and %d entities: "+
				"§6's collection order is no longer byte-locked by any golden",
				len(s.BlockEntities), len(s.Entities))
		}
		// The order the file was written in, read back: y, then z, then x.
		for i := 1; i < len(s.BlockEntities); i++ {
			a, b := s.BlockEntities[i-1].Pos, s.BlockEntities[i].Pos
			if !(a[1] < b[1] ||
				(a[1] == b[1] && a[2] < b[2]) ||
				(a[1] == b[1] && a[2] == b[2] && a[0] < b[0])) {
				t.Fatalf("collection structure block entities are not in (y,z,x) order: %v then %v", a, b)
			}
		}
	})

	t.Run("indexed_compact", func(t *testing.T) {
		src := filepath.Join(goldenDir, "golden_indexed_compact.pile")
		file, err := os.ReadFile(src)
		if err != nil {
			t.Skipf("no compacted golden yet: %v", err)
		}
		path := filepath.Join(t.TempDir(), "compact.pile")
		if err := os.WriteFile(path, file, 0o644); err != nil {
			t.Fatal(err)
		}
		w, err := OpenIndexed(path, reg, true)
		if err != nil {
			t.Fatalf("the current decoder cannot read the compacted golden: %v", err)
		}
		defer w.Close()
		if w.Recovered() {
			t.Fatal("compacted golden reports recovery")
		}
		if !w.HasDict() {
			t.Fatal("compacted golden lost its shared dictionary")
		}
		if w.ChunkCount() != 24 {
			t.Fatalf("compacted golden holds %d chunks, want 24", w.ChunkCount())
		}
		for _, k := range w.Positions() {
			if _, err := w.Column(k[0], k[1]); err != nil {
				t.Fatalf("compacted golden column (%d,%d): %v", k[0], k[1], err)
			}
		}
	})

	t.Run("structure_zstd", func(t *testing.T) {
		file, err := os.ReadFile(filepath.Join(goldenDir, "golden_structure_zstd.pile"))
		if err != nil {
			t.Skipf("no compressed structure golden yet: %v", err)
		}
		if _, err := ReadStructure(file, reg); err != nil {
			t.Fatalf("the current decoder cannot read the compressed structure golden: %v", err)
		}
	})

	for _, name := range []string{"indexed", "indexed_zstd"} {
		t.Run(name, func(t *testing.T) {
			file, err := os.ReadFile(filepath.Join(goldenDir, "golden_"+name+".pile"))
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
			_, _, markers, border := w.Meta()
			if !bytes.Equal(markers, want.Markers) || !bytes.Equal(border, want.Border) {
				t.Fatal("golden indexed world lost its metadata")
			}
			for _, c := range want.Columns {
				got, err := w.Column(c.X, c.Z)
				if err != nil {
					t.Fatalf("golden indexed column (%d,%d): %v", c.X, c.Z, err)
				}
				compareColumns(t, c, got)
			}
		})
	}

	// The torn fixture is the only one that must NOT open cleanly: its newest
	// checkpoint is destroyed, so a correct reader falls back to the previous
	// generation instead of refusing the file or serving damaged data.
	t.Run("indexed_torn", func(t *testing.T) {
		file, err := os.ReadFile(filepath.Join(goldenDir, "golden_indexed_torn.pile"))
		if err != nil {
			t.Skipf("no torn golden yet: %v", err)
		}
		path := filepath.Join(t.TempDir(), "torn.pile")
		if err := os.WriteFile(path, file, 0o644); err != nil {
			t.Fatal(err)
		}
		w, err := OpenIndexed(path, reg, true)
		if err != nil {
			t.Fatalf("a torn indexed world must recover, not fail: %v", err)
		}
		defer w.Close()
		if !w.Recovered() {
			t.Fatal("torn golden opened without reporting recovery: the damaged checkpoint was accepted")
		}
		if w.ChunkCount() != 2 {
			t.Fatalf("recovered chunks = %d, want 2", w.ChunkCount())
		}
		want := goldenWorld(t, reg)
		canonicaliseColumns(want)
		for _, c := range want.Columns {
			got, err := w.Column(c.X, c.Z)
			if err != nil {
				t.Fatalf("recovered column (%d,%d): %v", c.X, c.Z, err)
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

// TestUncoveredOptionPaths locks the two option paths that cannot be golden
// files. Worker count must not move a byte (determinism is a format
// guarantee), and multi-threaded zstd is allowed to move bytes but must not
// move content.
func TestUncoveredOptionPaths(t *testing.T) {
	reg := testRegistry(t)
	write := func(o Options) []byte {
		var buf bytes.Buffer
		if err := WriteWorld(&buf, goldenWorld(t, reg), reg, o); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}

	// Worker count is a scheduling detail, never a content one.
	base := write(Options{Compression: CompressionBest})
	if serial := write(Options{Compression: CompressionBest, Workers: 1}); !bytes.Equal(base, serial) {
		t.Fatalf("Workers=1 produced %d bytes, parallel produced %d: worker count must not affect output",
			len(serial), len(base))
	}

	// Multi-threaded zstd frames legitimately differ, so only the decoded
	// content is pinned.
	fast := write(Options{Compression: CompressionBest, FastCompression: true})
	wantHash, err := ContentHash(base, reg)
	if err != nil {
		t.Fatal(err)
	}
	gotHash, err := ContentHash(fast, reg)
	if err != nil {
		t.Fatal(err)
	}
	if gotHash != wantHash {
		t.Fatalf("FastCompression changed content hash: %016x, want %016x", gotHash, wantHash)
	}
	d, err := ReadWorld(fast, reg)
	if err != nil {
		t.Fatalf("FastCompression output does not decode: %v", err)
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
			t.Fatalf("FastCompression output lost column (%d,%d)", w.X, w.Z)
		}
		compareColumns(t, w, g)
	}
}

// reanchorBEs moves block entities onto the column at (cx, cz), keeping their
// position within the chunk. Cloning a column to a new key and leaving its
// block entities behind produces a record whose declared span does not contain
// them, which the writer refuses.
func reanchorBEs(bes []chunk.BlockEntity, cx, cz int32) []chunk.BlockEntity {
	if len(bes) == 0 {
		return bes
	}
	out := make([]chunk.BlockEntity, len(bes))
	for i, be := range bes {
		p := be.Pos
		out[i] = chunk.BlockEntity{
			Pos:  cube.Pos{int(cx)*16 + (p.X() & 15), p.Y(), int(cz)*16 + (p.Z() & 15)},
			Data: be.Data,
		}
	}
	return out
}

// reanchorTicks does for scheduled updates what reanchorBEs does for block
// entities: their positions are packed the same way and refused the same way.
func reanchorTicks(ts []chunk.ScheduledBlockUpdate, cx, cz int32) []chunk.ScheduledBlockUpdate {
	if len(ts) == 0 {
		return ts
	}
	out := make([]chunk.ScheduledBlockUpdate, len(ts))
	for i, t := range ts {
		p := t.Pos
		out[i] = t
		out[i].Pos = cube.Pos{int(cx)*16 + (p.X() & 15), p.Y(), int(cz)*16 + (p.Z() & 15)}
	}
	return out
}
