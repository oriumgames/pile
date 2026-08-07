package pile

import (
	"path/filepath"
	"testing"

	"github.com/df-mc/dragonfly/server/world"
	"github.com/oriumgames/pile/format"
)

// Tests that writers refuse content the readers would reject: producing a
// file that cannot be reopened is data loss, so every limit the decoder
// enforces must also be enforced when writing.

func TestWriterRejectsOversizedBlob(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	p, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, testColumn(t, reg, world.ChunkPos{0, 0})); err != nil {
		t.Fatal(err)
	}
	p.SetUserData(make([]byte, (16<<20)+1))
	if err := p.Save(); err == nil {
		t.Fatal("oversized user data was accepted")
	}
	_ = p.Close()
}

// TestRollbackStagesFiles: rollback must not leave the world half-restored
// when a snapshot is incomplete, and must restore what it has.

func TestStructureWriterRejectsOversized(t *testing.T) {
	reg := testRegistry(t)
	data, err := format.NewStructureData([3]int32{1, 1, 1})
	if err != nil {
		t.Fatal(err)
	}
	data.UserData = make([]byte, (16<<20)+1)
	s := newStructure(data, StructureRegistry(reg))
	if err := s.Save(filepath.Join(t.TempDir(), "s.pile")); err == nil {
		t.Fatal("oversized structure user data accepted")
	}
}

// TestNewStructureDataOverflow: extreme dimensions must error, not panic.

func TestNewStructureDataOverflow(t *testing.T) {
	if _, err := format.NewStructureData([3]int32{1<<31 - 1, 1, 1}); err == nil {
		t.Fatal("MaxInt32 structure dimension accepted")
	}
}

// TestPastePreservesUpperLayers covers layers beyond 1 through a paste.
// Dragonfly keeps sub chunk layers dense (compaction drops air-only layers
// and shifts the rest down), so the structure carries a full stack.

func TestStructureRejectsOutOfBoundsBlockEntity(t *testing.T) {
	reg := testRegistry(t)
	data, err := format.NewStructureData([3]int32{1, 1, 1})
	if err != nil {
		t.Fatal(err)
	}
	data.BlockEntities = append(data.BlockEntities, format.StructureBlockEntity{
		Pos: [3]int32{2, 0, 0}, Data: map[string]any{"id": "minecraft:chest"},
	})
	s := newStructure(data, StructureRegistry(reg))
	if err := s.Save(filepath.Join(t.TempDir(), "s.pile")); err == nil {
		t.Fatal("out-of-bounds block entity accepted by the writer")
	}
}
