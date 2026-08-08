package format

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/klauspost/compress/dict"
	"github.com/klauspost/compress/zstd"
)

// hostileDict builds a dictionary of exactly maxDictLen bytes, distinct per
// seed. A file supplies the dictionary bytes, so this is the largest one an
// attacker can make a process parse and cache.
func hostileDict(t *testing.T, seed int) []byte {
	t.Helper()
	words := []string{"minecraft:stone", "minecraft:dirt", "minecraft:air", "minecraft:grass_block",
		"minecraft:water", "minecraft:sand", "minecraft:oak_log", "minecraft:cobblestone"}
	samples := make([][]byte, 64)
	for i := range samples {
		var s []byte
		x := uint32(seed*1000003 + i*7919)
		for len(s) < 2048 {
			x = x*1664525 + 1013904223
			s = append(s, words[int(x>>28)%len(words)]...)
			s = append(s, 0, byte(seed), byte(seed>>8), byte(x>>16))
		}
		samples[i] = s
	}
	d, err := dict.BuildZstdDict(samples, dict.Options{
		MaxDictSize: 64 << 10, HashBytes: 6, ZstdDictID: uint32(0x50000000 + seed),
		ZstdLevel: zstd.SpeedDefault,
	})
	if err != nil {
		t.Skipf("this build cannot train a dictionary: %v", err)
	}
	// Grow the content section to the ceiling. The dictionary still parses;
	// its content is simply longer, which is exactly what a file that wants to
	// pin a megabyte would carry.
	out := make([]byte, maxDictLen)
	copy(out, d)
	for i := len(d); i < len(out); i++ {
		out[i] = byte(i * 31)
	}
	return out
}

// TestDictCodecCacheIsBounded: the dictionary codec cache is keyed by bytes a
// file supplies, so a sequence of files carrying distinct dictionaries must
// not pin one codec each for the life of the process.
func TestDictCodecCacheIsBounded(t *testing.T) {
	const n = 64
	for i := range n {
		if _, err := sharedDictCodec(hostileDict(t, 10_000+i), CompressionDefault); err != nil {
			t.Fatal(err)
		}
	}
	dictMu.Lock()
	held, weight := dictCodecs.Len(), dictCodecs.Weight()
	dictMu.Unlock()
	// Absolute figures rather than the constants: a bound asserted against its
	// own constant moves with the constant and cannot fail.
	if held > 16 {
		t.Fatalf("%d codecs held after %d distinct dictionaries, bound is 16", held, n)
	}
	if weight > 16<<20 {
		t.Fatalf("cached dictionaries weigh %d bytes, budget is %d", weight, 16<<20)
	}
	if held == 0 {
		t.Fatal("the cache holds nothing at all: it is not a cache, and every open re-parses")
	}
	// The constants are what the recorded measurement was taken against, so
	// moving them means re-measuring.
	if dictCacheEntries != 16 || dictCacheBytes != 16<<20 {
		t.Fatalf("the dictionary bounds moved to %d entries / %d bytes; re-measure before trusting the old figures",
			dictCacheEntries, dictCacheBytes)
	}
}

// TestEvictedDictCodecStaysUsable: eviction unlinks a codec, it does not close
// one. A handle that took a codec before it was evicted keeps decoding through
// it, and nothing the cache does may reach inside it. Run this under -race:
// the failure it is looking for is a decoder being torn down under a reader.
func TestEvictedDictCodecStaysUsable(t *testing.T) {
	reg := testRegistry(t)
	path := filepath.Join(t.TempDir(), "overworld.pile")
	w, err := CreateIndexed(path, reg, Options{Compression: CompressionDefault})
	if err != nil {
		t.Fatal(err)
	}
	for i := range int32(40) {
		if err := w.Store(buildTestColumn(t, reg, i, i%5)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Compact(); err != nil {
		t.Fatal(err)
	}
	if !w.HasDict() {
		t.Skip("this build cannot train a dictionary")
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	h, err := OpenIndexed(path, reg, true)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if h.dictCodec == nil {
		t.Fatal("the reopened handle holds no dictionary codec: nothing here is under test")
	}
	codec := h.dictCodec

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	// Four readers decoding through the codec the handle is holding.
	for r := range 4 {
		wg.Go(func() {
			for i := range int32(50) {
				x := (i * int32(r+1)) % 40
				got, err := h.Column(x, x%5)
				if err != nil {
					errs <- fmt.Errorf("read (%d,%d) after eviction: %w", x, x%5, err)
					return
				}
				if len(got.Col.Chunk.Sub()) == 0 {
					errs <- fmt.Errorf("read (%d,%d) decoded to nothing", x, x%5)
					return
				}
			}
		})
	}
	// Meanwhile, enough distinct dictionaries to evict the live one many
	// times over. The count is absolute rather than a multiple of the bound,
	// so raising the bound does not raise it too.
	wg.Go(func() {
		for i := range 64 {
			if _, err := sharedDictCodec(hostileDict(t, 20_000+i), CompressionDefault); err != nil {
				errs <- err
				return
			}
		}
	})
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	// The live codec was indeed evicted, or the test proved nothing: a cache
	// that never dropped it would exercise no eviction at all.
	dictMu.Lock()
	evicted := true
	for _, c := range dictCodecs.Values() {
		if c == codec {
			evicted = false
		}
	}
	dictMu.Unlock()
	if !evicted {
		t.Fatal("the codec under test was never evicted; the fixture does not reach the case")
	}
	// And it still decodes, after the cache has forgotten it entirely.
	if _, err := h.Column(3, 3); err != nil {
		t.Fatalf("read through an evicted codec: %v", err)
	}
}
