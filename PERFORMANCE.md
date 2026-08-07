# Performance baselines

`FREEZE.md`, "Known deferred work": *benchmarks exist but nothing gates them,
and there are no recorded baselines*. This is the record. It says what the
mechanism is and why it is not a threshold, what the numbers are, and which of
`STATUS.md`'s measured figures still hold.

Nothing here can invalidate a file. Optimisation is permitted at any time
provided the bytes do not move, which the golden and vector suites verify; this
document is about noticing when the opposite happens.

## The mechanism, and why it is not a gate

Four parts, in decreasing order of how much they can tell you and increasing
order of how reliably they can tell you it.

1. **`scripts/bench.sh`** pins the run parameters. A benchmark result is only
   comparable against another run of the same shape, and the shape includes
   `-benchtime`, `-count` and the package set. Leaving those to whoever runs it
   is how two numbers that measure different things end up in the same table.
2. **`testdata/benchmarks.txt`** and **`format/testdata/benchmarks.txt`** are
   the recorded baselines: raw `go test -bench` output, six samples per
   benchmark, in the format `benchstat` reads. `scripts/bench.sh compare <dir>`
   diffs a fresh run against them.
3. **`TestBenchmarkBaselinesRecorded`** (one per package) requires every
   benchmark in the tree to appear in the recorded baseline. It measures
   nothing and takes microseconds. What it prevents is the failure mode a
   checked-in results file has: a benchmark added or renamed months later,
   never re-recorded, and the record quietly stops covering the thing that
   regressed.
4. **`TestIndexedStoreDoesNotScaleWithPalette`** is the one assertion that can
   actually fail on a regression. It is a *ratio* between two allocation
   measurements taken in the same process, so it is exact and
   machine-independent, and it holds the one shape from the memory pass that
   had a benchmark and no test.

**There is deliberately no wall-clock threshold anywhere.** A time threshold in
an ordinary test run is noise on a shared machine, and the way a noisy gate ends
is muted rather than fixed — at which point the suite is worse off than with no
gate at all, because there is now a green check where there used to be a known
hole. The evidence for this is in the next section and it is not hypothetical:
the baselines below were recorded on a machine where the same benchmark varied
by a factor of six between samples.

The trade is explicit. This mechanism makes a regression **visible and
attributable** — `benchstat` against a checked-in six-sample baseline names the
benchmark, the direction and the confidence — and it does not make one **fail**,
except for allocation shapes where failing is sound.

### Recording and comparing

```sh
scripts/bench.sh record            # rewrite the checked-in baselines
scripts/bench.sh run /tmp/after    # a comparison run, same parameters
scripts/bench.sh compare /tmp/after
```

`benchstat` comes from `golang.org/x/perf/cmd/benchstat`. Record on an idle
machine; see below for what happens when you do not.

## The variance trap

Two distinct traps, both of which fired while this document was being written.

### A short run measures construction, not work

`-benchtime` must stay time-based. Never `Nx` for a small N. Several paths here
build a zstd encoder, a decoder pool or a dictionary codec on first use, and a
run of a handful of iterations charges that one-time cost to every one of them.
Measured on `BenchmarkProviderStoreColumnAppend`, same code, same machine:

| run | B/op |
|---|---|
| `-benchtime=1x` | 52,944,720 |
| `-benchtime=1s`, first of six samples | 291,140 |
| `-benchtime=1s`, the other five | ~160,490 |

Two orders of magnitude between the first row and the last, and none of it is a
difference in the work being measured. This has already been misread here once
as an allocation regression. The same shape is visible in the checked-in
baseline at `BenchmarkWriteWorldCompression/best` (4,196,814 in five samples,
4,579,195 in the sixth) and at `BenchmarkIndexedCompact/100`, where the spread
is dictionary training rather than encoder construction.

The fix is not to warm the benchmark up. It is to run long enough that the
one-time cost amortises, and to read a table of six samples rather than one
number — which is why the baselines are checked in as raw samples and not as a
summary.

### Wall-clock numbers do not survive a busy machine, and allocation counts do

The baselines below were recorded on an 8-core Apple M1 while other work was
running on the same host: load average measured between 12 and 30 across the
run. The effect on the two kinds of number is completely different, and the
difference is the whole reason the mechanism above pins one and not the other.

`BenchmarkCloneColumn` does one deep copy of a column and nothing else:

| run | ns/op | B/op | allocs/op |
|---|---|---|---|
| `-benchtime=1x`, quiet | 19,916 | 8,288 | 127 |
| `-benchtime=1s`, six samples | 8,512 – 8,972 | 8,288 | 127 |
| `-benchtime=1s`, three samples, later | 32,543 – 38,982 | 8,288 | 127 |

A 4.6× spread in the time and **not one byte** of movement in the allocations.
Across the whole recorded baseline the widest time spread within a single
benchmark's six samples is 5.7× (`BenchmarkProviderStoreColumn`), while every
allocation count is identical across all six samples and every `B/op` figure is
stable to within a fraction of a percent apart from the first-sample
construction effect above.

**Consequently: the `ns/op` column of the checked-in baseline is not a time
baseline.** It is an upper bound taken under contention, recorded so the
artefact is complete and so a later run can be compared against something. Treat
it as advisory, and re-record on an idle machine before using it to claim
anything about latency. The `B/op` and `allocs/op` columns are exact counts,
they are what this document pins, and they are what the two tests above enforce.

## The baselines

Recorded with `scripts/bench.sh record` — `-benchtime=1s -count=6 -benchmem` —
on `darwin/arm64`, Apple M1, 8 cores, Go 1.26.2, dragonfly v0.11.1. Full
per-sample output is in `testdata/benchmarks.txt` and
`format/testdata/benchmarks.txt`. `B/op` and `allocs/op` are medians; where six
samples disagreed the range is given.

### What a user feels

The format targets small worlds — lobby maps, minigame maps, skyblock — while
staying usable for large ones. These are the four numbers that decide whether it
does.

**Memory per open handle.** The indexed-mode contract is "directory, palettes,
and one record at a time". It holds, and the gap it buys is the reason append
mode exists:

| opening a 10,000-column world | B/op | allocs/op |
|---|---|---|
| solid (`BenchmarkProviderOpen/10000`) | 96,389,776 | 1,930,411 |
| append (`BenchmarkProviderOpenAppend/10000`) | 2,033,000 | 226 |

47× less memory and four orders of magnitude fewer allocations, and the append
figure is a directory rather than a world: 203 bytes per column, against 9.6 KB
per column for the solid path that decodes all of them. The marginal cost is
flat — 10,464 B at one column, 26,757 at a hundred, so about 165 B per further
column — and the allocation count does not grow with the world at all (137, 146,
226 at 1, 100 and 10,000 columns). At the format level the same shape:
`BenchmarkOpenIndexed/1000` is 670,290 B and **132 allocations** for a
thousand columns, against `BenchmarkReadWorldSize/10000` at 139 MB and 2.58M.

**Allocations per column read.**

| | B/op | allocs/op |
|---|---|---|
| `BenchmarkProviderLoadColumnCached` | 5,938 | 114 |
| `BenchmarkProviderLoadColumn` (solid, in memory) | 6,240 | 123 |
| `BenchmarkProviderLoadColumnUncached` | 17,214 | 228 |
| `BenchmarkIndexedColumn` (format, hot record) | 29,962 | 294 |
| `BenchmarkIndexedColumnRandom` (format, spread) | 35,316 | 295 |
| `BenchmarkCloneColumn` (the floor) | 8,288 | 127 |

The floor matters: `BenchmarkCloneColumn` is the deep copy the isolation
guarantee costs, and both `LoadColumn` and `StoreColumn` pay it once per call.
No provider read can go below 127 allocations without changing that guarantee.
An LRU hit costs slightly less than the clone alone (114 against 127) because
the two rows use different fixtures, not because a cache hit is cheaper than a
copy — it *is* a copy. A miss costs exactly twice a hit.

**Save latency.** Solid saves are a full deterministic rewrite; append saves are
a checkpoint.

| | ns/op (advisory) | B/op | allocs/op |
|---|---|---|---|
| `BenchmarkProviderSave/1` | 37.4 M | 239,320 | 324 |
| `BenchmarkProviderSave/100` | 32.6 M | 3,257,000 | 13,046 |
| `BenchmarkProviderSave/10000` | 298 M | 303,824,300 | 1,280,291 |
| `BenchmarkProviderSaveAppend/100` | 9.6 M | 7,402 | 25 |
| `BenchmarkProviderSaveAppend/10000` | 15.4 M | 664,813 | 40 |

A solid save costs about 30 KB and 128 allocations per column and rewrites
everything; a checkpoint costs 25–40 allocations whatever the world size, and
its bytes are the directory it rewrites — 66 B per column, which is the term
that does scale. At the sizes this format is aimed at (a lobby is tens to a few
hundred columns) a solid save is 3.3 MB of allocation and about 13,000
allocations, which is one autosave tick's worth of garbage and nothing a server
notices.

The `ns/op` figures here are the ones to distrust most: the two `Save/1` and
`Save/100` rows differ by less than their own sample spread, and both are
dominated by two fsyncs.

**The indexed-mode contract, at the format level.**

| | B/op | allocs/op |
|---|---|---|
| `BenchmarkIndexedStore` | 218,330 | 196 |
| `BenchmarkIndexedStoreLargePalette` | 218,329 | 196 |
| `BenchmarkIndexedStoreDistinct` | 218,460 | 196 |
| `BenchmarkIndexedCheckpoint` | 7,687 | 18 |
| `BenchmarkOpenIndexed/100` | 68,550 | 113 |
| `BenchmarkOpenIndexed/1000` | 670,290 | 132 |
| `BenchmarkIndexedColumn` | 29,962 | 294 |

Three `Store` variants, three identical allocation figures: storing into a
17,499-entry palette, into an empty one, and at 1,024 distinct positions all
cost the same. That is the contract holding under the three ways it could break.

Compaction is the exception and is meant to be: `BenchmarkIndexedCompact/1000`
allocates 292–304 MB, because compaction reads every live record and rewrites
it. It is the one indexed operation that is linear in the world, `Close`
performs it automatically past the garbage threshold, and that is the event a
large world's shutdown is paying for.

### Full table

Medians over six samples. `ns/op` advisory (see above); `B/op` and `allocs/op`
pinned.

#### `github.com/oriumgames/pile`

| benchmark | ns/op | B/op | allocs/op | ns spread |
|---|---|---|---|---|
| `ProviderStoreColumn` | 5,736,076 | 8,464 | 139 | 5.7× |
| `ProviderLoadColumn` | 83,460 | 6,240 | 123 | 2.2× |
| `ProviderStoreColumnAppend` | 3,877,800 | 160,490 | 313 | 2.0× |
| `ProviderLoadColumnCached` | 63,068 | 5,938 | 114 | 3.3× |
| `ProviderLoadColumnUncached` | 322,995 | 17,214 | 228 | 4.4× |
| `ProviderSave/1` | 37,385,306 | 239,320 | 324 | 1.6× |
| `ProviderSave/100` | 32,615,620 | 3,257,000 | 13,046 | 3.1× |
| `ProviderSave/10000` | 298,230,760 | 303,824,300 | 1,280,291 | 1.4× |
| `ProviderOpen/1` | 316,264 | 28,808 | 443 | 1.6× |
| `ProviderOpen/100` | 9,707,338 | 985,300 | 19,606 | 4.7× |
| `ProviderOpen/10000` | 344,115,170 | 96,386,000 | 1,930,411 | 2.7× |
| `ProviderOpenAppend/1` | 249,658 | 10,464 | 137 | 1.9× |
| `ProviderOpenAppend/100` | 412,298 | 26,757 | 146 | 1.2× |
| `ProviderOpenAppend/10000` | 18,563,440 | 2,033,000 | 226 | 1.2× |
| `ProviderSaveAppend/100` | 9,554,714 | 7,402 | 25 | 1.3× |
| `ProviderSaveAppend/10000` | 15,394,830 | 664,813 | 40 | 1.1× |
| `ProviderColumns` | 856,048 | 596,416 | 11,407 | 1.3× |
| `CloneColumn` | 8,718 | 8,288 | 127 | 1.1× |
| `StructureExtract` | 21,896,354 | 129,168 | 2,164 | 3.6× |
| `StructurePaste` | 53,733,432 | 192,800 | 3,709 | 1.7× |
| `StructureRotate` | 61,273,170 | 34,192 | 340 | 1.7× |
| `StructureSave` | 14,811,460 | 709,383 | 461 | 1.5× |
| `StructureLoad` | 681,338 | 98,690 | 375 | 1.2× |
| `MoveWorldFast` | 18,245,554 | 4,241,600 | 32,528 | 1.1× |
| `MoveWorldSlow` | 595,027,469 | 13,388,200 | 90,931 | 1.2× |
| `ColumnsFirst` | 22,848 | 14,016 | 267 | 1.1× |
| `ColumnsAll` | 1,549,437 | 863,264 | 16,947 | 1.5× |
| `StoreAppendSpread` | 369,590 | 160,830 | 316 | 1.6× |

#### `github.com/oriumgames/pile/format`

| benchmark | ns/op | B/op | allocs/op | ns spread |
|---|---|---|---|---|
| `WriteWorld` | 11,502,884 | 4,198,000 | 10,734 | 1.5× |
| `WriteWorldFast` | 9,550,638 | 4,198,000 | 10,730 | 1.5× |
| `ReadWorld` | 5,835,364 | 826,900 | 16,384 | 1.7× |
| `ReadWorldVaried` | 4,699,740 | 360,900 | 3,704 | 1.6× |
| `IndexedStore` | 472,956 | 218,330 | 196 | 1.3× |
| `IndexedStoreLargePalette` | 453,252 | 218,329 | 196 | 1.5× |
| `IndexedColumn` | 242,308 | 29,962 | 294 | 1.6× |
| `WriteWorldSize/1` | 416,056 | 276,692 | 249 | 1.4× |
| `WriteWorldSize/100` | 20,552,255 | 8,849,170 | 17,010 | 1.3× |
| `WriteWorldSize/10000` | 1,330,042,896 | 722,120,000 | 1,687,336 | 1.4× |
| `ReadWorldSize/1` | 331,093 | 33,390 | 308 | 1.3× |
| `ReadWorldSize/100` | 11,746,398 | 1,756,000 | 25,950 | 1.8× |
| `ReadWorldSize/10000` | 724,936,323 | 139,060,000 | 2,577,414 | 1.6× |
| `WriteWorldCompression/none` | 8,539,608 | 4,213,560 | 10,725 | 1.6× |
| `WriteWorldCompression/fast` | 9,859,874 | 4,200,000 | 10,726 | 2.2× |
| `WriteWorldCompression/default` | 8,625,722 | 4,197,030 | 10,727 | 1.2× |
| `WriteWorldCompression/best` | 7,987,726 | 4,196,814 | 10,727 | 1.2× |
| `ReadMeta` | 229,900 | 443,500 | 33 | 1.3× |
| `WriteStructure` | 3,150,811 | 788,872 | 819 | 1.2× |
| `ReadStructure` | 2,084,364 | 71,200 | 971 | 1.2× |
| `IndexedStoreDistinct` | 321,590 | 218,460 | 196 | 1.1× |
| `IndexedCheckpoint` | 9,156,942 | 7,687 | 18 | 1.1× |
| `IndexedCompact/100` | 499,405,142 | 34,121,626 – 41,837,352 | 51,480 | 3.0× |
| `IndexedCompact/1000` | 1,618,747,520 | 292,676,768 – 304,238,904 | 497,078 | 2.1× |
| `OpenIndexed/100` | 326,238 | 68,550 | 113 | 1.2× |
| `OpenIndexed/1000` | 2,346,910 | 670,290 | 132 | 1.1× |
| `IndexedColumnRandom` | 210,412 | 35,316 | 295 | 1.6× |

Two rows are worth reading twice.

`WriteWorldCompression` shows no useful separation between compression levels
in time, which is not credible for `none` against `best` — it is contention
swamping the signal, and it is the clearest single demonstration that this
column needs re-recording on an idle machine. The allocation figures across the
four levels are, by contrast, flat within 0.4%, which is the real finding: the
compressor level costs CPU and not memory.

`ReadMeta` allocates 443 KB and **33 allocations** to read the header of a
100-column world. It never touches a chunk record. `SECURITY.md` records that
`ReadMeta` can be made to allocate ~82 MiB transiently from a 132-byte settings
blob, and that remains true and remains within the §8 NBT container budget; the
number here is what it costs on a file that is not trying.

## `STATUS.md`'s figures, re-checked

`STATUS.md`'s memory section records six before-and-after measurements. Three of
them have a benchmark that corresponds; all three still hold. The other three
are retention figures with no per-operation benchmark behind them, and this
document does not confirm them.

| `STATUS.md` claim | benchmark | measured now | verdict |
|---|---|---|---|
| `Store` into a 17,499-entry palette: 1,680,870 → 218,342 B/op, "now identical to storing into an empty one" | `IndexedStoreLargePalette` vs `IndexedStore` | 218,329 vs 218,330 B/op, 196 allocs each | **holds**, and the "identical" claim is exact: the two differ by a single byte in the median and by nothing in the allocation count |
| `Columns()` breaking after one of 64: 883,665 → 14,058 B/op, 16,888 → 266 allocs | `ColumnsFirst` vs `ColumnsAll` | 14,016 B/op, 267 allocs, against 863,264 and 16,947 for the full iteration | **holds** (one allocation more than recorded; 62× less than a full iteration) |
| reading a world whose sections almost all differ: 513,604 → 360,876 B/op | `ReadWorldVaried` | 358,563 – 365,034 B/op, median 360,900 | **holds** |
| shared zstd decoders under 8 concurrent decodes: 194.6 MiB retained → 0 | — | not measured | no benchmark measures retained memory across concurrent handles; it is a heap-profile figure, and nothing here would notice it coming back |
| dictionary codecs with 8 writing handles on one dictionary: 88 MiB → 0 | — | not measured | as above |
| `udCache`/`unkCache` folded into one bounded LRU, 64 MiB weight budget | — | not measured | the bound is a policy, not a per-operation cost |

The three unmeasured rows are the real gap this document leaves open. They are
all of the same kind — *memory retained by a live handle*, not memory allocated
by an operation — and `go test -bench` cannot express them: `B/op` is a
cumulative allocation counter, and a leak that retains 194 MiB across eight
handles allocates it exactly once. Closing this properly means a heap-profile
harness rather than a benchmark, and it is not something a `B/op` figure can be
made to say. Recorded here rather than left implied.

## What no benchmark covers

- **Retained memory per live handle**, above. The three memory fixes with the
  largest numbers behind them are exactly the three nothing watches.
- **Concurrency.** Every benchmark is single-goroutine. The zstd pools, the
  provider's lock discipline and the column LRU are all shared state under
  concurrent load, and none of it is measured under any.
- **Cold cache and real I/O.** Every file benchmark runs against a `t.TempDir`
  the page cache has just written. `BenchmarkIndexedColumnRandom` spreads reads
  across a thousand records to avoid one hot page, which is as close as the
  suite gets; on a world larger than RAM, `pread` per frame is the cost that
  matters and nothing here reaches it.
- **The CLI.** `cmd/pile` has no benchmarks. `convert` on a real mcdb world is
  the longest single operation this project performs.
