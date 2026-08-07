package main

import (
	"errors"
	"flag"
	"fmt"

	"github.com/oriumgames/pile"
)

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

func isMcdb(dir string) bool { return pile.IsMCDB(dir) }

func isPile(dir string) bool { return pile.IsPile(dir) }

// convertMcdbToPile is pile.ImportMCDB plus the line the CLI prints.
func convertMcdbToPile(src, dst string) error {
	n, err := pile.ImportMCDB(src, dst)
	if err != nil {
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
