package format

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/df-mc/dragonfly/server/world/chunk"
)

// The caller's decode ceiling (MaxDecodedBytes) is policy layered over §8's
// validity rules, and the whole feature turns on the two never being confused:
//
//   - the default must decode exactly what a reader without the option decodes,
//     file for file and byte for byte, or the option has changed the format;
//   - a refusal under a caller's ceiling must not claim the file is invalid, or
//     a caller that quarantines corrupt files will quarantine conforming ones;
//   - a caller must not be able to raise the ceiling past §8's, or this reader
//     accepts files a conforming reader must refuse.
//
// One test each, below.

// budgetFiles returns every checked-in file that a whole-file reader can be
// pointed at: the goldens and the conformance vectors, positive and negative.
func budgetFiles(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, dir := range []string{goldenDir, filepath.Join(goldenDir, "vectors")} {
		ents, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range ents {
			if strings.HasSuffix(e.Name(), ".pile") {
				out = append(out, filepath.Join(dir, e.Name()))
			}
		}
	}
	if len(out) < 60 {
		t.Fatalf("only %d fixtures found; this test is meant to sweep the whole corpus", len(out))
	}
	return out
}

// TestDecodeBudgetDefaultChangesNothing is the load-bearing one. Every checked
// in file -- the goldens and all 76 conformance vectors, the accepted and the
// refused alike -- is read twice, once with no options and once with the
// explicit zero, and both must reach the same verdict for the same reason and
// produce the same ContentHash.
//
// A file refused for its own rule must still be refused for that rule, which is
// the part a passing suite does not prove on its own: a budget that fired too
// early would turn a negative vector from "refused for the rule it was built to
// break" into "refused for the budget", and every negative test that only
// checks for *an* error would stay green.
func TestDecodeBudgetDefaultChangesNothing(t *testing.T) {
	reg := testRegistry(t)
	for _, path := range budgetFiles(t) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			file, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			// The two spellings of "no policy": no option at all, and the
			// option carrying the value defined to mean §8's own ceiling.
			for _, opt := range [][]ReadOption{nil, {MaxDecodedBytes(0)}} {
				bare, bareErr := ContentHash(file, reg)
				got, gotErr := ContentHash(file, reg, opt...)
				switch {
				case (bareErr == nil) != (gotErr == nil):
					t.Fatalf("the default ceiling changed the verdict: %v vs %v", bareErr, gotErr)
				case bareErr != nil && bareErr.Error() != gotErr.Error():
					t.Fatalf("the default ceiling changed the reason:\n bare: %v\n opts: %v", bareErr, gotErr)
				case bareErr == nil && bare != got:
					t.Fatalf("the default ceiling changed the content hash: %016x vs %016x", bare, got)
				}
				// No file may ever be refused for the budget at the default.
				if gotErr != nil && errors.Is(gotErr, ErrDecodeBudget) {
					t.Fatalf("the default ceiling refused a file as over budget: %v", gotErr)
				}
			}
		})
	}
}

// TestDecodeBudgetCeilingIsUnreachableByDefault proves the property the sweep
// above can only sample: the default ceiling is above the most §8 permits one
// decode to cost, so it cannot fire on any conforming file rather than merely
// on the ones there are fixtures for.
//
// The trailing column of headroom is what makes the indexed split safe: a
// handle spends the ceiling on its directory first and hands a record whatever
// is left, and without it a directory at the entry ceiling would leave a
// remainder one column short of what §8 still lets one record reach.
func TestDecodeBudgetCeilingIsUnreachableByDefault(t *testing.T) {
	worst := int64(maxChunks)*columnBytes + int64(maxDecodedStorages)*storageBytes
	if decodedBytesCeiling <= worst {
		t.Fatalf("the default ceiling is %d and §8 permits a decode to reach %d: the default can refuse a conforming file",
			decodedBytesCeiling, worst)
	}
	// An indexed handle at the directory ceiling must still afford one whole
	// record at §8's own storage limit.
	left := decodedBytesCeiling - int64(maxChunks)*columnBytes
	if oneRecord := int64(columnBytes) + int64(maxDecodedStorages)*storageBytes; left < oneRecord {
		t.Fatalf("a directory at the entry ceiling leaves %d bytes, and §8 lets one record cost %d",
			left, oneRecord)
	}
}

// emptyColumnWorld builds the file that is the accepted residual: n chunk
// records that each declare one section and mark none present. It is a legal
// file — the reader accepts it, and the test below checks that it does — and it
// decodes into n whole columns and not one section storage.
//
// That is what makes it the fixture the column charge needs. Every other
// fixture carries storages too, so a ceiling tight enough to refuse it would be
// refused by the storage charge whether or not columns were charged at all, and
// a test built on one stays green with the column charge deleted. Here there
// are no storages, so a refusal can only have come from the columns.
func emptyColumnWorld(t *testing.T, n int) []byte {
	t.Helper()
	w := &writer{}
	hostileMetaPrefix(w)
	hostileEmptyPalettes(w)
	w.uvarint(0)         // blob table
	w.uvarint(uint64(n)) // chunk records
	for i := range n {
		if i == 0 {
			w.svarint(0) // dx: the first record is at x = 0
		} else {
			w.svarint(1) // and each later one a step east, so Morton keys ascend
		}
		w.svarint(0) // dz
		w.svarint(0) // minSection
		w.uvarint(1) // one section...
		w.u8(0)      // ...marked absent, so the record carries no storage
		w.u8(0)      // and no biome section either
		w.uvarint(0) // block entities
		w.uvarint(0) // entities
		w.svarint(0) // column tick
		w.uvarint(0) // scheduled ticks
		w.blob(nil)  // user data
	}
	return hostileSeal(KindWorld, w.bytes(), true)
}

// TestDecodeBudgetChargesColumnsWithNoStorages isolates the column charge from
// the storage charge, which no fixture carrying blocks can do.
func TestDecodeBudgetChargesColumnsWithNoStorages(t *testing.T) {
	reg := testRegistry(t)
	const n = 64
	file := emptyColumnWorld(t, n)
	d, err := ReadWorld(file, reg)
	if err != nil {
		t.Fatalf("the fixture is not a legal file, so it proves nothing: %v", err)
	}
	if len(d.Columns) != n {
		t.Fatalf("the fixture decoded into %d columns, want %d", len(d.Columns), n)
	}
	t.Logf("%d bytes decoding into %d columns and no section storages", len(file), n)

	// Wide enough for every storage the file has (none) and one column short of
	// its columns. Only a charge that counts columns can refuse this.
	if _, err := ReadWorld(file, reg, MaxDecodedBytes(n*columnBytes-1)); err == nil {
		t.Fatalf("a ceiling of %d bytes decoded %d columns: columns are not being charged",
			n*columnBytes-1, n)
	} else if !errors.Is(err, ErrDecodeBudget) {
		t.Fatalf("refused, but not as a budget refusal: %v", err)
	} else if errors.Is(err, ErrCorrupt) {
		t.Fatalf("a policy refusal claimed the file is corrupt: %v", err)
	}
	// Exactly its columns is enough, so the charge is one column per record and
	// not some larger number that would refuse conforming files early.
	if _, err := ReadWorld(file, reg, MaxDecodedBytes(n*columnBytes)); err != nil {
		t.Fatalf("a ceiling of exactly %d columns refused them: %v", n, err)
	}
}

// TestDecodeBudgetRefusesByPolicyNotValidity: a tight ceiling refuses a file
// that is entirely conforming, and says so as policy.
func TestDecodeBudgetRefusesByPolicyNotValidity(t *testing.T) {
	reg := testRegistry(t)
	file, err := os.ReadFile(filepath.Join(goldenDir, "golden_world_plain.pile"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadWorld(file, reg); err != nil {
		t.Fatalf("the fixture is not a conforming file: %v", err)
	}
	_, err = ReadWorld(file, reg, MaxDecodedBytes(1))
	if err == nil {
		t.Fatal("a one-byte ceiling decoded a two-column world")
	}
	if !errors.Is(err, ErrDecodeBudget) {
		t.Fatalf("refused, but not as a budget refusal: %v", err)
	}
	// The distinction the sentinel exists for. A caller that quarantines or
	// deletes files on ErrCorrupt must not do either to this one.
	if errors.Is(err, ErrCorrupt) {
		t.Fatalf("a policy refusal claimed the file is corrupt: %v", err)
	}
	// And the same file at a ceiling that fits still decodes, so what was
	// tested above is the ceiling and not the file.
	if _, err := ReadWorld(file, reg, MaxDecodedBytes(decodedBytesCeiling)); err != nil {
		t.Fatalf("a conforming file was refused at the widest ceiling: %v", err)
	}
}

// TestDecodeBudgetReachesEveryReader: every entry point that decodes honours
// the ceiling. ContentHash is here because it decodes internally, and would
// otherwise be the one way to make a reader do the work with no ceiling over
// it.
func TestDecodeBudgetReachesEveryReader(t *testing.T) {
	reg := testRegistry(t)
	worldFile, err := os.ReadFile(filepath.Join(goldenDir, "golden_world_plain.pile"))
	if err != nil {
		t.Fatal(err)
	}
	structFile, err := os.ReadFile(filepath.Join(goldenDir, "golden_structure.pile"))
	if err != nil {
		t.Fatal(err)
	}
	indexedFile, err := os.ReadFile(filepath.Join(goldenDir, "golden_indexed.pile"))
	if err != nil {
		t.Fatal(err)
	}
	indexedPath := filepath.Join(t.TempDir(), "indexed.pile")
	if err := os.WriteFile(indexedPath, indexedFile, 0o644); err != nil {
		t.Fatal(err)
	}

	// Each entry: a call that must succeed at the default, and the same call at
	// a ceiling of one byte, which must fail with ErrDecodeBudget.
	for _, c := range []struct {
		name string
		call func(opts ...ReadOption) error
	}{
		{"ReadWorld", func(o ...ReadOption) error { _, err := ReadWorld(worldFile, reg, o...); return err }},
		{"ReadStructure", func(o ...ReadOption) error { _, err := ReadStructure(structFile, reg, o...); return err }},
		{"ContentHash/world", func(o ...ReadOption) error { _, err := ContentHash(worldFile, reg, o...); return err }},
		{"ContentHash/structure", func(o ...ReadOption) error { _, err := ContentHash(structFile, reg, o...); return err }},
		{"OpenIndexed", func(o ...ReadOption) error {
			w, err := OpenIndexed(indexedPath, reg, true, o...)
			if err != nil {
				return err
			}
			defer func() { _ = w.Close() }()
			for _, k := range w.Positions() {
				if _, err := w.Column(k[0], k[1]); err != nil {
					return err
				}
			}
			return nil
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := c.call(); err != nil {
				t.Fatalf("the fixture does not decode at the default ceiling: %v", err)
			}
			err := c.call(MaxDecodedBytes(1))
			if err == nil {
				t.Fatal("a one-byte ceiling decoded the whole fixture: this reader ignores the option")
			}
			if !errors.Is(err, ErrDecodeBudget) {
				t.Fatalf("refused, but not as a budget refusal: %v", err)
			}
			if errors.Is(err, ErrCorrupt) {
				t.Fatalf("a policy refusal claimed the file is corrupt: %v", err)
			}
		})
	}

	// ReadMeta takes the option for uniformity and documents that it cannot
	// bind, because it decodes no columns and no storages. Pinned so the
	// documentation and the behaviour cannot drift apart silently.
	if _, err := ReadMeta(worldFile, MaxDecodedBytes(1)); err != nil {
		t.Fatalf("ReadMeta refused under a ceiling it decodes nothing against: %v", err)
	}
}

// TestDecodeBudgetClampsUpwardOnly: a caller may tighten the ceiling and may
// not loosen it.
//
// The clamp is asserted on the resolver rather than through a decode, and
// deliberately so: §8's own count ceilings are enforced independently and bind
// first, so no file exists that a raised byte ceiling could let through. That
// is the design working, not the test dodging. What the resolver decides is the
// whole of the contract, so that is what is pinned, alongside the behavioural
// half — that a file §8 refuses is still refused for §8's reason, and not
// admitted, when the caller asks for an enormous ceiling.
func TestDecodeBudgetClampsUpwardOnly(t *testing.T) {
	for _, n := range []int64{math.MaxInt64, decodedBytesCeiling + 1, 1 << 62} {
		if got := (readConfig{maxDecodedBytes: n}).decodedByteCeiling(); got != decodedBytesCeiling {
			t.Fatalf("a caller asking for %d got a ceiling of %d, above §8's %d", n, got, decodedBytesCeiling)
		}
	}
	// Below the ceiling, the caller's number is taken as given.
	for _, n := range []int64{1, 1 << 20, decodedBytesCeiling - 1} {
		if got := (readConfig{maxDecodedBytes: n}).decodedByteCeiling(); got != n {
			t.Fatalf("a caller asking for %d got %d", n, got)
		}
	}
	// Not a number: the default policy budget, which is deliberately *not*
	// §8's ceiling. The two were the same until maxChunks moved to 2^26 for
	// big-world support; letting the default follow it would have made every
	// unconfigured reader sixteen times cheaper to attack in exchange for a
	// capability nobody asked that reader for.
	for _, n := range []int64{0, -1, math.MinInt64} {
		if got := (readConfig{maxDecodedBytes: n}).decodedByteCeiling(); got != defaultDecodedBytes {
			t.Fatalf("a ceiling of %d resolved to %d, not to the default %d", n, got, defaultDecodedBytes)
		}
	}
	// And the default must be reachable from above: a caller holding a world
	// larger than the default admits has to be able to say so, or the ceiling
	// is a world-size limit wearing a budget's name.
	if defaultDecodedBytes >= decodedBytesCeiling {
		t.Fatalf("the default budget %d is not below §8's ceiling %d, so the option cannot be raised",
			defaultDecodedBytes, decodedBytesCeiling)
	}
	if got := (readConfig{maxDecodedBytes: defaultDecodedBytes * 4}).decodedByteCeiling(); got != defaultDecodedBytes*4 {
		t.Fatalf("a caller raising the budget to %d got %d", defaultDecodedBytes*4, got)
	}

	// The behavioural half: a body naming one column more than §8 allows is
	// refused for §8's rule however wide the caller's ceiling is.
	reg := testRegistry(t)
	w := &writer{}
	hostileMetaPrefix(w)
	hostileEmptyPalettes(w)
	w.uvarint(0) // blob table
	w.uvarint(maxChunks + 1)
	w.raw(make([]byte, 64))
	over := hostileSeal(KindWorld, w.bytes(), true)
	_, err := ReadWorld(over, reg, MaxDecodedBytes(math.MaxInt64))
	if err == nil {
		t.Fatalf("a body declaring %d columns was accepted under a raised ceiling", maxChunks+1)
	}
	if !strings.Contains(err.Error(), "exceeds limit") {
		t.Fatalf("refused, but not for §8's column ceiling: %v", err)
	}
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("a §8 refusal stopped being a validity error: %v", err)
	}
}

// TestIndexedDecodeBudgetIsPerHandle: the ceiling covers the directory the
// handle retains plus one record on top of it, not each call independently.
//
// Per call would bound nothing. An indexed directory reaches four million
// entries, every one of them a column the handle holds, and a per-call ceiling
// would let a hostile file spend the caller's whole budget once per column
// while the handle's own footprint grew without limit.
func TestIndexedDecodeBudgetIsPerHandle(t *testing.T) {
	reg := testRegistry(t)
	path := filepath.Join(t.TempDir(), "indexed.pile")
	w, err := CreateIndexed(path, reg, Options{Compression: CompressionNone})
	if err != nil {
		t.Fatal(err)
	}
	const entries = 16
	for i := range int32(entries) {
		if err := w.Store(buildTestColumn(t, reg, i, 0)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	dirCost := int64(entries) * columnBytes

	open := func(ceiling int64) (*IndexedWorld, error) {
		return OpenIndexed(path, reg, true, MaxDecodedBytes(ceiling))
	}

	// One byte short of the directory: refused at open, before a single column
	// is read. That is the only point at which a caller can still decline the
	// file rather than discover the cost after paying it.
	if iw, err := open(dirCost - 1); err == nil {
		_ = iw.Close()
		t.Fatalf("a ceiling of %d opened a directory of %d entries costing %d", dirCost-1, entries, dirCost)
	} else if !errors.Is(err, ErrDecodeBudget) {
		t.Fatalf("refused at open, but not as a budget refusal: %v", err)
	} else if errors.Is(err, ErrCorrupt) {
		t.Fatalf("a policy refusal at open claimed the file is corrupt: %v", err)
	}

	// Exactly the directory and nothing more: the open succeeds, because the
	// directory is what the handle holds, and every column read fails, because
	// there is nothing left to decode one with. This is the case that separates
	// per handle from per call: a per-call ceiling of dirCost would decode any
	// of these columns without noticing the directory at all.
	iw, err := open(dirCost)
	if err != nil {
		t.Fatalf("a ceiling of exactly the directory refused to open it: %v", err)
	}
	if _, err := iw.Column(0, 0); err == nil {
		t.Fatal("a column decoded with the whole ceiling already spent on the directory")
	} else if !errors.Is(err, ErrDecodeBudget) {
		t.Fatalf("the column was refused, but not as a budget refusal: %v", err)
	}
	if err := iw.Close(); err != nil {
		t.Fatal(err)
	}

	// Room for the directory and a record: every column reads, and reads again.
	// A budget that was spent rather than reset per record would fail the
	// second pass, which is the other half of "the directory plus any single
	// record".
	iw, err = open(dirCost + columnBytes + 64*storageBytes)
	if err != nil {
		t.Fatal(err)
	}
	for pass := range 2 {
		for _, k := range iw.Positions() {
			if _, err := iw.Column(k[0], k[1]); err != nil {
				t.Fatalf("pass %d: column (%d,%d): %v", pass, k[0], k[1], err)
			}
		}
	}
	if err := iw.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestIndexedDecodeBudgetChargesTheRecordColumn is the indexed half of
// TestDecodeBudgetChargesColumnsWithNoStorages, and it exists for the same
// reason: a record holding blocks would be refused by the storage charge alone,
// so only a record with none proves that a decoded column is charged for.
func TestIndexedDecodeBudgetChargesTheRecordColumn(t *testing.T) {
	reg := testRegistry(t)
	path := filepath.Join(t.TempDir(), "empty.pile")
	w, err := CreateIndexed(path, reg, Options{Compression: CompressionNone})
	if err != nil {
		t.Fatal(err)
	}
	// An untouched chunk is air everywhere, so every section is absent and the
	// record decodes into no storages at all.
	if err := w.Store(Column{X: 0, Z: 0, Col: &chunk.Column{Chunk: chunk.New(reg, overworldRange)}}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// One directory entry, and a record whose only cost is its column.
	const dirCost = 1 * columnBytes
	iw, err := OpenIndexed(path, reg, true, MaxDecodedBytes(dirCost+columnBytes-1))
	if err != nil {
		t.Fatalf("a ceiling of the directory plus almost a column would not open: %v", err)
	}
	if _, err := iw.Column(0, 0); err == nil {
		t.Fatal("a record with no storages decoded one byte under a column's worth of ceiling: the record's column is not charged")
	} else if !errors.Is(err, ErrDecodeBudget) {
		t.Fatalf("refused, but not as a budget refusal: %v", err)
	} else if errors.Is(err, ErrCorrupt) {
		t.Fatalf("a policy refusal claimed the record is corrupt: %v", err)
	}
	if err := iw.Close(); err != nil {
		t.Fatal(err)
	}

	// One byte more is enough, so the charge is one column and not more.
	iw, err = OpenIndexed(path, reg, true, MaxDecodedBytes(dirCost+columnBytes))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := iw.Column(0, 0); err != nil {
		t.Fatalf("a ceiling of exactly the directory and one column refused the record: %v", err)
	}
	if err := iw.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestIndexedDecodeBudgetDoesNotFallBack: a checkpoint refused for the caller's
// ceiling must not send recovery looking for an older one.
//
// Recovery exists to step back past a checkpoint that is damaged. A checkpoint
// that is merely larger than the caller allowed is not damaged, and silently
// adopting an older one would answer a resource limit with data loss and report
// it as a successful open.
func TestIndexedDecodeBudgetDoesNotFallBack(t *testing.T) {
	reg := testRegistry(t)
	path := filepath.Join(t.TempDir(), "indexed.pile")
	w, err := CreateIndexed(path, reg, Options{Compression: CompressionNone})
	if err != nil {
		t.Fatal(err)
	}
	// A small first checkpoint, then a much larger second one, so an older
	// candidate exists and is cheap enough to fit a ceiling the newest cannot.
	if err := w.Store(buildTestColumn(t, reg, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if err := w.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	for i := int32(1); i < 32; i++ {
		if err := w.Store(buildTestColumn(t, reg, i, 0)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// A ceiling that fits the first checkpoint's one entry and not the last
	// one's thirty-two.
	iw, err := OpenIndexed(path, reg, true, MaxDecodedBytes(4*columnBytes))
	if err == nil {
		n := len(iw.Positions())
		_ = iw.Close()
		t.Fatalf("the open succeeded with %d columns: it fell back to an older checkpoint rather than reporting the ceiling", n)
	}
	if !errors.Is(err, ErrDecodeBudget) {
		t.Fatalf("refused, but not as a budget refusal: %v", err)
	}
	// And the same file opens fully when the ceiling allows it, so what was
	// refused was the ceiling and not the file.
	iw, err = OpenIndexed(path, reg, true)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(iw.Positions()); n != 32 {
		t.Fatalf("the file holds %d columns, want 32", n)
	}
	if err := iw.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestDecodeBudgetSentinelIsNotCorrupt pins the one documented exception to
// §8's "decode errors wrap ErrCorrupt": this sentinel must not, and must not
// start to.
func TestDecodeBudgetSentinelIsNotCorrupt(t *testing.T) {
	if errors.Is(ErrDecodeBudget, ErrCorrupt) {
		t.Fatal("ErrDecodeBudget wraps ErrCorrupt: a policy refusal now claims the file is invalid")
	}
	b := &storageBudget{limit: maxDecodedStorages, byteLimit: 1}
	err := b.chargeColumns(1)
	if err == nil {
		t.Fatal("a one-byte budget accepted a whole column")
	}
	if !errors.Is(err, ErrDecodeBudget) || errors.Is(err, ErrCorrupt) {
		t.Fatalf("the budget's own error is wrong: %v", err)
	}
	// §8's storage ceiling in the same type is a validity rule and still says
	// so, so the two halves of storageBudget report differently on purpose.
	c := &storageBudget{limit: 1, byteLimit: decodedBytesCeiling}
	err = c.charge(2)
	if err == nil {
		t.Fatal("the §8 storage ceiling stopped rejecting")
	}
	if !errors.Is(err, ErrCorrupt) || errors.Is(err, ErrDecodeBudget) {
		t.Fatalf("§8's storage ceiling is no longer a validity error: %v", err)
	}
}

// TestIndexedColumnDecodesOutsideTheLockSafely drives Column against a
// concurrent Store, which nothing else in the suite did.
//
// Column releases w.mu before decoding, deliberately, so a long decode does not
// block a writer. Everything the decode needs is therefore snapshotted while
// the lock is held — the palettes, and (since MaxDecodedBytes) the record
// budget, which is derived from the live directory that Store grows. Reading
// any of them off the struct after the unlock is a data race, and the race
// detector is the only thing that will say so: the values involved are a map
// length and some slice headers, so a torn read produces a wrong answer or a
// panic rather than a test failure.
//
// Run with -race for this to mean anything, which `go test -race ./...` does.
func TestIndexedColumnDecodesOutsideTheLockSafely(t *testing.T) {
	reg := testRegistry(t)
	path := filepath.Join(t.TempDir(), "race.pile")
	w, err := CreateIndexed(path, reg, Options{Compression: CompressionNone})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	const seeded = 8
	for i := range int32(seeded) {
		if err := w.Store(buildTestColumn(t, reg, i, 0)); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	// Writers, growing the directory the readers' budget is derived from.
	wg.Go(func() {
		for i := int32(seeded); i < seeded+64; i++ {
			if err := w.Store(buildTestColumn(t, reg, i, 1)); err != nil {
				t.Errorf("store (%d,1): %v", i, err)
				return
			}
		}
	})
	// Readers, decoding outside the lock while it happens.
	for range 4 {
		wg.Go(func() {
			for range 64 {
				for i := range int32(seeded) {
					if _, err := w.Column(i, 0); err != nil {
						t.Errorf("column (%d,0): %v", i, err)
						return
					}
				}
			}
		})
	}
	wg.Wait()
}
