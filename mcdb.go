package pile

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/mcdb"
)

// mcdbDimensions is the set a conversion walks. dragonfly's provider interface
// is per-dimension and has no "every dimension" call, so the list is written
// out; a custom dimension has no leveldb encoding of its own and so cannot
// appear in one of these worlds.
var mcdbDimensions = []world.Dimension{world.Overworld, world.Nether, world.End}

// IsMCDB reports whether dir holds a dragonfly leveldb world.
func IsMCDB(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "level.dat"))
	return err == nil
}

// IsPile reports whether dir holds a pile world.
func IsPile(dir string) bool {
	matches, _ := filepath.Glob(filepath.Join(dir, "*.pile"))
	return len(matches) > 0
}

// ImportMCDB converts a dragonfly leveldb world into a fresh pile world and
// returns the number of columns converted.
//
// The output is garbage-free by construction: only live columns are visited,
// and a pile save is a canonical full rewrite, so the result is byte-identical
// to any other conversion of the same content. That is the property worth
// converting for -- an mcdb world accumulates dead keys that a copy preserves.
//
// dst must not already hold a pile world. Refusing rather than merging is
// deliberate: a conversion into an existing world would mix two worlds' columns
// and leave no way to tell which came from where.
//
// Options are the provider options the destination is opened with, so a caller
// can choose compression, append mode or a block registry. Reading the source
// takes none: it is a leveldb world and its cost is bounded by its own files.
func ImportMCDB(src, dst string, opts ...Option) (int, error) {
	if IsPile(dst) {
		return 0, fmt.Errorf("pile: %s already contains a pile world; refusing to write into it", dst)
	}
	// The source is read with the same registry the destination is written
	// with. mcdb.Open would use dragonfly's default, so a caller who passed
	// Registry() -- the only way to convert a world holding blocks from a
	// behaviour pack, see RegisterMCDBStates -- had their registry used for
	// one half of the conversion and ignored for the other.
	conf := defaultConfig()
	for _, o := range opts {
		o(&conf)
	}
	db, err := (mcdb.Config{Blocks: conf.registry}).Open(src)
	if err != nil {
		return 0, fmt.Errorf("pile: open mcdb %s: %w", src, err)
	}
	defer func() { _ = db.Close() }()

	p, err := Open(dst, opts...)
	if err != nil {
		return 0, err
	}
	// A conversion that fails must leave nothing, not a partial world.
	// Provider.Close saves, so closing on the error path is what wrote a
	// four-chunk world after a source that failed on its fifth chunk -- and a
	// partial world is worse than none, because it opens, renders and looks
	// like a finished one. The destination is safe to remove: this refuses to
	// write into an existing pile world above, so whatever is there now was
	// made by this call.
	// closed guards against Close being called twice: it is called once here
	// on the way out and once by abandon, and the second call must not undo
	// the first's error.
	closed := false
	abandon := func(err error) (int, error) {
		if !closed {
			_ = p.Close()
			closed = true
		}
		if rmErr := os.RemoveAll(dst); rmErr != nil {
			return 0, fmt.Errorf("%w (and the partial world could not be removed: %v)", err, rmErr)
		}
		return 0, err
	}
	total := 0
	it := db.NewColumnIterator(nil)
	defer it.Release()
	for it.Next() {
		if err := p.StoreColumn(it.Position(), it.Dimension(), it.Column()); err != nil {
			return abandon(fmt.Errorf("pile: store column %v: %w", it.Position(), err))
		}
		total++
	}
	if err := it.Error(); err != nil {
		return abandon(fmt.Errorf("pile: iterate mcdb: %w", err))
	}
	p.SaveSettings(db.Settings())
	// Close is where a solid world is written, so it is where validation
	// happens: every column above can store fine and the whole conversion
	// still fail here. That makes this an abandon path too, not a plain
	// return -- it was the one that left the partial world.
	closed = true
	if err := p.Close(); err != nil {
		return abandon(err)
	}
	return total, nil
}

// ExportMCDB converts a pile world into a freshly created leveldb world and
// returns the number of columns converted.
//
// dst must not already hold an mcdb world, so the result contains only live
// keys rather than whatever was there plus this world on top.
//
// Options are the provider options the *source* is opened with. A caller
// converting a world it did not write should pass MaxDecodedBytes: this walks
// every column of every dimension, so it is exactly the operation a hostile
// file makes expensive.
func ExportMCDB(src, dst string, opts ...Option) (int, error) {
	if IsMCDB(dst) {
		return 0, fmt.Errorf("pile: %s already contains an mcdb world; refusing to write into it", dst)
	}
	p, err := Open(src, append(opts, ReadOnly())...)
	if err != nil {
		return 0, err
	}
	defer func() { _ = p.Close() }()

	db, err := mcdb.Open(dst)
	if err != nil {
		return 0, fmt.Errorf("pile: create mcdb %s: %w", dst, err)
	}
	total := 0
	for _, dim := range mcdbDimensions {
		for pos, col := range p.Columns(dim) {
			if err := db.StoreColumn(pos, dim, col); err != nil {
				_ = db.Close()
				return total, fmt.Errorf("pile: store column %v (%v): %w", pos, dim, err)
			}
			total++
		}
		// A short iteration would silently drop chunks from the output, and a
		// conversion that loses part of a world without saying so is worse
		// than one that fails.
		if err := p.IterError(); err != nil {
			_ = db.Close()
			return total, err
		}
	}
	db.SaveSettings(p.Settings())
	if err := db.Close(); err != nil {
		return total, err
	}
	return total, nil
}
