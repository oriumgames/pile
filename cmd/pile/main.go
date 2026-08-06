// Command pile is the tooling CLI for the pile world format: conversion to
// and from dragonfly's leveldb format (mcdb), inspection and verification.
package main

import (
	"fmt"
	"os"

	_ "github.com/df-mc/dragonfly/server/block"       // register vanilla blocks
	_ "github.com/df-mc/dragonfly/server/world/biome" // register vanilla biomes
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "convert":
		err = cmdConvert(os.Args[2:])
	case "inspect":
		err = cmdInspect(os.Args[2:])
	case "verify":
		err = cmdVerify(os.Args[2:])
	case "stats":
		err = cmdStats(os.Args[2:])
	case "extract":
		err = cmdExtract(os.Args[2:])
	case "paste":
		err = cmdPaste(os.Args[2:])
	case "export":
		err = cmdExport(os.Args[2:])
	case "import":
		err = cmdImport(os.Args[2:])
	case "compact":
		err = cmdCompact(os.Args[2:])
	case "mode":
		err = cmdMode(os.Args[2:])
	case "move":
		err = cmdMove(os.Args[2:])
	case "origin":
		err = cmdOrigin(os.Args[2:])
	case "diff":
		err = cmdDiff(os.Args[2:])
	case "patch":
		err = cmdPatch(os.Args[2:])
	case "apply":
		err = cmdApply(os.Args[2:])
	case "render":
		err = cmdRender(os.Args[2:])
	case "upgrade":
		err = cmdUpgrade(os.Args[2:])
	case "check":
		err = cmdCheck(os.Args[2:])
	case "prune":
		err = cmdPrune(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "pile: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "pile:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: pile <command> [arguments]

commands:
  convert <src> <dst>   convert between mcdb (leveldb) and pile world
                        directories; direction is detected from src
  inspect <file.pile>   print header and metadata without decoding chunks
  verify  <dir|file>    fully decode and validate pile files
  stats   <dir|file>    print content statistics of pile files
  extract <world> <out.pile> --min x,y,z --max x,y,z [--dim d] [--skip-air]
                        cut a region of a pile world into a structure file
  paste   <structure.pile> <world> --at x,y,z [--dim d] [--skip-air]
                        build a structure file into a pile world
  export  <world> <out-dir> [--dim d]
                        unpack a world into structure.pile + editable data.json
                        (settings, markers, user data, origin)
  import  <export-dir> <world-dir>
                        rebuild a world from structure.pile + data.json
  compact <dir|file>    rewrite indexed files without garbage
  mode    <dir|file> <solid|indexed>
                        convert between solid and indexed file modes
  move    <world> (--by dx,dy,dz | --spawn-to x,y,z | --center)
                        translate a whole world (blocks, entities, spawn,
                        markers, border); refuses lossy moves unless --clip;
                        --dry-run previews, auto-backup to snapshots/pre-move
  origin  <structure.pile> (--set x,y,z | --zero | --center)
                        change a structure's paste anchor (pure metadata)
  diff    <world-a> <world-b>
                        chunk-level change report between two worlds
  patch   <old> <new> -o file.pilepatch
                        build a binary update from old to new
  apply   <world> <file.pilepatch>
                        apply a patch to a world
  render  <world> [-o map.png] [--dim d] [--bg #rrggbb]
                        top-down PNG preview (transparent background unless --bg)
  upgrade <dir|file>    re-encode at the current Minecraft block version
  check   <dir|file>    list block states that do not resolve against the
                        current registry (exit 1 if any)
  prune   <world> --bounds x1,z1,x2,z2
                        drop chunks outside a block box
`)
}
