package main

import (
	"errors"
	"flag"
	"fmt"
	"math"
	"os"

	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/oriumgames/pile"
	"github.com/oriumgames/pile/format"
)

func cmdMove(args []string) error {
	fs := flag.NewFlagSet("move", flag.ContinueOnError)
	byFlag := fs.String("by", "", "translate by offset dx,dy,dz")
	spawnTo := fs.String("spawn-to", "", "translate so the spawn point lands at x,y,z")
	center := fs.Bool("center", false, "translate so the world's bounds center on x=0,z=0")
	clip := fs.Bool("clip", false, "allow cutting content that leaves the vertical range")
	dryRun := fs.Bool("dry-run", false, "report what would happen without writing")
	noBackup := fs.Bool("no-backup", false, "skip the automatic snapshots/pre-move backup")
	limit := addDecodeLimit(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	modes := 0
	for _, set := range []bool{*byFlag != "", *spawnTo != "", *center} {
		if set {
			modes++
		}
	}
	if fs.NArg() != 1 || modes != 1 {
		return errors.New("usage: pile move <world-dir> (--by dx,dy,dz | --spawn-to x,y,z | --center) [--clip] [--dry-run] [--no-backup]")
	}
	dir := fs.Arg(0)

	var offset cube.Pos
	switch {
	case *byFlag != "":
		p, err := parsePos(*byFlag)
		if err != nil {
			return err
		}
		offset = p
	case *spawnTo != "":
		target, err := parsePos(*spawnTo)
		if err != nil {
			return err
		}
		p, err := pile.Open(dir, append(limit.providerOpts(), pile.ReadOnly())...)
		if err != nil {
			return err
		}
		spawn := p.Settings().Spawn
		_ = p.Close()
		offset = target.Sub(spawn)
	case *center:
		p, err := pile.Open(dir, append(limit.providerOpts(), pile.ReadOnly())...)
		if err != nil {
			return err
		}
		min, max, ok := pile.WorldBounds(p, world.Overworld)
		_ = p.Close()
		if !ok {
			return errors.New("world has no blocks to center")
		}
		offset = cube.Pos{-(min.X() + max.X()) / 2, 0, -(min.Z() + max.Z()) / 2}
	}
	if offset == (cube.Pos{}) {
		fmt.Println("offset is 0,0,0; nothing to do")
		return nil
	}

	report, err := pile.MoveWorld(dir, pile.MoveOptions{
		Offset: offset, Clip: *clip, DryRun: *dryRun, Backup: !*noBackup,
		MaxDecoded: limit.value(),
	})
	if errors.Is(err, pile.ErrWouldClip) {
		fmt.Printf("refused: moving by (%d,%d,%d) would clip %d blocks, %d block entities, %d entities, %d scheduled ticks outside the vertical range\n",
			offset.X(), offset.Y(), offset.Z(),
			report.ClippedBlocks, report.ClippedBlockEntities, report.ClippedEntities, report.ClippedTicks)
		fmt.Println("re-run with --clip to cut that content")
		os.Exit(1)
	} else if err != nil {
		return err
	}

	action := "moved"
	if *dryRun {
		action = "would move"
	}
	path := "block-level rewrite"
	if report.FastPath {
		path = "chunk-aligned fast path"
	}
	fmt.Printf("%s %d chunks by (%d,%d,%d) (%s)\n", action, report.Chunks, offset.X(), offset.Y(), offset.Z(), path)
	if report.ClippedTotal() > 0 {
		fmt.Printf("clipped: %d blocks, %d block entities, %d entities, %d ticks\n",
			report.ClippedBlocks, report.ClippedBlockEntities, report.ClippedEntities, report.ClippedTicks)
	}
	if !*dryRun && !*noBackup {
		fmt.Println("backup written to snapshots/pre-move")
	}
	return nil
}

func cmdOrigin(args []string) error {
	fs := flag.NewFlagSet("origin", flag.ContinueOnError)
	set := fs.String("set", "", "set the paste anchor to x,y,z")
	zero := fs.Bool("zero", false, "reset the paste anchor to 0,0,0")
	center := fs.Bool("center", false, "anchor at the structure's XZ center, bottom Y")
	limit := addDecodeLimit(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	modes := 0
	for _, m := range []bool{*set != "", *zero, *center} {
		if m {
			modes++
		}
	}
	if fs.NArg() != 1 || modes != 1 {
		return errors.New("usage: pile origin <structure.pile> (--set x,y,z | --zero | --center)")
	}
	path := fs.Arg(0)

	world.DefaultBlockRegistry.Finalize()
	file, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	data, err := format.ReadStructure(file, world.DefaultBlockRegistry, limit.readOpts()...)
	if err != nil {
		return err
	}
	old := data.Origin
	switch {
	case *set != "":
		p, err := parsePos(*set)
		if err != nil {
			return err
		}
		// The anchor is three int32s on the wire. A value that does not fit was
		// narrowed silently, so `--set 4294967296,0,0` rewrote the file, set the
		// anchor to 0 and printed that it had set it to 0 — a wrong answer
		// reported as success, over the file it had just replaced.
		for i, v := range [3]int{p.X(), p.Y(), p.Z()} {
			if v < math.MinInt32 || v > math.MaxInt32 {
				return fmt.Errorf("origin axis %d is %d, which does not fit the format's 32-bit field", i, v)
			}
		}
		data.Origin = [3]int32{int32(p.X()), int32(p.Y()), int32(p.Z())}
	case *zero:
		data.Origin = [3]int32{}
	case *center:
		data.Origin = [3]int32{-data.Size[0] / 2, 0, -data.Size[2] / 2}
	}

	tmp := path + ".tmp"
	f, err := createStaged(tmp)
	if err != nil {
		return err
	}
	err = format.WriteStructure(f, data, world.DefaultBlockRegistry, format.Options{Compression: format.CompressionBest})
	if err2 := f.Sync(); err == nil {
		err = err2
	}
	if err2 := f.Close(); err == nil {
		err = err2
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := preserveMode(tmp, path); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	fmt.Printf("origin: %v -> %v\n", old, data.Origin)
	return nil
}
