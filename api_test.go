package pile

import (
	"bytes"
	"errors"
	"slices"
	"testing"

	"github.com/df-mc/dragonfly/server/block"
	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/google/uuid"
	"github.com/oriumgames/pile/format"
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
	_ = p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, testColumn(t, reg, world.ChunkPos{0, 0}))

	p.SetMarker(Marker{Name: "a", Kind: "poi", Pos: &[3]float64{}})
	p.SetMarker(Marker{Name: "b", Kind: "poi", Pos: &[3]float64{}})
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
		if err := p.StoreColumn(world.ChunkPos{k[0], k[1]}, world.Overworld, testColumn(t, reg, world.ChunkPos{k[0], k[1]})); err != nil {
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

// TestProviderMaxDecodedBytes threads the provider's decode ceiling down to the
// format readers, in both storage modes.
//
// The provider is where the option is actually reached from, so a format-level
// test alone would leave the wiring unproved: pile.MaxDecodedBytes could set a
// field nothing reads and every format test would stay green.
func TestProviderMaxDecodedBytes(t *testing.T) {
	reg := testRegistry(t)
	for _, mode := range []struct {
		name string
		opts []Option
	}{
		{"solid", nil},
		{"append", []Option{AppendMode()}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			dir := t.TempDir()
			p, err := Open(dir, mode.opts...)
			if err != nil {
				t.Fatal(err)
			}
			for x := int32(0); x < 8; x++ {
				if err := p.StoreColumn(world.ChunkPos{x, 0}, world.Overworld, testColumn(t, reg, world.ChunkPos{x, 0})); err != nil {
					t.Fatal(err)
				}
			}
			if err := p.Close(); err != nil {
				t.Fatal(err)
			}

			// The default opens it.
			q, err := Open(dir, mode.opts...)
			if err != nil {
				t.Fatalf("the world does not open at the default ceiling: %v", err)
			}
			if err := q.Close(); err != nil {
				t.Fatal(err)
			}

			// A ceiling of one byte does not, and says so as policy rather than
			// as corruption.
			_, err = Open(dir, append(slices.Clone(mode.opts), MaxDecodedBytes(1))...)
			if err == nil {
				t.Fatal("a one-byte ceiling opened the world: the option is not reaching the reader")
			}
			if !errors.Is(err, format.ErrDecodeBudget) {
				t.Fatalf("refused, but not as a budget refusal: %v", err)
			}
			if errors.Is(err, format.ErrCorrupt) {
				t.Fatalf("a policy refusal claimed the world is corrupt: %v", err)
			}
		})
	}
}

// TestAreaMarkersRoundTrip: an area is a marker carrying bounds (§7.3), so it
// has to survive everything a point marker survives -- a save, a reopen, and a
// move -- without a code path of its own.
//
// The move is the half worth testing. moveMarkers translated positions and
// nothing else, so an area added without touching it would have been left
// behind while every point followed, and the world would look right until
// somebody stood in a region that was no longer where it said.
func TestAreaMarkersRoundTrip(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	p, err := Open(dir, Registry(reg))
	if err != nil {
		t.Fatal(err)
	}
	pos := world.ChunkPos{0, 0}
	if err := p.StoreColumn(pos, world.Overworld, testColumn(t, reg, pos)); err != nil {
		t.Fatal(err)
	}
	p.SetMarker(Point("hub", "spawn", [3]float64{1, 65, 2}))
	// Corners deliberately the wrong way round: Area orders them, which is
	// where repairing belongs -- before the bytes exist. The wire refuses an
	// inverted box rather than fixing it.
	p.SetMarker(Area("arena", "pvp", [3]float64{10, 70, 10}, [3]float64{-10, 60, -10}))
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	reopen := func(t *testing.T) map[string]Marker {
		t.Helper()
		q, err := Open(dir, Registry(reg), ReadOnly())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = q.Close() }()
		out := map[string]Marker{}
		for _, m := range q.Markers() {
			out[m.Name] = m
		}
		return out
	}

	got := reopen(t)
	area, ok := got["arena"]
	if !ok || area.Bounds == nil {
		t.Fatalf("the area did not survive the save: %+v", got["arena"])
	}
	if area.Pos != nil {
		t.Errorf("an area with no point came back carrying one: %v", *area.Pos)
	}
	if area.Bounds.Min != [3]float64{-10, 60, -10} || area.Bounds.Max != [3]float64{10, 70, 10} {
		t.Errorf("bounds came back as %+v, want min{-10,60,-10} max{10,70,10}", *area.Bounds)
	}
	if hub := got["hub"]; hub.Pos == nil || hub.Bounds != nil {
		t.Errorf("the point marker came back as %+v", hub)
	}

	// And the move.
	if _, err := MoveWorld(dir, MoveOptions{Offset: cube.Pos{100, 0, -50}}); err != nil {
		t.Fatal(err)
	}
	moved := reopen(t)
	if b := moved["arena"].Bounds; b == nil ||
		b.Min != [3]float64{90, 60, -60} || b.Max != [3]float64{110, 70, -40} {
		t.Errorf("the area did not move with the world: %+v", moved["arena"].Bounds)
	}
	if pt := moved["hub"].Pos; pt == nil || *pt != [3]float64{101, 65, -48} {
		t.Errorf("the point marker moved to %v", moved["hub"].Pos)
	}
}
