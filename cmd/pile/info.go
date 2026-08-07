package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/df-mc/dragonfly/server/world"
	"github.com/oriumgames/pile"
	"github.com/oriumgames/pile/format"
)

// pileFiles resolves an argument to a list of pile files: either the file
// itself or all *.pile files of a directory.
func pileFiles(path string) ([]string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return []string{path}, nil
	}
	matches, err := filepath.Glob(filepath.Join(path, "*.pile"))
	if err != nil || len(matches) == 0 {
		return nil, fmt.Errorf("no .pile files in %s", path)
	}
	sort.Strings(matches)
	return matches, nil
}

func cmdInspect(args []string) error {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	limit := addDecodeLimit(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: pile inspect <file.pile> [--max-decoded n]")
	}
	files, err := pileFiles(fs.Arg(0))
	if err != nil {
		return err
	}
	for _, f := range files {
		mode, err := pile.FileMode(f)
		if err != nil {
			return err
		}
		if mode == format.ModeIndexed {
			if err := inspectIndexed(f, limit); err != nil {
				return err
			}
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		m, err := format.ReadMeta(data, limit.readOpts()...)
		if err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		fmt.Printf("%s:\n", f)
		fmt.Printf("  size          %d bytes\n", len(data))
		fmt.Printf("  kind          %s\n", kindName(m.Kind))
		fmt.Printf("  mode          %s\n", modeName(m.Mode))
		fmt.Printf("  flags         0x%08X\n", m.Flags)
		fmt.Printf("  block version %d\n", m.BlockVersion)
		printNBTBlob("settings", m.Settings)
		fmt.Printf("  user data     %d bytes\n", len(m.UserData))
		printNBTBlob("markers", m.Markers)
	}
	return nil
}

func inspectIndexed(f string, limit decodeLimit) error {
	world.DefaultBlockRegistry.Finalize()
	w, err := format.OpenIndexed(f, world.DefaultBlockRegistry, true, limit.readOpts()...)
	if err != nil {
		return fmt.Errorf("%s: %w", f, err)
	}
	defer w.Close()
	st, _ := os.Stat(f)
	settings, userData, markers, _ := w.Meta()
	fmt.Printf("%s:\n", f)
	fmt.Printf("  size          %d bytes\n", st.Size())
	fmt.Printf("  kind          world\n")
	fmt.Printf("  mode          indexed\n")
	fmt.Printf("  generation    %d\n", w.Generation())
	fmt.Printf("  chunks        %d\n", w.ChunkCount())
	fmt.Printf("  garbage       %.0f%%\n", w.GarbageRatio()*100)
	printNBTBlob("settings", settings)
	fmt.Printf("  user data     %d bytes\n", len(userData))
	printNBTBlob("markers", markers)
	return nil
}

func kindName(k uint8) string {
	switch k {
	case format.KindWorld:
		return "world"
	case format.KindStructure:
		return "structure"
	}
	return fmt.Sprintf("unknown (%d)", k)
}

func modeName(m uint8) string {
	switch m {
	case format.ModeSolid:
		return "solid"
	case format.ModeIndexed:
		return "indexed"
	}
	return fmt.Sprintf("unknown (%d)", m)
}

func printNBTBlob(name string, b []byte) {
	if len(b) == 0 {
		fmt.Printf("  %-13s (none)\n", name)
		return
	}
	m, err := format.UnmarshalNBT(b)
	if err != nil {
		fmt.Printf("  %-13s %d bytes (undecodable: %v)\n", name, len(b), err)
		return
	}
	fmt.Printf("  %-13s %v\n", name, m)
}

func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	limit := addDecodeLimit(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: pile verify <dir|file> [--max-decoded n]")
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
		if mode == format.ModeIndexed {
			w, err := format.OpenIndexed(f, world.DefaultBlockRegistry, true, limit.readOpts()...)
			if err != nil {
				return fmt.Errorf("%s: %w", f, err)
			}
			n := 0
			for _, k := range w.Positions() {
				if _, err := w.Column(k[0], k[1]); err != nil {
					_ = w.Close()
					return fmt.Errorf("%s: chunk (%d,%d): %w", f, k[0], k[1], err)
				}
				n++
			}
			_ = w.Close()
			fmt.Printf("%s: ok (%d chunks, indexed)\n", f, n)
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		d, err := format.ReadWorld(data, world.DefaultBlockRegistry, limit.readOpts()...)
		if err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		fmt.Printf("%s: ok (%d chunks)\n", f, len(d.Columns))
	}
	return nil
}

func cmdStats(args []string) error {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	limit := addDecodeLimit(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: pile stats <dir|file> [--max-decoded n]")
	}
	files, err := pileFiles(fs.Arg(0))
	if err != nil {
		return err
	}
	world.DefaultBlockRegistry.Finalize()
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		var d *format.WorldData
		if mode, err := pile.FileMode(f); err != nil {
			return err
		} else if mode == format.ModeIndexed {
			w, err := format.OpenIndexed(f, world.DefaultBlockRegistry, true, limit.readOpts()...)
			if err != nil {
				return fmt.Errorf("%s: %w", f, err)
			}
			d = &format.WorldData{}
			for _, k := range w.Positions() {
				c, err := w.Column(k[0], k[1])
				if err != nil {
					_ = w.Close()
					return fmt.Errorf("%s: chunk (%d,%d): %w", f, k[0], k[1], err)
				}
				d.Columns = append(d.Columns, c)
			}
			_ = w.Close()
		} else if d, err = format.ReadWorld(data, world.DefaultBlockRegistry, limit.readOpts()...); err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		var ents, bes, ticks, sections int
		for _, c := range d.Columns {
			ents += len(c.Col.Entities)
			bes += len(c.Col.BlockEntities)
			ticks += len(c.Col.ScheduledBlocks)
			for _, sub := range c.Col.Chunk.Sub() {
				if !sub.Empty() {
					sections++
				}
			}
		}
		fmt.Printf("%s:\n", f)
		fmt.Printf("  file size        %d bytes\n", len(data))
		fmt.Printf("  chunks           %d\n", len(d.Columns))
		fmt.Printf("  filled sections  %d\n", sections)
		fmt.Printf("  entities         %d\n", ents)
		fmt.Printf("  block entities   %d\n", bes)
		fmt.Printf("  scheduled ticks  %d\n", ticks)
		if len(d.Columns) > 0 {
			fmt.Printf("  bytes/chunk      %.1f\n", float64(len(data))/float64(len(d.Columns)))
		}
	}
	return nil
}
