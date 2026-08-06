package pile

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/df-mc/dragonfly/server/block"
	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	_ "github.com/df-mc/dragonfly/server/world/biome" // populate the biome registry
	"github.com/df-mc/dragonfly/server/world/chunk"
	"github.com/df-mc/goleveldb/leveldb"
)

var regOnce sync.Once

func testRegistry(t testing.TB) world.BlockRegistry {
	t.Helper()
	regOnce.Do(world.DefaultBlockRegistry.Finalize)
	return world.DefaultBlockRegistry
}

func testColumn(t testing.TB, reg world.BlockRegistry) *chunk.Column {
	t.Helper()
	ch := chunk.New(reg, cube.Range{-64, 319})
	stone := reg.BlockRuntimeID(block.Stone{})
	for x := range uint8(16) {
		for z := range uint8(16) {
			for y := int16(-64); y < 0; y++ {
				ch.SetBlock(x, y, z, 0, stone)
			}
		}
	}
	return &chunk.Column{
		Chunk: ch,
		Entities: []chunk.Entity{{ID: 7, Data: map[string]any{
			"identifier": "minecraft:cow", "UniqueID": int64(7),
		}}},
		BlockEntities: []chunk.BlockEntity{{
			Pos:  cube.Pos{3, 1, 5},
			Data: map[string]any{"id": "minecraft:chest", "x": int32(3), "y": int32(1), "z": int32(5)},
		}},
		Tick: 100,
	}
}

func TestProviderRoundTrip(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()

	p, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	col := testColumn(t, reg)
	if err := p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, col); err != nil {
		t.Fatal(err)
	}
	if err := p.StoreColumn(world.ChunkPos{1, 2}, world.Nether, testColumn(t, reg)); err != nil {
		t.Fatal(err)
	}
	s := &world.Settings{Name: "lobby", Time: 6000, TickRange: 4,
		DefaultGameMode: world.GameModeAdventure, Difficulty: world.DifficultyPeaceful}
	p.SaveSettings(s)
	p.SetUserData([]byte("cfg"))
	p.SetMarker(Marker{Name: "spawn", Kind: "spawn", Pos: [3]float64{1, 65, 2}})
	_ = p.SetChunkUserData(world.ChunkPos{0, 0}, world.Overworld, []byte("chunky"))
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	q, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if got := q.Settings(); got.Name != "lobby" || got.Time != 6000 || got.TickRange != 4 {
		t.Fatalf("settings did not round trip: %+v", got)
	}
	if !bytes.Equal(q.UserData(), []byte("cfg")) {
		t.Fatal("user data did not round trip")
	}
	ms := q.Markers()
	if len(ms) != 1 || ms[0].Name != "spawn" || ms[0].Pos != [3]float64{1, 65, 2} {
		t.Fatalf("markers did not round trip: %+v", ms)
	}
	if !bytes.Equal(q.ChunkUserData(world.ChunkPos{0, 0}, world.Overworld), []byte("chunky")) {
		t.Fatal("chunk user data did not round trip")
	}
	got, err := q.LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	stone := reg.BlockRuntimeID(block.Stone{})
	if rid := got.Chunk.Block(4, -10, 4, 0); rid != stone {
		t.Fatalf("expected stone, got rid %d", rid)
	}
	if len(got.Entities) != 1 || got.Entities[0].ID != 7 {
		t.Fatalf("entities did not round trip: %+v", got.Entities)
	}
	if len(got.BlockEntities) != 1 || got.BlockEntities[0].Pos != (cube.Pos{3, 1, 5}) {
		t.Fatalf("block entities did not round trip: %+v", got.BlockEntities)
	}
	if got.Tick != 100 {
		t.Fatalf("tick did not round trip: %d", got.Tick)
	}
	if q.ChunkCount(world.Nether) != 1 {
		t.Fatal("nether column missing")
	}
	if _, err := q.LoadColumn(world.ChunkPos{9, 9}, world.Overworld); !errors.Is(err, leveldb.ErrNotFound) {
		t.Fatalf("missing column error = %v, want leveldb.ErrNotFound", err)
	}
}

func TestLoadColumnIsolation(t *testing.T) {
	reg := testRegistry(t)
	p, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, testColumn(t, reg)); err != nil {
		t.Fatal(err)
	}
	a, _ := p.LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
	air := reg.AirRuntimeID()
	a.Chunk.SetBlock(4, -10, 4, 0, air)
	a.Entities[0].Data["identifier"] = "minecraft:pig"

	b, _ := p.LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
	stone := reg.BlockRuntimeID(block.Stone{})
	if rid := b.Chunk.Block(4, -10, 4, 0); rid != stone {
		t.Fatal("stored column was mutated through a loaded copy")
	}
	if b.Entities[0].Data["identifier"] != "minecraft:cow" {
		t.Fatal("stored entity NBT was mutated through a loaded copy")
	}
}

func TestReadOnly(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	p, _ := Open(dir)
	_ = p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, testColumn(t, reg))
	_ = p.Close()
	before, err := os.ReadFile(filepath.Join(dir, "overworld.pile"))
	if err != nil {
		t.Fatal(err)
	}

	r, err := Open(dir, ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	_ = r.StoreColumn(world.ChunkPos{5, 5}, world.Overworld, testColumn(t, reg))
	r.SaveSettings(&world.Settings{Name: "changed"})
	if err := r.Save(); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("Save on read-only = %v, want ErrReadOnly", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "overworld.pile"))
	if !bytes.Equal(before, after) {
		t.Fatal("read-only provider modified the file")
	}
	if r.ChunkCount(world.Overworld) != 1 {
		t.Fatal("read-only store was not ignored")
	}
}

func TestSkipAndFilters(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	p, err := Open(dir,
		Skip(SkipEntities|SkipScheduledTicks),
		FilterBlockEntity(func(be chunk.BlockEntity) bool { return be.Data["id"] != "minecraft:chest" }),
		FilterColumn(func(pos world.ChunkPos, _ world.Dimension) bool { return pos[0] < 10 }),
	)
	if err != nil {
		t.Fatal(err)
	}
	_ = p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, testColumn(t, reg))
	_ = p.StoreColumn(world.ChunkPos{99, 0}, world.Overworld, testColumn(t, reg)) // filtered out
	_ = p.Close()

	q, _ := Open(dir)
	defer q.Close()
	if q.ChunkCount(world.Overworld) != 1 {
		t.Fatalf("filtered column persisted; count %d", q.ChunkCount(world.Overworld))
	}
	col, _ := q.LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
	if len(col.Entities) != 0 {
		t.Fatal("skipped entities persisted")
	}
	if len(col.BlockEntities) != 0 {
		t.Fatal("filtered block entity persisted")
	}
}

func TestDeterministicResave(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	p, _ := Open(dir)
	_ = p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, testColumn(t, reg))
	_ = p.Close()
	path := filepath.Join(dir, "overworld.pile")
	first, _ := os.ReadFile(path)

	q, _ := Open(dir)
	// Store identical content again and force a rewrite.
	_ = q.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, testColumn(t, reg))
	if err := q.Save(); err != nil {
		t.Fatal(err)
	}
	_ = q.Close()
	second, _ := os.ReadFile(path)
	if !bytes.Equal(first, second) {
		t.Fatalf("resave of identical content changed bytes: %d vs %d", len(first), len(second))
	}
}
