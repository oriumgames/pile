package pile

import (
	"bytes"
	"encoding/binary"
	"github.com/df-mc/dragonfly/server/world/chunk"
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

// TestRejectedAppendStoreLeavesNoState: the indexed writer winds its palettes
// back when a store fails, and the provider has to match. A rejected call that
// published its sidecar into the cache would hand it to the next ordinary
// store, whose nil sidecar should have inherited what is on disk, so two
// providers with identical files and identical successful calls would write
// different bytes because one saw a rejected call in between.
func TestRejectedAppendStoreLeavesNoState(t *testing.T) {
	reg := testRegistry(t)
	run := func(offerRejected bool) []byte {
		dir := t.TempDir()
		p, err := Open(dir, AppendMode())
		if err != nil {
			t.Fatal(err)
		}
		pos := world.ChunkPos{0, 0}
		if err := p.StoreColumn(pos, world.Overworld, testColumn(t, reg)); err != nil {
			t.Fatal(err)
		}
		if offerRejected {
			// A column the indexed writer refuses, carrying an explicit
			// sidecar the provider would otherwise remember.
			c := testColumn(t, reg)
			for i := range 6 {
				c.Entities = append(c.Entities, chunk.Entity{
					ID:   int64(2000 + i),
					Data: map[string]any{"identifier": "minecraft:cow", "pad": make([]byte, 12<<20)},
				})
			}
			// The sidecar has to be observable: a preserved state only
			// reaches the palette when an entry references it, so the entry
			// comes too, aimed at a position holding the placeholder.
			side := &sidecar{
				states: []format.BlockState{{Name: "audit:poison", Version: 1}},
				unknown: []format.UnknownBlock{{
					Section: -4, Layer: 0, Index: 0, State: 0,
				}},
			}
			if err := p.storeColumn(pos, world.Overworld, c, side); err == nil {
				t.Fatal("the oversized column was accepted")
			}
		}
		// The placeholder is what a preserved state attaches to, so the final
		// column carries one at the position the sidecar names.
		final := testColumn(t, reg)
		info, ok := reg.StateToRuntimeID("minecraft:info_update", map[string]any{})
		if !ok {
			t.Skip("this registry has no placeholder block")
		}
		final.Chunk.SetBlock(0, -64, 0, 0, info)
		if err := p.StoreColumn(pos, world.Overworld, final); err != nil {
			t.Fatal(err)
		}
		if err := p.Close(); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(DimPath(dir, world.Overworld))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	poisoned, clean := run(true), run(false)
	if !bytes.Equal(poisoned, clean) {
		t.Fatalf("a rejected store changed what a later successful one wrote: %d vs %d bytes",
			len(poisoned), len(clean))
	}
}

// TestSidecarPublishRechecksRecord: a sidecar is decoded outside the lock, so
// a store can replace the record before it is published. Publishing anyway
// puts a stale sidecar where the newer one belongs, and the next store with no
// explicit sidecar inherits it. The interleaving that produces this cannot be
// staged deterministically through the public API, so the recheck itself is
// driven with an identity that is deliberately out of date.
func TestSidecarPublishRechecksRecord(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	p, err := Open(dir, AppendMode())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	pos := world.ChunkPos{0, 0}
	if err := p.StoreColumn(pos, world.Overworld, testColumn(t, reg)); err != nil {
		t.Fatal(err)
	}
	ds := p.dim(world.Overworld)
	key := [2]int32{0, 0}
	id, ok := ds.iw.RecordID(0, 0)
	if !ok {
		t.Fatal("no record to identify")
	}

	stale := chunkMeta{side: sidecar{states: []format.BlockState{{Name: "audit:stale", Version: 1}}}}
	p.mu.Lock()
	ds.meta.Drop(key)
	p.mu.Unlock()

	// An identity no record has: the decode this stands for was superseded.
	p.publishMeta(ds, ds.iw, key, id+1, stale)
	p.mu.Lock()
	_, published := ds.meta.Get(key)
	p.mu.Unlock()
	if published {
		t.Fatal("a sidecar from a superseded record was published")
	}

	// The current identity does publish, so the recheck is not simply
	// refusing everything.
	p.publishMeta(ds, ds.iw, key, id, stale)
	p.mu.Lock()
	got, published := ds.meta.Get(key)
	p.mu.Unlock()
	if !published || len(got.side.states) != 1 || got.side.states[0].Name != "audit:stale" {
		t.Fatalf("a current sidecar was not published: %v %v", published, got.side.states)
	}
}

// TestChunkMetaCacheBounded: the per-chunk user data and preserved-state
// sidecars a provider remembers between stores used to live in two maps with
// no delete anywhere in the package, so a position touched once was held for
// the life of the provider. Append mode exists to keep memory at
// directory-plus-palette level, which that quietly broke.
func TestChunkMetaCacheBounded(t *testing.T) {
	reg := testRegistry(t)
	p, err := Open(t.TempDir(), AppendMode())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	col := testColumn(t, reg)
	const n = 400
	for i := range int32(n) {
		if err := p.StoreColumn(world.ChunkPos{i, i}, world.Overworld, col); err != nil {
			t.Fatal(err)
		}
	}
	p.mu.Lock()
	held := p.dim(world.Overworld).meta.Len()
	p.mu.Unlock()
	if held > metaCacheColumns {
		t.Fatalf("%d positions of metadata retained after %d stores, bound is %d", held, n, metaCacheColumns)
	}

	// Eviction must cost correctness nothing: a column whose metadata was
	// dropped still keeps its user data across an overwrite, because the
	// previous record is re-read.
	pos := world.ChunkPos{0, 0}
	if err := p.SetChunkUserData(pos, world.Overworld, []byte("keep-me")); err != nil {
		t.Fatal(err)
	}
	for i := range int32(n) {
		if err := p.StoreColumn(world.ChunkPos{i + n, i}, world.Overworld, col); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.StoreColumn(pos, world.Overworld, col); err != nil {
		t.Fatal(err)
	}
	if got := p.ChunkUserData(pos, world.Overworld); !bytes.Equal(got, []byte("keep-me")) {
		t.Fatalf("user data lost across an eviction: %q", got)
	}
}

// TestChunkMetaCacheWeighed: one enormous entry must not keep the count bound
// meaningful while the memory bound is not. A chunk's user data blob can be
// megabytes, so the cache is weighed as well as counted.
func TestChunkMetaCacheWeighed(t *testing.T) {
	c := newMetaCache(0)
	big := make([]byte, metaCacheBytes/4)
	for i := range 8 {
		c.Put([2]int32{int32(i), 0}, chunkMeta{ud: big})
	}
	if c.Weight() > metaCacheBytes {
		t.Fatalf("cache holds %d bytes, budget is %d", c.Weight(), metaCacheBytes)
	}
	if c.Len() >= 8 {
		t.Fatalf("byte budget evicted nothing: %d entries", c.Len())
	}
	// The most recent entry survives: a caller gets back what it just put in.
	if _, ok := c.Get([2]int32{7, 0}); !ok {
		t.Fatal("the entry just stored was evicted")
	}
}

// TestStagingRefusesAnExistingPath: every path that stages a file creates it
// exclusively. os.Create follows a symlink at the target, so in a
// world-readable directory another user can pre-create the predictable ".tmp"
// name pointing anywhere this process can write.
func TestStagingRefusesAnExistingPath(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "staged.tmp")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("this filesystem has no symlinks: %v", err)
	}
	f, err := createExclusive(link)
	if err != nil {
		t.Fatalf("staging over a symlink failed for the wrong reason: %v", err)
	}
	_, _ = f.WriteString("staged")
	_ = f.Close()
	// The symlink was replaced, not followed: the target is untouched.
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "original" {
		t.Fatalf("the symlink target was written through: %q", got)
	}
}

// TestColumnsAppendLazy: an append-mode iteration must decode one record at a
// time. It used to decode and retain every column in the dimension before
// yielding the first, which is what WorldBounds pays to compute a bounding box
// it consumes one column at a time, and what any caller that stops early pays
// for columns it never looks at.
func TestColumnsAppendLazy(t *testing.T) {
	reg := testRegistry(t)
	p, err := Open(t.TempDir(), AppendMode())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	col := testColumn(t, reg)
	const n = 64
	for i := range int32(n) {
		if err := p.StoreColumn(world.ChunkPos{i, 0}, world.Overworld, col); err != nil {
			t.Fatal(err)
		}
	}

	seen := 0
	for range p.Columns(world.Overworld) {
		seen++
	}
	if seen != n {
		t.Fatalf("full iteration yielded %d columns, want %d", seen, n)
	}
	if err := p.IterError(); err != nil {
		t.Fatal(err)
	}

	first := testing.AllocsPerRun(3, func() {
		for range p.Columns(world.Overworld) {
			break
		}
	})
	full := testing.AllocsPerRun(3, func() {
		for range p.Columns(world.Overworld) {
		}
	})
	// Stopping after one column must not cost anything like the whole
	// dimension. The eager version made these two identical.
	if first > full/8 {
		t.Fatalf("stopping after one column allocated %.0f, a full iteration %.0f", first, full)
	}
	if err := p.IterError(); err != nil {
		t.Fatal(err)
	}
}
