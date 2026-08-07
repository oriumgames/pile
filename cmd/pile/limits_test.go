package main

import (
	"errors"
	"flag"
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
		"0": 0, "1": 1, "1024": 1024,
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
	for _, in := range []string{"", "  ", "-1", "MiB", "1.5MiB", "8EiB", "9223372036854775807GiB"} {
		if got, err := parseSize(in); err == nil {
			t.Errorf("parseSize(%q) = %d, want an error", in, got)
		}
	}
}

// TestDecodeLimitFlagTakesSuffixes drives the flag the way a command does,
// because parseSize passing says nothing about whether the flag reaches it.
func TestDecodeLimitFlagTakesSuffixes(t *testing.T) {
	parse := func(t *testing.T, args ...string) (decodeLimit, error) {
		t.Helper()
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.SetOutput(os.NewFile(0, os.DevNull))
		l := addDecodeLimit(fs)
		return l, fs.Parse(args)
	}
	for _, c := range []struct {
		args []string
		want int64
	}{
		{[]string{"--max-decoded=32MiB"}, 32 << 20},
		{[]string{"--max-decoded", "32MiB"}, 32 << 20},
		{[]string{"-max-decoded=64"}, 64},
		{nil, 0},
	} {
		limit, err := parse(t, c.args...)
		if err != nil {
			t.Fatalf("%v: %v", c.args, err)
		}
		if got := limit.value(); got != c.want {
			t.Errorf("%v: value = %d, want %d", c.args, got, c.want)
		}
	}
	if _, err := parse(t, "--max-decoded=banana"); err == nil {
		t.Error("--max-decoded=banana was accepted")
	}
}

// TestDecodeLimitBoundsARealDecode is the control that matters: it proves the
// flag reaches a decode rather than merely parsing into a variable. Without it
// the plumbing could be wired to nothing and every other test here would still
// pass.
func TestDecodeLimitBoundsARealDecode(t *testing.T) {
	reg := world.DefaultBlockRegistry
	reg.Finalize()

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

	path := filepath.Join(t.TempDir(), "w.pile")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := format.WriteWorld(f, d, reg, format.Options{Compression: format.CompressionNone}); err != nil {
		t.Fatal(err)
	}
	f.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	limit := func(args ...string) decodeLimit {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.SetOutput(os.NewFile(0, os.DevNull))
		l := addDecodeLimit(fs)
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		return l
	}

	if _, err := format.ReadWorld(data, reg, limit().readOpts()...); err != nil {
		t.Fatalf("the default ceiling refused a file this test just wrote: %v", err)
	}

	_, err = format.ReadWorld(data, reg, limit("--max-decoded=1").readOpts()...)
	if !errors.Is(err, format.ErrDecodeBudget) {
		t.Fatalf("with a 1-byte ceiling: err = %v, want ErrDecodeBudget", err)
	}
	// A policy refusal must not claim the file is corrupt: an operator who set
	// the ceiling too low needs to tell that apart from a bad file, and so does
	// any tooling that branches on it.
	if errors.Is(err, format.ErrCorrupt) {
		t.Error("a budget refusal reported the file as corrupt")
	}
}
