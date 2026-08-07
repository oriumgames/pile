package format

// The hostile-input matrix.
//
// `FREEZE.md` names three security preconditions and gives this file as the
// exit criterion for the first: no allocation is sized from unvalidated input,
// proved by "truncation at every field boundary, every count at 0, 1, max,
// max+1 — driven through ReadWorld, ReadStructure, ReadMeta and OpenIndexed".
// The other two — no integer computation wraps before its bounds check, and no
// input causes an unbounded loop — are exercised by the same inputs, since a
// wrap and a runaway loop both show up here as a panic or as a decode that
// never returns.
//
// The lesson this file is designed around is `STATUS.md`'s: the bounds
// discipline was real but bounded the wrong thing. Checking a count against
// the bytes that remain is necessary and not sufficient, because several
// values a file declares cost far more to decode than to write. So the matrix
// is not only "does it reject the file": TestHostileAllocationCeilings
// measures how many bytes a decode allocates for a file of a few kilobytes and
// fails when the ratio is wrong. Every ceiling in it was over-run before the
// fix it guards, and the numbers are in `SECURITY.md`.
//
// Nothing here may change which files are accepted. Every case therefore
// asserts on behaviour a conforming reader already had: no panic, no hang,
// an error that wraps ErrCorrupt where one is due, and a bounded allocation.

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/cespare/xxhash/v2"
	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
)

// ---------------------------------------------------------------------------
// File assembly
// ---------------------------------------------------------------------------

// hostileBlockVersion is a non-zero blockVersion, which §2.1 requires.
const hostileBlockVersion = 17959425

// hostileSeal wraps a body in a header and an authenticated footer. compress
// selects whether the body is stored as a zstd frame, which is what makes a
// large body reachable from a small file and is therefore the whole point of
// the allocation cases.
func hostileSeal(kind uint8, body []byte, compress bool) []byte {
	flags := uint32(FlagUncompressed)
	stored := body
	if compress {
		flags = 0
		stored = compressBody(body, CompressionBest, false)
	}
	hdr := &writer{}
	hdr.raw(headerMagic[:])
	hdr.u16(Version)
	hdr.u8(kind)
	hdr.u8(ModeSolid)
	hdr.u32(flags)
	hdr.i32(hostileBlockVersion)
	tail := &writer{}
	tail.u64(0)
	tail.u64(0)
	tail.u64(0)
	tail.u64(0)
	tail.raw(footerMagic[:])
	ftr := &writer{}
	ftr.u64(checkpointHash(hdr.bytes(), stored, tail.bytes()))
	ftr.raw(tail.bytes())
	out := make([]byte, 0, headerSize+len(stored)+footerSize)
	out = append(out, hdr.bytes()...)
	out = append(out, stored...)
	return append(out, ftr.bytes()...)
}

// hostileWorldBody returns the uncompressed body of a small but complete world
// file: two columns, several block states, biomes, a block entity, an entity,
// a scheduled update and per-chunk user data, so that every field the format
// defines for a solid world is present somewhere in it.
func hostileWorldBody(t testing.TB, reg world.BlockRegistry) []byte {
	t.Helper()
	stone, _ := reg.StateToRuntimeID("minecraft:stone", map[string]any{})
	water, _ := reg.StateToRuntimeID("minecraft:water", map[string]any{"liquid_depth": int32(0)})
	var cols []Column
	for _, pos := range [][2]int32{{0, 0}, {1, 0}} {
		ch := chunk.New(reg, cube.Range{0, 15})
		ch.SetBlock(0, 0, 0, 0, stone)
		ch.SetBlock(1, 0, 0, 0, water)
		ch.SetBlock(1, 0, 0, 1, water)
		ch.SetBiome(0, 0, 0, 1)
		col := &chunk.Column{Chunk: ch}
		col.BlockEntities = append(col.BlockEntities, chunk.BlockEntity{
			Pos:  cube.Pos{int(pos[0]) * 16, 0, int(pos[1]) * 16},
			Data: map[string]any{"id": "minecraft:chest"},
		})
		col.Entities = append(col.Entities, chunk.Entity{
			ID: 7, Data: map[string]any{"UniqueID": int64(7), "identifier": "minecraft:pig"},
		})
		col.ScheduledBlocks = append(col.ScheduledBlocks, chunk.ScheduledBlockUpdate{
			Pos: cube.Pos{int(pos[0]) * 16, 0, int(pos[1]) * 16}, Block: stone, Tick: 3,
		})
		cols = append(cols, Column{X: pos[0], Z: pos[1], Col: col, UserData: []byte{1, 2, 3}})
	}
	d := &WorldData{
		Columns:  cols,
		Settings: mustNBT(t, map[string]any{"name": "hostile"}),
		UserData: []byte{9},
	}
	var buf bytes.Buffer
	if err := WriteWorld(&buf, d, reg, Options{Compression: CompressionNone}); err != nil {
		t.Fatal(err)
	}
	f := buf.Bytes()
	return append([]byte(nil), f[headerSize:len(f)-footerSize]...)
}

// hostileStructureBody returns the uncompressed body of a small structure with
// a populated cell, a block entity and an entity.
func hostileStructureBody(t testing.TB, reg world.BlockRegistry) []byte {
	t.Helper()
	s, err := NewStructureData([3]int32{18, 18, 18})
	if err != nil {
		t.Fatal(err)
	}
	stone, _ := reg.StateToRuntimeID("minecraft:stone", map[string]any{})
	cell := chunk.NewSubChunk(reg.AirRuntimeID())
	cell.SetBlock(1, 2, 3, 0, stone)
	s.Cells[0] = cell
	s.BlockEntities = append(s.BlockEntities, StructureBlockEntity{
		Pos: [3]int32{1, 2, 3}, Data: map[string]any{"id": "minecraft:chest"},
	})
	s.Entities = append(s.Entities, map[string]any{"identifier": "minecraft:pig"})
	s.UserData = []byte{4, 5}
	var buf bytes.Buffer
	if err := WriteStructure(&buf, s, reg, Options{Compression: CompressionNone}); err != nil {
		t.Fatal(err)
	}
	f := buf.Bytes()
	return append([]byte(nil), f[headerSize:len(f)-footerSize]...)
}

func mustNBT(t testing.TB, m map[string]any) []byte {
	t.Helper()
	b, err := marshalNBT(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ---------------------------------------------------------------------------
// Truncation
// ---------------------------------------------------------------------------

// TestHostileTruncation cuts a complete body at every byte offset, re-seals the
// file so the cut is what the reader meets rather than the checkpoint hash, and
// drives every solid entry point over it. Cutting at every offset covers every
// field boundary by construction, and also every offset inside a field, which
// is where a length read half-way through is most likely to be believed.
//
// The requirement is only that the reader returns: no panic, no hang, and an
// error rather than a truncated success for every prefix but the whole body.
func TestHostileTruncation(t *testing.T) {
	reg := testRegistry(t)
	// whole says the reader consumes the entire body, so every proper prefix
	// of it must be refused. ReadMeta stops after the meta block and
	// UnresolvedStates after the block palette, both by design (§9), so a cut
	// past what they read is legitimately accepted and only the clean-return
	// requirement applies to them.
	cases := []struct {
		name  string
		kind  uint8
		whole bool
		body  []byte
		read  func(file []byte) error
	}{
		{"world/ReadWorld", KindWorld, true, hostileWorldBody(t, reg), func(f []byte) error {
			_, err := ReadWorld(f, reg)
			return err
		}},
		{"world/ReadMeta", KindWorld, false, hostileWorldBody(t, reg), func(f []byte) error {
			_, err := ReadMeta(f)
			return err
		}},
		{"world/UnresolvedStates", KindWorld, false, hostileWorldBody(t, reg), func(f []byte) error {
			_, err := UnresolvedStates(f, reg)
			return err
		}},
		{"structure/ReadStructure", KindStructure, true, hostileStructureBody(t, reg), func(f []byte) error {
			_, err := ReadStructure(f, reg)
			return err
		}},
		{"structure/ReadMeta", KindStructure, false, hostileStructureBody(t, reg), func(f []byte) error {
			_, err := ReadMeta(f)
			return err
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.read(hostileSeal(c.kind, c.body, false)); err != nil {
				t.Fatalf("the complete body was rejected: %v", err)
			}
			shortest := -1
			for cut := range len(c.body) {
				err := c.read(hostileSeal(c.kind, c.body[:cut], false))
				if err == nil {
					if c.whole {
						t.Fatalf("a body truncated to %d of %d bytes was accepted", cut, len(c.body))
					}
					if shortest < 0 {
						shortest = cut
					}
					continue
				}
				if shortest >= 0 {
					t.Fatalf("a body truncated to %d bytes was refused although %d bytes sufficed", cut, shortest)
				}
				requireCleanRejection(t, fmt.Sprintf("cut at %d", cut), err)
			}
			if !c.whole && shortest < 0 {
				t.Fatal("a partial reader refused every prefix, so nothing was exercised past the cut")
			}
		})
	}
}

// requireCleanRejection asserts that a rejection is a decode error and not
// something that escaped from a runtime bounds check or an allocator.
func requireCleanRejection(t *testing.T, what string, err error) {
	t.Helper()
	if errors.Is(err, ErrCorrupt) || errors.Is(err, ErrChecksum) ||
		errors.Is(err, ErrUnknownFlags) || errors.Is(err, ErrUnsupportedMode) ||
		errors.Is(err, ErrUnsupportedVersion) {
		return
	}
	// gophertunnel's NBT decoder and the block upgrader return their own error
	// types; those are still clean returns. What must never appear is a
	// runtime error, which is what a bad index or a wrapped size produces.
	if strings.HasPrefix(err.Error(), "runtime error") {
		t.Fatalf("%s: rejected by the runtime, not by the decoder: %v", what, err)
	}
}

// TestHostileIndexedTruncation truncates a real indexed file at every byte
// offset. §5.6 recovery means most prefixes must still open (from an earlier
// checkpoint or not at all), so the requirement is that OpenIndexed returns,
// and that whatever it opens can be read to the end without panicking.
func TestHostileIndexedTruncation(t *testing.T) {
	reg := testRegistry(t)
	full := hostileIndexedFile(t, reg)
	p := filepath.Join(t.TempDir(), "trunc.pile")
	for cut := range len(full) + 1 {
		if err := os.WriteFile(p, full[:cut], 0o644); err != nil {
			t.Fatal(err)
		}
		hostileOpenAndDrain(t, p, reg)
	}
}

// hostileOpenAndDrain opens a file as an indexed world and reads everything it
// exposes. Errors are expected; returning at all is the assertion.
func hostileOpenAndDrain(t *testing.T, path string, reg world.BlockRegistry) {
	t.Helper()
	w, err := OpenIndexed(path, reg, true)
	if err != nil {
		return
	}
	defer func() { _ = w.Close() }()
	for _, k := range w.Positions() {
		_, _ = w.Column(k[0], k[1])
	}
	_, _ = w.UnresolvedStates()
	w.Meta()
}

// hostileIndexedFile writes a small indexed world with two checkpoints, so
// truncating it exercises the recovery path as well as the parse.
func hostileIndexedFile(t testing.TB, reg world.BlockRegistry) []byte {
	t.Helper()
	p := filepath.Join(t.TempDir(), "src.pile")
	w, err := CreateIndexed(p, reg, Options{Compression: CompressionDefault})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Store(buildTestColumn(t, reg, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if err := w.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := w.Store(buildTestColumn(t, reg, 1, 0)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ---------------------------------------------------------------------------
// Counts at 0, 1, max and max+1
// ---------------------------------------------------------------------------

// hostileCount is one count field of the format, driven at its boundaries. The
// builder writes a body whose count field carries v and whose remaining bytes
// are whatever the builder chooses; the assertion is that the decoder returns
// cleanly for every v, and rejects max+1 specifically for exceeding its limit
// rather than by running out of input, which is the difference between a limit
// that is enforced and one that happens to be unreachable.
type hostileCount struct {
	name string
	max  uint64
	// body returns a complete body with the count set to v.
	body func(v uint64) []byte
	kind uint8
	read func(file []byte, reg world.BlockRegistry) error
}

// hostileMetaPrefix writes the four meta blobs a solid body opens with.
func hostileMetaPrefix(w *writer) {
	w.blob(nil)
	w.blob(nil)
	w.blob(nil)
	w.blob(nil)
}

// hostileEmptyPalettes writes an empty block palette (entries and overrides)
// and an empty biome palette.
func hostileEmptyPalettes(w *writer) {
	w.uvarint(0)
	w.uvarint(0)
	w.uvarint(0)
}

func hostileReadWorld(f []byte, reg world.BlockRegistry) error {
	_, err := ReadWorld(f, reg)
	return err
}

func hostileReadStructure(f []byte, reg world.BlockRegistry) error {
	_, err := ReadStructure(f, reg)
	return err
}

func hostileReadMeta(f []byte, _ world.BlockRegistry) error {
	_, err := ReadMeta(f)
	return err
}

func hostileCounts() []hostileCount {
	return []hostileCount{
		{
			name: "solid/chunk records", max: maxChunks, kind: KindWorld, read: hostileReadWorld,
			body: func(v uint64) []byte {
				w := &writer{}
				hostileMetaPrefix(w)
				hostileEmptyPalettes(w)
				w.uvarint(0) // blob table
				w.uvarint(v)
				return w.bytes()
			},
		},
		{
			name: "solid/blob table", max: maxBlobs, kind: KindWorld, read: hostileReadWorld,
			body: func(v uint64) []byte {
				w := &writer{}
				hostileMetaPrefix(w)
				hostileEmptyPalettes(w)
				w.uvarint(v)
				return w.bytes()
			},
		},
		{
			name: "solid/block palette", max: maxPalette, kind: KindWorld, read: hostileReadWorld,
			body: func(v uint64) []byte {
				w := &writer{}
				hostileMetaPrefix(w)
				w.uvarint(v)
				return w.bytes()
			},
		},
		{
			name: "solid/biome palette", max: maxPalette, kind: KindWorld, read: hostileReadWorld,
			body: func(v uint64) []byte {
				w := &writer{}
				hostileMetaPrefix(w)
				w.uvarint(0)
				w.uvarint(0)
				w.uvarint(v)
				return w.bytes()
			},
		},
		{
			name: "solid/string length", max: maxStringLen, kind: KindWorld, read: hostileReadWorld,
			body: func(v uint64) []byte {
				w := &writer{}
				hostileMetaPrefix(w)
				w.uvarint(1) // one palette entry
				w.uvarint(v) // its name's length
				return w.bytes()
			},
		},
		{
			name: "solid/blob length", max: maxBlobLen, kind: KindWorld, read: hostileReadMeta,
			body: func(v uint64) []byte {
				w := &writer{}
				w.uvarint(v) // the settings blob's length
				return w.bytes()
			},
		},
		{
			name: "solid/state properties", max: 64, kind: KindWorld, read: hostileReadWorld,
			body: func(v uint64) []byte {
				w := &writer{}
				hostileMetaPrefix(w)
				w.uvarint(1)
				w.str("minecraft:stone")
				w.uvarint(v)
				return w.bytes()
			},
		},
		{
			name: "solid/sections per chunk", max: maxSectionCnt, kind: KindWorld, read: hostileReadWorld,
			body: func(v uint64) []byte {
				w := &writer{}
				hostileMetaPrefix(w)
				hostileEmptyPalettes(w)
				w.uvarint(0) // blob table
				w.uvarint(1) // one record
				w.svarint(0)
				w.svarint(0)
				w.svarint(0) // minSection
				w.uvarint(v)
				return w.bytes()
			},
		},
		{
			name: "solid/layers per section", max: maxLayers, kind: KindWorld, read: hostileReadWorld,
			body: func(v uint64) []byte {
				w := &writer{}
				hostileMetaPrefix(w)
				hostileEmptyPalettes(w)
				w.uvarint(0)
				w.uvarint(1)
				w.svarint(0)
				w.svarint(0)
				w.svarint(0)
				w.uvarint(1) // one section
				w.u8(1)      // present
				w.uvarint(v) // its layer count
				return w.bytes()
			},
		},
		{
			name: "solid/block entities", max: maxPerChunk, kind: KindWorld, read: hostileReadWorld,
			body: hostilePerChunkBody(0),
		},
		{
			name: "solid/entities", max: maxPerChunk, kind: KindWorld, read: hostileReadWorld,
			body: hostilePerChunkBody(1),
		},
		{
			name: "solid/scheduled ticks", max: maxPerChunk, kind: KindWorld, read: hostileReadWorld,
			body: hostilePerChunkBody(2),
		},
		{
			name: "solid/section palette", max: 1 << 16, kind: KindWorld, read: hostileReadWorld,
			body: func(v uint64) []byte {
				w := &writer{}
				hostileMetaPrefix(w)
				hostileEmptyPalettes(w)
				w.uvarint(1) // one blob in the table
				w.uvarint(v) // its local palette count
				return w.bytes()
			},
		},
		{
			name: "structure/size per axis", max: maxStructureSize, kind: KindStructure, read: hostileReadStructure,
			body: func(v uint64) []byte {
				w := &writer{}
				hostileMetaPrefix(w)
				hostileEmptyPalettes(w)
				w.uvarint(0) // blob table
				w.uvarint(v)
				w.uvarint(1)
				w.uvarint(1)
				return w.bytes()
			},
		},
		{
			name: "structure/block entities", max: maxPerChunk, kind: KindStructure, read: hostileReadStructure,
			body: func(v uint64) []byte {
				w := &writer{}
				hostileMetaPrefix(w)
				hostileEmptyPalettes(w)
				w.uvarint(0) // blob table
				w.uvarint(1) // size
				w.uvarint(1)
				w.uvarint(1)
				w.svarint(0) // origin
				w.svarint(0)
				w.svarint(0)
				w.u8(0)      // cell presence: the one cell is absent
				w.uvarint(v) // block entity count
				return w.bytes()
			},
		},
		{
			name: "structure/entities", max: maxPerChunk, kind: KindStructure, read: hostileReadStructure,
			body: func(v uint64) []byte {
				w := &writer{}
				hostileMetaPrefix(w)
				hostileEmptyPalettes(w)
				w.uvarint(0)
				w.uvarint(1)
				w.uvarint(1)
				w.uvarint(1)
				w.svarint(0)
				w.svarint(0)
				w.svarint(0)
				w.u8(0)
				w.uvarint(0) // no block entities
				w.uvarint(v)
				return w.bytes()
			},
		},
	}
}

// hostilePerChunkBody returns a builder for one of the three per-chunk
// collection counts (0 = block entities, 1 = entities, 2 = scheduled ticks),
// each of which follows the section data of a single empty record.
func hostilePerChunkBody(which int) func(uint64) []byte {
	return func(v uint64) []byte {
		w := &writer{}
		hostileMetaPrefix(w)
		hostileEmptyPalettes(w)
		w.uvarint(0) // blob table
		w.uvarint(1) // one record
		w.svarint(0)
		w.svarint(0)
		w.svarint(0)
		w.uvarint(1) // one section
		w.u8(0)      // no block sections present
		w.u8(0)      // no biome sections present
		for i := range 3 {
			switch {
			case i == which:
				w.uvarint(v)
				return w.bytes()
			case i == 2:
				// The chunk tick precedes the scheduled update count.
				w.svarint(0)
				w.uvarint(0)
			default:
				w.uvarint(0)
			}
		}
		return w.bytes()
	}
}

// hostileIndexedCounts is the same idea for the two counts that live inside a
// directory frame, which is the only place OpenIndexed reads a count from. A
// directory frame is not a body: it carries its own prologue and is
// authenticated by the footer, so the bodies are built and sealed separately.
type hostileIndexedCount struct {
	name string
	max  uint64
	// complete says a body built with a count of zero is a whole valid
	// directory rather than a truncated one, which is what an empty world's
	// directory is, so that case must open instead of being refused.
	complete bool
	body     func(v uint64) []byte
}

func hostileIndexedCounts() []hostileIndexedCount {
	prologue := func(w *writer) {
		w.u8(KindWorld)
		w.u8(ModeIndexed)
		w.u32(0)
		w.u32(hostileBlockVersion)
		for range 2 { // meta and dictionary references, both absent
			w.uvarint(0)
			w.uvarint(0)
			w.u64(0)
		}
	}
	return []hostileIndexedCount{
		{
			name: "indexed/palette segments", max: 1 << 20,
			body: func(v uint64) []byte {
				w := &writer{}
				prologue(w)
				w.uvarint(v)
				return w.bytes()
			},
		},
		{
			name: "indexed/directory entries", max: maxDirEntries, complete: true,
			body: func(v uint64) []byte {
				w := &writer{}
				prologue(w)
				w.uvarint(0) // block palette segments
				w.uvarint(0) // biome palette segments
				w.uvarint(v)
				return w.bytes()
			},
		},
	}
}

// TestHostileIndexedCountBoundaries drives OpenIndexed over the same
// boundaries. A directory that does not parse leaves no valid checkpoint, so
// every case here must fail the open; what matters is that it fails by
// returning, having allocated nothing the directory did not pay for.
func TestHostileIndexedCountBoundaries(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	for _, c := range hostileIndexedCounts() {
		t.Run(c.name, func(t *testing.T) {
			for _, v := range []uint64{0, 1, c.max, c.max + 1} {
				p := filepath.Join(dir, "c.pile")
				if err := os.WriteFile(p, hostileSealDirectory(c.body(v)), 0o644); err != nil {
					t.Fatal(err)
				}
				w, err := OpenIndexed(p, reg, true)
				if err == nil {
					_ = w.Close()
					if c.complete && v == 0 {
						continue // an empty directory is a fresh world
					}
					t.Fatalf("a directory truncated after a count of %d opened", v)
				}
				requireCleanRejection(t, fmt.Sprintf("count=%d", v), err)
			}
		})
	}
}

// hostileSealDirectory wraps a directory frame body in an indexed file with a
// single authenticated checkpoint footer naming it.
func hostileSealDirectory(dirBody []byte) []byte {
	h := &writer{}
	h.raw(headerMagic[:])
	h.u16(Version)
	h.u8(KindWorld)
	h.u8(ModeIndexed)
	h.u32(0)
	h.i32(hostileBlockVersion)
	buf := h.bytes()
	hdr := append([]byte(nil), buf...)

	stored := compressBody(dirBody, CompressionBest, false)
	off := int64(len(buf))
	buf = append(buf, stored...)

	tail := &writer{}
	tail.u64(uint64(off))
	tail.u64(uint64(len(stored)))
	tail.u64(1)
	tail.u64(0)
	tail.raw(footerMagic[:])
	f := &writer{}
	f.u64(checkpointHash(hdr, stored, tail.bytes()))
	f.raw(tail.bytes())
	return append(buf, f.bytes()...)
}

// TestHostileCountBoundaries drives every count the format defines at 0, 1,
// its documented maximum and one past it. The bodies are deliberately
// truncated right after the count, so what is under test is what the decoder
// does with the number before it has any of the bytes the number promises:
// that is where an allocation sized from unvalidated input shows up.
func TestHostileCountBoundaries(t *testing.T) {
	reg := testRegistry(t)
	for _, c := range hostileCounts() {
		t.Run(c.name, func(t *testing.T) {
			for _, v := range []uint64{0, 1, c.max, c.max + 1} {
				for _, compressed := range []bool{false, true} {
					body := c.body(v)
					err := c.read(hostileSeal(c.kind, body, compressed), reg)
					if err != nil {
						requireCleanRejection(t, fmt.Sprintf("count=%d", v), err)
					}
					if v == c.max+1 && err == nil {
						t.Fatalf("count %d, one past the limit %d, was accepted", v, c.max)
					}
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Allocation ceilings
// ---------------------------------------------------------------------------

// hostileAlloc runs fn and returns the bytes it allocated. Cumulative
// allocation rather than retained heap: it is deterministic, it does not depend
// on when the collector runs, and it is exactly the quantity a count-sized
// allocation inflates.
func hostileAlloc(fn func()) uint64 {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// TestHostileAllocationCeilings is the part of the matrix that catches the
// shape a count check does not: a value that is within its limit and within
// the bytes that remain, and still costs far more to decode than to write.
//
// Each case names a file of a few kilobytes, the ceiling its decode must stay
// under, and what the same input allocated before the guard it exercises. The
// ceilings are generous — several times what the fixed code uses — because the
// failure being guarded against is two orders of magnitude, not ten per cent.
func TestHostileAllocationCeilings(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates tens of megabytes")
	}
	reg := testRegistry(t)
	dir := t.TempDir()
	cases := []struct {
		name    string
		file    []byte
		ceiling uint64
		before  string
		run     func(file []byte)
	}{
		{
			// blob.go sized three containers from the blob-table count: a
			// []decBlob at 56 bytes an entry, a map[uint64][]int and a
			// [][2]int. Three bytes of input reserved about a hundred.
			name:    "solid/blob table count",
			file:    hostileBlobTableFile(1 << 22),
			ceiling: 48 << 20,
			before:  "4,194,304 blobs from a 482-byte file allocated 635 MiB; the whole limit, 16,777,216, allocated 2.42 GiB from 1,634 bytes",
			run:     func(f []byte) { _, _ = ReadWorld(f, reg) },
		},
		{
			// palette.go sized []parsedState from the palette count, and each
			// entry carries a property map, so two bytes of input reserved
			// eighty.
			name:    "solid/block palette count",
			file:    hostileBlockPaletteFile(1 << 20),
			ceiling: 16 << 20,
			before:  "1,048,576 entries from a 157-byte file allocated 84 MiB",
			run:     func(f []byte) { _, _ = ReadWorld(f, reg) },
		},
		{
			// The biome palette sized two slices and a map the same way.
			name:    "solid/biome palette count",
			file:    hostileBiomePaletteFile(1 << 20),
			ceiling: 16 << 20,
			before:  "1,048,576 entries from a 159-byte file allocated 65 MiB",
			run:     func(f []byte) { _, _ = ReadWorld(f, reg) },
		},
		{
			// structure.go allocated the cell grid, eight bytes a cell, from
			// three uvarints — before reading the presence bitset that a file
			// describing that many cells has to carry.
			name:    "structure/cell grid",
			file:    hostileStructureCellFile(1<<20, 256, 16),
			ceiling: 1 << 20,
			before:  "1,048,576 cells from an 88-byte file allocated 8 MiB",
			run:     func(f []byte) { _, _ = ReadStructure(f, reg) },
		},
		{
			// The directory's palette-segment list sized a []frameRef from the
			// segment count, before any reference had been read or found.
			name:    "indexed/palette segment count",
			file:    hostileSegmentCountFile(1 << 20),
			ceiling: 8 << 20,
			before:  "1,048,576 segments from a 165-byte file allocated 27 MiB",
			run: func(f []byte) {
				p := filepath.Join(dir, "segs.pile")
				if err := os.WriteFile(p, f, 0o644); err != nil {
					t.Fatal(err)
				}
				w, err := OpenIndexed(p, reg, true)
				if err == nil {
					_ = w.Close()
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := hostileAlloc(func() { c.run(c.file) })
			t.Logf("file %d bytes, decode allocated %d bytes (before the guard: %s)",
				len(c.file), got, c.before)
			if got > c.ceiling {
				t.Fatalf("decoding %d bytes allocated %d, over the ceiling of %d: an allocation is being sized from the input's declared counts rather than from the input",
					len(c.file), got, c.ceiling)
			}
		})
	}
}

// hostileBlobTableFile declares n blob-table entries and supplies exactly the
// three bytes an entry needs, so the count passes its input bound. The bytes
// are zero, so the first blob is refused; what is measured is what was
// allocated before that happened.
func hostileBlobTableFile(n int) []byte {
	w := &writer{}
	hostileMetaPrefix(w)
	hostileEmptyPalettes(w)
	w.uvarint(uint64(n))
	w.raw(make([]byte, n*3))
	return hostileSeal(KindWorld, w.bytes(), true)
}

// hostileBlockPaletteFile declares n block palette entries with the two bytes
// each needs, and makes the first one unreadable so the parse stops there.
func hostileBlockPaletteFile(n int) []byte {
	w := &writer{}
	hostileMetaPrefix(w)
	w.uvarint(uint64(n))
	pad := make([]byte, n*2)
	pad[0] = 0xFF // an unterminated uvarint: the first name's length
	w.raw(pad)
	return hostileSeal(KindWorld, w.bytes(), true)
}

// hostileBiomePaletteFile is the same for the biome palette. A zero-length
// name is refused for not being namespaced, at the first entry.
func hostileBiomePaletteFile(n int) []byte {
	w := &writer{}
	hostileMetaPrefix(w)
	w.uvarint(0)
	w.uvarint(0)
	w.uvarint(uint64(n))
	w.raw(make([]byte, n*2))
	return hostileSeal(KindWorld, w.bytes(), true)
}

// hostileStructureCellFile declares a structure whose cell grid is at the
// ceiling and supplies nothing after it.
func hostileStructureCellFile(x, y, z uint64) []byte {
	w := &writer{}
	hostileMetaPrefix(w)
	hostileEmptyPalettes(w)
	w.uvarint(0) // blob table
	w.uvarint(x)
	w.uvarint(y)
	w.uvarint(z)
	return hostileSeal(KindStructure, w.bytes(), true)
}

// hostileSegmentCountFile builds an indexed file whose directory declares n
// palette segments and supplies the two bytes each needs to pass the input
// bound, and nothing that could be a real reference.
func hostileSegmentCountFile(n int) []byte {
	d := &writer{}
	d.u8(KindWorld)
	d.u8(ModeIndexed)
	d.u32(0)
	d.u32(hostileBlockVersion)
	for range 2 { // meta and dictionary references, both absent
		d.uvarint(0)
		d.uvarint(0)
		d.u64(0)
	}
	d.uvarint(uint64(n))
	d.raw(make([]byte, n*2))
	return hostileSealDirectory(d.bytes())
}

// ---------------------------------------------------------------------------
// Concurrency
// ---------------------------------------------------------------------------

// TestUnresolvedStatesHoldsTheLock drives UnresolvedStates against Compact,
// which replaces the file handle, the compression state and the dictionary
// codec while it rewrites the file. Reading the palette segments outside the
// lock raced all three, and readFrame does not re-check a frame's hash, so
// offsets taken from the old directory were decoded against the new file.
// Under -race this fails without the lock; without -race it can still return
// states that were never in either file.
func TestUnresolvedStatesHoldsTheLock(t *testing.T) {
	reg := testRegistry(t)
	p := filepath.Join(t.TempDir(), "w.pile")
	w, err := CreateIndexed(p, reg, Options{Compression: CompressionDefault})
	if err != nil {
		t.Fatal(err)
	}
	for i := range int32(8) {
		if err := w.Store(buildTestColumn(t, reg, i, 0)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 40 {
			if _, err := w.UnresolvedStates(); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for range 5 {
			if err := w.Compact(); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	wg.Wait()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	// And after Close it reports the handle is closed rather than reading a
	// file descriptor the handle no longer owns.
	if _, err := w.UnresolvedStates(); err == nil {
		t.Fatal("UnresolvedStates succeeded on a closed world")
	}
}

// ---------------------------------------------------------------------------
// Recovery work
// ---------------------------------------------------------------------------

// TestHostileCheckpointReplay records what a file of a few kilobytes can make
// recovery do. Every footer candidate whose directory parses is loaded in full
// and has every record it names verified, and the candidate list runs to
// maxCheckpointChain, so the work is the product of two limits §8 states
// separately. This test does not assert a bound — bounding it would refuse
// files that open today, which is a format change — it asserts the shape stays
// linear in the candidate count and that the open terminates.
func TestHostileCheckpointReplay(t *testing.T) {
	reg := testRegistry(t)
	const entries = 1 << 14
	for _, footers := range []int{1, 8} {
		file := hostileReplayFile(entries, footers)
		p := filepath.Join(t.TempDir(), "replay.pile")
		if err := os.WriteFile(p, file, 0o644); err != nil {
			t.Fatal(err)
		}
		var err error
		got := hostileAlloc(func() { _, err = OpenIndexed(p, reg, true) })
		if err == nil {
			t.Fatal("a file whose every record fails its hash was opened")
		}
		if !errors.Is(err, ErrChecksum) {
			t.Fatalf("footers=%d: %v", footers, err)
		}
		t.Logf("%d footers over a %d-entry directory: %d-byte file, open allocated %d bytes",
			footers, entries, len(file), got)
		// The cost per candidate is the directory: decompress it, parse every
		// entry and build the map. verifyRecords stops at the first record
		// whose hash is wrong, so it is not what multiplies here.
		if ceiling := uint64(footers) * 32 << 20; got > ceiling {
			t.Fatalf("opening %d bytes allocated %d, over %d", len(file), got, ceiling)
		}
	}
}

// TestIndexedVerifyReusesOneBuffer covers the other half of recovery: when the
// records a directory names are intact, verifyRecords reads all of them.
// Allocating a buffer per record made opening a world cost its whole live size
// in garbage, four million times over on a directory at the §8 ceiling, and
// recovery repeats that for every checkpoint candidate it tries.
func TestIndexedVerifyReusesOneBuffer(t *testing.T) {
	reg := testRegistry(t)
	const entries = 1 << 14
	file := hostileVerifyFile(entries)
	p := filepath.Join(t.TempDir(), "verify.pile")
	if err := os.WriteFile(p, file, 0o644); err != nil {
		t.Fatal(err)
	}
	var w *IndexedWorld
	var err error
	got := hostileAlloc(func() { w, err = OpenIndexed(p, reg, true) })
	if err != nil {
		t.Fatalf("a directory of %d intact records did not open: %v", entries, err)
	}
	defer func() { _ = w.Close() }()
	if w.ChunkCount() != entries {
		t.Fatalf("opened %d entries, want %d", w.ChunkCount(), entries)
	}
	t.Logf("%d entries of %d bytes: %d-byte file, open allocated %d bytes",
		entries, hostileReplayRecordLen, len(file), got)
	// The directory itself is about 12 bytes an entry and the map that holds
	// it a few times that. A buffer per record adds the record size on top,
	// which at 4 KiB an entry is an order of magnitude more.
	if ceiling := uint64(entries) * 512; got > ceiling {
		t.Fatalf("opening %d bytes allocated %d, over %d: verifyRecords is allocating per record rather than reusing one buffer",
			len(file), got, ceiling)
	}
}

// hostileVerifyFile builds an indexed file whose directory names `entries`
// records that all share one intact span, so every hash matches and
// verifyRecords walks the whole directory.
func hostileVerifyFile(entries int) []byte {
	return hostileIndexedDirFile(entries, true, 1)
}

// hostileReplayRecordLen is the length every entry in the replay directory
// claims. It is what makes the per-record allocation visible: at one byte a
// record the buffer is rounded up to the allocator's smallest size class and
// the difference hides.
const hostileReplayRecordLen = 4096

// hostileReplayFile builds an indexed file with one directory frame naming
// `entries` records whose hash never matches, and `footers` checkpoint footers
// all naming that directory, chained newest to oldest, so recovery tries every
// one of them.
func hostileReplayFile(entries, footers int) []byte {
	return hostileIndexedDirFile(entries, false, footers)
}

// hostileIndexedDirFile assembles an indexed file by hand: one shared record
// span, one directory frame naming `entries` entries over it, and `footers`
// chained checkpoint footers. intact selects whether the entries carry the
// record's real hash.
func hostileIndexedDirFile(entries int, intact bool, footers int) []byte {
	h := &writer{}
	h.raw(headerMagic[:])
	h.u16(Version)
	h.u8(KindWorld)
	h.u8(ModeIndexed)
	h.u32(0)
	h.i32(hostileBlockVersion)
	buf := h.bytes()
	hdr := append([]byte(nil), buf...)

	// One run of record bytes that every entry points at. Overlapping records
	// are legal — the directory bounds each entry against the file and nothing
	// says two may not name the same span — so a four-kilobyte run backs
	// however many entries the directory declares.
	recOff := int64(len(buf))
	rec := make([]byte, hostileReplayRecordLen)
	buf = append(buf, rec...)
	recHash := uint64(0xDEADBEEF)
	if intact {
		recHash = xxhash.Sum64(rec)
	}

	d := &writer{}
	d.u8(KindWorld)
	d.u8(ModeIndexed)
	d.u32(0)
	d.u32(hostileBlockVersion)
	for range 2 { // meta and dictionary references, both absent
		d.uvarint(0)
		d.uvarint(0)
		d.u64(0)
	}
	d.uvarint(0) // block palette segments
	d.uvarint(0) // biome palette segments
	d.uvarint(uint64(entries))
	for i := range entries {
		if i == 0 {
			d.svarint(0)
			d.svarint(0)
			d.svarint(recOff)
		} else {
			d.svarint(1)
			d.svarint(0)
			d.svarint(0)
		}
		d.uvarint(hostileReplayRecordLen)
		d.u64(recHash)
	}
	dirStored := compressBody(d.bytes(), CompressionBest, false)
	dirOff := int64(len(buf))
	buf = append(buf, dirStored...)

	var prev int64
	for i := range footers {
		off := int64(len(buf))
		tail := &writer{}
		tail.u64(uint64(dirOff))
		tail.u64(uint64(len(dirStored)))
		tail.u64(uint64(i + 1))
		tail.u64(uint64(prev))
		tail.raw(footerMagic[:])
		f := &writer{}
		f.u64(checkpointHash(hdr, dirStored, tail.bytes()))
		f.raw(tail.bytes())
		buf = append(buf, f.bytes()...)
		prev = off
	}
	return buf
}

// ---------------------------------------------------------------------------
// NBT
// ---------------------------------------------------------------------------

// TestHostileNBTContainerBudget pins what the container budget of §8 does and
// does not cover. A list of compounds is charged per element, which is the
// amplification the budget was written for. A compound that is a field of
// another compound is not charged, so a blob can decode into more containers
// than the stated ceiling. That is recorded in `SECURITY.md` as an
// under-enforced validity rule rather than fixed here: making the reader
// enforce it would refuse files this version accepts, which is a format change.
func TestHostileNBTContainerBudget(t *testing.T) {
	atLimit := hostileNBTListOfCompounds(maxNBTElements)
	if err := validateNBT(atLimit); err != nil {
		t.Fatalf("a list of exactly %d compounds was refused: %v", maxNBTElements, err)
	}
	overLimit := hostileNBTListOfCompounds(maxNBTElements + 1)
	if err := validateNBT(overLimit); err == nil {
		t.Fatalf("a list of %d compounds was accepted", maxNBTElements+1)
	}
	// The gap. If this ever starts failing, the budget has been extended to
	// compound fields and SECURITY.md's "what remains" is out of date.
	overByNesting := hostileNBTSiblingCompounds(2 * maxNBTElements)
	if err := validateNBT(overByNesting); err != nil {
		t.Fatalf("the sibling-compound case now fails (%v); SECURITY.md says it does not", err)
	}
}

// TestHostileOverrideDeltaWraps pins the one integer computation in the decode
// paths that wraps before its bounds check, so that whoever changes it knows
// what they are changing.
//
// §3.1's version-override table carries index deltas as uvarints and the
// decoder accumulates them in a uint64: `idx := prev + delta`. The bounds test
// that follows catches an index past the palette, so nothing here is
// memory-unsafe, but the sum is modular and a uvarint can express the modular
// representative of a negative step. A delta of 2^64-2 after index 5 lands on
// index 3, which is a descent, and §3.1 says the indices strictly ascend. The
// decoder only enforces that through "a later delta MUST be non-zero", which
// is sufficient exactly as long as the sum cannot wrap.
//
// The other two delta chains in the format — a record's chunk position and a
// directory entry's frame offset — accumulate in an int64 and range-check each
// step, and there the modular representative of a wrap lands next to the
// bottom of int64, nowhere near a legal value. This one is unsigned, which is
// the whole of the difference.
//
// It is recorded and not fixed: refusing it would reject a file this reader
// accepts today, which `SECURITY.md` reports as a format change for someone
// else to decide. If this test starts failing, that decision has been taken.
func TestHostileOverrideDeltaWraps(t *testing.T) {
	reg := testRegistry(t)
	w := &writer{}
	hostileMetaPrefix(w)
	const entries = 6
	w.uvarint(entries)
	for i := range entries {
		w.str(fmt.Sprintf("minecraft:x%d", i))
		w.uvarint(0) // no properties
	}
	w.uvarint(2)              // two overrides
	w.uvarint(5)              // the first names index 5
	w.u32(1)                  // at some other version
	w.uvarint(^uint64(0) - 1) // 2^64-2, so 5 + delta == 3 (mod 2^64)
	w.u32(2)
	w.uvarint(0) // biome palette
	w.uvarint(0) // blob table
	w.uvarint(0) // chunk records
	if _, err := ReadWorld(hostileSeal(KindWorld, w.bytes(), false), reg); err != nil {
		t.Fatalf("the wrapping override chain is now refused (%v); SECURITY.md says it is accepted, and refusing it is a format change", err)
	}
}

func hostileNBTListOfCompounds(n int) []byte {
	w := &writer{}
	w.u8(tagCompound)
	w.u16(0)
	w.u8(tagList)
	w.u16(1)
	w.raw([]byte("a"))
	w.u8(tagCompound)
	w.u32(uint32(n))
	for range n {
		w.u8(tagEnd)
	}
	w.u8(tagEnd)
	return w.bytes()
}

func hostileNBTSiblingCompounds(n int) []byte {
	w := &writer{}
	w.u8(tagCompound)
	w.u16(0)
	for i := range n {
		w.u8(tagCompound)
		k := [3]byte{byte(i >> 16), byte(i >> 8), byte(i)}
		w.u16(3)
		w.raw(k[:])
		w.u8(tagEnd)
	}
	w.u8(tagEnd)
	return w.bytes()
}
