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

**Flags come before the paths.** Go's flag package stops reading flags at the
first argument that is not one, so `pile prune world --dry-run` silently leaves
`--dry-run` unset and fails on its argument count, while
`pile prune --dry-run world` works. Every signature below is written in the
order that works.

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
| `pile convert [--permissive] <src> <dst>` | Convert between dragonfly's leveldb world (mcdb) and pile, both directions, detected from `<src>`. Output is garbage-free: pile output is a canonical solid write; mcdb output is written into a fresh database (refuses an existing one). `--permissive` converts a world built on a behaviour pack — see below. |
| `pile mode <dir\|file> <solid\|indexed>` | Convert files between solid mode (small worlds, deterministic full-rewrite saves) and indexed mode (large worlds, append saves). |

## Inspection

| command | |
|---------|---|
| `pile inspect <file.pile>` | Header, flags, decoded settings, sizes; no chunk decode. Indexed files additionally show generation, chunk count and garbage ratio. |
| `pile verify <dir\|file>` | Full decode with checksum verification; per-record verification for indexed files. On a world directory it also compares the metadata each dimension file carries and warns when they disagree. |
| `pile stats <dir\|file>` | Chunk/section/entity counts, bytes per chunk. |
| `pile check [--allow ns,ns] <dir\|file>` | List block states that do not resolve against the current registry (they would load as placeholder blocks). Exits 1 if any. Run before deploying maps after a dragonfly upgrade. `--allow customnamespace` treats a namespace as expected — a world whose blocks come from a behaviour pack has states this binary can never resolve, and without it the command is useless for exactly the worlds most worth checking. |
| `pile render [-o map.png] [--dim d] [--bg #rrggbb] <world>` | Top-down PNG preview, height-shaded, dye-colored wool/concrete/terracotta. Background is transparent unless `--bg`. |
| `pile blocks [--custom] [--quiet] <mcdb-world\|dir\|file>` | List the block identifiers a world uses and the property values each takes. Needs **no registry**, because it reads the palettes without decoding a chunk — so it works on the worlds `pile convert` cannot open. Takes an mcdb world or a pile one. `--custom` lists only what is outside `minecraft:`; `--quiet` prints bare identifiers. |
| `pile hash [--quiet] <dir\|file> [<dir\|file>...]` | Content identity per dimension. Identical content gives an identical hash whatever the compression or file mode, so this is what "a file hash is a map version" means in practice. With two or more arguments and `--quiet`, exits 1 if they differ — the deploy check. |
| `pile version` | pile, wire format and dragonfly versions. The first three lines of any bug report. |

## Maintenance

| command | |
|---------|---|
| `pile compact <dir\|file>` | Rewrite indexed files without garbage (also retrains the shared compression dictionary). Solid files are always canonical already. |
| `pile upgrade <dir\|file>` | Re-encode at the current Minecraft block version so servers never pay state-upgrade cost at load. |
| `pile prune (--bounds x1,z1,x2,z2 \| --empty) [--dry-run] [--no-backup] <world>` | Drop chunks outside a block box, or — with `--empty` — chunks that hold no blocks at all. Either may be given, or both. Backup in `snapshots/pre-prune`. See below. |

## Snapshots

`move`, `prune` and `apply` back the world up to `snapshots/pre-<command>`
before touching it. These are how you reach those, and your own.

| command | |
|---------|---|
| `pile snapshot <world> <name>` | Save the current state into `snapshots/<name>`. |
| `pile snapshots <world>` | List them. |
| `pile rollback [--backup name] <world> <name>` | Restore one. The current state is kept as `pre-rollback` first, so picking the wrong snapshot is itself recoverable; `--backup ""` skips that. |
| `pile unsnapshot <world> <name>` | Delete one. |

### Why the metadata is checked across files

Every dimension file carries its own copy of the world's settings and user
data, so that a single `.pile` is self-describing — you can hand
somebody `nether.pile` and it still knows its name and spawn. The provider reads
the **overworld's** copy and ignores the rest.

Nothing in the format requires the copies to agree, and no reader could enforce
it, because a reader only ever sees one file. So a world whose files disagree is
valid, loads, and silently uses one copy. `pile verify` is the only place that
sees all of them at once, which is why the check lives there — and why
`pile edit` takes a world directory rather than a single file: it rewrites every
dimension's copy together.

## Editing a world's metadata

| command | |
|---------|---|
| `pile edit [--print] [--apply file.json] [--no-backup] <world>` | Open the world's settings and user data as JSON in `$VISUAL`/`$EDITOR`, and write back what you save. `--print` dumps the JSON and changes nothing; `--apply` reads it from a file instead of opening an editor, which is what a script wants. Backs up to `snapshots/pre-edit` first. |

Chunks are not in the file. This is for moving a spawn point or adjusting an
application's own configuration — editing blocks as JSON would be a worse tool
than the game.

Settings go through typed fields rather than raw NBT, so an `int32` does not
come back a `float64` — the failure that took a server down earlier in this
project. A user data blob that you do not change is written back as the same
bytes it went in as, rather than reflowed by the marshaller, so changing the
spawn does not move the world's `ContentHash`. A blob that is not JSON is shown
as base64 and restored from it.

Everything an edit can get wrong is checked before the world is opened, so a
refusal means nothing was written. A JSON syntax error keeps your edit in a temp
file and tells you the path, so the cost of a typo is fixing the typo.

## Map surgery

| command | |
|---------|---|
| `pile move (--by dx,dy,dz \| --spawn-to x,y,z \| --center) [--clip] [--dry-run] [--no-backup] [--keep-user-data] <world>` | Translate a whole world: blocks, biomes, entities, block entities, scheduled ticks and spawn move as one unit. Lossless by default: a move that would push content outside the vertical range is refused with exact counts unless `--clip`. A world carrying user data is refused unless `--keep-user-data`, because pile cannot find a coordinate in an opaque blob and would leave every position it holds behind — see below. Chunk-aligned horizontal moves take a re-key fast path. Backup in `snapshots/pre-move`. |
| `pile extract --min x,y,z --max x,y,z [--dim d] [--skip-air] <world> <out.pile>` | Cut a region into a structure file (blocks, block entities, entities). |
| `pile paste --at x,y,z [--dim d] [--skip-air] <structure.pile> <world>` | Build a structure file into a world. |
| `pile origin (--set x,y,z \| --zero \| --center) <structure.pile>` | Change a structure's paste anchor. Pure metadata, content untouched. |

| `pile replace (--from name \| --unresolved) [--to name] [--dry-run] [--no-backup] <world>` | Rewrite block states throughout a world. `--unresolved` takes every state this build cannot resolve — the custom blocks a `--permissive` conversion brought across. `--to` defaults to `minecraft:air`, so the bare form deletes them. Backup in `snapshots/pre-replace`. |

### Converting a world that uses a behaviour pack

dragonfly's chunk decoder resolves every palette entry against the block
registry and fails outright on one it does not know, so a single block from a
pack stops the conversion at whichever chunk happens to hold it:

```
cannot get runtime ID of block state cubecraft:portal_side{...}
```

`--permissive` scans the source's own palettes first and registers every state
the build does not know, as dragonfly's bare placeholder. The conversion then
runs, and the identifiers survive: pile stores a palette entry as its name and
properties, so the file carries the real `cubecraft:portal_side`, and `pile
check` lists it as unresolved rather than silently substituting something.

A server that registers those blocks properly resolves them from the same file.
This does **not** make the block behave — a placeholder has no model, no
collision and no behaviour. It moves the world; implementing the pack is a
separate job.

`pile blocks --custom <world>` lists what a world needs registered, and works
on worlds `pile convert` cannot open, because it reads palettes without
decoding a chunk.

If you would rather not register them, `pile replace` is the other half:

```sh
pile check world                                  # what is unresolved
pile replace --unresolved --dry-run world         # what would go, and how many blocks
pile replace --unresolved world                   # delete them (--to defaults to air)
pile replace --from cubecraft:portal_side --to minecraft:obsidian world
```

An identifier on its own takes every state of that block; written as
`name[k=v,k=v]` it takes only the state matching. Matching is by what the file
says, not by what the registry knows, so it works on states nothing implements.

### Why a converted world is mostly air

Bedrock writes a chunk record for every chunk that has ever entered simulation
— render distance around spawn, anywhere a builder flew — and in a void map
that record holds no blocks. It is not the same as never storing the chunk: a
stored empty chunk says "this is void", where an absent one sends the server to
its generator. So `pile convert` keeps them, and so does the game.

They arrive in bulk. A converted Skywars map here held **10 225 columns of
which 68 contained a single block**, with the map itself 87 000 blocks from a
pad of empty spawn chunks.

On disk they are free: all 10 157 of them cost 700 bytes, because they are
identical and compress to nothing. In memory they are not — loading that world
holds 55 MB, against 1.6 MB for the same map with the air dropped. That is the
number that matters if you keep minigame maps resident, and it multiplies by
the number of instances.

```sh
pile prune --empty --dry-run world   # says what would go
pile prune --empty world             # backs up to snapshots/pre-prune first
```

A chunk survives if it holds any block, any block entity, any entity, any
scheduled update or any chunk user data. Emptiness is dragonfly's own
definition, so a waterlogged section whose layer 0 is uniform air has two
storages and stays.

### Why moving refuses a world with user data

`pile move` translates everything it understands: blocks, biomes, entities,
block entities, scheduled ticks and the spawn point. User data is the one thing
it cannot, because the format stores it as an opaque blob and pile has no way to
find a coordinate inside one.

So a move would shift every block while every position your application stored —
spawn points, region corners, NPC locations — stayed exactly where it was, all
of them now pointing at whatever occupies the old coordinates. Nothing would
report it. You would find out when somebody stood in an arena that had moved out
from under its own boundary.

`--keep-user-data` is you saying you will re-anchor it yourself. The blob is
copied through untouched. `--dry-run` reports the refusal too, since a dry run
promising a move that would in fact be refused is the same silent failure one
step earlier.

## Distribution

| command | |
|---------|---|
| `pile diff <world-a> <world-b>` | Chunk-level change report (added/removed/modified per dimension, metadata changes) using exact canonical comparison. |
| `pile patch -o file.pilepatch <old> <new>` | Binary update containing only changed/added chunks, removals and new metadata. |
| `pile apply [--force] [--no-backup] <world> <file.pilepatch>` | Apply a patch: `apply(old, patch(old→new)) == new`, chunk-for-chunk. Refuses a target whose content does not match the patch's base world (override with `--force`); backs up to `snapshots/pre-apply` first. |
| `pile export [--dim d] <world> <out-dir>` | Unpack a world into `structure.pile` + human-editable `data.json` (settings, origin; user data inlined when it is JSON). |
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
pile check --allow customnamespace ./maps/lobby
```

Without it the command reports every one of the pack's states and exits 1, which
is true and useless. With it, it answers the question that is actually worth
asking about a converted world: do the *vanilla* blocks still resolve, or did a
dragonfly upgrade take one away?
