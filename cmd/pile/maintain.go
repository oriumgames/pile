package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/df-mc/dragonfly/server/world"
	"github.com/oriumgames/pile"
	"github.com/oriumgames/pile/format"
)

func cmdCompact(args []string) error {
	fs := flag.NewFlagSet("compact", flag.ContinueOnError)
	limit := addDecodeLimit(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: pile compact <dir|file> [--max-decoded n]")
	}
	files, err := pileFiles(fs.Arg(0))
	if err != nil {
		return err
	}
	world.DefaultBlockRegistry.Finalize()
	for _, f := range files {
		mode, err := pile.FileMode(f)
		if err != nil {
			return err
		}
		if mode != format.ModeIndexed {
			// "already canonical" is a claim about the file, so the file has to
			// be read before it is made. pile.FileMode checks the magic and
			// nothing else, so twelve bytes of "PILE" followed by zeroes used to
			// be reported as a canonical solid world.
			data, err := os.ReadFile(f)
			if err != nil {
				return err
			}
			if _, err := format.ReadMeta(data, limit.readOpts()...); err != nil {
				return fmt.Errorf("%s: %w", f, err)
			}
			fmt.Printf("%s: solid file, already canonical\n", f)
			continue
		}
		before, _ := os.Stat(f)
		w, err := format.OpenIndexed(f, world.DefaultBlockRegistry, false, limit.readOpts()...)
		if err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		ratio := w.GarbageRatio()
		if err := w.Compact(); err != nil {
			_ = w.Close()
			return fmt.Errorf("%s: %w", f, err)
		}
		if err := w.Close(); err != nil {
			return err
		}
		after, _ := os.Stat(f)
		fmt.Printf("%s: compacted (garbage %.0f%%, %d -> %d bytes)\n", f, ratio*100, before.Size(), after.Size())
	}
	return nil
}

func cmdMode(args []string) error {
	fs := flag.NewFlagSet("mode", flag.ContinueOnError)
	limit := addDecodeLimit(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 || (fs.Arg(1) != "solid" && fs.Arg(1) != "indexed") {
		return errors.New("usage: pile mode <dir|file> <solid|indexed> [--max-decoded n]")
	}
	files, err := pileFiles(fs.Arg(0))
	if err != nil {
		return err
	}
	reg := world.DefaultBlockRegistry
	reg.Finalize()
	want := fs.Arg(1)
	wantIndexed := want == "indexed"

	for _, f := range files {
		mode, err := pile.FileMode(f)
		if err != nil {
			return err
		}
		if (mode == format.ModeIndexed) == wantIndexed {
			fmt.Printf("%s: already %s\n", f, want)
			continue
		}
		tmp := f + ".mode"
		_ = os.Remove(tmp)
		if wantIndexed {
			err = solidToIndexed(f, tmp, reg, limit)
		} else {
			err = indexedToSolid(f, tmp, reg, limit)
		}
		if err != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("%s: %w", f, err)
		}
		if err := preserveMode(tmp, f); err != nil {
			return err
		}
		if err := os.Rename(tmp, f); err != nil {
			return err
		}
		fmt.Printf("%s: converted to %s\n", f, want)
	}
	return nil
}

func solidToIndexed(src, dst string, reg world.BlockRegistry, limit decodeLimit) error {
	file, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	d, err := format.ReadWorld(file, reg, limit.readOpts()...)
	if err != nil {
		return err
	}
	// Mode conversion preserves everything the header says, the dimension
	// included: a nether file converted to indexed is still the nether.
	w, err := format.CreateIndexed(dst, reg, format.Options{
		Compression: format.CompressionDefault, Dimension: d.Dimension,
	})
	if err != nil {
		return err
	}
	for _, c := range d.Columns {
		if err := w.Store(c); err != nil {
			_ = w.Close()
			return err
		}
	}
	if err := w.SetMeta(d.Settings, d.UserData, d.Markers, d.Border); err != nil {
		_ = w.Close()
		return err
	}
	return w.Close()
}

func indexedToSolid(src, dst string, reg world.BlockRegistry, limit decodeLimit) error {
	w, err := format.OpenIndexed(src, reg, true, limit.readOpts()...)
	if err != nil {
		return err
	}
	defer w.Close()
	d := &format.WorldData{Dimension: w.Dimension()}
	d.Settings, d.UserData, d.Markers, d.Border = w.Meta()
	for _, k := range w.Positions() {
		c, err := w.Column(k[0], k[1])
		if err != nil {
			return err
		}
		d.Columns = append(d.Columns, c)
	}
	out, err := createStaged(dst)
	if err != nil {
		return err
	}
	if err := format.WriteWorld(out, d, reg, format.Options{Compression: format.CompressionBest}); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
