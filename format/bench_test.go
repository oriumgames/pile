package format

import (
	"bytes"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/df-mc/dragonfly/server/block"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
)

// benchWorld builds a 64-chunk world with realistic content.
func benchWorld(b *testing.B) *WorldData {
	reg := testRegistry(b)
	d := &WorldData{}
	for x := range int32(8) {
		for z := range int32(8) {
			d.Columns = append(d.Columns, buildTestColumn(b, reg, x, z))
		}
	}
	return d
}

// benchSizes are the world sizes the scaling benchmarks cover: a single column
// (fixed overhead), a small arena, and a big world.
var benchSizes = []int{1, 100, 10000}

var (
	benchWorldMu    sync.Mutex
	benchWorldCache = map[int]*WorldData{}
)

// benchWorldN builds an n-column world laid out as a 100-wide strip. Fixtures
// are cached per size because building 10000 columns costs seconds; neither
// encoding nor decoding mutates a WorldData, so sharing one is safe.
func benchWorldN(b *testing.B, n int) *WorldData {
	b.Helper()
	benchWorldMu.Lock()
	defer benchWorldMu.Unlock()
	if d, ok := benchWorldCache[n]; ok {
		return d
	}
	reg := testRegistry(b)
	dirt := rid(b, reg, block.Dirt{})
	d := &WorldData{Columns: make([]Column, 0, n)}
	for i := range int32(n) {
		c := buildTestColumn(b, reg, i%100, i/100)
		// A per-column variation, so the encoder's record dedup does not
		// collapse the whole world into one record and hide the real cost.
		c.Col.Chunk.SetBlock(uint8(i&15), int16(-20), uint8((i>>4)&15), 0, dirt)
		d.Columns = append(d.Columns, c)
	}
	benchWorldCache[n] = d
	return d
}

// benchStructure builds a 48 cubed structure: every cell half filled with
// stone, plus a second layer, block entities and entities.
func benchStructure(b *testing.B, reg world.BlockRegistry) *StructureData {
	b.Helper()
	size := [3]int32{48, 48, 48}
	data, err := NewStructureData(size)
	if err != nil {
		b.Fatal(err)
	}
	stone, dirt := rid(b, reg, block.Stone{}), rid(b, reg, block.Dirt{})
	air := reg.AirRuntimeID()
	nx, ny, nz := CellDims(size)
	for cx := range nx {
		for cy := range ny {
			for cz := range nz {
				cell := chunk.NewSubChunk(air)
				for bx := range uint8(16) {
					for bz := range uint8(16) {
						for by := range uint8(8) {
							cell.SetBlock(bx, by, bz, 0, stone)
						}
					}
				}
				cell.SetBlock(3, 5, 7, 1, dirt)
				data.Cells[CellIndex(size, cx, cy, cz)] = cell
			}
		}
	}
	for i := range int32(16) {
		data.BlockEntities = append(data.BlockEntities, StructureBlockEntity{
			Pos: [3]int32{i, 4, i},
			Data: map[string]any{
				"id": "minecraft:chest", "CustomName": "loot",
				"x": i, "y": int32(4), "z": i,
			},
		})
		data.Entities = append(data.Entities, map[string]any{
			"identifier": "minecraft:zombie",
			"Pos":        []any{float32(i) + 0.5, float32(5), float32(i) + 0.5},
			"UniqueID":   int64(i),
		})
	}
	return data
}

// benchIndexed creates an indexed world holding n distinct columns, half of
// which are overwritten once so the file carries garbage.
func benchIndexed(b *testing.B, path string, reg world.BlockRegistry, n int) *IndexedWorld {
	b.Helper()
	w, err := CreateIndexed(path, reg, Options{Compression: CompressionDefault})
	if err != nil {
		b.Fatal(err)
	}
	src := benchWorldN(b, n)
	for _, c := range src.Columns {
		if err := w.Store(c); err != nil {
			b.Fatal(err)
		}
	}
	for _, c := range src.Columns[:n/2] {
		if err := w.Store(c); err != nil {
			b.Fatal(err)
		}
	}
	if err := w.Checkpoint(); err != nil {
		b.Fatal(err)
	}
	return w
}

func BenchmarkWriteWorld(b *testing.B) {
	b.ReportAllocs()
	reg := testRegistry(b)
	d := benchWorld(b)
	b.ResetTimer()
	for b.Loop() {
		var buf bytes.Buffer
		if err := WriteWorld(&buf, d, reg, Options{Compression: CompressionDefault}); err != nil {
			b.Fatal(err)
		}
		b.SetBytes(int64(buf.Len()))
	}
}

func BenchmarkWriteWorldFast(b *testing.B) {
	b.ReportAllocs()
	reg := testRegistry(b)
	d := benchWorld(b)
	b.ResetTimer()
	for b.Loop() {
		var buf bytes.Buffer
		if err := WriteWorld(&buf, d, reg, Options{Compression: CompressionDefault, FastCompression: true}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkReadWorld(b *testing.B) {
	b.ReportAllocs()
	reg := testRegistry(b)
	d := benchWorld(b)
	var buf bytes.Buffer
	if err := WriteWorld(&buf, d, reg, Options{Compression: CompressionDefault}); err != nil {
		b.Fatal(err)
	}
	file := buf.Bytes()
	b.SetBytes(int64(len(file)))
	b.ResetTimer()
	for b.Loop() {
		if _, err := ReadWorld(file, reg); err != nil {
			b.Fatal(err)
		}
	}
}

// benchVariedWorld builds a world whose sections almost all differ, so the
// blob table cannot deduplicate them away and reading one exercises the
// duplicate check over a table of realistic size.
func benchVariedWorld(b *testing.B) *WorldData {
	b.Helper()
	reg := testRegistry(b)
	stone := reg.BlockRuntimeID(block.Stone{})
	dirt := reg.BlockRuntimeID(block.Dirt{})
	d := &WorldData{}
	seed := uint32(12345)
	next := func() uint32 {
		seed = seed*1664525 + 1013904223
		return seed >> 16
	}
	for x := range int32(4) {
		for z := range int32(4) {
			ch := chunk.New(reg, overworldRange)
			for y := int16(-64); y < 64; y++ {
				for bx := range uint8(16) {
					for bz := range uint8(16) {
						if next()%2 == 0 {
							ch.SetBlock(bx, y, bz, 0, stone)
						} else {
							ch.SetBlock(bx, y, bz, 0, dirt)
						}
					}
				}
			}
			d.Columns = append(d.Columns, Column{X: x, Z: z, Col: &chunk.Column{Chunk: ch}})
		}
	}
	return d
}

// BenchmarkReadWorldVaried reads a world with a large blob table. Duplicate
// detection there must not cost a copy of the table.
func BenchmarkReadWorldVaried(b *testing.B) {
	reg := testRegistry(b)
	d := benchVariedWorld(b)
	var buf bytes.Buffer
	if err := WriteWorld(&buf, d, reg, Options{Compression: CompressionDefault}); err != nil {
		b.Fatal(err)
	}
	file := buf.Bytes()
	b.SetBytes(int64(len(file)))
	b.ResetTimer()
	for b.Loop() {
		if _, err := ReadWorld(file, reg); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkIndexedStore(b *testing.B) {
	b.ReportAllocs()
	reg := testRegistry(b)
	path := filepath.Join(b.TempDir(), "bench.pile")
	w, err := CreateIndexed(path, reg, Options{Compression: CompressionDefault})
	if err != nil {
		b.Fatal(err)
	}
	defer w.Close()
	col := buildTestColumn(b, reg, 0, 0)
	b.ResetTimer()
	for b.Loop() {
		if err := w.Store(col); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkIndexedStoreLargePalette stores a column that introduces no new
// palette entries into a world whose global palette is already large. The
// per-Store cost must depend on what the column adds, not on how big the
// palette already is.
func BenchmarkIndexedStoreLargePalette(b *testing.B) {
	reg := testRegistry(b)
	path := filepath.Join(b.TempDir(), "bench.pile")
	w, err := CreateIndexed(path, reg, Options{Compression: CompressionDefault})
	if err != nil {
		b.Fatal(err)
	}
	defer w.Close()
	if err := w.Store(paletteFillColumn(b, reg)); err != nil {
		b.Fatal(err)
	}
	if got := len(w.rids); got < 4096 {
		b.Fatalf("palette only grew to %d entries", got)
	}
	col := buildTestColumn(b, reg, 0, 0)
	b.ResetTimer()
	for b.Loop() {
		if err := w.Store(col); err != nil {
			b.Fatal(err)
		}
	}
}

// paletteFillColumn builds a column referencing every runtime ID the registry
// knows, so storing it grows the global palette to the registry's full size.
func paletteFillColumn(b testing.TB, reg world.BlockRegistry) Column {
	b.Helper()
	ch := chunk.New(reg, overworldRange)
	next := uint32(0)
	for y := int16(-64); y < 320 && next < 1<<20; y++ {
		for bx := range uint8(16) {
			for bz := range uint8(16) {
				if _, _, ok := reg.RuntimeIDToState(next); !ok {
					next = 1 << 20
					break
				}
				ch.SetBlock(bx, y, bz, 0, next)
				next++
			}
		}
	}
	return Column{X: 1, Z: 1, Col: &chunk.Column{Chunk: ch}}
}

func BenchmarkIndexedColumn(b *testing.B) {
	b.ReportAllocs()
	reg := testRegistry(b)
	path := filepath.Join(b.TempDir(), "bench.pile")
	w, err := CreateIndexed(path, reg, Options{Compression: CompressionDefault})
	if err != nil {
		b.Fatal(err)
	}
	defer w.Close()
	if err := w.Store(buildTestColumn(b, reg, 0, 0)); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for b.Loop() {
		if _, err := w.Column(0, 0); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWriteWorldSize measures solid encoding across world sizes. Compared
// against each other the three points show whether encoding is linear in the
// column count or carries a super-linear term.
func BenchmarkWriteWorldSize(b *testing.B) {
	reg := testRegistry(b)
	for _, n := range benchSizes {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			b.ReportAllocs()
			d := benchWorldN(b, n)
			b.ResetTimer()
			for b.Loop() {
				var buf bytes.Buffer
				if err := WriteWorld(&buf, d, reg, Options{Compression: CompressionDefault}); err != nil {
					b.Fatal(err)
				}
				b.SetBytes(int64(buf.Len()))
			}
		})
	}
}

// BenchmarkReadWorldSize measures solid decoding across world sizes: the load
// path a server pays on every Open of a solid world.
func BenchmarkReadWorldSize(b *testing.B) {
	reg := testRegistry(b)
	for _, n := range benchSizes {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			b.ReportAllocs()
			var buf bytes.Buffer
			if err := WriteWorld(&buf, benchWorldN(b, n), reg, Options{Compression: CompressionDefault}); err != nil {
				b.Fatal(err)
			}
			file := buf.Bytes()
			b.SetBytes(int64(len(file)))
			b.ResetTimer()
			for b.Loop() {
				if _, err := ReadWorld(file, reg); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkWriteWorldCompression separates the zstd cost from the encoding
// cost. The provider defaults to CompressionBest, which no other benchmark
// covers.
func BenchmarkWriteWorldCompression(b *testing.B) {
	reg := testRegistry(b)
	levels := []struct {
		name  string
		level CompressionLevel
	}{
		{"none", CompressionNone},
		{"fast", CompressionFast},
		{"default", CompressionDefault},
		{"best", CompressionBest},
	}
	d := benchWorld(b)
	for _, l := range levels {
		b.Run(l.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				var buf bytes.Buffer
				if err := WriteWorld(&buf, d, reg, Options{Compression: l.level}); err != nil {
					b.Fatal(err)
				}
				b.SetBytes(int64(buf.Len()))
			}
		})
	}
}

// BenchmarkReadMeta measures the header-only read that tooling uses to inspect
// a world without decoding any chunk data.
func BenchmarkReadMeta(b *testing.B) {
	b.ReportAllocs()
	reg := testRegistry(b)
	var buf bytes.Buffer
	if err := WriteWorld(&buf, benchWorldN(b, 100), reg, Options{Compression: CompressionDefault, Stats: true}); err != nil {
		b.Fatal(err)
	}
	file := buf.Bytes()
	b.ResetTimer()
	for b.Loop() {
		if _, err := ReadMeta(file); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWriteStructure measures structure encoding, the save half of the
// template/library workflow.
func BenchmarkWriteStructure(b *testing.B) {
	b.ReportAllocs()
	reg := testRegistry(b)
	s := benchStructure(b, reg)
	b.ResetTimer()
	for b.Loop() {
		var buf bytes.Buffer
		if err := WriteStructure(&buf, s, reg, Options{Compression: CompressionDefault}); err != nil {
			b.Fatal(err)
		}
		b.SetBytes(int64(buf.Len()))
	}
}

// BenchmarkReadStructure measures structure decoding, paid every time a
// template is loaded from a library.
func BenchmarkReadStructure(b *testing.B) {
	b.ReportAllocs()
	reg := testRegistry(b)
	var buf bytes.Buffer
	if err := WriteStructure(&buf, benchStructure(b, reg), reg, Options{Compression: CompressionDefault}); err != nil {
		b.Fatal(err)
	}
	file := buf.Bytes()
	b.SetBytes(int64(len(file)))
	b.ResetTimer()
	for b.Loop() {
		if _, err := ReadStructure(file, reg); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkIndexedStoreDistinct appends columns at distinct positions, so the
// record directory grows instead of one key being overwritten in place.
func BenchmarkIndexedStoreDistinct(b *testing.B) {
	b.ReportAllocs()
	reg := testRegistry(b)
	path := filepath.Join(b.TempDir(), "bench.pile")
	w, err := CreateIndexed(path, reg, Options{Compression: CompressionDefault})
	if err != nil {
		b.Fatal(err)
	}
	defer w.Close()
	col := buildTestColumn(b, reg, 0, 0)
	i := int32(0)
	b.ResetTimer()
	for b.Loop() {
		col.X, col.Z = i%1024, i/1024
		if err := w.Store(col); err != nil {
			b.Fatal(err)
		}
		i++
	}
}

// BenchmarkIndexedCheckpoint measures the append-mode save: one stored column
// followed by a durable checkpoint (palette segments, directory, footer,
// fsync). This is what Provider.Save costs in append mode.
func BenchmarkIndexedCheckpoint(b *testing.B) {
	b.ReportAllocs()
	reg := testRegistry(b)
	path := filepath.Join(b.TempDir(), "bench.pile")
	w := benchIndexed(b, path, reg, 100)
	defer w.Close()
	col := buildTestColumn(b, reg, 0, 0)
	i := int32(0)
	b.ResetTimer()
	for b.Loop() {
		b.StopTimer()
		col.X, col.Z = i%100, i/100
		if err := w.Store(col); err != nil {
			b.Fatal(err)
		}
		i++
		b.StartTimer()
		if err := w.Checkpoint(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkIndexedCompact measures rewriting the live set into a fresh file,
// which Close performs automatically past the garbage threshold. Only the
// first iteration sees garbage; the rest measure the live-set rewrite, which
// is the term that scales with world size.
func BenchmarkIndexedCompact(b *testing.B) {
	reg := testRegistry(b)
	for _, n := range []int{100, 1000} {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			b.ReportAllocs()
			w := benchIndexed(b, filepath.Join(b.TempDir(), "bench.pile"), reg, n)
			defer w.Close()
			b.ResetTimer()
			for b.Loop() {
				if err := w.Compact(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkOpenIndexed measures opening an existing indexed file: the
// directory and palette segments are read, chunk data is not. This is the
// append-mode equivalent of ReadWorld and should be far cheaper.
func BenchmarkOpenIndexed(b *testing.B) {
	reg := testRegistry(b)
	for _, n := range []int{100, 1000} {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			b.ReportAllocs()
			path := filepath.Join(b.TempDir(), "bench.pile")
			w := benchIndexed(b, path, reg, n)
			if err := w.Close(); err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			for b.Loop() {
				r, err := OpenIndexed(path, reg, true)
				if err != nil {
					b.Fatal(err)
				}
				if err := r.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkIndexedColumnRandom reads columns spread across a large file rather
// than the same hot record, so directory lookup and frame decode are measured
// without the page cache serving one record repeatedly.
func BenchmarkIndexedColumnRandom(b *testing.B) {
	b.ReportAllocs()
	reg := testRegistry(b)
	const n = 1000
	w := benchIndexed(b, filepath.Join(b.TempDir(), "bench.pile"), reg, n)
	defer w.Close()
	pos := w.Positions()
	i := 0
	b.ResetTimer()
	for b.Loop() {
		p := pos[i%len(pos)]
		if _, err := w.Column(p[0], p[1]); err != nil {
			b.Fatal(err)
		}
		i++
	}
}
