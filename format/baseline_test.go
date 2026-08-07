package format

// Performance baselines. PERFORMANCE.md is the record; this file is what keeps
// the record honest.
//
// Nothing here measures anything. That is deliberate, and it is the whole
// design: a wall-clock threshold in the ordinary test run is noise on a shared
// machine, and the way a noisy gate ends is muted rather than fixed. The
// baselines are checked-in `go test -bench` output that benchstat can diff
// (scripts/bench.sh), and what is enforced on every run is the two things that
// can be enforced without measuring:
//
//  1. every benchmark in the package has a recorded baseline, so a benchmark
//     added or renamed without re-recording is a failure and not a silent hole;
//  2. the allocation *shapes* the memory pass established, as ratios between
//     two measurements taken in the same process. A ratio of allocation counts
//     is exact and machine-independent: it does not care what else the machine
//     is doing, which is exactly what a nanosecond figure cannot say.

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/df-mc/dragonfly/server/world"
)

const baselineFile = "testdata/benchmarks.txt"

// benchmarkNames returns the benchmark functions declared in the given source
// files, read as text rather than through reflection because a test binary
// cannot enumerate the benchmarks it was linked with.
func benchmarkNames(t *testing.T, files ...string) []string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^func (Benchmark\w+)\(`)
	var out []string
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(src), -1) {
			out = append(out, m[1])
		}
	}
	return out
}

// baselineHas reports whether the recorded output holds a result line for the
// named benchmark, at the top level or as any sub-benchmark of it.
func baselineHas(recorded, name string) bool {
	for line := range strings.SplitSeq(recorded, "\n") {
		rest, ok := strings.CutPrefix(line, name)
		if !ok {
			continue
		}
		// "BenchmarkFoo-8", "BenchmarkFoo/100-8". Not "BenchmarkFooBar-8".
		if rest == "" || rest[0] == '-' || rest[0] == '/' {
			return true
		}
	}
	return false
}

// TestBenchmarkBaselinesRecorded requires every benchmark in this package to
// appear in the checked-in baseline.
//
// It is the cheapest useful thing a test can say about performance: it cannot
// tell you a benchmark got slower, but it can tell you that nobody would find
// out if it did. FREEZE.md's open item was "benchmarks exist but nothing gates
// them, and there are no recorded baselines" — a record that silently stops
// covering new work is the same hole with a file in front of it.
func TestBenchmarkBaselinesRecorded(t *testing.T) {
	b, err := os.ReadFile(baselineFile)
	if err != nil {
		t.Fatalf("%v (record it with scripts/bench.sh record; see PERFORMANCE.md)", err)
	}
	recorded := string(b)
	for _, name := range benchmarkNames(t, "bench_test.go") {
		if !baselineHas(recorded, name) {
			t.Errorf("%s has no recorded baseline in %s; re-record with scripts/bench.sh record",
				name, filepath.ToSlash(baselineFile))
		}
	}
}

// TestIndexedStoreDoesNotScaleWithPalette holds one of the two shapes STATUS.md
// says must not be optimised back: storing a column that introduces no new
// palette entries must cost what it adds, not what the palette already holds.
//
// The regression it exists for is a real one that was fixed and had no test.
// Store used to copy the whole runtime-ID map on every call, so a column stored
// into a 17,499-entry palette allocated 1,680,870 B/op against 218,342 for the
// same column stored into an empty one. BenchmarkIndexedStoreLargePalette
// measures it, and a benchmark nobody runs is not a guard.
//
// The assertion is a ratio between two measurements taken in the same process,
// against the same column, so it survives being run on a loaded machine, on a
// different architecture, and past any change to what a Store legitimately
// costs. Only the shape is claimed: linear in the palette or not.
func TestIndexedStoreDoesNotScaleWithPalette(t *testing.T) {
	reg := testRegistry(t)
	col := buildTestColumn(t, reg, 0, 0)

	empty := storeAllocs(t, reg, col, nil)
	full := storeAllocs(t, reg, col, func(t *testing.T, w *IndexedWorld) {
		if err := w.Store(paletteFillColumn(t, reg)); err != nil {
			t.Fatal(err)
		}
		if len(w.rids) < 4096 {
			t.Fatalf("the palette only grew to %d entries; the fixture is not exercising the shape", len(w.rids))
		}
	})
	// Generous on purpose: a per-Store cost that tracked the palette would be
	// off by the better part of an order of magnitude, and anything under this
	// is a constant that happens to differ.
	if full > empty*2 {
		t.Fatalf("storing into a large palette allocated %d bytes against %d into an empty one; "+
			"the per-Store cost is tracking the palette size again", full, empty)
	}
}

// storeAllocs returns the bytes one Store allocates, after prep has run.
func storeAllocs(t *testing.T, reg world.BlockRegistry, col Column, prep func(*testing.T, *IndexedWorld)) uint64 {
	t.Helper()
	w, err := CreateIndexed(filepath.Join(t.TempDir(), "shape.pile"), reg, Options{Compression: CompressionDefault})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if prep != nil {
		prep(t, w)
	}
	// One warm Store first: the shared encoder and the frame buffers are built
	// on first use, and charging that to the measurement is the same mistake a
	// short benchmark run makes. PERFORMANCE.md, "The variance trap".
	if err := w.Store(col); err != nil {
		t.Fatal(err)
	}
	const n = 8
	before := allocatedBytes()
	for range n {
		if err := w.Store(col); err != nil {
			t.Fatal(err)
		}
	}
	return (allocatedBytes() - before) / n
}

// allocatedBytes is the process's cumulative allocation counter. Differences
// of it are what -benchmem reports as B/op, and unlike a duration they are a
// count: two readings taken around the same work agree whatever else the
// machine is doing.
func allocatedBytes() uint64 {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.TotalAlloc
}
