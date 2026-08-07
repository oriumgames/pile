package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/mcdb"
	"github.com/oriumgames/pile"
)

var dimensions = []world.Dimension{world.Overworld, world.Nether, world.End}

func cmdConvert(args []string) error {
	fs := flag.NewFlagSet("convert", flag.ContinueOnError)
	limit := addDecodeLimit(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return errors.New("usage: pile convert <src> <dst> [--max-decoded n]")
	}
	src, dst := fs.Arg(0), fs.Arg(1)
	switch {
	case isMcdb(src):
		return convertMcdbToPile(src, dst)
	case isPile(src):
		return convertPileToMcdb(src, dst, limit)
	default:
		return fmt.Errorf("%s is neither an mcdb world (level.dat) nor a pile world (*.pile)", src)
	}
}

func isMcdb(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "level.dat"))
	return err == nil
}

func isPile(dir string) bool {
	matches, _ := filepath.Glob(filepath.Join(dir, "*.pile"))
	return len(matches) > 0
}

// convertMcdbToPile reads every column of every dimension from a leveldb
// world and writes a fresh pile world. Output is garbage-free by
// construction: only live columns are visited and the pile save is a
// canonical solid write.
func convertMcdbToPile(src, dst string) error {
	if isPile(dst) {
		return fmt.Errorf("destination %s already contains a pile world; refusing to write into it", dst)
	}
	db, err := mcdb.Open(src)
	if err != nil {
		return fmt.Errorf("open mcdb %s: %w", src, err)
	}
	defer db.Close()

	p, err := pile.Open(dst)
	if err != nil {
		return err
	}
	total := 0
	iter := db.NewColumnIterator(nil)
	defer iter.Release()
	for iter.Next() {
		if err := p.StoreColumn(iter.Position(), iter.Dimension(), iter.Column()); err != nil {
			return fmt.Errorf("store column %v: %w", iter.Position(), err)
		}
		total++
	}
	if err := iter.Error(); err != nil {
		return fmt.Errorf("iterate mcdb: %w", err)
	}
	p.SaveSettings(db.Settings())
	if err := p.Close(); err != nil {
		return err
	}
	fmt.Printf("converted %d chunks: %s -> %s\n", total, src, dst)
	return nil
}

// convertPileToMcdb writes a pile world into a freshly created leveldb world.
// The destination must not already contain an mcdb world, so the result holds
// only live keys.
func convertPileToMcdb(src, dst string, limit decodeLimit) error {
	if isMcdb(dst) {
		return fmt.Errorf("destination %s already contains an mcdb world; refusing to write into it", dst)
	}
	p, err := pile.Open(src, append(limit.providerOpts(), pile.ReadOnly())...)
	if err != nil {
		return err
	}
	defer p.Close()

	db, err := mcdb.Open(dst)
	if err != nil {
		return fmt.Errorf("create mcdb %s: %w", dst, err)
	}
	total := 0
	for _, dim := range dimensions {
		for pos, col := range p.Columns(dim) {
			if err := db.StoreColumn(pos, dim, col); err != nil {
				_ = db.Close()
				return fmt.Errorf("store column %v (%v): %w", pos, dim, err)
			}
			total++
		}
		// A short iteration would silently drop chunks from the output.
		if err := p.IterError(); err != nil {
			_ = db.Close()
			return err
		}
	}
	db.SaveSettings(p.Settings())
	if err := db.Close(); err != nil {
		return err
	}
	fmt.Printf("converted %d chunks: %s -> %s\n", total, src, dst)
	return nil
}
