package main

import (
	"errors"
	"flag"
	"fmt"
	"github.com/df-mc/dragonfly/server/world"
	"strings"

	"github.com/oriumgames/pile"
)

func cmdConvert(args []string) error {
	fs := flag.NewFlagSet("convert", flag.ContinueOnError)
	permissive := fs.Bool("permissive", false,
		"register block states the registry does not know, so a world using a behaviour pack converts")
	limit := addDecodeLimit(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return errors.New("usage: pile convert [--permissive] <src> <dst> [--max-decoded n]")
	}
	src, dst := fs.Arg(0), fs.Arg(1)
	switch {
	case isMcdb(src):
		return convertMcdbToPile(src, dst, *permissive)
	case isPile(src):
		return convertPileToMcdb(src, dst, limit)
	default:
		return fmt.Errorf("%s is neither an mcdb world (level.dat) nor a pile world (*.pile)", src)
	}
}

func isMcdb(dir string) bool { return pile.IsMCDB(dir) }

func isPile(dir string) bool { return pile.IsPile(dir) }

// convertMcdbToPile is pile.ImportMCDB plus the line the CLI prints.
//
// Under --permissive the source's own palettes are scanned first and every
// state the registry does not know is registered as a placeholder, which is
// what lets a world built on a behaviour pack convert at all: dragonfly's chunk
// decoder fails outright on a state it cannot resolve, so one block from a pack
// stops the conversion at whichever chunk holds it.
func convertMcdbToPile(src, dst string, permissive bool) error {
	// The process-wide registry, not a clone. world.NewBlockRegistry would be
	// the better answer -- placeholders belong to one conversion rather than to
	// the process -- but in dragonfly v0.11.1 it panics on Finalize: Clone
	// resets bitSize to 0 and the block hashes then collide ("block Stairs...
	// already registered by block.Stairs..."). Until that is fixed upstream a
	// converter gets one shot at the registry, which is fine for a CLI whose
	// process ends with the conversion.
	reg := world.DefaultBlockRegistry
	if permissive {
		added, err := pile.RegisterMCDBStates(src, reg)
		if err != nil {
			return err
		}
		if added > 0 {
			fmt.Printf("registered %d block states this build does not know\n", added)
		}
	}
	reg.Finalize()
	n, err := pile.ImportMCDB(src, dst, pile.Registry(reg))
	if err != nil {
		if !permissive && strings.Contains(err.Error(), "cannot get runtime ID of block state") {
			return fmt.Errorf("%w\n\nthis world uses blocks this build does not know, most likely from a "+
				"behaviour pack.\nre-run with --permissive to convert it anyway, or `pile blocks %s` to "+
				"see what it uses", err, src)
		}
		return err
	}
	fmt.Printf("converted %d chunks: %s -> %s\n", n, src, dst)
	return nil
}

// convertPileToMcdb is pile.ExportMCDB plus the line the CLI prints.
func convertPileToMcdb(src, dst string, limit decodeLimit) error {
	n, err := pile.ExportMCDB(src, dst, limit.providerOpts()...)
	if err != nil {
		return err
	}
	fmt.Printf("converted %d chunks: %s -> %s\n", n, src, dst)
	return nil
}
