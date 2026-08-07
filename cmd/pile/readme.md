# pile CLI

Tooling for pile worlds and structures: conversion, inspection, maintenance,
map surgery and distribution.

```
go install github.com/oriumgames/pile/cmd/pile@latest
```

A "world" argument is a directory holding `overworld.pile` (plus optional
`nether.pile`, `end.pile`). Commands accepting `<dir|file>` operate on every
`*.pile` file in a directory or on a single file. All rewrites are atomic
(temp file + rename); destructive commands make automatic backups unless told
otherwise.

## Files from other people

A pile file is compressed and heavily deduplicated, so a small file can
legitimately decode into a very large one — a valid 1.2 KB world decodes into
about a gigabyte of live objects in roughly a second, and that is within the
format's rules rather than a bug in it. The format's own ceilings bound the
worst case at about four gigabytes, which is not a useful bound if you are
inspecting a file a stranger sent you.

`--max-decoded` sets a lower ceiling, and goes **before** the command:

```
pile --max-decoded=256MiB verify downloaded-world/
pile --max-decoded=64MiB inspect suspicious.pile
```

Sizes take `KiB`/`MiB`/`GiB` or `KB`/`MB`/`GB`; a bare number is bytes. The
ceiling applies to every decode the command performs, including the ones
inside the provider.

There is no default ceiling, deliberately. The tool cannot tell which worlds
are meant to be enormous, and refusing to open a legitimate large world by
default would be a worse failure than the one a default prevents. A file
refused under `--max-decoded` reports a decode-budget error and **not** a
corruption error, so a ceiling set too low is distinguishable from a bad file.

This is a resource bound, not an authenticity check. A pile file's integrity
hashes detect corruption, not tampering; see "Compatibility" in the root
readme.

## Conversion

| command | |
|---------|---|
| `pile convert <src> <dst>` | Convert between dragonfly's leveldb world (mcdb) and pile, both directions, detected from `<src>`. Output is garbage-free: pile output is a canonical solid write; mcdb output is written into a fresh database (refuses an existing one). |
| `pile mode <dir\|file> <solid\|indexed>` | Convert files between solid mode (small worlds, deterministic full-rewrite saves) and indexed mode (large worlds, append saves). |

## Inspection

| command | |
|---------|---|
| `pile inspect <file.pile>` | Header, flags, decoded settings/markers, sizes; no chunk decode. Indexed files additionally show generation, chunk count and garbage ratio. |
| `pile verify <dir\|file>` | Full decode with checksum verification; per-record verification for indexed files. |
| `pile stats <dir\|file>` | Chunk/section/entity counts, bytes per chunk. |
| `pile check <dir\|file>` | List block states that do not resolve against the current registry (they would load as placeholder blocks). Exits 1 if any. Run before deploying maps after a dragonfly upgrade. |
| `pile render <world> [-o map.png] [--dim d] [--bg #rrggbb]` | Top-down PNG preview, height-shaded, dye-colored wool/concrete/terracotta. Background is transparent unless `--bg`. |

## Maintenance

| command | |
|---------|---|
| `pile compact <dir\|file>` | Rewrite indexed files without garbage (also retrains the shared compression dictionary). Solid files are always canonical already. |
| `pile upgrade <dir\|file>` | Re-encode at the current Minecraft block version so servers never pay state-upgrade cost at load. |
| `pile prune <world> --bounds x1,z1,x2,z2 [--dry-run] [--no-backup]` | Drop chunks outside a block box (e.g. stray chunks created while flying around during map creation). Backup in `snapshots/pre-prune`. |

## Map surgery

| command | |
|---------|---|
| `pile move <world> (--by dx,dy,dz \| --spawn-to x,y,z \| --center) [--clip] [--dry-run] [--no-backup]` | Translate a whole world: blocks, biomes, entities, block entities, scheduled ticks, chunk metadata, spawn, markers and border move as one unit. Lossless by default: a move that would push content outside the vertical range is refused with exact counts unless `--clip`. Chunk-aligned horizontal moves take a re-key fast path. Backup in `snapshots/pre-move`. |
| `pile extract <world> <out.pile> --min x,y,z --max x,y,z [--dim d] [--skip-air]` | Cut a region into a structure file (blocks, block entities, entities). |
| `pile paste <structure.pile> <world> --at x,y,z [--dim d] [--skip-air]` | Build a structure file into a world. |
| `pile origin <structure.pile> (--set x,y,z \| --zero \| --center)` | Change a structure's paste anchor. Pure metadata, content untouched. |

## Distribution

| command | |
|---------|---|
| `pile diff <world-a> <world-b>` | Chunk-level change report (added/removed/modified per dimension, metadata changes) using exact canonical comparison. |
| `pile patch <old> <new> -o file.pilepatch` | Binary update containing only changed/added chunks, removals and new metadata. |
| `pile apply <world> <file.pilepatch> [--force] [--no-backup]` | Apply a patch: `apply(old, patch(old→new)) == new`, chunk-for-chunk. Refuses a target whose content does not match the patch's base world (override with `--force`); backs up to `snapshots/pre-apply` first. |
| `pile export <world> <out-dir> [--dim d]` | Unpack a world into `structure.pile` + human-editable `data.json` (settings, markers, origin; user data inlined when it is JSON). |
| `pile import <export-dir> <world-dir>` | Rebuild a world from an export. Refuses an existing destination. |

## Examples

```sh
# Convert an mcdb lobby and check the result
pile convert ./worlds/lobby ./maps/lobby
pile verify ./maps/lobby
pile stats  ./maps/lobby

# Fix a lobby built at odd coordinates so spawn sits at the origin
pile move ./maps/lobby --spawn-to 0,65,0 --dry-run
pile move ./maps/lobby --spawn-to 0,65,0

# Cut the spawn building into a reusable structure anchored at its center
pile extract ./maps/lobby ./structures/spawn.pile --min -12,60,-12 --max 12,80,12
pile origin  ./structures/spawn.pile --center

# Ship a map update to servers as a small patch
pile patch ./maps/lobby-v1 ./maps/lobby-v2 -o lobby-v2.pilepatch
pile apply /srv/maps/lobby lobby-v2.pilepatch

# Editable round trip: tweak settings in a text editor
pile export ./maps/lobby ./lobby-src
$EDITOR ./lobby-src/data.json
pile import ./lobby-src ./maps/lobby-new
```
