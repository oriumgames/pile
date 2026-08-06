package format

import (
	"bytes"
	"testing"

	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
)

// shuffledRegistry is the default registry with its runtime ID space reversed.
// Nothing about the blocks changes, only the numbers they are addressed by.
//
// A world written under one registry and read under another must come back
// identical: the format stores block states by name and properties, so runtime
// IDs are a process-local detail that must never reach the wire. A registry
// whose IDs are deliberately wrong is the only way to prove that, since the
// default registry would make an ID-based encoder look correct.
type shuffledRegistry struct {
	world.BlockRegistry
	n uint32
}

func newShuffledRegistry(base world.BlockRegistry) *shuffledRegistry {
	return &shuffledRegistry{BlockRegistry: base, n: uint32(base.BlockCount())}
}

// flip maps between the two ID spaces. It is its own inverse.
func (r *shuffledRegistry) flip(rid uint32) uint32 {
	if rid >= r.n {
		return rid
	}
	return r.n - 1 - rid
}

func (r *shuffledRegistry) AirRuntimeID() uint32 {
	return r.flip(r.BlockRegistry.AirRuntimeID())
}

func (r *shuffledRegistry) RuntimeIDToState(rid uint32) (string, map[string]any, bool) {
	return r.BlockRegistry.RuntimeIDToState(r.flip(rid))
}

func (r *shuffledRegistry) StateToRuntimeID(name string, props map[string]any) (uint32, bool) {
	rid, ok := r.BlockRegistry.StateToRuntimeID(name, props)
	if !ok {
		return 0, false
	}
	return r.flip(rid), true
}

func (r *shuffledRegistry) BlockRuntimeID(b world.Block) uint32 {
	return r.flip(r.BlockRegistry.BlockRuntimeID(b))
}

func (r *shuffledRegistry) BlockByRuntimeID(rid uint32) (world.Block, bool) {
	return r.BlockRegistry.BlockByRuntimeID(r.flip(rid))
}

func (r *shuffledRegistry) BlockByRuntimeIDOrAir(rid uint32) world.Block {
	return r.BlockRegistry.BlockByRuntimeIDOrAir(r.flip(rid))
}

func (r *shuffledRegistry) FilteringBlock(rid uint32) uint8 {
	return r.BlockRegistry.FilteringBlock(r.flip(rid))
}

func (r *shuffledRegistry) LightBlock(rid uint32) uint8 {
	return r.BlockRegistry.LightBlock(r.flip(rid))
}

func (r *shuffledRegistry) RandomTickBlock(rid uint32) bool {
	return r.BlockRegistry.RandomTickBlock(r.flip(rid))
}

func (r *shuffledRegistry) NBTBlock(rid uint32) bool {
	return r.BlockRegistry.NBTBlock(r.flip(rid))
}

func (r *shuffledRegistry) LiquidDisplacingBlock(rid uint32) bool {
	return r.BlockRegistry.LiquidDisplacingBlock(r.flip(rid))
}

func (r *shuffledRegistry) LiquidBlock(rid uint32) bool {
	return r.BlockRegistry.LiquidBlock(r.flip(rid))
}

// TestCrossRegistryPortability writes under one registry and reads under
// another whose runtime IDs are entirely different.
func TestCrossRegistryPortability(t *testing.T) {
	base := testRegistry(t)
	other := newShuffledRegistry(base)
	if other.AirRuntimeID() == base.AirRuntimeID() {
		t.Fatal("the shuffled registry is not actually shuffled")
	}

	d := goldenWorld(t, base)
	var buf bytes.Buffer
	if err := WriteWorld(&buf, d, base, Options{Compression: CompressionNone}); err != nil {
		t.Fatal(err)
	}
	file := buf.Bytes()

	got, err := ReadWorld(file, other)
	if err != nil {
		t.Fatalf("a world written under one registry does not read under another: %v", err)
	}
	if len(got.Columns) != len(d.Columns) {
		t.Fatalf("columns = %d, want %d", len(got.Columns), len(d.Columns))
	}
	// Compare by state, not by runtime ID: the IDs are supposed to differ.
	for _, w := range d.Columns {
		var g Column
		for _, c := range got.Columns {
			if c.X == w.X && c.Z == w.Z {
				g = c
			}
		}
		if g.Col == nil {
			t.Fatalf("column (%d,%d) missing", w.X, w.Z)
		}
		r := w.Col.Chunk.Range()
		for x := range uint8(16) {
			for z := range uint8(16) {
				for y := int16(r[0]); y <= int16(r[1]); y++ {
					for layer := range uint8(2) {
						wn, wp, _ := base.RuntimeIDToState(w.Col.Chunk.Block(x, y, z, layer))
						gn, gp, _ := other.RuntimeIDToState(g.Col.Chunk.Block(x, y, z, layer))
						if wn != gn || len(wp) != len(gp) {
							t.Fatalf("block at (%d,%d,%d) layer %d: got %s, want %s", x, y, z, layer, gn, wn)
						}
					}
				}
			}
		}
	}

	// Re-encoding under the other registry must reproduce the same bytes: the
	// canonical form cannot depend on which process wrote it.
	var round bytes.Buffer
	if err := WriteWorld(&round, got, other, Options{Compression: CompressionNone}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(file, round.Bytes()) {
		t.Fatalf("re-encoding under a different registry changed the bytes: %d vs %d",
			len(file), round.Len())
	}
}

// noPlaceholderRegistry hides minecraft:info_update, so an unresolved state
// falls all the way back to air. That is the registry where a preserved state
// and an empty position are indistinguishable by runtime ID alone, which is
// exactly when the sidecar is the only thing keeping the state alive. Any code
// that tests for air before consulting the sidecar loses the state there.
type noPlaceholderRegistry struct {
	world.BlockRegistry
}

func newNoPlaceholderRegistry(base world.BlockRegistry) *noPlaceholderRegistry {
	return &noPlaceholderRegistry{BlockRegistry: base}
}

func (r *noPlaceholderRegistry) StateToRuntimeID(name string, props map[string]any) (uint32, bool) {
	if name == "minecraft:info_update" {
		return 0, false
	}
	return r.BlockRegistry.StateToRuntimeID(name, props)
}

// TestPlaceholderFallsBackToAir documents the premise the tests above rely on.
func TestPlaceholderFallsBackToAir(t *testing.T) {
	reg := newNoPlaceholderRegistry(testRegistry(t))
	if got := placeholderRid(reg); got != reg.AirRuntimeID() {
		t.Fatalf("placeholder = %d, want air %d", got, reg.AirRuntimeID())
	}
}

// TestStructureAliasCountsOneAppearance: structures share the palette builder
// with worlds and need the same rule. Two runtime IDs for one state in a cell
// are one appearance, so which alias a caller happens to hold must not reorder
// the global palette.
func TestStructureAliasCountsOneAppearance(t *testing.T) {
	reg := newAliasRegistry(testRegistry(t))
	twin := reg.alias - 1
	other, _ := reg.StateToRuntimeID("minecraft:dirt", map[string]any{})
	air := reg.AirRuntimeID()

	build := func(second uint32) []byte {
		data, err := NewStructureData([3]int32{16, 16, 16})
		if err != nil {
			t.Fatal(err)
		}
		sub := chunk.NewSubChunk(air)
		sub.SetBlock(0, 0, 0, 0, twin)
		sub.SetBlock(1, 0, 0, 0, second)
		sub.SetBlock(2, 0, 0, 0, other)
		data.Cells[0] = sub
		var buf bytes.Buffer
		if err := WriteStructure(&buf, data, reg, Options{Compression: CompressionNone}); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	if a, b := build(reg.alias), build(twin); !bytes.Equal(a, b) {
		t.Fatalf("the choice of alias changed the structure: %d vs %d bytes", len(a), len(b))
	}
}

// TestEdgeCellWholeStorageKeepsContent: a whole-storage sidecar on an edge cell
// covers real content and padding at once. Dropping it wholesale to canonicalise
// the padding throws away the content with it.
func TestEdgeCellWholeStorageKeepsContent(t *testing.T) {
	reg := testRegistry(t)
	data, err := NewStructureData([3]int32{1, 1, 1})
	if err != nil {
		t.Fatal(err)
	}
	sub := chunk.NewSubChunk(reg.AirRuntimeID())
	sub.SetBlock(0, 0, 0, 0, placeholderRid(reg))
	data.Cells[0] = sub
	data.UnknownStates = []BlockState{{Name: "audit:unknown", Version: 1}}
	data.Unknown = []UnknownBlock{{Section: 0, Layer: 0, Index: WholeStorage, State: 0}}

	var buf bytes.Buffer
	if err := WriteStructure(&buf, data, reg, Options{Compression: CompressionNone}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("audit:unknown")) {
		t.Fatal("the in-bounds part of a whole-storage sidecar was discarded with the padding")
	}
	got, err := ReadStructure(buf.Bytes(), reg)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Unknown) != 1 || got.Unknown[0].Index != 0 {
		t.Fatalf("recovered %d entries at %v, want one at index 0", len(got.Unknown), got.Unknown)
	}
	// And the padding is still air: only position (0,0,0) is in the box.
	var second bytes.Buffer
	if err := WriteStructure(&second, got, reg, Options{Compression: CompressionNone}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), second.Bytes()) {
		t.Fatal("an edge-cell whole-storage sidecar does not survive a round trip")
	}
}
