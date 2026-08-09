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
	case "replace":
		err = cmdReplace(os.Args[2:])
	case "hash":
		err = cmdHash(os.Args[2:])
	case "blocks":
		err = cmdBlocks(os.Args[2:])
	case "edit":
		err = cmdEdit(os.Args[2:])
	case "snapshot":
		err = cmdSnapshot(os.Args[2:])
	case "snapshots":
		err = cmdSnapshots(os.Args[2:])
	case "rollback":
		err = cmdRollback(os.Args[2:])
	case "unsnapshot":
		err = cmdDeleteSnapshot(os.Args[2:])
	case "version", "--version":
		err = cmdVersion(os.Args[2:])
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

flags come before the paths: "pile prune --dry-run world", not
"pile prune world --dry-run", which leaves the flag unread

commands:
  convert [--permissive] <src> <dst>
                        convert between mcdb (leveldb) and pile world;
                        --permissive registers block states this build does not
                        know, so a world built on a behaviour pack converts
                        directories; direction is detected from src
  inspect <file.pile>   print header and metadata without decoding chunks
  verify  <dir|file>    fully decode and validate pile files
  stats   <dir|file>    print content statistics of pile files
  extract --min x,y,z --max x,y,z [--dim d] [--skip-air] <world> <out.pile>
                        cut a region of a pile world into a structure file
  paste   --at x,y,z [--dim d] [--skip-air] <structure.pile> <world>
                        build a structure file into a pile world
  export  [--dim d] <world> <out-dir>
                        unpack a world into structure.pile + editable data.json
                        (settings, user data, origin)
  import  <export-dir> <world-dir>
                        rebuild a world from structure.pile + data.json
  compact <dir|file>    rewrite indexed files without garbage
  mode    <dir|file> <solid|indexed>
                        convert between solid and indexed file modes
  move    (--by dx,dy,dz | --spawn-to x,y,z | --center) <world>
                        translate a whole world (blocks, entities, spawn);
                        refuses lossy moves unless --clip, and refuses a world
                        carrying user data unless --keep-user-data;
                        --dry-run previews, auto-backup to snapshots/pre-move
  origin  (--set x,y,z | --zero | --center) <structure.pile>
                        change a structure's paste anchor (pure metadata)
  diff    <world-a> <world-b>
                        chunk-level change report between two worlds
  patch   -o file.pilepatch <old> <new>
                        build a binary update from old to new
  apply   <world> <file.pilepatch>
                        apply a patch to a world
  render  [-o map.png] [--dim d] [--bg #rrggbb] <world>
                        top-down PNG preview (transparent background unless --bg)
  upgrade <dir|file>    re-encode at the current Minecraft block version
  check   <dir|file>    list block states that do not resolve against the
                        current registry (exit 1 if any)
  replace (--from name | --unresolved) [--to name] <world>
                        rewrite block states throughout a world; --unresolved
                        takes everything this build cannot resolve, --to
                        defaults to minecraft:air
  prune   (--bounds x1,z1,x2,z2 | --empty) <world>
                        drop chunks outside a block box, or chunks holding no
                        blocks at all (converted worlds are mostly these)
  edit    <world>       edit the world's settings and user data as JSON in
                        $EDITOR; --print dumps it, --apply reads it back from
                        a file
  blocks  <mcdb-world>  list the block identifiers a leveldb world uses and the
                        property values each takes, without needing a registry:
                        what a server must register before converting a world
                        whose blocks come from a behaviour pack
  hash    <dir|file>... print the content identity of each world or file;
                        identical content gives an identical hash whatever the
                        mode or compression. --quiet exits 1 if they differ
  snapshot   <world> <name>    copy the world into snapshots/<name>
  snapshots  <world>           list snapshots
  rollback   <world> <name>    restore a snapshot (keeps the current state as
                               pre-rollback unless --backup is empty)
  unsnapshot <world> <name>    delete a snapshot
  version               print pile, wire format and dragonfly versions

every command that decodes chunk content also takes:
  --max-decoded n       refuse a file whose decode would exceed n bytes of live
                        state, e.g. 256MiB. Set it for files you did not write:
                        a valid file of a few kilobytes can decode into
                        gigabytes. 0, the default, is the format's own ceiling
`)
}
