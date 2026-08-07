package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/df-mc/dragonfly/server/world"
	"github.com/oriumgames/pile"
	"github.com/oriumgames/pile/format"
)

func cmdCompact(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: pile compact <dir|file>")
	}
	files, err := pileFiles(args[0])
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
			fmt.Printf("%s: solid file, already canonical\n", f)
			continue
		}
		before, _ := os.Stat(f)
		w, err := format.OpenIndexed(f, world.DefaultBlockRegistry, false)
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
	if len(args) != 2 || (args[1] != "solid" && args[1] != "indexed") {
		return errors.New("usage: pile mode <dir|file> <solid|indexed>")
	}
	files, err := pileFiles(args[0])
	if err != nil {
		return err
	}
	reg := world.DefaultBlockRegistry
	reg.Finalize()
	wantIndexed := args[1] == "indexed"

	for _, f := range files {
		mode, err := pile.FileMode(f)
		if err != nil {
			return err
		}
		if (mode == format.ModeIndexed) == wantIndexed {
			fmt.Printf("%s: already %s\n", f, args[1])
			continue
		}
		tmp := f + ".mode"
		_ = os.Remove(tmp)
		if wantIndexed {
			err = solidToIndexed(f, tmp, reg)
		} else {
			err = indexedToSolid(f, tmp, reg)
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
		fmt.Printf("%s: converted to %s\n", f, args[1])
	}
	return nil
}

func solidToIndexed(src, dst string, reg world.BlockRegistry) error {
	file, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	d, err := format.ReadWorld(file, reg)
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

func indexedToSolid(src, dst string, reg world.BlockRegistry) error {
	w, err := format.OpenIndexed(src, reg, true)
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
