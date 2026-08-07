# The Go API, reviewed as a surface

`FREEZE.md`, "Known deferred work": *the Go API surface has never been reviewed
as a surface*. This is that review. The API is frozen separately from the wire
format and later, which is what makes now the cheap moment: after the API
freeze every item below costs a major version.

The rule this review works under is the one the wire format works under in
reverse. **Nothing here may change the bytes a writer produces or which files a
reader accepts** — every change made below is a doc comment or an addition, and
the golden and both vector suites were green with no flags throughout.
**Anything that would break a caller is recommended and not done**, because
this is a library other code depends on and a review is not a licence to break
it.

Each item says what it is, what it should be, and which of three states it is
in: **done** (made here), **recommended** (safe but breaking, so left), or
**settled** (looked at, deliberately left as it is).

---

## The surface, in one place

**`github.com/oriumgames/pile`** — 15 types (`Provider`, `Option`, `SkipMask`,
`SpawnStore`, `Marker`, `Border`, `Structure`, `StructureOption`,
`StructureLibrary`, `Builder`, `Template`, `MoveOptions`, `MoveReport`,
`WorldFiles`, `DimFile`), 14 `Option` constructors, 2 `StructureOption`
constructors, 4 re-exported compression levels, 5 error values (3 of them added
here), 44 methods on `*Provider`, `*Structure`, `*Builder`, `*Template`,
`*StructureLibrary`, `*WorldFiles` and `*MoveReport`, and 12 package functions.

**`github.com/oriumgames/pile/format`** — 16 types, 19 functions, 8 error
values and 28 constants, eleven of which are the harness vocabulary of item 9.

## What was changed

Five doc comments and three error values. All additive or explanatory; no
signature moved.

### 1. `pile.ErrNotFound`, `pile.ErrCorrupt`, `pile.ErrDecodeBudget` — **done**

The brief's "error sentinels a caller must be able to branch on" was the
clearest hole in the surface. Before this change, a caller of `pile` that wanted
to branch on the three things that actually happen had to import three packages:

```go
if errors.Is(err, leveldb.ErrNotFound) { ... }        // github.com/df-mc/goleveldb
if errors.Is(err, format.ErrDecodeBudget) { ... }     // github.com/oriumgames/pile/format
if errors.Is(err, format.ErrCorrupt) { ... }
```

`leveldb.ErrNotFound` is the worst of the three. It is dragonfly's
`world.Provider` contract and it is not going to change, but a caller of a
single-file world format has no reason to have heard of leveldb, and
`LoadColumn`'s doc comment named it as the thing to compare against without
saying it lived in a third module.

The three are now re-exported as `pile.ErrNotFound`, `pile.ErrCorrupt` and
`pile.ErrDecodeBudget`. They are the same values, so `errors.Is` against either
name works and nothing that compiles today stops compiling. The precedent is the
package's own: the four compression levels are already re-exported from `format`
exactly this way.

`ErrDecodeBudget` in particular has to be reachable, because `MaxDecodedBytes`'s
documentation instructs the caller to branch on it — a provider option whose
doc comment sends you to another package for the error it produces.

### 2. `pile.ErrReadOnly`'s doc claimed one caller and has six — **done**

It said "returned by `Save` on a read-only provider". It is returned by `Save`,
`SetBorder`, `SetChunkUserData`, `Snapshot`, `DeleteSnapshot` and `Rollback`.
This is the `ContentHash` shape: a doc comment that describes the case its
author had in mind rather than the behaviour. The comment now names all six and,
more usefully, names the four mutators that *cannot* return it — see item 8.

### 3. `SaveAsync` and `AutoSave` never report a failure — **done** (doc)

Neither can return an error, so a failed background save is stashed in
`lastSaveErr` and returned by the next `Save` or `Close`. That is the right
design and it was documented nowhere: `AutoSave`'s comment described a ticker
and stopped, so a caller reading it has no reason to believe a failing autosave
will ever surface. Two further facts that were not written down and are now:
only the *most recent* failure is kept, and a process that only ever calls
`SaveAsync` observes none of them.

### 4. `Provider.Settings()` returns the live pointer — **done** (doc)

`Settings()` returns `p.settings` itself, not a copy, and `SaveSettings` stores
the pointer it is given. That is what dragonfly's `world.Provider` contract
requires — the engine takes the pointer at startup, mutates it as the world runs
and hands it back at shutdown — but it means a caller that changes a setting
through the returned pointer has changed it *and* not marked the world dirty, so
it may never be written. Documented rather than changed: changing it would break
dragonfly.

### 5. `Structure.Data()` hands out mutable internals — **done** (doc)

"Data exposes the underlying structure data" was true and unhelpful. It is the
`Structure`'s own `*format.StructureData`; the encoder requires `Cells` to stay
exactly `CellDims(Size)` long, so a caller that resizes one field without the
other has produced a value that fails to save, at save time, far from the
mistake.

### 6. `format.CheckpointHash`'s doc justified it by its test callers — **done**

"so tooling (and tests that rewrite files) can recompute it" is a reason to have
an unexported helper, not a reason for public API. The function is genuinely
public surface — the preimage is §2.4, it is frozen with everything else, and a
second implementation checking itself against the conformance vectors needs it
stated in code as well as in prose. The comment now says that and spells the
preimage out.

### 7. `BenchmarkProviderSaveAppend`'s comment overclaimed — **done**

Not API, but the same defect: "a checkpoint ... which should not scale with the
world size". Its allocation count does not scale (25 at a hundred columns, 40 at
ten thousand). Its bytes do, at about 66 B per column, because a checkpoint
rewrites the directory. Corrected against the recorded baseline.

---

## What is recommended and was not done

Every item here would break a caller. They are listed in the order I would take
them at the API freeze.

### 8. Read-only mutators disagree about how they refuse — **recommended**

Four different behaviours across ten mutating methods on one type:

| behaviour | methods |
|---|---|
| returns `ErrReadOnly` | `Save`, `SetBorder`, `SetChunkUserData`, `Snapshot`, `DeleteSnapshot`, `Rollback` |
| returns `nil`, silently discarding the write | `StoreColumn` |
| returns `false`, indistinguishable from "no such marker" | `RemoveMarker` |
| returns nothing at all | `SaveSettings`, `SetUserData`, `SetMarker` |

`ReadOnly()`'s own doc says "all mutating operations are no-ops", so none of this
is undocumented — but `StoreColumn` returning `nil` after dropping a column is a
silent-data-loss shape, and it is the one method a server calls in a loop.

Recommend `StoreColumn` return `ErrReadOnly`. **Not done**: dragonfly's world
engine calls `StoreColumn` on every chunk unload and does not expect an error
from a provider the operator deliberately opened read-only, so this turns a
quiet no-op into a log full of errors for every existing user of `ReadOnly()`.
It is a behaviour change and it needs to arrive with the API freeze, not before
it. `IsReadOnly()` is the escape hatch today and the doc now points at it.

### 9. `format` exports its own test harness's vocabulary — **recommended**

`Invariant`, `Category`, `Enforcement` and the eight constants `Presence`,
`Bound`, `Ordering`, `Omission`, `Normalisation`, `Integrity`, `Decoded` and
`WriterOnly` are all exported. The table they describe, `invariants`, is not:

```go
var invariants = []Invariant{ ... }   // format/invariants.go:80
```

So a caller can construct an `Invariant` and can never obtain one. Eleven
symbols of public surface that lead `go doc format` and mean nothing outside
this repository's own harness. Nothing in the tree references them from outside
the `format` package.

Recommend unexporting all eleven. **Not done**: unexporting removes symbols,
which is breaking by definition even when nothing can usefully have used them.

### 10. `Meta` cannot tell you which dimension a file holds — **recommended**

This is the one item where the missing API is visible in shipped behaviour.
`ReadMeta` is the cheap header read — 33 allocations, no chunk record touched —
and it returns `Kind`, `Mode`, `Flags`, `BlockVersion` and the metadata blobs.
The dimension is in `Flags`, at bits 5-7, and both the mask and the shift are
unexported:

```go
dimensionMask  = uint32(0b111) << dimensionShift   // format/format.go:87
dimensionShift = 5
```

A caller therefore cannot name the dimension of a solid file without decoding
the whole thing with `ReadWorld`, or hard-coding two constants it has to read
the specification to learn. The consequence is in this repository's own CLI:
`pile inspect` prints

```
flags         0x00000018
```

and leaves the reader to decode it, because it has nothing better to print.
(`IndexedWorld.Dimension()` exists; the solid path has no equivalent.)

Recommend a method — `func (m *Meta) Dimension() Dimension` — rather than a
field, because a method is purely additive and a new struct field would break
any unkeyed composite literal of `Meta`. **Not done**: it is new API on a type
under review, and adding it is the API freeze's call rather than this review's.
`format.ContentHash`'s documented caveat makes it load-bearing — the caveat is
"key on the dimension alongside this value", and this is the reason a caller
cannot cheaply do so.

### 11. `pile.FileMode` reads as a permission bit — **recommended**

```go
func FileMode(path string) (uint8, error)   // returns format.ModeSolid or ModeIndexed
```

Every Go programmer reads `FileMode` as `os.FileMode`. This one returns a
storage-mode byte. Recommend `StorageMode`, returning a named type rather than
`uint8` (see item 12). **Not done**: rename.

### 12. `Meta.Kind` and `Meta.Mode` are bare `uint8` — **recommended**

`Dimension` is a named type with a `String()` method. `Flags` has typed `uint32`
constants. `Kind` and `Mode`, in the same struct, are `uint8` with *untyped*
constants (`KindWorld = 0`, `ModeSolid = 0`), so `m.Kind == format.ModeIndexed`
compiles and is meaningless. Recommend `type Kind uint8` and `type Mode uint8`
with `String()`, which would also give `cmd/pile` its `kindName`/`modeName`
helpers for free. **Not done**: changing a struct field's type is breaking.

### 13. `WithSpawnStore` is the naming outlier — **recommended**

The brief names it, and the cause is worth recording because it constrains the
fix. Thirteen of the fourteen options are bare nouns or verbs — `ReadOnly`,
`StoreLight`, `FastSaves`, `CacheColumns`, `AppendMode`, `Compression`,
`Registry`, `MaxDecodedBytes`, `Skip`, `LoadSkip`, `FilterEntity`,
`FilterBlockEntity`, `FilterColumn`. The fourteenth is `WithSpawnStore`, and it
carries the `With` prefix because `SpawnStore` is already taken by the interface
it accepts.

Two ways out: rename the option (`Spawns(s SpawnStore)`), or rename the
interface (`SpawnStorage`, freeing `SpawnStore` for the option). The first is
smaller. **Not done**, and I recommend explicitly *not* softening it by adding
`Spawns` now and deprecating `WithSpawnStore`: two names for one option is worse
than one odd name until the deprecation is collected, and the API freeze is the
moment to collect it.

Note that `StructureOption` has the same pressure and resolved it the other way:
`StructureRegistry()` is prefixed because `Registry()` is taken by the
`Provider` option. That is one namespace serving two option families, and it is
the underlying cause of both awkward names.

### 14. `Builder.Settings` is a setter that reads as a getter — **recommended**

```go
func (b *Builder) Settings(s *world.Settings)    // sets
func (p *Provider) Settings() *world.Settings    // gets
```

Same package, same name, opposite meanings. Every other `Builder` method is
`SetBlock`, `SetMarker`, `SetUserData`, `AddEntity`, `AddBlockEntity`, `Fill`,
`FillBiome` — so `Settings` is also the outlier within its own type. Recommend
`SetSettings`. **Not done**: rename.

### 15. `IterError` is one slot, shared, and clears on read — **recommended**,
and it is a defect rather than a taste question

```go
func (p *Provider) IterError() error   // returns and clears
```

`Columns` returns an `iter.Seq2` and an iterator cannot yield an error, so the
first error is parked on the `Provider` and retrieved afterwards. The mechanism
is sound; the storage is not. There is exactly one `p.iterErr` for the whole
provider, and `IterError` clears it. Two consequences:

- Two goroutines iterating two dimensions share the slot. The first error wins,
  and whichever caller calls `IterError` first consumes it — so one iteration
  can silently swallow the other's error, and the doc's promise that "any caller
  for which a short iteration would mean data loss must check this after
  iterating" does not hold under concurrency.
- Checking twice returns `nil` the second time.

Recommend the error be carried per iteration rather than per provider — the
usual Go shape is for `Columns` to return `(iter.Seq2[...], func() error)`, so
the error belongs to the iterator that produced it and cannot be taken by
someone else. **Not done**: it changes `Columns`'s signature, which is the most
used method on the type after `LoadColumn`/`StoreColumn`. Reported here rather
than fixed, per this task's rules.

### 16. `WorldFiles`, `DimFile` and friends are the CLI's plumbing — **recommended**

`LoadWorldFiles`, `WorldFiles`, `DimFile`, `(*WorldFiles).Write`, `.Backup`,
`.Dim`, plus `DimPath` and `FileMode`, exist so that `cmd/pile`'s offline tools
(`move`, `diff`, `patch`, `prune`) can share code. `LoadWorldFiles`'s doc is
honest about it — "Intended for offline tools; servers use `Open`" — but they
are on the library's front page, and `DimFile.Columns []format.Column` means a
caller of `pile` who follows them has to learn `format`'s decoded model too.

Recommend moving them to `internal/worldfiles` at the API freeze; `cmd/pile` is
in the same module, so nothing in the tree would break. **Not done**: it removes
symbols an external caller may be using, and `MoveWorld` — which is public,
useful, and not going anywhere — is built on them, so the split needs designing
rather than performing.

### 17. §8's ceilings are documented as a caller's tool and are unexported — **recommended**

`SECURITY.md` tells a caller to "size its own limits" against the resource use a
hostile decode can reach. Of the numbers it would need, exactly one is exported:

```go
MaxLayers = maxLayers    // 255
```

`maxChunks` (4,194,304), `maxPalette`, `maxBlobLen`, `maxDecodedStorages`,
`maxNBTElements` and the rest are not. `MaxLayers` is exported with a good
reason in its comment — a mover that walks layers must walk all of them — and
the reason generalises: a caller sizing a limit against the format's own is in
the same position.

Recommend exporting a small named set (`MaxColumns`, `MaxDecodedStorages`,
`MaxBlobLen`) with the freeze's guarantee attached: they are validity rules, so
they can only be raised by a format revision. **Not done**: additive, so it is
safe, but adding public constants nobody has asked for to a package under review
is the kind of thing an API freeze should decide rather than inherit.

---

## What was looked at and settled

### 18. `Options.Compression`'s zero value is `CompressionNone` — **settled, and locked**

`format.Options{}` writes an uncompressed file, because `CompressionNone` is the
`iota` zero. That is a zero value which is not the sensible default: the
provider separately defaults to `CompressionBest`, and `Options`'s own comment
says "callers typically want `CompressionBest`".

This one cannot be fixed even in principle. `CompressionNone` sets the
`Uncompressed` header flag, so reordering the constants or changing the zero
would change the bytes an existing caller's `Options{}` produces. That is a wire
format change and it needs `format.Version` incremented. Left, documented,
flagged.

### 19. `WholeStorage = 0xFFFF` — **settled**

An untyped sentinel for `UnknownBlock.Index`, whose name does not say which
field it belongs to. `IndexWholeStorage` would read better. Too small to spend a
breaking change on; noted so it can ride along with items 11–14 if they are ever
taken together.

### 20. `ErrNoColumn` and `ErrReadOnlyFile` live away from the other sentinels — **settled**

`format.go` has a documented `var (...)` block of six sentinels headed "Decoding
errors wrap `ErrCorrupt` unless stated otherwise". `ErrNoColumn` is at
`indexed.go:60` and `ErrReadOnlyFile` at `indexed.go:2303`. Neither is a decoding
error and neither wraps `ErrCorrupt`, which is correct; they are simply
somewhere else, and `go doc` sorts them together anyway. Moving them would be
pure churn in production files for no observable gain.

### 21. `Structure.At`'s ignored parameter — **settled**

```go
func (s *Structure) At(x, y, z int, _ func(x, y, z int) world.Block) (world.Block, world.Liquid)
```

The blank parameter is dragonfly's `world.Structure` interface, not a mistake.

### 22. `format.MaxDecodedBytes` and `pile.MaxDecodedBytes` share a name — **settled**

Two functions, one name, different types (`ReadOption` and `Option`), one
mirroring the other. This is right: a caller moving between the two layers
should not have to learn a second name for one policy, and `FREEZE.md` and the
README already refer to them as a pair.

### 23. `NewMemory` returns no error where `Open` does — **settled**

`NewMemory(opts ...Option) *Provider` cannot fail: there is no directory to
open. Correct as it is.

### 24. `format.MarshalNBT` / `UnmarshalNBT` are one-line wrappers — **settled**

They forward to the unexported `marshalNBT`/`unmarshalNBT`. `pile` and
`cmd/pile` both use them, and a caller handed a `Meta.Settings` blob or a
`Column.UserData` blob has no other way to read it — every metadata field in the
public API is a `[]byte` of little-endian NBT. They earn their place.

---

## The one thing a maintainer should take from this

The surface is in good shape and the review turned up no defect that can
corrupt a file. What it turned up is a consistent pattern: **the parts of the
API that exist for this repository's own convenience are indistinguishable, from
outside, from the parts that exist for a caller.** Items 9, 16 and, in its
original form, 6 are all that shape — harness vocabulary, CLI plumbing and a
test helper, sitting in the same namespace as `Open` and `LoadColumn`.

The single mechanical improvement that would do the most for the surface is
therefore not a rename. It is deciding, per exported symbol, whether a caller
outside this module is meant to use it, and moving the answers that are "no"
into `internal/`.
