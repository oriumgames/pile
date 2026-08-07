# Pile

A compact, deterministic single-file world format and `world.Provider` for
[dragonfly](https://github.com/df-mc/dragonfly) servers, built for lobbies,
minigame maps, skyblock islands and structures.

One `.pile` file per dimension. On the benchmark map, 5–20× smaller than the
vanilla leveldb format (141× on dedup-friendly flat worlds), loading in a
single sequential read.

```
go get github.com/oriumgames/pile
```

## Why

- **Zero garbage.** A save is a canonical full rewrite (atomic temp + rename):
  edit a lobby on a live server, save, and the file is byte-identical to a
  fresh conversion of the same content. Identical content ⇒ identical bytes,
  so a file hash *is* a map version.
- **Small.** World-global block palette, content-hash deduplication of
  repeated sections (empty sections cost one bit), byte-aligned indices under
  zstd.
- **Safe.** xxHash64 integrity, no-panic decoding of hostile files, crash-safe
  append mode with torn-write recovery, automatic block-state upgrades across
  Minecraft versions, unknown states preserved through load/save.
- **Fast.** Parallel encode/decode, direct (layout-asserted unsafe) access to
  dragonfly's chunk internals: no per-block work on either path.

## Quick start

```go
p, err := pile.Open("maps/lobby")
w := world.Config{Provider: p}.New()
// ...
defer p.Close() // saves
```

Options: `pile.ReadOnly()`, `pile.Compression(...)`, `pile.AppendMode()`,
`pile.Skip(pile.SkipEntities|...)`, `pile.FilterEntity(...)`,
`pile.LoadSkip(...)`, `pile.CacheColumns(n)`, `pile.FastSaves()`,
`pile.StoreLight()`, `pile.Registry(...)`, `pile.WithSpawnStore(...)`,
`pile.MaxDecodedBytes(n)`. Player data never lives in pile files.

`pile.MaxDecodedBytes(n)` is the one to reach for if you open worlds you did
not write. The format's own ceilings are set at what it can represent rather
than at what a server wants to spend, so a legal file of about a kilobyte
decodes into a gigabyte; this caps it. A world refused under the cap fails with
`format.ErrDecodeBudget`, which does **not** wrap `format.ErrCorrupt` — the file
is too big for your limit, not broken, so do not quarantine it as though it
were.

**If the worlds come from strangers**, open them like this:

```go
p, err := pile.Open(dir,
    pile.ReadOnly(),                  // nothing is written back
    pile.MaxDecodedBytes(256<<20),    // whatever your box can spare
    pile.CacheColumns(0),             // no cache: one column at a time
)
```

The ceiling charges everything a decode produces — columns, section storages,
and the block entities, entities and scheduled updates inside them. What it does
not bound is **wall-clock time**: a small file can legally cost seconds of CPU,
so do not decode foreign worlds on a request path or unbounded in parallel.

`LoadSkip` is **not** a bound. It drops categories after the file is decoded, so
it removes nothing from the peak — use it to keep content out of your runtime,
not to keep it out of memory.

And the integrity hashes detect corruption, not tampering: xxHash64 is keyless,
so anyone who can author a file can make its checksums agree. A file that
verifies is well-formed, never trustworthy.

## Two file modes

| | solid (default) | indexed (`pile.AppendMode()`) |
|---|---|---|
| for | lobbies, minigames, islands, anything read-mostly | large or save-heavy worlds |
| save | deterministic full rewrite, zero garbage | append + checkpoint; auto-compaction on close |
| memory | whole world | directory + palettes; columns decoded on demand (optional LRU) |
| durability | atomic rename | footer checkpoints, torn-write recovery, per-chunk checksums |

Convert between modes with `pile mode`.

## Templates & instances

The minigame primitive: one decoded base world, any number of throwaway
copy-on-write instances.

```go
tmpl, _ := pile.OpenTemplate("maps/bedwars")
inst := tmpl.Instance()                       // in-memory world.Provider, COW
w := world.Config{Provider: inst}.New()
// ... play a round; blocks break, beds explode ...
inst.Close()                                  // everything evaporates; base pristine
// or: inst.SaveAs("maps/edited")             // keep it
```

`pile.NewMemory()` is the same machinery with no base (generated arenas).

## Structures

Same format, first-class API:

```go
s, _ := pile.LoadStructure("structures/spawn.pile")
tx.BuildStructure(pos, s)                     // s implements world.Structure
s.PasteInto(p, world.Overworld, pos)          // fast path; carries entities + block-entity NBT
s2, _ := pile.ExtractStructure(p, world.Overworld, lo, hi)
s3 := s2.Rotate(1)                            // 90° clockwise, block states included
lib, _ := pile.LoadStructureLibrary("structures/") // name → structure
```

## Building worlds in code

```go
b := pile.NewBuilder(nil, cube.Range{-64, 319})
b.Fill(lo, hi, block.Stone{})
b.SetMarker(pile.Marker{Name: "spawn", Kind: "spawn", Pos: [3]float64{0, 65, 0}})
p := b.Provider()          // in-memory world
_ = b.Save("maps/arena")   // or straight to disk
```

## Self-describing maps

Settings, named markers (spawn points, NPC spots, regions), a world border
and arbitrary world/chunk metadata travel inside the file:

```go
p.Markers() / p.SetMarker(m)
p.Border()  / p.SetBorder(&pile.Border{...})
p.UserData() / p.SetUserData(b)
p.ChunkUserData(pos, dim) / p.SetChunkUserData(pos, dim, b)
```

Snapshots for versions and grief rollback: `p.Snapshot("clean")`,
`p.Rollback("clean")`, `p.Snapshots()`. Autosave: `stop := p.AutoSave(5*time.Minute)`.

## CLI

`go install github.com/oriumgames/pile/cmd/pile@latest` installs: convert (mcdb ⇄
pile), inspect, verify, stats, check, render, compact, mode, upgrade, prune,
move, extract, paste, origin, diff, patch/apply, export/import. Every command
that decodes chunk content takes `--max-decoded n`, the CLI's
`pile.MaxDecodedBytes`. See [cmd/pile/readme.md](cmd/pile/readme.md).

## Notes

- Pin your dragonfly version: the codec uses layout-asserted unsafe access to
  chunk internals and panics loudly at startup (instead of corrupting data)
  if a dragonfly upgrade changes those layouts.
- For an enormous constantly-mutating survival world, dragonfly's own mcdb
  remains the better fit; providers are per-world and coexist.

Based on ideas from [hollow-cube/polar](https://github.com/hollow-cube/polar).
MIT licensed; see [license.md](license.md).
