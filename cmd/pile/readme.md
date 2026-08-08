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

## `--max-decoded`

Every command that decodes chunk content takes `--max-decoded n`: the ceiling,
in bytes, on the live decoded state one file may produce. It is
`pile.MaxDecodedBytes` on the command line. The default, 0, is the format's own
ceiling, which is set at what the format can *represent* — a legal file of about
a kilobyte decodes into more than a gigabyte — rather than at what a workstation
wants to spend.

**Set it whenever the file came from somebody else.** A file refused under it
fails with `format.ErrDecodeBudget`, which is not a claim that the file is
corrupt; the file is bigger than you asked for.

```sh
pile inspect suspect/overworld.pile             # header + metadata only, no chunks
pile verify  suspect --max-decoded 67108864     # full decode, bounded at 64 MiB
```

The ceiling charges everything a decode produces: columns, section storages, and
the block entities, entities and scheduled updates inside them. A file refused
under it reports a decode-budget error rather than a corruption error, so a
ceiling set too low is distinguishable from a bad file.

This is a resource bound, not an authenticity check. A pile file's hashes detect
corruption, not tampering.

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
| `pile check <dir\|file> [--allow ns,ns]` | List block states that do not resolve against the current registry (they would load as placeholder blocks). Exits 1 if any. Run before deploying maps after a dragonfly upgrade. `--allow hive` treats a namespace as expected — a world whose blocks come from a behaviour pack has states this binary can never resolve, and without it the command is useless for exactly the worlds most worth checking. |
| `pile render <world> [-o map.png] [--dim d] [--bg #rrggbb]` | Top-down PNG preview, height-shaded, dye-colored wool/concrete/terracotta. Background is transparent unless `--bg`. |
| `pile blocks <mcdb-world\|dir\|file>` | List the block identifiers a world uses and the property values each takes. Needs **no registry**, because it reads the palettes without decoding a chunk — so it works on the worlds `pile convert` cannot open. Takes an mcdb world or a pile one. `--custom` lists only what is outside `minecraft:`; `--quiet` prints bare identifiers. |
| `pile hash <dir\|file>...` | Content identity per dimension. Identical content gives an identical hash whatever the compression or file mode, so this is what "a file hash is a map version" means in practice. With two or more arguments and `--quiet`, exits 1 if they differ — the deploy check. |
| `pile version` | pile, wire format and dragonfly versions. The first three lines of any bug report. |

## Maintenance

| command | |
|---------|---|
| `pile compact <dir\|file>` | Rewrite indexed files without garbage (also retrains the shared compression dictionary). Solid files are always canonical already. |
| `pile upgrade <dir\|file>` | Re-encode at the current Minecraft block version so servers never pay state-upgrade cost at load. |
| `pile prune <world> --bounds x1,z1,x2,z2 [--dry-run] [--no-backup]` | Drop chunks outside a block box (e.g. stray chunks created while flying around during map creation). Backup in `snapshots/pre-prune`. |

## Snapshots

`move`, `prune` and `apply` back the world up to `snapshots/pre-<command>`
before touching it. These are how you reach those, and your own.

| command | |
|---------|---|
| `pile snapshot <world> <name>` | Save the current state into `snapshots/<name>`. |
| `pile snapshots <world>` | List them. |
| `pile rollback <world> <name> [--backup name]` | Restore one. The current state is kept as `pre-rollback` first, so picking the wrong snapshot is itself recoverable; `--backup ""` skips that. |
| `pile unsnapshot <world> <name>` | Delete one. |

## Editing a world's metadata

| command | |
|---------|---|
| `pile edit <world> [--print] [--apply file.json] [--no-backup]` | Open the world's settings, markers, border and user data as JSON in `$VISUAL`/`$EDITOR`, and write back what you save. `--print` dumps the JSON and changes nothing; `--apply` reads it from a file instead of opening an editor, which is what a script wants. Backs up to `snapshots/pre-edit` first. |

Chunks are not in the file. This is for moving a spawn point, renaming a marker
or adjusting an area — editing blocks as JSON would be a worse tool than the
game.

The list you are shown is the whole list, so deleting a marker there deletes the
marker. Settings and markers go through typed fields rather than raw NBT, so an
`int32` does not come back a `float64` — the failure that took a server down
earlier in this project. A user data blob that you do not change is written back
as the same bytes it went in as, rather than reflowed by the marshaller, so
renaming a marker does not move the world's `ContentHash`.

An edit that breaks a §7 rule — an area whose corners are the wrong way round, a
marker with neither a position nor bounds — is refused when the world is
written, which means nothing is written. A JSON syntax error keeps your edit in
a temp file and tells you the path, so the cost of a typo is fixing the typo.

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

## Converting a world whose blocks come from a behaviour pack

`pile convert` links the vanilla block registry, and dragonfly's decoder refuses
a block state it cannot resolve rather than substituting anything. So a world
using a pack's blocks cannot be converted by this binary at all — no flag fixes
that, because Go cannot load your block types into a prebuilt program.

The conversion belongs in your own server, where your registry exists:

```sh
pile blocks --custom ./the-world     # what your registry has to cover
```

```go
// then, in your server, with the pack's blocks already registered:
n, err := pile.ImportMCDB("./the-world", "./maps/lobby", pile.Registry(myRegistry))
```

That is a one-time step. A `.pile` file stores block **names and properties**,
never runtime IDs, so the converted world loads under a different registry, a
different dragonfly, or a build where the pack has changed — and any state that
stops resolving is preserved verbatim through load and save rather than lost.

`pile blocks` prints the property schema per identifier for a reason: a custom
block is announced to the client once per *identifier*, and the client generates
the state list itself from the declared properties. Declare fewer states than
you registered and the client's palette is shorter than the server's, which
shows up as every block past the first custom one rendering as something else.

Afterwards, the same commands work on the converted world — `verify`, `stats`,
`inspect`, `hash` and `blocks` never resolve a block state, so a pack's blocks
do not trouble them. `check` does resolve, by design, so give it the namespace:

```sh
pile check --allow hive ./maps/lobby
```

Without it the command reports every one of the pack's states and exits 1, which
is true and useless. With it, it answers the question that is actually worth
asking about a converted world: do the *vanilla* blocks still resolve, or did a
dragonfly upgrade take one away?
