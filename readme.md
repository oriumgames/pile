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

**If the worlds come from strangers, read the recipe** in
[SECURITY.md](SECURITY.md), "Loading a file somebody sent you". It is three
options long, it has been tested against hostile files rather than reasoned
about, and it says plainly what is still not bounded — the short version being
that `MaxDecodedBytes` charges columns and section storages and charges nothing
for the entities, block entities and scheduled updates a column may hold a
million of. `LoadSkip` is **not** a bound: it drops categories after the file is
decoded, so it removes nothing from the peak.

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

## Compatibility

The v2 wire format is frozen. What that does and does not promise, for anyone
deciding whether to depend on it:

**What is frozen.** The bytes a writer produces for given content are fixed:
identical content encodes to an identical file, on any build that writes v2, and
moving those bytes is a breaking change requiring `format.Version` to be
incremented. A v2 reader accepts exactly the files
[format/format.md](format/format.md) defines and refuses everything else. That
includes other versions in both directions: a v3 reader will refuse a v2 file and
a v2 reader will refuse a v3 file. There is no forward-compatibility lane, by the
decision recorded in §2.1 — not an omission.

**What is not frozen.**

- **Compressed bytes.** Zstandard admits many valid encodings of the same
  content, so a different compressor, level or version can store the same world
  as different bytes without the format having changed. This is why file
  identity is `format.ContentHash` (decode, re-encode uncompressed) rather than
  a hash of the stored bytes.
- **Indexed-mode byte layout over time.** Indexed files (`pile.AppendMode()`)
  are history-dependent by design: the same content stored in a different order
  is a different file. Their identity is `ContentHash` too.
- **The Go API.** Frozen separately and later. It has now been reviewed as a
  surface ([API.md](API.md)); the changes that would break a caller are
  recommended there and deliberately not made, so expect it to move at the API
  freeze.
- **Performance and memory.** Optimisation is permitted at any time, provided
  the bytes do not move. Recorded baselines are in
  [PERFORMANCE.md](PERFORMANCE.md).

**`ContentHash` identifies the body, not the file.** The dimension lives in the
header, and decoding does not fold it into the body, so two files holding the
same chunks in different dimensions have the *same* `ContentHash`. That is
deliberate and is frozen along with everything else. A caller that spans
dimensions must key on the dimension separately — this is the easiest thing here
to get wrong.

**Integrity against corruption, not against tampering.** xxHash64 is not a
cryptographic hash. The checksums catch damage; they do not establish that a
file is the one you wrote, and an attacker who can author content and induce a
truncation can forge a checkpoint that verifies. Files from untrusted sources
are untrusted content. Decoding one will not panic and malformed input is
rejected, but a *conforming* file of about a kilobyte can still legally decode
into more than a gigabyte, because the format's own ceilings are set at what it
can represent. `pile.MaxDecodedBytes(n)` — `format.MaxDecodedBytes` on the codec,
`--max-decoded` on the command line — is the knob for that case. It bounds
columns and section storages and does not bound the per-chunk collections, so it
is a dial and not a fence; the full threat model, the recipe and the measured
gap are in [SECURITY.md](SECURITY.md).

**Writing a second implementation.** The binary specification is
[format/format.md](format/format.md); where it and this implementation disagree,
the implementation wins and the specification has a bug. The conformance
appendix is [format/vectors.md](format/vectors.md): 18 positive vectors with
their expected `ContentHash` and 59 negative ones, each naming the rule a
conforming reader must refuse it for, with the files themselves in
`format/testdata/vectors/`. Sixteen of the positives pin bytes; the two indexed
ones pin only what a reader must conclude, because §5 makes an indexed file's
bytes history-dependent. It also records what no vector can express. Codec
package docs: [format/readme.md](format/readme.md).

## Notes

- Pin your dragonfly version: the codec uses layout-asserted unsafe access to
  chunk internals and panics loudly at startup (instead of corrupting data)
  if a dragonfly upgrade changes those layouts.
- For an enormous constantly-mutating survival world, dragonfly's own mcdb
  remains the better fit; providers are per-world and coexist.

Based on ideas from [hollow-cube/polar](https://github.com/hollow-cube/polar).
MIT licensed; see [license.md](license.md).
