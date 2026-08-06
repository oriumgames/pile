package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/df-mc/dragonfly/server/world"
	"github.com/oriumgames/pile"
	"github.com/oriumgames/pile/format"
)

// canonicalColumnBytes encodes a single column as a canonical solid world,
// giving byte-comparable content identity.
func canonicalColumnBytes(c format.Column, reg world.BlockRegistry) ([]byte, error) {
	var buf bytes.Buffer
	err := format.WriteWorld(&buf, &format.WorldData{Columns: []format.Column{c}}, reg, format.Options{})
	return buf.Bytes(), err
}

// dimDiff is the chunk-level difference of one dimension.
type dimDiff struct {
	dim                      world.Dimension
	added, removed, modified [][2]int32
}

// diffWorlds computes chunk-level differences between two loaded worlds.
func diffWorlds(a, b *pile.WorldFiles, reg world.BlockRegistry) ([]dimDiff, error) {
	var out []dimDiff
	for _, dim := range dimensions {
		da, db := a.Dim(dim), b.Dim(dim)
		if da == nil && db == nil {
			continue
		}
		d := dimDiff{dim: dim}
		am := map[[2]int32][]byte{}
		if da != nil {
			for _, c := range da.Columns {
				enc, err := canonicalColumnBytes(c, reg)
				if err != nil {
					return nil, err
				}
				am[[2]int32{c.X, c.Z}] = enc
			}
		}
		seen := map[[2]int32]bool{}
		if db != nil {
			for _, c := range db.Columns {
				k := [2]int32{c.X, c.Z}
				seen[k] = true
				old, ok := am[k]
				if !ok {
					d.added = append(d.added, k)
					continue
				}
				enc, err := canonicalColumnBytes(c, reg)
				if err != nil {
					return nil, err
				}
				if !bytes.Equal(old, enc) {
					d.modified = append(d.modified, k)
				}
			}
		}
		for k := range am {
			if !seen[k] {
				d.removed = append(d.removed, k)
			}
		}
		for _, s := range [][][2]int32{d.added, d.removed, d.modified} {
			sort.Slice(s, func(i, j int) bool {
				return s[i][0] < s[j][0] || (s[i][0] == s[j][0] && s[i][1] < s[j][1])
			})
		}
		if len(d.added)+len(d.removed)+len(d.modified) > 0 {
			out = append(out, d)
		}
	}
	return out, nil
}

func cmdDiff(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: pile diff <world-a> <world-b>")
	}
	reg := world.DefaultBlockRegistry
	reg.Finalize()
	a, err := pile.LoadWorldFiles(args[0], reg)
	if err != nil {
		return err
	}
	b, err := pile.LoadWorldFiles(args[1], reg)
	if err != nil {
		return err
	}
	diffs, err := diffWorlds(a, b, reg)
	if err != nil {
		return err
	}
	metaChanged := !bytes.Equal(a.Settings, b.Settings) || !bytes.Equal(a.UserData, b.UserData) ||
		!bytes.Equal(a.Markers, b.Markers) || !bytes.Equal(a.Border, b.Border)
	if len(diffs) == 0 && !metaChanged {
		fmt.Println("worlds are identical")
		return nil
	}
	if metaChanged {
		fmt.Println("metadata differs (settings, markers, user data or border)")
	}
	printPos := func(label string, s [][2]int32) {
		if len(s) == 0 {
			return
		}
		fmt.Printf("  %s %d:", label, len(s))
		for i, k := range s {
			if i == 8 {
				fmt.Printf(" ... (+%d more)", len(s)-8)
				break
			}
			fmt.Printf(" (%d,%d)", k[0], k[1])
		}
		fmt.Println()
	}
	for _, d := range diffs {
		name, _ := world.DimensionID(d.dim)
		fmt.Printf("dimension %d:\n", name)
		printPos("added", d.added)
		printPos("removed", d.removed)
		printPos("modified", d.modified)
	}
	return nil
}

func cmdPrune(args []string) error {
	fs := flag.NewFlagSet("prune", flag.ContinueOnError)
	boundsFlag := fs.String("bounds", "", "keep only chunks intersecting block box x1,z1,x2,z2 (required)")
	dryRun := fs.Bool("dry-run", false, "report without writing")
	noBackup := fs.Bool("no-backup", false, "skip the snapshots/pre-prune backup")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || *boundsFlag == "" {
		return errors.New("usage: pile prune <world> --bounds x1,z1,x2,z2 [--dry-run] [--no-backup]")
	}
	var x1, z1, x2, z2 int
	if _, err := fmt.Sscanf(*boundsFlag, "%d,%d,%d,%d", &x1, &z1, &x2, &z2); err != nil {
		return fmt.Errorf("invalid bounds %q: %w", *boundsFlag, err)
	}
	if x2 < x1 {
		x1, x2 = x2, x1
	}
	if z2 < z1 {
		z1, z2 = z2, z1
	}
	dir := fs.Arg(0)
	reg := world.DefaultBlockRegistry
	reg.Finalize()
	wf, err := pile.LoadWorldFiles(dir, reg)
	if err != nil {
		return err
	}
	dropped := 0
	for i := range wf.Dims {
		var kept []format.Column
		for _, c := range wf.Dims[i].Columns {
			bx0, bx1 := int(c.X)*16, int(c.X)*16+15
			bz0, bz1 := int(c.Z)*16, int(c.Z)*16+15
			if bx1 >= x1 && bx0 <= x2 && bz1 >= z1 && bz0 <= z2 {
				kept = append(kept, c)
			} else {
				dropped++
			}
		}
		wf.Dims[i].Columns = kept
	}
	if *dryRun {
		fmt.Printf("would drop %d chunks outside (%d,%d)..(%d,%d)\n", dropped, x1, z1, x2, z2)
		return nil
	}
	if dropped == 0 {
		fmt.Println("nothing to prune")
		return nil
	}
	if !*noBackup {
		if err := wf.Backup(dir, "pre-prune"); err != nil {
			return err
		}
	}
	if err := wf.Write(dir, reg); err != nil {
		return err
	}
	fmt.Printf("dropped %d chunks outside (%d,%d)..(%d,%d)\n", dropped, x1, z1, x2, z2)
	return nil
}

func cmdUpgrade(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: pile upgrade <dir|file>")
	}
	files, err := pileFiles(args[0])
	if err != nil {
		return err
	}
	reg := world.DefaultBlockRegistry
	reg.Finalize()
	for _, f := range files {
		mode, err := pile.FileMode(f)
		if err != nil {
			return err
		}
		if mode == format.ModeIndexed {
			w, err := format.OpenIndexed(f, reg, false)
			if err != nil {
				return fmt.Errorf("%s: %w", f, err)
			}
			// Compaction re-encodes every record and palette at the current
			// block version.
			if err := w.Compact(); err != nil {
				_ = w.Close()
				return fmt.Errorf("%s: %w", f, err)
			}
			if err := w.Close(); err != nil {
				return err
			}
			fmt.Printf("%s: upgraded (indexed, recompacted)\n", f)
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		m, err := format.ReadMeta(data)
		if err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		d, err := format.ReadWorld(data, reg)
		if err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		tmp := f + ".upgrade"
		out, err := os.Create(tmp)
		if err != nil {
			return err
		}
		err = format.WriteWorld(out, d, reg, format.Options{
			Compression: format.CompressionBest,
			StoreLight:  m.Flags&format.FlagStoreLight != 0,
			Stats:       m.Flags&format.FlagStats != 0,
		})
		if err2 := out.Sync(); err == nil {
			err = err2
		}
		if err2 := out.Close(); err == nil {
			err = err2
		}
		if err != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("%s: %w", f, err)
		}
		if err := os.Rename(tmp, f); err != nil {
			return err
		}
		fmt.Printf("%s: upgraded (block version %d -> current)\n", f, m.BlockVersion)
	}
	return nil
}

func cmdCheck(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: pile check <dir|file>")
	}
	files, err := pileFiles(args[0])
	if err != nil {
		return err
	}
	reg := world.DefaultBlockRegistry
	reg.Finalize()
	bad := false
	for _, f := range files {
		mode, err := pile.FileMode(f)
		if err != nil {
			return err
		}
		var unresolved []string
		if mode == format.ModeIndexed {
			w, err := format.OpenIndexed(f, reg, true)
			if err != nil {
				return fmt.Errorf("%s: %w", f, err)
			}
			unresolved, err = w.UnresolvedStates()
			_ = w.Close()
			if err != nil {
				return fmt.Errorf("%s: %w", f, err)
			}
		} else {
			data, err := os.ReadFile(f)
			if err != nil {
				return err
			}
			if unresolved, err = format.UnresolvedStates(data, reg); err != nil {
				return fmt.Errorf("%s: %w", f, err)
			}
		}
		if len(unresolved) == 0 {
			fmt.Printf("%s: all block states resolve\n", f)
			continue
		}
		bad = true
		fmt.Printf("%s: %d unresolved states (would load as placeholder blocks):\n", f, len(unresolved))
		for _, s := range unresolved {
			fmt.Printf("  %s\n", s)
		}
	}
	if bad {
		os.Exit(1)
	}
	return nil
}
