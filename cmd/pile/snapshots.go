package main

import (
	"errors"
	"flag"
	"fmt"

	"github.com/oriumgames/pile"
)

// The snapshot commands exist because the tool already makes snapshots and had
// no way to use them.
//
// move, prune and apply each back the world up to snapshots/pre-<command>
// before touching it. Until now, recovering from a bad one meant writing a Go
// program or moving directories by hand -- which is exactly the moment a user
// is least able to afford either. A safety net the tool opens and cannot reach
// is barely a safety net.

func cmdSnapshot(args []string) error {
	fs := flag.NewFlagSet("snapshot", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return errors.New("usage: pile snapshot <world> <name>")
	}
	dir, name := fs.Arg(0), fs.Arg(1)
	p, err := pile.Open(dir)
	if err != nil {
		return err
	}
	if err := p.Snapshot(name); err != nil {
		_ = p.Close()
		return err
	}
	if err := p.Close(); err != nil {
		return err
	}
	fmt.Printf("snapshot %q taken of %s\n", name, dir)
	return nil
}

func cmdSnapshots(args []string) error {
	fs := flag.NewFlagSet("snapshots", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: pile snapshots <world>")
	}
	dir := fs.Arg(0)
	// Read-only: listing must not create or save anything, least of all on a
	// world the user is inspecting because something already went wrong.
	p, err := pile.Open(dir, pile.ReadOnly())
	if err != nil {
		return err
	}
	defer func() { _ = p.Close() }()
	names, err := p.Snapshots()
	if err != nil {
		return err
	}
	if len(names) == 0 {
		fmt.Printf("%s has no snapshots\n", dir)
		return nil
	}
	for _, n := range names {
		fmt.Println(n)
	}
	return nil
}

func cmdRollback(args []string) error {
	fs := flag.NewFlagSet("rollback", flag.ContinueOnError)
	keep := fs.String("backup", "pre-rollback", "snapshot the current state under this name first; empty to skip")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return errors.New("usage: pile rollback <world> <name> [--backup name]")
	}
	dir, name := fs.Arg(0), fs.Arg(1)
	p, err := pile.Open(dir)
	if err != nil {
		return err
	}
	// Back the current state up before replacing it. A rollback is the one
	// destructive command whose whole purpose is undoing a mistake, and
	// discovering it was the wrong snapshot leaves nothing to return to
	// otherwise.
	if *keep != "" {
		if err := p.Snapshot(*keep); err != nil {
			_ = p.Close()
			return fmt.Errorf("taking the %q backup first: %w", *keep, err)
		}
	}
	if err := p.Rollback(name); err != nil {
		_ = p.Close()
		return err
	}
	if err := p.Close(); err != nil {
		return err
	}
	fmt.Printf("rolled %s back to %q", dir, name)
	if *keep != "" {
		fmt.Printf(" (previous state kept as %q)", *keep)
	}
	fmt.Println()
	return nil
}

func cmdDeleteSnapshot(args []string) error {
	fs := flag.NewFlagSet("unsnapshot", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return errors.New("usage: pile unsnapshot <world> <name>")
	}
	dir, name := fs.Arg(0), fs.Arg(1)
	p, err := pile.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = p.Close() }()
	if err := p.DeleteSnapshot(name); err != nil {
		return err
	}
	fmt.Printf("deleted snapshot %q of %s\n", name, dir)
	return nil
}
