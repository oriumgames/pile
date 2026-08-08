package main

import (
	"errors"
	"flag"
	"fmt"
	"strconv"
	"strings"

	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/oriumgames/pile"
)

// parsePos parses "x,y,z".
func parsePos(s string) (cube.Pos, error) {
	parts := strings.Split(s, ",")
	if len(parts) != 3 {
		return cube.Pos{}, fmt.Errorf("invalid position %q, want x,y,z", s)
	}
	var p cube.Pos
	for i, part := range parts {
		v, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil {
			return cube.Pos{}, fmt.Errorf("invalid position %q: %w", s, err)
		}
		p[i] = v
	}
	return p, nil
}

func parseDim(s string) (world.Dimension, error) {
	switch strings.ToLower(s) {
	case "", "overworld":
		return world.Overworld, nil
	case "nether":
		return world.Nether, nil
	case "end":
		return world.End, nil
	}
	return nil, fmt.Errorf("unknown dimension %q (overworld, nether, end)", s)
}

func cmdExtract(args []string) error {
	fs := flag.NewFlagSet("extract", flag.ContinueOnError)
	minFlag := fs.String("min", "", "minimum corner x,y,z (required)")
	maxFlag := fs.String("max", "", "maximum corner x,y,z (required)")
	dimFlag := fs.String("dim", "overworld", "dimension to extract from")
	skipAir := fs.Bool("skip-air", false, "mark the structure to leave air positions untouched when built")
	limit := addDecodeLimit(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 || *minFlag == "" || *maxFlag == "" {
		return errors.New("usage: pile extract --min x,y,z --max x,y,z [--dim overworld] [--skip-air] <world-dir> <out.pile>")
	}
	minPos, err := parsePos(*minFlag)
	if err != nil {
		return err
	}
	maxPos, err := parsePos(*maxFlag)
	if err != nil {
		return err
	}
	dim, err := parseDim(*dimFlag)
	if err != nil {
		return err
	}

	p, err := pile.Open(fs.Arg(0), append(limit.providerOpts(), pile.ReadOnly())...)
	if err != nil {
		return err
	}
	defer p.Close()
	var opts []pile.StructureOption
	if *skipAir {
		opts = append(opts, pile.SkipAir())
	}
	s, err := pile.ExtractStructure(p, dim, minPos, maxPos, opts...)
	if err != nil {
		return err
	}
	if err := s.Save(fs.Arg(1)); err != nil {
		return err
	}
	d := s.Dimensions()
	fmt.Printf("extracted %dx%dx%d structure to %s\n", d[0], d[1], d[2], fs.Arg(1))
	return nil
}

func cmdPaste(args []string) error {
	fs := flag.NewFlagSet("paste", flag.ContinueOnError)
	atFlag := fs.String("at", "", "paste position x,y,z (required)")
	dimFlag := fs.String("dim", "overworld", "dimension to paste into")
	skipAir := fs.Bool("skip-air", false, "leave existing blocks in the structure's air positions")
	limit := addDecodeLimit(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 || *atFlag == "" {
		return errors.New("usage: pile paste --at x,y,z [--dim overworld] [--skip-air] <structure.pile> <world-dir>")
	}
	at, err := parsePos(*atFlag)
	if err != nil {
		return err
	}
	dim, err := parseDim(*dimFlag)
	if err != nil {
		return err
	}

	opts := limit.structureOpts()
	if *skipAir {
		opts = append(opts, pile.SkipAir())
	}
	s, err := pile.LoadStructure(fs.Arg(0), opts...)
	if err != nil {
		return err
	}
	p, err := pile.Open(fs.Arg(1), limit.providerOpts()...)
	if err != nil {
		return err
	}
	if err := s.PasteInto(p, dim, at); err != nil {
		_ = p.Close()
		return err
	}
	if err := p.Close(); err != nil {
		return err
	}
	d := s.Dimensions()
	fmt.Printf("pasted %dx%dx%d structure into %s at %v\n", d[0], d[1], d[2], fs.Arg(1), at)
	return nil
}
