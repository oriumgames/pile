package pile

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/df-mc/dragonfly/server/block"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/goleveldb/leveldb"
)

func TestAppendProviderRoundTrip(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()

	p, err := Open(dir, AppendMode())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, testColumn(t, reg, world.ChunkPos{0, 0})); err != nil {
		t.Fatal(err)
	}
	if err := p.StoreColumn(world.ChunkPos{4, -3}, world.Nether, testColumn(t, reg, world.ChunkPos{4, -3})); err != nil {
		t.Fatal(err)
	}
	p.SaveSettings(&world.Settings{Name: "append-world", Time: 99, TickRange: 5})
	p.SetMarker(Marker{Name: "hub", Kind: "poi", Pos: &[3]float64{0, 64, 0}})
	_ = p.SetChunkUserData(world.ChunkPos{0, 0}, world.Overworld, []byte("cud"))
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	q, err := Open(dir, AppendMode())
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if got := q.Settings(); got.Name != "append-world" || got.Time != 99 || got.TickRange != 5 {
		t.Fatalf("settings did not round trip: %+v", got)
	}
	if ms := q.Markers(); len(ms) != 1 || ms[0].Name != "hub" {
		t.Fatalf("markers did not round trip: %+v", ms)
	}
	col, err := q.LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	stone := reg.BlockRuntimeID(block.Stone{})
	if rid := col.Chunk.Block(4, -10, 4, 0); rid != stone {
		t.Fatalf("expected stone, got rid %d", rid)
	}
	if len(col.Entities) != 1 || col.Entities[0].ID != 7 {
		t.Fatalf("entities did not round trip: %+v", col.Entities)
	}
	if !bytes.Equal(q.ChunkUserData(world.ChunkPos{0, 0}, world.Overworld), []byte("cud")) {
		t.Fatal("chunk user data did not round trip")
	}
	if q.ChunkCount(world.Nether) != 1 {
		t.Fatal("nether column missing")
	}
	if _, err := q.LoadColumn(world.ChunkPos{9, 9}, world.Overworld); !errors.Is(err, leveldb.ErrNotFound) {
		t.Fatalf("missing column error = %v", err)
	}

	// Overwriting a column preserves its user data.
	if err := q.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, testColumn(t, reg, world.ChunkPos{0, 0})); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(q.ChunkUserData(world.ChunkPos{0, 0}, world.Overworld), []byte("cud")) {
		t.Fatal("chunk user data lost on overwrite")
	}
}

func TestAppendAutoCompactOnClose(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	p, err := Open(dir, AppendMode())
	if err != nil {
		t.Fatal(err)
	}
	// Heavy overwriting produces garbage well past the threshold.
	for range 20 {
		if err := p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, testColumn(t, reg, world.ChunkPos{0, 0})); err != nil {
			t.Fatal(err)
		}
		if err := p.Save(); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "overworld.pile")
	before, _ := os.Stat(path)
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	if after.Size() >= before.Size() {
		t.Fatalf("auto-compact did not shrink file: %d -> %d", before.Size(), after.Size())
	}

	q, err := Open(dir, AppendMode())
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if q.ChunkCount(world.Overworld) != 1 {
		t.Fatalf("chunk count after compact = %d", q.ChunkCount(world.Overworld))
	}
}

func TestAppendRejectsSolidFile(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	p, _ := Open(dir)
	_ = p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, testColumn(t, reg, world.ChunkPos{0, 0}))
	_ = p.Close()

	if _, err := Open(dir, AppendMode()); err == nil {
		t.Fatal("append mode opened a solid file")
	}
	// And the reverse: solid open of an indexed world.
	dir2 := t.TempDir()
	a, _ := Open(dir2, AppendMode())
	_ = a.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, testColumn(t, reg, world.ChunkPos{0, 0}))
	_ = a.Close()
	if _, err := Open(dir2); err == nil {
		t.Fatal("solid mode opened an indexed file")
	}
}

func TestSnapshotRollbackSolid(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	p, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	stone := reg.BlockRuntimeID(block.Stone{})
	dirt := reg.BlockRuntimeID(block.Dirt{})

	if err := p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, testColumn(t, reg, world.ChunkPos{0, 0})); err != nil {
		t.Fatal(err)
	}
	if err := p.Snapshot("clean"); err != nil {
		t.Fatal(err)
	}

	// Grief the world.
	col, _ := p.LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
	col.Chunk.SetBlock(4, -10, 4, 0, dirt)
	_ = p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, col)
	_ = p.StoreColumn(world.ChunkPos{5, 5}, world.Overworld, testColumn(t, reg, world.ChunkPos{5, 5}))
	if err := p.Save(); err != nil {
		t.Fatal(err)
	}

	names, err := p.Snapshots()
	if err != nil || len(names) != 1 || names[0] != "clean" {
		t.Fatalf("snapshots = %v (%v)", names, err)
	}
	if err := p.Rollback("clean"); err != nil {
		t.Fatal(err)
	}
	got, err := p.LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	if rid := got.Chunk.Block(4, -10, 4, 0); rid != stone {
		t.Fatalf("rollback did not restore block, rid %d", rid)
	}
	if _, err := p.LoadColumn(world.ChunkPos{5, 5}, world.Overworld); !errors.Is(err, leveldb.ErrNotFound) {
		t.Fatal("rollback kept a post-snapshot chunk")
	}
}

func TestSnapshotRollbackAppend(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	p, err := Open(dir, AppendMode())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	stone := reg.BlockRuntimeID(block.Stone{})
	dirt := reg.BlockRuntimeID(block.Dirt{})

	if err := p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, testColumn(t, reg, world.ChunkPos{0, 0})); err != nil {
		t.Fatal(err)
	}
	if err := p.Snapshot("island-v1"); err != nil {
		t.Fatal(err)
	}
	col, _ := p.LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
	col.Chunk.SetBlock(4, -10, 4, 0, dirt)
	_ = p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, col)
	if err := p.Save(); err != nil {
		t.Fatal(err)
	}

	if err := p.Rollback("island-v1"); err != nil {
		t.Fatal(err)
	}
	got, err := p.LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	if rid := got.Chunk.Block(4, -10, 4, 0); rid != stone {
		t.Fatalf("append rollback did not restore block, rid %d", rid)
	}
	// Provider still writable after rollback.
	if err := p.StoreColumn(world.ChunkPos{1, 1}, world.Overworld, testColumn(t, reg, world.ChunkPos{1, 1})); err != nil {
		t.Fatal(err)
	}
}
