package format

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/df-mc/dragonfly/server/world/chunk"
)

// FuzzReadStructure asserts the structure decoder never panics on arbitrary
// input.
func FuzzReadStructure(f *testing.F) {
	reg := testRegistry(f)
	data, err := NewStructureData([3]int32{20, 10, 20})
	if err != nil {
		f.Fatal(err)
	}
	stone, _ := reg.StateToRuntimeID("minecraft:stone", map[string]any{})
	cell := chunk.NewSubChunk(reg.AirRuntimeID())
	cell.SetBlock(1, 2, 3, 0, stone)
	data.Cells[0] = cell
	data.BlockEntities = append(data.BlockEntities, StructureBlockEntity{
		Pos: [3]int32{1, 2, 3}, Data: map[string]any{"id": "minecraft:chest"},
	})
	var buf bytes.Buffer
	if err := WriteStructure(&buf, data, reg, Options{Compression: CompressionDefault}); err != nil {
		f.Fatal(err)
	}
	f.Add(buf.Bytes())
	f.Add([]byte("PILE"))
	f.Fuzz(func(t *testing.T, in []byte) {
		_, _ = ReadStructure(in, reg)
		_, _ = UnresolvedStates(in, reg)
	})
}

// memFile is an in-memory indexedFile: the bytes of a would-be file, with no
// file behind them.
//
// It exists so FuzzOpenIndexed can stop paying for the filesystem on every
// execution. The target used to write each input to a fresh t.TempDir and open
// it, and that is where its throughput went: over a 20-minute session it
// managed 168k executions against 11-16 million for the three in-memory
// targets, roughly 2/s. Sampling the workers put their time in mkdir, open,
// write, unlinkat and rmdir, and a CPU profile of the decode alone put 85% of
// it in syscall.rawsyscalln, 63% under readFrame's ReadAt — every frame an
// indexed reader touches is a pread, where ReadWorld reads a []byte. Measured
// per execution on one machine: 3.64 ms with a fresh temp dir, 1.43 ms reusing
// one path, and 0.15 ms with no file at all.
//
// It is not a stub in the path being fuzzed. openIndexedOn takes this same
// interface — it is how the durability suite injects a file that tears — so
// every candidate walk, directory load, palette segment and record decode runs
// exactly as it does over a real file. What is skipped is OpenIndexed's own six
// lines of os.OpenFile, which nothing about a decoder depends on and which
// TestHostileIndexedTruncation and the durability suite drive over real files
// anyway.
type memFile struct{ b []byte }

func (m *memFile) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("memfile: negative offset")
	}
	if off >= int64(len(m.b)) {
		return 0, io.EOF
	}
	n := copy(p, m.b[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (m *memFile) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("memfile: negative offset")
	}
	if end := off + int64(len(p)); end > int64(len(m.b)) {
		m.b = append(m.b, make([]byte, end-int64(len(m.b)))...)
	}
	return copy(m.b[off:], p), nil
}

func (m *memFile) Write(p []byte) (int, error) {
	m.b = append(m.b, p...)
	return len(p), nil
}

func (m *memFile) Close() error { return nil }
func (m *memFile) Sync() error  { return nil }

func (m *memFile) Truncate(n int64) error {
	if n < 0 {
		return errors.New("memfile: negative size")
	}
	if n <= int64(len(m.b)) {
		m.b = m.b[:n]
		return nil
	}
	m.b = append(m.b, make([]byte, n-int64(len(m.b)))...)
	return nil
}

func (m *memFile) Stat() (os.FileInfo, error) { return memFileInfo{n: int64(len(m.b))}, nil }

type memFileInfo struct{ n int64 }

func (i memFileInfo) Name() string       { return "memfile" }
func (i memFileInfo) Size() int64        { return i.n }
func (i memFileInfo) Mode() os.FileMode  { return 0o600 }
func (i memFileInfo) ModTime() time.Time { return time.Time{} }
func (i memFileInfo) IsDir() bool        { return false }
func (i memFileInfo) Sys() any           { return nil }

// TestMemFileMatchesOsFile keeps the harness honest. A fuzz target reading
// through a file model that behaves differently from a real file is fuzzing the
// model, so the two are required to agree on every fixture: same verdict, same
// columns, same bytes back.
func TestMemFileMatchesOsFile(t *testing.T) {
	reg := testRegistry(t)
	for _, name := range []string{"golden_indexed.pile", "golden_indexed_zstd.pile", "golden_indexed_torn.pile", "golden_indexed_compact.pile"} {
		t.Run(name, func(t *testing.T) {
			in, err := os.ReadFile(filepath.Join(goldenDir, name))
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "real.pile")
			if err := os.WriteFile(path, in, 0o644); err != nil {
				t.Fatal(err)
			}
			onDisk, diskErr := OpenIndexed(path, reg, true)
			mem, memErr := openIndexedOn(&memFile{b: slices.Clone(in)}, "memfile", reg, true)
			if (diskErr == nil) != (memErr == nil) {
				t.Fatalf("the two file models disagree on the verdict: disk %v, mem %v", diskErr, memErr)
			}
			if diskErr != nil {
				return
			}
			defer func() { _ = onDisk.Close(); _ = mem.Close() }()
			rp, mp := onDisk.Positions(), mem.Positions()
			if len(rp) != len(mp) {
				t.Fatalf("the file on disk has %d columns, the memfile %d", len(rp), len(mp))
			}
			for _, k := range rp {
				rc, rerr := onDisk.Column(k[0], k[1])
				mc, merr := mem.Column(k[0], k[1])
				if (rerr == nil) != (merr == nil) {
					t.Fatalf("column (%d,%d): disk %v, mem %v", k[0], k[1], rerr, merr)
				}
				if rerr != nil {
					continue
				}
				var rb, mb bytes.Buffer
				if err := WriteWorld(&rb, &WorldData{Columns: []Column{rc}}, reg, Options{}); err != nil {
					t.Fatal(err)
				}
				if err := WriteWorld(&mb, &WorldData{Columns: []Column{mc}}, reg, Options{}); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(rb.Bytes(), mb.Bytes()) {
					t.Fatalf("column (%d,%d) decoded differently through the two file models", k[0], k[1])
				}
			}
		})
	}
}

// FuzzOpenIndexed asserts opening and fully reading arbitrary bytes as an
// indexed world never panics.
func FuzzOpenIndexed(f *testing.F) {
	reg := testRegistry(f)
	// Seed: a valid indexed file, and one truncated to half its length so the
	// recovery paths are reachable from the corpus rather than only by luck.
	dir := f.TempDir()
	path := filepath.Join(dir, "seed.pile")
	w, err := CreateIndexed(path, reg, Options{Compression: CompressionDefault})
	if err != nil {
		f.Fatal(err)
	}
	if err := w.Store(buildTestColumn(f, reg, 0, 0)); err != nil {
		f.Fatal(err)
	}
	// A second checkpoint, so a seed exists with a footer chain to walk back
	// along: recovery over a one-checkpoint file has nothing to fall back to,
	// and the corpus is where the fuzzer starts from.
	if err := w.Checkpoint(); err != nil {
		f.Fatal(err)
	}
	if err := w.Store(buildTestColumn(f, reg, 1, 0)); err != nil {
		f.Fatal(err)
	}
	if err := w.Close(); err != nil {
		f.Fatal(err)
	}
	seed, err := os.ReadFile(path)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Add(seed[:len(seed)/2])
	// The tail cut just short of the last footer: the shape recovery exists
	// for, and the one a random mutation is least likely to produce, because a
	// footer's magic and hash have to survive intact further back in the file.
	if len(seed) > footerSize {
		f.Add(seed[:len(seed)-footerSize/2])
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		// Cloned: the fuzzer owns and reuses the input buffer, and a handle
		// that truncates or repairs its file must not reach back into it.
		iw, err := openIndexedOn(&memFile{b: slices.Clone(in)}, "fuzz.pile", reg, true)
		if err != nil {
			return
		}
		for _, k := range iw.Positions() {
			_, _ = iw.Column(k[0], k[1])
		}
		_, _ = iw.UnresolvedStates()
		_ = iw.Close()
	})
}

// FuzzNBTStability: any bytes that decode as NBT must re-encode
// deterministically and re-decode to the same value (canonical stability of
// the deterministic encoder against gophertunnel's decoder).
func FuzzNBTStability(f *testing.F) {
	seed, _ := marshalNBT(map[string]any{
		"a": int64(1), "b": "x", "c": []any{float32(1), float32(2)},
		"d": map[string]any{"e": byte(3)}, "f": []int32{1, 2, 3},
	})
	f.Add(seed)
	f.Add([]byte{10, 0, 0, 0}) // empty root compound
	f.Fuzz(func(t *testing.T, in []byte) {
		m, err := unmarshalNBT(in)
		if err != nil {
			return
		}
		enc1, err := marshalNBT(m)
		if err != nil {
			// Values gophertunnel produces must always be encodable.
			t.Fatalf("decoded NBT failed to re-encode: %v (%#v)", err, m)
		}
		m2, err := unmarshalNBT(enc1)
		if err != nil {
			t.Fatalf("re-encoded NBT failed to decode: %v", err)
		}
		enc2, err := marshalNBT(m2)
		if err != nil {
			t.Fatal(err)
		}
		// Compare encodings rather than values: they express the same
		// equivalence for storage purposes and, unlike reflect.DeepEqual,
		// treat NaN payloads as equal to themselves.
		if !bytes.Equal(enc1, enc2) {
			t.Fatalf("NBT value changed across round trip:\n%#v\n%#v", m, m2)
		}
	})
}
