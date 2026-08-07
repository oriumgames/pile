package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/df-mc/dragonfly/server/block"
	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
	"github.com/oriumgames/pile/format"
)

func TestParseSize(t *testing.T) {
	ok := map[string]int64{
		"1": 1, "1024": 1024,
		"1KiB": 1 << 10, "8MiB": 8 << 20, "2GiB": 2 << 30,
		"1KB": 1000, "5MB": 5_000_000,
		"64M": 64 << 20, "3G": 3 << 30,
		"  16MiB  ": 16 << 20, "16mib": 16 << 20,
	}
	for in, want := range ok {
		got, err := parseSize(in)
		if err != nil {
			t.Errorf("parseSize(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseSize(%q) = %d, want %d", in, got, want)
		}
	}
	for _, in := range []string{"", "  ", "0", "-1", "MiB", "1.5MiB", "8EiB", "9223372036854775807GiB"} {
		if got, err := parseSize(in); err == nil {
			t.Errorf("parseSize(%q) = %d, want an error", in, got)
		}
	}
}

func TestStripGlobalFlags(t *testing.T) {
	t.Cleanup(func() { maxDecoded = 0 })

	for _, form := range [][]string{
		{"--max-decoded=32MiB", "verify", "w"},
		{"--max-decoded", "32MiB", "verify", "w"},
		{"-max-decoded=32MiB", "verify", "w"},
	} {
		maxDecoded = 0
		rest, err := stripGlobalFlags(form)
		if err != nil {
			t.Fatalf("%v: %v", form, err)
		}
		if maxDecoded != 32<<20 {
			t.Errorf("%v: maxDecoded = %d, want %d", form, maxDecoded, 32<<20)
		}
		if len(rest) != 2 || rest[0] != "verify" || rest[1] != "w" {
			t.Errorf("%v: rest = %v, want [verify w]", form, rest)
		}
	}

	// A command's own flags must survive untouched, including one that looks
	// like a global flag but arrives after the command.
	maxDecoded = 0
	rest, err := stripGlobalFlags([]string{"render", "w", "--max-decoded=1MiB"})
	if err != nil {
		t.Fatal(err)
	}
	if maxDecoded != 0 {
		t.Errorf("a flag after the command set the ceiling to %d", maxDecoded)
	}
	if len(rest) != 3 {
		t.Errorf("rest = %v, want all three arguments", rest)
	}

	if _, err := stripGlobalFlags([]string{"--max-decoded"}); err == nil {
		t.Error("a bare --max-decoded with no size was accepted")
	}
	if _, err := stripGlobalFlags([]string{"--max-decoded=banana", "verify"}); err == nil {
		t.Error("--max-decoded=banana was accepted")
	}
}

// TestReadOptsBoundsARealDecode is the one that matters: it proves the flag
// reaches an actual decode rather than merely parsing. Without it the plumbing
// could be wired to nothing and every other test here would still pass.
func TestReadOptsBoundsARealDecode(t *testing.T) {
	t.Cleanup(func() { maxDecoded = 0 })
	reg := world.DefaultBlockRegistry
	reg.Finalize()

	dir := t.TempDir()
	path := filepath.Join(dir, "w.pile")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	ch := chunk.New(reg, cube.Range{-64, 319})
	stone := reg.BlockRuntimeID(block.Stone{})
	for bx := range uint8(16) {
		for bz := range uint8(16) {
			ch.SetBlock(bx, 0, bz, 0, stone)
		}
	}
	d := &format.WorldData{Columns: []format.Column{
		{X: 0, Z: 0, Col: &chunk.Column{Chunk: ch}},
	}}
	if err := format.WriteWorld(f, d, reg, format.Options{Compression: format.CompressionNone}); err != nil {
		t.Fatal(err)
	}
	f.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	maxDecoded = 0
	if _, err := format.ReadWorld(data, reg, readOpts()...); err != nil {
		t.Fatalf("default ceiling refused a file this test just wrote: %v", err)
	}

	maxDecoded = 1
	_, err = format.ReadWorld(data, reg, readOpts()...)
	if !errors.Is(err, format.ErrDecodeBudget) {
		t.Fatalf("with a 1-byte ceiling: err = %v, want ErrDecodeBudget", err)
	}
	// A policy refusal must not claim the file is corrupt: an operator who
	// set the ceiling too low needs to be able to tell that apart from a bad
	// file, and so does any tooling that branches on it.
	if errors.Is(err, format.ErrCorrupt) {
		t.Error("a budget refusal reported the file as corrupt")
	}
}
