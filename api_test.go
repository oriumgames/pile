package pile

import (
	"bytes"
	"testing"

	"github.com/df-mc/dragonfly/server/block"
	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/google/uuid"
)

func TestLoadSkip(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	p, _ := Open(dir)
	if err := p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, tickColumn(t, reg)); err != nil {
		t.Fatal(err)
	}
	_ = p.Close()

	q, err := Open(dir, LoadSkip(SkipEntities|SkipBlockEntities|SkipScheduledTicks))
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	col, err := q.LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	if len(col.Entities) != 0 || len(col.BlockEntities) != 0 || len(col.ScheduledBlocks) != 0 {
		t.Fatalf("LoadSkip ignored: %d entities, %d block entities, %d scheduled updates",
			len(col.Entities), len(col.BlockEntities), len(col.ScheduledBlocks))
	}
	// The file still contains them: a plain open sees them.
	r, _ := Open(dir, ReadOnly())
	defer r.Close()
	full, _ := r.LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
	if len(full.Entities) != 1 || len(full.BlockEntities) != 1 || len(full.ScheduledBlocks) != 1 {
		t.Fatalf("the fixture is missing a category in the file: %d entities, %d block entities, %d scheduled updates",
			len(full.Entities), len(full.BlockEntities), len(full.ScheduledBlocks))
	}
}

// memSpawnStore is a test SpawnStore.
type memSpawnStore struct {
	m map[uuid.UUID]cube.Pos
}

func (s *memSpawnStore) LoadPlayerSpawn(id uuid.UUID) (cube.Pos, bool, error) {
	pos, ok := s.m[id]
	return pos, ok, nil
}

func (s *memSpawnStore) SavePlayerSpawn(id uuid.UUID, pos cube.Pos) error {
	s.m[id] = pos
	return nil
}

func TestSpawnStore(t *testing.T) {
	store := &memSpawnStore{m: map[uuid.UUID]cube.Pos{}}
	p := NewMemory(WithSpawnStore(store))
	defer p.Close()
	id := uuid.New()

	if _, ok, err := p.LoadPlayerSpawnPosition(id); ok || err != nil {
		t.Fatalf("unexpected spawn: ok=%v err=%v", ok, err)
	}
	if err := p.SavePlayerSpawnPosition(id, cube.Pos{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	pos, ok, err := p.LoadPlayerSpawnPosition(id)
	if !ok || err != nil || pos != (cube.Pos{1, 2, 3}) {
		t.Fatalf("spawn round trip failed: %v %v %v", pos, ok, err)
	}

	// Without a store: always absent, saves are no-ops.
	q := NewMemory()
	defer q.Close()
	if err := q.SavePlayerSpawnPosition(id, cube.Pos{9, 9, 9}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := q.LoadPlayerSpawnPosition(id); ok {
		t.Fatal("provider without SpawnStore reported a spawn")
	}
}

func TestFillBiome(t *testing.T) {
	reg := testRegistry(t)
	desert, ok := world.BiomeByName("minecraft:desert")
	if !ok {
		t.Skip("desert biome not registered")
	}
	b := NewBuilder(reg, cube.Range{-64, 319})
	b.Fill(cube.Pos{0, 0, 0}, cube.Pos{15, 3, 15}, block.Stone{})
	b.FillBiome(cube.Pos{0, -64, 0}, cube.Pos{15, 319, 15}, desert)
	p := b.Provider()
	defer p.Close()

	col, err := p.LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	if got := col.Chunk.Biome(4, 10, 4); got != uint32(desert.EncodeBiome()) {
		t.Fatalf("biome = %d, want desert (%d)", got, desert.EncodeBiome())
	}
}

func TestMarkerRemoveAndSnapshotDelete(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	p, _ := Open(dir)
	defer p.Close()
	_ = p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, testColumn(t, reg))

	p.SetMarker(Marker{Name: "a", Kind: "poi"})
	p.SetMarker(Marker{Name: "b", Kind: "poi"})
	if !p.RemoveMarker("a") {
		t.Fatal("RemoveMarker(a) = false")
	}
	if p.RemoveMarker("a") {
		t.Fatal("second RemoveMarker(a) = true")
	}
	if ms := p.Markers(); len(ms) != 1 || ms[0].Name != "b" {
		t.Fatalf("markers after remove: %+v", ms)
	}

	if err := p.Snapshot("s1"); err != nil {
		t.Fatal(err)
	}
	if err := p.Snapshot("s2"); err != nil {
		t.Fatal(err)
	}
	if err := p.DeleteSnapshot("s1"); err != nil {
		t.Fatal(err)
	}
	names, err := p.Snapshots()
	if err != nil || len(names) != 1 || names[0] != "s2" {
		t.Fatalf("snapshots after delete: %v (%v)", names, err)
	}
	if err := p.DeleteSnapshot("../evil"); err == nil {
		t.Fatal("path traversal in snapshot name accepted")
	}
}

func TestColumnsIterator(t *testing.T) {
	reg := testRegistry(t)
	p := NewMemory()
	defer p.Close()
	want := map[[2]int32]bool{{0, 0}: true, {3, -2}: true, {-5, 7}: true}
	for k := range want {
		if err := p.StoreColumn(world.ChunkPos{k[0], k[1]}, world.Overworld, testColumn(t, reg)); err != nil {
			t.Fatal(err)
		}
	}
	_ = p.SetChunkUserData(world.ChunkPos{0, 0}, world.Overworld, []byte("x"))

	seen := 0
	stone := reg.BlockRuntimeID(block.Stone{})
	for pos, col := range p.Columns(world.Overworld) {
		if !want[[2]int32{pos[0], pos[1]}] {
			t.Fatalf("unexpected column %v", pos)
		}
		if col.Chunk.Block(4, -10, 4, 0) != stone {
			t.Fatalf("column %v content wrong", pos)
		}
		seen++
		if seen == 2 {
			break // early break must not panic or leak
		}
	}
	if seen != 2 {
		t.Fatalf("early break iterated %d", seen)
	}
	// Full iteration count.
	total := 0
	for range p.Columns(world.Overworld) {
		total++
	}
	if total != 3 {
		t.Fatalf("iterated %d columns, want 3", total)
	}
	if !bytes.Equal(p.ChunkUserData(world.ChunkPos{0, 0}, world.Overworld), []byte("x")) {
		t.Fatal("chunk user data lost")
	}
	// Read-only provider errors for snapshots.
	r := NewMemory()
	defer r.Close()
	if err := r.Snapshot("x"); err == nil {
		t.Fatal("in-memory snapshot accepted")
	}
}
