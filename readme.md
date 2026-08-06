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
`pile.StoreLight()`, `pile.Registry(...)`, `pile.WithSpawnStore(...)`.
Player data never lives in pile files.

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
move, extract, paste, origin, diff, patch/apply, export/import. See
[cmd/pile/readme.md](cmd/pile/readme.md).

## Format

Binary specification for other implementations:
[format/format.md](format/format.md). Codec package docs:
[format/readme.md](format/readme.md).

## Notes

- Pin your dragonfly version: the codec uses layout-asserted unsafe access to
  chunk internals and panics loudly at startup (instead of corrupting data)
  if a dragonfly upgrade changes those layouts.
- For an enormous constantly-mutating survival world, dragonfly's own mcdb
  remains the better fit; providers are per-world and coexist.

Based on ideas from [hollow-cube/go-polar](https://github.com/hollow-cube/go-polar).
MIT licensed; see [license.md](license.md).
