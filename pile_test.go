package pile

import (
	"bytes"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cespare/xxhash/v2"
	"github.com/df-mc/dragonfly/server/block"
	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	_ "github.com/df-mc/dragonfly/server/world/biome" // populate the biome registry
	"github.com/df-mc/dragonfly/server/world/chunk"
	"github.com/df-mc/goleveldb/leveldb"
	"github.com/google/uuid"
	"github.com/oriumgames/pile/format"
)

var regOnce sync.Once

func testRegistry(t testing.TB) world.BlockRegistry {
	t.Helper()
	regOnce.Do(world.DefaultBlockRegistry.Finalize)
	return world.DefaultBlockRegistry
}

// testColumn builds a filled column for the chunk position it will be stored
// at, defaulting to the origin.
//
// The position matters: a block entity's position is absolute, and a record
// stores its x and z as one packed nibble pair, so a block entity outside the
// column it is stored in has no representation. The writer used to fold it back
// in silently — this fixture's (3,1,5) chest, stored at chunk (1,2), was written
// and read back as (19,1,37) — and now refuses instead. Every caller that stores
// away from the origin has to say where.
func testColumn(t testing.TB, reg world.BlockRegistry, at ...world.ChunkPos) *chunk.Column {
	t.Helper()
	pos := world.ChunkPos{0, 0}
	if len(at) > 0 {
		pos = at[0]
	}
	ch := chunk.New(reg, cube.Range{-64, 319})
	stone := reg.BlockRuntimeID(block.Stone{})
	for x := range uint8(16) {
		for z := range uint8(16) {
			for y := int16(-64); y < 0; y++ {
				ch.SetBlock(x, y, z, 0, stone)
			}
		}
	}
	bx, by, bz := int(pos[0])*16+3, 1, int(pos[1])*16+5
	return &chunk.Column{
		Chunk: ch,
		Entities: []chunk.Entity{{ID: 7, Data: map[string]any{
			"identifier": "minecraft:cow", "UniqueID": int64(7),
		}}},
		BlockEntities: []chunk.BlockEntity{{
			Pos:  cube.Pos{bx, by, bz},
			Data: map[string]any{"id": "minecraft:chest", "x": int32(bx), "y": int32(by), "z": int32(bz)},
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
	if err := p.StoreColumn(world.ChunkPos{1, 2}, world.Nether, testColumn(t, reg, world.ChunkPos{1, 2})); err != nil {
		t.Fatal(err)
	}
	s := &world.Settings{Name: "lobby", Time: 6000, TickRange: 4,
		DefaultGameMode: world.GameModeAdventure, Difficulty: world.DifficultyPeaceful}
	p.SaveSettings(s)
	p.SetUserData([]byte("cfg"))
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
	if err := p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, testColumn(t, reg, world.ChunkPos{0, 0})); err != nil {
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
	_ = p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, testColumn(t, reg, world.ChunkPos{0, 0}))
	_ = p.Close()
	before, err := os.ReadFile(filepath.Join(dir, "overworld.pile"))
	if err != nil {
		t.Fatal(err)
	}

	r, err := Open(dir, ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	_ = r.StoreColumn(world.ChunkPos{5, 5}, world.Overworld, testColumn(t, reg, world.ChunkPos{5, 5}))
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

// TestReadOnlyRefusesEveryMutator: every method that changes state has its own
// read-only guard, and Save's guard covers only itself. Snapshot, Rollback and
// DeleteSnapshot write to disk directly, and the setters change what the
// provider reports whether or not a save follows, so a test on Save alone
// leaves ten guards with nothing behind them.
func TestReadOnlyRefusesEveryMutator(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	p, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, testColumn(t, reg, world.ChunkPos{0, 0})); err != nil {
		t.Fatal(err)
	}
	p.SaveSettings(&world.Settings{Name: "original", TickRange: 6})
	p.SetUserData([]byte("original"))
	if err := p.SetChunkUserData(world.ChunkPos{0, 0}, world.Overworld, []byte("original")); err != nil {
		t.Fatal(err)
	}
	if err := p.Snapshot("base"); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	before := worldFingerprint(t, dir)

	r, err := Open(dir, ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	spawns := &memSpawnStore{m: map[uuid.UUID]cube.Pos{}}
	s, err := Open(dir, ReadOnly(), WithSpawnStore(spawns))
	if err != nil {
		t.Fatal(err)
	}

	r.SaveSettings(&world.Settings{Name: "changed"})
	if got := r.Settings().Name; got != "original" {
		t.Fatalf("SaveSettings changed a read-only provider: name %q", got)
	}
	r.SetUserData([]byte("changed"))
	if got := r.UserData(); !bytes.Equal(got, []byte("original")) {
		t.Fatalf("SetUserData changed a read-only provider: %q", got)
	}
	if err := r.SetChunkUserData(world.ChunkPos{0, 0}, world.Overworld, []byte("changed")); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("SetChunkUserData on read-only = %v, want ErrReadOnly", err)
	}
	if got := r.ChunkUserData(world.ChunkPos{0, 0}, world.Overworld); !bytes.Equal(got, []byte("original")) {
		t.Fatalf("SetChunkUserData changed a read-only provider: %q", got)
	}
	if err := r.Snapshot("forbidden"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("Snapshot on read-only = %v, want ErrReadOnly", err)
	}
	if err := r.DeleteSnapshot("base"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("DeleteSnapshot on read-only = %v, want ErrReadOnly", err)
	}
	if err := r.Rollback("base"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("Rollback on read-only = %v, want ErrReadOnly", err)
	}
	if err := s.SavePlayerSpawnPosition(uuid.Nil, cube.Pos{1, 2, 3}); err != nil {
		t.Fatalf("SavePlayerSpawnPosition on read-only = %v, want nil", err)
	}
	if len(spawns.m) != 0 {
		t.Fatalf("SavePlayerSpawnPosition wrote through a read-only provider: %+v", spawns.m)
	}
	// A background save and a Close must both leave the files alone too.
	r.SaveAsync()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if after := worldFingerprint(t, dir); !maps.Equal(before, after) {
		t.Fatalf("a read-only provider changed the world directory:\nbefore %v\nafter  %v", before, after)
	}
}

// worldFingerprint hashes every regular file under dir, so a test can assert
// that nothing at all was written.
func worldFingerprint(t *testing.T, dir string) map[string]uint64 {
	t.Helper()
	out := map[string]uint64{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		out[rel] = xxhash.Sum64(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestSaveSkipsCleanDimensions: a dimension that is on disk, unchanged, and
// whose world metadata is unchanged is not rewritten. The bytes would be
// identical either way — the world encodes deterministically — so the only
// thing that can show it is that no file was replaced.
func TestSaveSkipsCleanDimensions(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	p, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, testColumn(t, reg, world.ChunkPos{0, 0})); err != nil {
		t.Fatal(err)
	}
	if err := p.Save(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "overworld.pile")
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	if err := p.Save(); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !st.ModTime().Equal(old) {
		t.Fatalf("a clean dimension was rewritten: mtime moved to %v", st.ModTime())
	}

	// And a dirty one still is, so the skip is not simply refusing every save.
	if err := p.StoreColumn(world.ChunkPos{1, 0}, world.Overworld, testColumn(t, reg, world.ChunkPos{1, 0})); err != nil {
		t.Fatal(err)
	}
	if err := p.Save(); err != nil {
		t.Fatal(err)
	}
	if st, err = os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if st.ModTime().Equal(old) {
		t.Fatal("a dirty dimension was not rewritten")
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
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
	_ = p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, testColumn(t, reg, world.ChunkPos{0, 0}))
	_ = p.StoreColumn(world.ChunkPos{99, 0}, world.Overworld, testColumn(t, reg, world.ChunkPos{99, 0})) // filtered out
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

// tickColumn is testColumn plus a scheduled update and a distinctive biome,
// so a test can exercise the store categories testColumn leaves empty.
func tickColumn(t testing.TB, reg world.BlockRegistry) *chunk.Column {
	t.Helper()
	col := testColumn(t, reg)
	col.ScheduledBlocks = []chunk.ScheduledBlockUpdate{
		{Pos: cube.Pos{1, -60, 1}, Block: reg.BlockRuntimeID(block.Stone{}), Tick: 9},
	}
	return col
}

// TestStoreSkipMaskCoversEveryCategory: SkipMask has five bits and the store
// path acts on four of them in four separate branches. The existing fixture
// set two and carried no scheduled update and no chunk user data, so three of
// the four could be disabled with the suite still green.
func TestStoreSkipMaskCoversEveryCategory(t *testing.T) {
	reg := testRegistry(t)

	// First, without any skipping, so the fixture is known to carry all four.
	plain := t.TempDir()
	p, err := Open(plain)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, tickColumn(t, reg)); err != nil {
		t.Fatal(err)
	}
	if err := p.SetChunkUserData(world.ChunkPos{0, 0}, world.Overworld, []byte("ud")); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	q, err := Open(plain, ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	full, err := q.LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Entities) != 1 || len(full.BlockEntities) != 1 || len(full.ScheduledBlocks) != 1 ||
		len(q.ChunkUserData(world.ChunkPos{0, 0}, world.Overworld)) == 0 {
		t.Fatalf("fixture is missing a category: %d entities, %d block entities, %d ticks, %d user data bytes",
			len(full.Entities), len(full.BlockEntities), len(full.ScheduledBlocks),
			len(q.ChunkUserData(world.ChunkPos{0, 0}, world.Overworld)))
	}
	_ = q.Close()

	dir := t.TempDir()
	r, err := Open(dir, Skip(SkipEntities|SkipBlockEntities|SkipScheduledTicks|SkipChunkUserData))
	if err != nil {
		t.Fatal(err)
	}
	// User data attaches to a stored column, and the store is what drops it,
	// so the second store is the one under test.
	if err := r.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, tickColumn(t, reg)); err != nil {
		t.Fatal(err)
	}
	if err := r.SetChunkUserData(world.ChunkPos{0, 0}, world.Overworld, []byte("ud")); err != nil {
		t.Fatal(err)
	}
	if err := r.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, tickColumn(t, reg)); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir, ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Entities) != 0 {
		t.Fatalf("SkipEntities ignored: %d entities", len(got.Entities))
	}
	if len(got.BlockEntities) != 0 {
		t.Fatalf("SkipBlockEntities ignored: %d block entities", len(got.BlockEntities))
	}
	if len(got.ScheduledBlocks) != 0 {
		t.Fatalf("SkipScheduledTicks ignored: %d scheduled updates", len(got.ScheduledBlocks))
	}
	if ud := s.ChunkUserData(world.ChunkPos{0, 0}, world.Overworld); len(ud) != 0 {
		t.Fatalf("SkipChunkUserData ignored: %q", ud)
	}
}

// TestFilterEntityDropsOnStore: FilterEntity sits in the else arm of
// SkipEntities, so a fixture that sets SkipEntities can never reach it.
func TestFilterEntityDropsOnStore(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	p, err := Open(dir, FilterEntity(func(e chunk.Entity) bool {
		return e.Data["identifier"] != "minecraft:cow"
	}))
	if err != nil {
		t.Fatal(err)
	}
	col := testColumn(t, reg)
	col.Entities = append(col.Entities, chunk.Entity{ID: 8, Data: map[string]any{
		"identifier": "minecraft:pig", "UniqueID": int64(8),
	}})
	if err := p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, col); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	q, err := Open(dir, ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	got, err := q.LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Entities) != 1 || got.Entities[0].Data["identifier"] != "minecraft:pig" {
		t.Fatalf("FilterEntity ignored on store: %+v", got.Entities)
	}
}

// TestStoreLightAndSkipBiomesReachTheWriter: both options are carried into the
// encoder's Options by one field each in the save path, and nothing else
// carries them, so a suite that never asserts on the resulting file cannot see
// either field go.
func TestStoreLightAndSkipBiomesReachTheWriter(t *testing.T) {
	reg := testRegistry(t)
	desert, ok := world.BiomeByName("desert")
	if !ok {
		t.Skip("registry has no desert biome")
	}
	lit := t.TempDir()
	p, err := Open(lit, StoreLight())
	if err != nil {
		t.Fatal(err)
	}
	col := testColumn(t, reg)
	col.Chunk.SetBiome(0, -60, 0, uint32(desert.EncodeBiome()))
	// StoreLight only claims light the records actually carry, so the column
	// has to hold some.
	chunk.LightArea([]*chunk.Chunk{col.Chunk}, 0, 0).Fill()
	if err := p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, col); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	_, flags, err := fileHeader(filepath.Join(lit, "overworld.pile"))
	if err != nil {
		t.Fatal(err)
	}
	if flags&format.FlagStoreLight == 0 {
		t.Fatalf("StoreLight did not reach the writer: flags %#x", flags)
	}

	// SkipBiomes, in its own world so the two assertions cannot borrow each
	// other's file.
	nobio := t.TempDir()
	q, err := Open(nobio, Skip(SkipBiomes))
	if err != nil {
		t.Fatal(err)
	}
	col2 := testColumn(t, reg)
	col2.Chunk.SetBiome(0, -60, 0, uint32(desert.EncodeBiome()))
	if err := q.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, col2); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
	if _, flags, err = fileHeader(filepath.Join(nobio, "overworld.pile")); err != nil {
		t.Fatal(err)
	}
	if flags&format.FlagStoreLight != 0 {
		t.Fatalf("StoreLight set without the option: flags %#x", flags)
	}
	r, err := Open(nobio, ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, err := r.LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	if got.Chunk.Biome(0, -60, 0) == uint32(desert.EncodeBiome()) {
		t.Fatal("SkipBiomes did not reach the writer: the biome survived")
	}
}

func TestDeterministicResave(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	p, _ := Open(dir)
	_ = p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, testColumn(t, reg, world.ChunkPos{0, 0}))
	_ = p.Close()
	path := filepath.Join(dir, "overworld.pile")
	first, _ := os.ReadFile(path)

	q, _ := Open(dir)
	// Store identical content again and force a rewrite.
	_ = q.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, testColumn(t, reg, world.ChunkPos{0, 0}))
	if err := q.Save(); err != nil {
		t.Fatal(err)
	}
	_ = q.Close()
	second, _ := os.ReadFile(path)
	if !bytes.Equal(first, second) {
		t.Fatalf("resave of identical content changed bytes: %d vs %d", len(first), len(second))
	}
}
