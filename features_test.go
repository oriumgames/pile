package pile

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/df-mc/dragonfly/server/block"
	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/oriumgames/pile/format"
)

func TestCacheColumns(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	p, err := Open(dir, AppendMode(), CacheColumns(4))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	stone := reg.BlockRuntimeID(block.Stone{})
	dirt := reg.BlockRuntimeID(block.Dirt{})
	if err := p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, testColumn(t, reg)); err != nil {
		t.Fatal(err)
	}

	// First load fills the cache, second load hits it; both must be correct
	// and isolated.
	a, err := p.LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	a.Chunk.SetBlock(1, 1, 1, 0, dirt) // mutate the returned copy only
	b, err := p.LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	if rid := b.Chunk.Block(1, 1, 1, 0); rid == dirt {
		t.Fatal("cache leaked a mutable column")
	}
	if rid := b.Chunk.Block(4, -10, 4, 0); rid != stone {
		t.Fatal("cached column content wrong")
	}

	// Storing invalidates: the next load sees the new content.
	col := testColumn(t, reg)
	col.Chunk.SetBlock(2, 2, 2, 0, dirt)
	if err := p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, col); err != nil {
		t.Fatal(err)
	}
	c, err := p.LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
	if err != nil {
		t.Fatal(err)
	}
	if rid := c.Chunk.Block(2, 2, 2, 0); rid != dirt {
		t.Fatal("cache served stale data after store")
	}
}

func TestAutoSave(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	p, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, testColumn(t, reg)); err != nil {
		t.Fatal(err)
	}
	stop := p.AutoSave(20 * time.Millisecond)
	defer stop()

	path := filepath.Join(dir, "overworld.pile")
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("autosave never wrote the file")
		}
		time.Sleep(10 * time.Millisecond)
	}
	stop()
	stop() // idempotent
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestBorderRoundTripAndMove(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	p, _ := Open(dir)
	_ = p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, testColumn(t, reg))
	if err := p.SetBorder(&Border{Min: [2]int32{-64, -64}, Max: [2]int32{64, 64}}); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	q, err := Open(dir, ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	got := q.Border()
	_ = q.Close()
	if got == nil || got.Min != [2]int32{-64, -64} || got.Max != [2]int32{64, 64} {
		t.Fatalf("border round trip: %+v", got)
	}

	// A world move translates the border.
	if _, err := MoveWorld(dir, MoveOptions{Offset: cube.Pos{16, 0, 32}, Backup: false}); err != nil {
		t.Fatal(err)
	}
	r, err := Open(dir, ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	moved := r.Border()
	if moved == nil || moved.Min != [2]int32{-48, -32} || moved.Max != [2]int32{80, 96} {
		t.Fatalf("border not translated: %+v", moved)
	}
}

func TestStructureLibrary(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	for _, name := range []string{"house", "tower"} {
		b := NewBuilder(reg, cube.Range{-64, 319})
		b.Fill(cube.Pos{0, 0, 0}, cube.Pos{4, 4, 4}, block.Stone{})
		p := b.Provider()
		s, err := ExtractStructure(p, world.Overworld, cube.Pos{0, 0, 0}, cube.Pos{4, 4, 4})
		_ = p.Close()
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Save(filepath.Join(dir, name+".pile")); err != nil {
			t.Fatal(err)
		}
	}
	lib, err := LoadStructureLibrary(dir)
	if err != nil {
		t.Fatal(err)
	}
	if lib.Len() != 2 {
		t.Fatalf("library size = %d, want 2", lib.Len())
	}
	names := lib.Names()
	if names[0] != "house" || names[1] != "tower" {
		t.Fatalf("names = %v", names)
	}
	s, ok := lib.Get("tower")
	if !ok || s.Dimensions() != [3]int{5, 5, 5} {
		t.Fatalf("Get(tower) = %v %v", s, ok)
	}
}

func TestStructureRotate(t *testing.T) {
	reg := testRegistry(t)
	stone := reg.BlockRuntimeID(block.Stone{})
	logX, okLog := reg.StateToRuntimeID("minecraft:oak_log", map[string]any{"pillar_axis": "x"})
	if !okLog {
		t.Skip("oak_log state unavailable")
	}

	data, err := format.NewStructureData([3]int32{3, 1, 2})
	if err != nil {
		t.Fatal(err)
	}
	s := newStructure(data)
	s.setLocal(0, 0, 0, 0, stone) // corner marker
	s.setLocal(2, 0, 1, 0, logX)  // X-axis log at the far corner

	r := s.Rotate(1)
	if r.Dimensions() != [3]int{2, 1, 3} {
		t.Fatalf("rotated dimensions = %v", r.Dimensions())
	}
	// (0,0) -> (sizeZ-1-0, 0) = (1, 0); (2,1) -> (0, 2).
	if rid, _ := r.ridAt(1, 0, 0); rid != stone {
		t.Fatalf("corner marker not rotated correctly")
	}
	rid, _ := r.ridAt(0, 0, 2)
	name, props, _ := reg.RuntimeIDToState(rid)
	if name != "minecraft:oak_log" || props["pillar_axis"] != "z" {
		t.Fatalf("log state not rotated: %s %v", name, props)
	}

	// Four quarter turns are the identity.
	r4 := s.Rotate(4)
	for x := range 3 {
		for z := range 2 {
			w, _ := s.ridAt(x, 0, z)
			g, _ := r4.ridAt(x, 0, z)
			if w != g {
				t.Fatalf("Rotate(4) not identity at (%d,%d)", x, z)
			}
		}
	}
}

func TestBuilderEntityIDsUnique(t *testing.T) {
	reg := testRegistry(t)
	b := NewBuilder(reg, cube.Range{-64, 319})
	b.AddEntity(map[string]any{
		"identifier": "minecraft:cow", "UniqueID": int64(1),
		"Pos": []any{float32(0.5), float32(1), float32(0.5)},
	})
	b.AddEntity(map[string]any{
		"identifier": "minecraft:pig",
		"Pos":        []any{float32(1.5), float32(1), float32(1.5)},
	})
	p := b.Provider()
	defer p.Close()
	seen := map[int64]bool{}
	for _, col := range p.Columns(world.Overworld) {
		for _, e := range col.Entities {
			if seen[e.ID] {
				t.Fatalf("duplicate entity ID %d", e.ID)
			}
			seen[e.ID] = true
		}
	}
	if len(seen) != 2 {
		t.Fatalf("got %d entities, want 2", len(seen))
	}
}

// TestExtractCarriesEntityID: an entity whose NBT has no UniqueID yet must
// keep its stable ID through extraction and paste.

// TestDimensionHeaderSurvivesEveryWriter: the dimension lives in the file
// header, and every path that writes a world has to set it. A path that forgets
// leaves the zero value, which is a valid claim to be the overworld, so the
// mistake is invisible to anything that trusts the file name instead. This test
// walks each writer and reads the header back.
func TestDimensionHeaderSurvivesEveryWriter(t *testing.T) {
	reg := testRegistry(t)
	dims := []struct {
		dim  world.Dimension
		file string
		want format.Dimension
	}{
		{world.Overworld, "overworld.pile", format.Overworld},
		{world.Nether, "nether.pile", format.Nether},
		{world.End, "end.pile", format.End},
	}

	headerDimension := func(t *testing.T, path string) format.Dimension {
		t.Helper()
		// Read the 16-byte header directly: solid and indexed files share it,
		// and the point is what the bytes say rather than what a decoder makes
		// of them.
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(b) < 16 {
			t.Fatalf("%s is %d bytes", path, len(b))
		}
		flags := binary.LittleEndian.Uint32(b[8:12])
		return format.Dimension((flags >> 5) & 0b111)
	}

	t.Run("save", func(t *testing.T) {
		dir := t.TempDir()
		p, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range dims {
			if err := p.StoreColumn(world.ChunkPos{0, 0}, d.dim, testColumn(t, reg)); err != nil {
				t.Fatal(err)
			}
		}
		if err := p.Close(); err != nil {
			t.Fatal(err)
		}
		for _, d := range dims {
			if got := headerDimension(t, filepath.Join(dir, d.file)); got != d.want {
				t.Errorf("%s: header says %v, want %v", d.file, got, d.want)
			}
		}
	})

	t.Run("append", func(t *testing.T) {
		dir := t.TempDir()
		p, err := Open(dir, AppendMode())
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range dims {
			if err := p.StoreColumn(world.ChunkPos{0, 0}, d.dim, testColumn(t, reg)); err != nil {
				t.Fatal(err)
			}
		}
		if err := p.Close(); err != nil {
			t.Fatal(err)
		}
		for _, d := range dims {
			if got := headerDimension(t, filepath.Join(dir, d.file)); got != d.want {
				t.Errorf("%s: header says %v, want %v", d.file, got, d.want)
			}
		}
	})

	t.Run("offline rewrite", func(t *testing.T) {
		dir := t.TempDir()
		p, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range dims {
			if err := p.StoreColumn(world.ChunkPos{0, 0}, d.dim, testColumn(t, reg)); err != nil {
				t.Fatal(err)
			}
		}
		if err := p.Close(); err != nil {
			t.Fatal(err)
		}
		wf, err := LoadWorldFiles(dir, reg)
		if err != nil {
			t.Fatal(err)
		}
		if err := wf.Write(dir, reg); err != nil {
			t.Fatal(err)
		}
		for _, d := range dims {
			if got := headerDimension(t, filepath.Join(dir, d.file)); got != d.want {
				t.Errorf("%s: header says %v, want %v", d.file, got, d.want)
			}
		}
	})
}
