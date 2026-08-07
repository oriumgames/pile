package pile

import (
	"path/filepath"
	"strconv"
	"testing"

	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
)

// benchSizes are the world sizes the provider scaling benchmarks cover:
// a single column (fixed overhead), a small arena, and a big world.
var benchSizes = []int{1, 100, 10000}

// benchStore fills a provider with n columns laid out as a 100-wide strip.
func benchStore(b *testing.B, p *Provider, n int) {
	b.Helper()
	reg := testRegistry(b)
	for i := range int32(n) {
		if err := p.StoreColumn(world.ChunkPos{i % 100, i / 100}, world.Overworld, scaleColumn(reg, i)); err != nil {
			b.Fatal(err)
		}
	}
}

// benchWorldDir writes an n-column world into a fresh directory and returns
// its path. opts configure the provider that writes it.
func benchWorldDir(b *testing.B, n int, opts ...Option) string {
	b.Helper()
	dir := b.TempDir()
	p, err := Open(dir, opts...)
	if err != nil {
		b.Fatal(err)
	}
	benchStore(b, p, n)
	if err := p.Close(); err != nil {
		b.Fatal(err)
	}
	return dir
}

// benchArena builds an in-memory 4x4-chunk world of stone, the source for the
// structure benchmarks.
func benchArena(b *testing.B) *Provider {
	b.Helper()
	reg := testRegistry(b)
	p := NewMemory()
	for x := range int32(4) {
		for z := range int32(4) {
			if err := p.StoreColumn(world.ChunkPos{x, z}, world.Overworld, scaleColumn(reg, x*4+z)); err != nil {
				b.Fatal(err)
			}
		}
	}
	return p
}

// benchRegion is the 64x64x64 block box the structure benchmarks work on.
var benchRegionLo, benchRegionHi = cube.Pos{0, -64, 0}, cube.Pos{63, -1, 63}

// benchExtract extracts benchRegion from a fresh arena.
func benchExtract(b *testing.B) *Structure {
	b.Helper()
	p := benchArena(b)
	defer p.Close()
	s, err := ExtractStructure(p, world.Overworld, benchRegionLo, benchRegionHi)
	if err != nil {
		b.Fatal(err)
	}
	return s
}

// sink keeps results of benchmarks whose work is otherwise dead.
var sink *chunk.Column

// BenchmarkProviderStoreColumn measures the dragonfly Provider write path for
// a solid world: deep copy, safe compaction, filters, map insert. Every chunk
// the server unloads pays this.
func BenchmarkProviderStoreColumn(b *testing.B) {
	b.ReportAllocs()
	reg := testRegistry(b)
	p := NewMemory()
	defer p.Close()
	col := testColumn(b, reg)
	b.ResetTimer()
	for b.Loop() {
		if err := p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, col); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkProviderLoadColumn measures the dragonfly Provider read path for a
// solid world: the column is already in memory, so this is the deep copy the
// isolation guarantee costs.
func BenchmarkProviderLoadColumn(b *testing.B) {
	b.ReportAllocs()
	reg := testRegistry(b)
	p := NewMemory()
	defer p.Close()
	if err := p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, testColumn(b, reg)); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for b.Loop() {
		col, err := p.LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
		if err != nil {
			b.Fatal(err)
		}
		sink = col
	}
}

// BenchmarkProviderStoreColumnAppend measures the same write path in append
// mode, where the column is encoded and appended to the file immediately
// instead of being parked in a map.
func BenchmarkProviderStoreColumnAppend(b *testing.B) {
	b.ReportAllocs()
	reg := testRegistry(b)
	p, err := Open(b.TempDir(), AppendMode())
	if err != nil {
		b.Fatal(err)
	}
	defer p.Close()
	col := testColumn(b, reg)
	b.ResetTimer()
	for b.Loop() {
		if err := p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, col); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkProviderLoadColumnCached measures an append-mode load that hits the
// column LRU: no frame decode, only the copy handed to the caller.
func BenchmarkProviderLoadColumnCached(b *testing.B) {
	b.ReportAllocs()
	dir := benchWorldDir(b, 16, AppendMode())
	p, err := Open(dir, AppendMode(), CacheColumns(64))
	if err != nil {
		b.Fatal(err)
	}
	defer p.Close()
	if _, err := p.LoadColumn(world.ChunkPos{0, 0}, world.Overworld); err != nil {
		b.Fatal(err) // prime the cache
	}
	b.ResetTimer()
	for b.Loop() {
		col, err := p.LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
		if err != nil {
			b.Fatal(err)
		}
		sink = col
	}
}

// BenchmarkProviderLoadColumnUncached measures the same load with caching off,
// so every call decodes the record from the file. The gap against
// BenchmarkProviderLoadColumnCached is what CacheColumns buys.
func BenchmarkProviderLoadColumnUncached(b *testing.B) {
	b.ReportAllocs()
	dir := benchWorldDir(b, 16, AppendMode())
	p, err := Open(dir, AppendMode(), CacheColumns(0))
	if err != nil {
		b.Fatal(err)
	}
	defer p.Close()
	b.ResetTimer()
	for b.Loop() {
		col, err := p.LoadColumn(world.ChunkPos{0, 0}, world.Overworld)
		if err != nil {
			b.Fatal(err)
		}
		sink = col
	}
}

// BenchmarkProviderSave measures a full solid save across world sizes: encode
// every column, write a temp file, fsync, rename, fsync the directory.
func BenchmarkProviderSave(b *testing.B) {
	reg := testRegistry(b)
	for _, n := range benchSizes {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			b.ReportAllocs()
			p, err := Open(b.TempDir())
			if err != nil {
				b.Fatal(err)
			}
			defer p.Close()
			benchStore(b, p, n)
			b.ResetTimer()
			for b.Loop() {
				// A save with nothing dirty is a no-op, so dirty one column.
				b.StopTimer()
				if err := p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, scaleColumn(reg, 0)); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				if err := p.Save(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkProviderOpen measures loading a solid world from disk across world
// sizes: the whole file is read and every column decoded up front.
func BenchmarkProviderOpen(b *testing.B) {
	for _, n := range benchSizes {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			b.ReportAllocs()
			dir := benchWorldDir(b, n)
			b.ResetTimer()
			for b.Loop() {
				p, err := Open(dir, ReadOnly())
				if err != nil {
					b.Fatal(err)
				}
				if err := p.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkProviderOpenAppend measures the append-mode open: the directory and
// palettes are read, chunk data is not. Compare against BenchmarkProviderOpen
// at the same size to see what append mode saves at startup.
func BenchmarkProviderOpenAppend(b *testing.B) {
	for _, n := range benchSizes {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			b.ReportAllocs()
			dir := benchWorldDir(b, n, AppendMode())
			b.ResetTimer()
			for b.Loop() {
				p, err := Open(dir, AppendMode(), ReadOnly())
				if err != nil {
					b.Fatal(err)
				}
				if err := p.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkProviderSaveAppend measures an append-mode save: a checkpoint over
// an already-appended world.
//
// Its allocation count is what does not scale — 25 at a hundred columns and 40
// at ten thousand. Its bytes do, because a checkpoint rewrites the directory,
// at about 66 B per column; the comment here used to claim the whole operation
// was flat in the world size, which the recorded baseline does not support.
// PERFORMANCE.md, "Save latency".
func BenchmarkProviderSaveAppend(b *testing.B) {
	reg := testRegistry(b)
	for _, n := range []int{100, 10000} {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			b.ReportAllocs()
			p, err := Open(b.TempDir(), AppendMode())
			if err != nil {
				b.Fatal(err)
			}
			defer p.Close()
			benchStore(b, p, n)
			b.ResetTimer()
			for b.Loop() {
				b.StopTimer()
				if err := p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, scaleColumn(reg, 0)); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				if err := p.Save(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkProviderColumns measures iterating a dimension, the path offline
// tooling (conversion, export, backup) takes. Every column is deep copied.
func BenchmarkProviderColumns(b *testing.B) {
	b.ReportAllocs()
	p := NewMemory()
	defer p.Close()
	benchStore(b, p, 100)
	b.ResetTimer()
	for b.Loop() {
		n := 0
		for _, col := range p.Columns(world.Overworld) {
			sink = col
			n++
		}
		if n != 100 {
			b.Fatalf("iterated %d columns, want 100", n)
		}
	}
}

// BenchmarkCloneColumn measures the deep copy alone. Both StoreColumn and
// LoadColumn pay it once per call, so it bounds how cheap they can ever be.
func BenchmarkCloneColumn(b *testing.B) {
	b.ReportAllocs()
	reg := testRegistry(b)
	col := testColumn(b, reg)
	b.ResetTimer()
	for b.Loop() {
		sink = cloneColumn(col)
	}
}

// BenchmarkStructureExtract measures copying a 64x64x64 region out of a world
// into a structure: the capture half of the template workflow.
func BenchmarkStructureExtract(b *testing.B) {
	b.ReportAllocs()
	p := benchArena(b)
	defer p.Close()
	b.ResetTimer()
	for b.Loop() {
		if _, err := ExtractStructure(p, world.Overworld, benchRegionLo, benchRegionHi); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkStructurePaste measures writing that structure back into a world,
// the operation an arena reset performs between rounds.
func BenchmarkStructurePaste(b *testing.B) {
	b.ReportAllocs()
	s := benchExtract(b)
	dst := NewMemory()
	defer dst.Close()
	b.ResetTimer()
	for b.Loop() {
		if err := s.PasteInto(dst, world.Overworld, cube.Pos{0, 0, 0}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkStructureRotate measures the block-state aware rotation, which
// rewrites every cell and re-resolves rotatable states.
func BenchmarkStructureRotate(b *testing.B) {
	b.ReportAllocs()
	s := benchExtract(b)
	b.ResetTimer()
	for b.Loop() {
		if s.Rotate(1) == nil {
			b.Fatal("rotate returned nil")
		}
	}
}

// BenchmarkStructureSave measures encoding a structure to disk.
func BenchmarkStructureSave(b *testing.B) {
	b.ReportAllocs()
	s := benchExtract(b)
	path := filepath.Join(b.TempDir(), "bench.pile")
	b.ResetTimer()
	for b.Loop() {
		if err := s.Save(path); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkStructureLoad measures reading a structure back, the cost a
// StructureLibrary pays per entry at startup.
func BenchmarkStructureLoad(b *testing.B) {
	b.ReportAllocs()
	s := benchExtract(b)
	path := filepath.Join(b.TempDir(), "bench.pile")
	if err := s.Save(path); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for b.Loop() {
		if _, err := LoadStructure(path); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkMoveWorldFast measures the chunk-aligned move: columns are re-keyed
// and the files rewritten, but no block is touched.
func BenchmarkMoveWorldFast(b *testing.B) {
	b.ReportAllocs()
	dir := benchWorldDir(b, 100)
	b.ResetTimer()
	for b.Loop() {
		if _, err := MoveWorld(dir, MoveOptions{Offset: cube.Pos{16, 0, 0}}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkMoveWorldSlow measures the general move: the offset straddles chunk
// borders, so every block, biome and entry is read and rewritten into freshly
// allocated destination columns.
func BenchmarkMoveWorldSlow(b *testing.B) {
	b.ReportAllocs()
	dir := benchWorldDir(b, 100)
	b.ResetTimer()
	for b.Loop() {
		if _, err := MoveWorld(dir, MoveOptions{Offset: cube.Pos{1, 0, 1}}); err != nil {
			b.Fatal(err)
		}
	}
}

// benchAppendWorld builds an append-mode world holding n columns and returns a
// freshly opened provider over it.
func benchAppendWorld(b *testing.B, n int, opts ...Option) *Provider {
	b.Helper()
	reg := testRegistry(b)
	dir := b.TempDir()
	p, err := Open(dir, append([]Option{AppendMode()}, opts...)...)
	if err != nil {
		b.Fatal(err)
	}
	col := testColumn(b, reg)
	for i := range int32(n) {
		if err := p.StoreColumn(world.ChunkPos{i, 0}, world.Overworld, col); err != nil {
			b.Fatal(err)
		}
	}
	if err := p.Save(); err != nil {
		b.Fatal(err)
	}
	return p
}

// BenchmarkColumnsFirst measures what an append-mode Columns iteration costs to
// produce its first result. Callers that stop early (a bounding box that has
// seen enough, a search) must not pay for the whole dimension.
func BenchmarkColumnsFirst(b *testing.B) {
	b.ReportAllocs()
	p := benchAppendWorld(b, 64)
	defer p.Close()
	b.ResetTimer()
	for b.Loop() {
		for range p.Columns(world.Overworld) {
			break
		}
		if err := p.IterError(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkColumnsAll measures a full append-mode iteration, which must not
// regress when the first result gets cheaper.
func BenchmarkColumnsAll(b *testing.B) {
	b.ReportAllocs()
	p := benchAppendWorld(b, 64)
	defer p.Close()
	b.ResetTimer()
	for b.Loop() {
		n := 0
		for range p.Columns(world.Overworld) {
			n++
		}
		if err := p.IterError(); err != nil {
			b.Fatal(err)
		}
		if n != 64 {
			b.Fatalf("iterated %d columns", n)
		}
	}
}

// BenchmarkStoreAppendSpread stores across a wide spread of positions. The
// per-chunk metadata the provider keeps between stores is bounded, so the
// resident cost must not grow with the number of positions ever touched.
func BenchmarkStoreAppendSpread(b *testing.B) {
	b.ReportAllocs()
	reg := testRegistry(b)
	dir := b.TempDir()
	p, err := Open(dir, AppendMode())
	if err != nil {
		b.Fatal(err)
	}
	defer p.Close()
	col := testColumn(b, reg)
	x := int32(0)
	b.ResetTimer()
	for b.Loop() {
		if err := p.StoreColumn(world.ChunkPos{x, x}, world.Overworld, col); err != nil {
			b.Fatal(err)
		}
		x++
	}
}
