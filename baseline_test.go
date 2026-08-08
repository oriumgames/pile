package pile

// Performance baselines for the provider. See format/baseline_test.go for why
// nothing here measures a duration. The record itself is testdata/benchmarks.txt,
// written by scripts/bench.sh.

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

const baselineFile = "testdata/benchmarks.txt"

// benchmarkNames returns the benchmark functions declared in src, read as text
// because a test binary cannot enumerate the benchmarks it was linked with.
func benchmarkNames(t *testing.T, files ...string) []string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^func (Benchmark\w+)\(`)
	var out []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
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
		if rest == "" || rest[0] == '-' || rest[0] == '/' {
			return true
		}
	}
	return false
}

// TestBenchmarkBaselinesRecorded requires every benchmark in this package to
// appear in the checked-in baseline, so a benchmark added or renamed without
// re-recording is a failure rather than a hole in the record.
func TestBenchmarkBaselinesRecorded(t *testing.T) {
	b, err := os.ReadFile(baselineFile)
	if err != nil {
		t.Fatalf("%v (record it with scripts/bench.sh record)", err)
	}
	recorded := string(b)
	for _, name := range benchmarkNames(t, "bench_test.go") {
		if !baselineHas(recorded, name) {
			t.Errorf("%s has no recorded baseline in %s; re-record with scripts/bench.sh record",
				name, baselineFile)
		}
	}
}
