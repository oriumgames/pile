package main

import (
	"errors"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
	"github.com/oriumgames/pile"
	"github.com/oriumgames/pile/format"
)

// Hostile files and hostile arguments driven through the subcommands.
//
// The CLI is the surface a person points at a file somebody sent them, before
// any server is involved, so its arguments are as much an input as the file is.
// Everything here must fail with an error, refuse, or produce a bounded result;
// nothing may panic, hang, or reserve memory the input does not justify.

func hostileReg(t testing.TB) world.BlockRegistry {
	t.Helper()
	reg := world.DefaultBlockRegistry
	reg.Finalize()
	return reg
}

// writeWorldAt writes a solid world file holding the given columns.
func writeWorldAt(t testing.TB, path string, cols []format.Column) {
	t.Helper()
	reg := hostileReg(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	d := &format.WorldData{Columns: cols}
	if err := format.WriteWorld(f, d, reg, format.Options{Compression: format.CompressionBest}); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// solidColumn builds a column with one non-air block, at a chosen position.
func solidColumn(t testing.TB, x, z int32) format.Column {
	t.Helper()
	reg := hostileReg(t)
	ch := chunk.New(reg, cube.Range{0, 15})
	ch.SetBlock(0, 0, 0, 0, reg.BlockRuntimeID(nil)+1)
	return format.Column{X: x, Z: z, Col: &chunk.Column{Chunk: ch}}
}

// TestRenderRefusesAWorldWhoseSpanWrapsInt32.
//
// The image size came from int32(maxCX-minCX)+1, and the difference of two
// chunk coordinates does not fit an int32: a world holding one column at
// X=-2147483648 and one at X=0 produced a width of -34359738352, which is not
// above 8192 and so passed the "too large to render" test. image.Rect
// canonicalises a negative rectangle into a positive one, so image.NewRGBA then
// reserved four bytes per pixel of a 34,359,738,352 x 16 image: measured at
// 2,199,023,190,016 bytes of heap from a 4,269-byte file holding two chunks. On
// a machine that cannot reserve two terabytes of address space it is a fatal
// out-of-memory instead.
func TestRenderRefusesAWorldWhoseSpanWrapsInt32(t *testing.T) {
	for _, c := range []struct {
		name   string
		lo, hi int32
	}{
		{"full int32 span", math.MinInt32, math.MaxInt32},
		{"half int32 span", math.MinInt32, 0},
		// Not a wrap: this span really is too large, and was refused before
		// and after. It is here so the wrapping cases are not the only ones.
		{"genuinely too large", math.MinInt32, -1},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			writeWorldAt(t, filepath.Join(dir, "overworld.pile"),
				[]format.Column{solidColumn(t, c.lo, 0), solidColumn(t, c.hi, 0)})
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			err := cmdRender([]string{"-o", filepath.Join(dir, "map.png"), dir})
			runtime.ReadMemStats(&after)
			if err == nil {
				t.Fatal("a world spanning the whole int32 chunk range was rendered")
			}
			if !strings.Contains(err.Error(), "too large to render") {
				t.Fatalf("refused for the wrong reason: %v", err)
			}
			if grew := int64(after.HeapSys) - int64(before.HeapSys); grew > 1<<30 {
				t.Fatalf("the refusal still reserved %d bytes of heap", grew)
			}
		})
	}
}

// TestRenderStillRendersAnOrdinaryWorld is the other half: the bound must not
// have been tightened onto worlds people actually have.
func TestRenderStillRendersAnOrdinaryWorld(t *testing.T) {
	dir := t.TempDir()
	var cols []format.Column
	for x := range int32(4) {
		cols = append(cols, solidColumn(t, x, 0))
	}
	writeWorldAt(t, filepath.Join(dir, "overworld.pile"), cols)
	out := filepath.Join(dir, "map.png")
	if err := cmdRender([]string{"-o", out, dir}); err != nil {
		t.Fatalf("an ordinary four-chunk world was refused: %v", err)
	}
	fi, err := os.Stat(out)
	if err != nil || fi.Size() == 0 {
		t.Fatalf("no image was written: %v", err)
	}
	// A world exactly at the 512-chunk ceiling renders; one past it does not.
	edge := t.TempDir()
	writeWorldAt(t, filepath.Join(edge, "overworld.pile"),
		[]format.Column{solidColumn(t, 0, 0), solidColumn(t, 511, 0)})
	if err := cmdRender([]string{"-o", filepath.Join(edge, "m.png"), edge}); err != nil {
		t.Fatalf("a 512-chunk-wide world was refused: %v", err)
	}
	over := t.TempDir()
	writeWorldAt(t, filepath.Join(over, "overworld.pile"),
		[]format.Column{solidColumn(t, 0, 0), solidColumn(t, 512, 0)})
	if err := cmdRender([]string{"-o", filepath.Join(over, "m.png"), over}); err == nil {
		t.Fatal("a 513-chunk-wide world was rendered")
	}
}

// TestExtractRefusesAnUnrepresentableBox: `pile extract --max 4294967296,0,0`
// used to never return. The span narrowed to int32 before it became a structure
// size, so the size was 1 and the chunk loop underneath walked 268,435,457
// positions.
func TestExtractRefusesAnUnrepresentableBox(t *testing.T) {
	dir := t.TempDir()
	writeWorldAt(t, filepath.Join(dir, "overworld.pile"), []format.Column{solidColumn(t, 0, 0)})
	for _, args := range [][]string{
		{"--min", "0,0,0", "--max", "4294967296,0,0"},
		{"--min", "0,0,0", "--max", "0,0,4294967296"},
		{"--min", "0,0,0", "--max", "-1,0,0"},
		{"--min", "-9223372036854775808,0,0", "--max", "9223372036854775807,0,0"},
		{"--min", "0,0,0", "--max", "9223372036854775807,9223372036854775807,9223372036854775807"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			done := make(chan error, 1)
			go func() {
				done <- cmdExtract(append(append([]string{}, args...), dir, filepath.Join(t.TempDir(), "s.pile")))
			}()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("an unrepresentable box was accepted")
				}
			case <-time.After(30 * time.Second):
				t.Fatal("cmdExtract did not return")
			}
		})
	}
}

// TestPruneHandlesExtremeBounds: --bounds is four integers from the command
// line, and every one of them may be at its limit, inverted, malformed or
// missing. What survives is asserted rather than merely that nothing failed,
// because "no error" is true of a prune that kept the wrong chunks.
func TestPruneHandlesExtremeBounds(t *testing.T) {
	const maxI = "9223372036854775807"
	const minI = "-9223372036854775808"
	for _, c := range []struct {
		bounds  string
		wantErr bool
		// keep is the set of chunk X coordinates that must survive, of the two
		// the fixture holds: (0,0) and (1000,1000).
		keep []int32
	}{
		{bounds: minI + "," + minI + "," + maxI + "," + maxI, keep: []int32{0, 1000}},
		{bounds: "0,0," + maxI + "," + maxI, keep: []int32{0, 1000}},
		// Inverted: the box is normalised, so it is the same box as above.
		{bounds: maxI + "," + maxI + ",0,0", keep: []int32{0, 1000}},
		{bounds: "0,0,15,15", keep: []int32{0}},
		{bounds: "16000,16000,16015,16015", keep: []int32{1000}},
		{bounds: minI + "," + minI + ",-1,-1", keep: nil},
		{bounds: "1,2,3,4,5,6,7", keep: []int32{0}}, // extra fields are ignored
		{bounds: maxI + "0,0,0,0", wantErr: true},   // does not fit an int
		{bounds: "1,2,3", wantErr: true},            // too few
		{bounds: "not,a,box,at all", wantErr: true}, // not numbers
	} {
		t.Run(c.bounds, func(t *testing.T) {
			dir := t.TempDir()
			writeWorldAt(t, filepath.Join(dir, "overworld.pile"),
				[]format.Column{solidColumn(t, 0, 0), solidColumn(t, 1000, 1000)})
			err := cmdPrune([]string{"--no-backup", "--bounds", c.bounds, dir})
			if c.wantErr {
				if err == nil {
					t.Fatalf("bounds %q were accepted", c.bounds)
				}
				return
			}
			if err != nil {
				t.Fatalf("bounds %q: %v", c.bounds, err)
			}
			reg := hostileReg(t)
			raw, err := os.ReadFile(filepath.Join(dir, "overworld.pile"))
			if err != nil {
				t.Fatal(err)
			}
			d, err := format.ReadWorld(raw, reg)
			if err != nil {
				t.Fatalf("the pruned world does not read back: %v", err)
			}
			got := map[int32]bool{}
			for _, col := range d.Columns {
				got[col.X] = true
			}
			if len(got) != len(c.keep) {
				t.Fatalf("bounds %q kept %d chunks, want %d", c.bounds, len(got), len(c.keep))
			}
			for _, x := range c.keep {
				if !got[x] {
					t.Fatalf("bounds %q dropped the chunk at X=%d", c.bounds, x)
				}
			}
		})
	}
}

// TestMoveHandlesExtremeOffsets: --by takes three integers with no range of
// their own, and the offset is applied to every position in the world.
//
// The vertical cases go through pile.MoveWorld rather than cmdMove because the
// clip refusal is an os.Exit(1) in the command, which a test in this process
// cannot survive. What is under test is the arithmetic, which is the same on
// both sides of that call.
func TestMoveHandlesExtremeOffsets(t *testing.T) {
	for _, by := range []string{
		"9223372036854775807,0,0",
		"-9223372036854775808,0,0",
		"2147483647,0,2147483647",
		"-9223372036854775808,0,-9223372036854775808",
		"9223372036854775808,0,0", // does not fit an int
	} {
		t.Run(by, func(t *testing.T) {
			dir := t.TempDir()
			writeWorldAt(t, filepath.Join(dir, "overworld.pile"), []format.Column{solidColumn(t, 0, 0)})
			before, err := os.ReadFile(filepath.Join(dir, "overworld.pile"))
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				_ = cmdMove([]string{"--by", by, "--dry-run", dir})
			}()
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Fatal("cmdMove did not return")
			}
			// A dry run writes nothing, whatever it decided.
			after, err := os.ReadFile(filepath.Join(dir, "overworld.pile"))
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Fatal("a dry run rewrote the world file")
			}
		})
	}

	// A horizontal offset that no longer fits the format's 32-bit position
	// fields must be refused rather than narrowed: a silent narrowing would put
	// columns at coordinates nobody asked for and collide two of them onto one
	// key, which is chunks quietly lost.
	for _, dx := range []int{1 << 31, 1 << 40, math.MaxInt64, math.MinInt64} {
		dir := t.TempDir()
		writeWorldAt(t, filepath.Join(dir, "overworld.pile"),
			[]format.Column{solidColumn(t, 0, 0), solidColumn(t, 1, 0)})
		before, err := os.ReadFile(filepath.Join(dir, "overworld.pile"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pile.MoveWorld(dir, pile.MoveOptions{Offset: cube.Pos{dx, 0, 0}}); err == nil {
			t.Fatalf("moving by dx=%d was accepted", dx)
		}
		after, err := os.ReadFile(filepath.Join(dir, "overworld.pile"))
		if err != nil {
			t.Fatal(err)
		}
		if string(before) != string(after) {
			t.Fatalf("a refused move by dx=%d still rewrote the world", dx)
		}
	}

	// A vertical offset no column can survive must be reported as a clip, with
	// counts, rather than wrapping into a plausible-looking small move.
	for _, dy := range []int{math.MaxInt64, math.MinInt64, 1 << 40} {
		dir := t.TempDir()
		writeWorldAt(t, filepath.Join(dir, "overworld.pile"), []format.Column{solidColumn(t, 0, 0)})
		rep, err := pile.MoveWorld(dir, pile.MoveOptions{Offset: cube.Pos{0, dy, 0}, DryRun: true})
		if !errors.Is(err, pile.ErrWouldClip) {
			t.Fatalf("moving by dy=%d was not refused as a clip: %v", dy, err)
		}
		if rep == nil || rep.ClippedTotal() == 0 {
			t.Fatalf("moving by dy=%d clipped nothing", dy)
		}
	}
}

// TestEveryCommandRefusesGarbage drives every subcommand at a file that is not
// a pile file, and at a directory that holds one. None may panic, and none may
// report success.
func TestEveryCommandRefusesGarbage(t *testing.T) {
	junk := []struct {
		name    string
		content []byte
	}{
		{"empty", nil},
		{"text", []byte("this is not a pile file")},
		{"magic only", []byte("PILE")},
		{"header only", append([]byte("PILE"), make([]byte, 8)...)},
		{"magic and noise", append([]byte("PILE\x02\x00\x00\x00"), []byte("\xff\xff\xff\xff\xff\xff\xff\xff")...)},
	}
	cmds := []struct {
		name string
		run  func(dir, file, out string) error
	}{
		{"inspect", func(dir, file, out string) error { return cmdInspect([]string{file}) }},
		{"verify", func(dir, file, out string) error { return cmdVerify([]string{file}) }},
		{"stats", func(dir, file, out string) error { return cmdStats([]string{file}) }},
		// cmdCheck exits the process when it finds unresolved states, but every
		// shape here fails before that, which is the point: a garbage file must
		// be an error and not an exit code about block states.
		{"check", func(dir, file, out string) error { return cmdCheck([]string{file}) }},
		{"compact", func(dir, file, out string) error { return cmdCompact([]string{file}) }},
		{"mode", func(dir, file, out string) error { return cmdMode([]string{file, "indexed"}) }},
		{"upgrade", func(dir, file, out string) error { return cmdUpgrade([]string{file}) }},
		{"render", func(dir, file, out string) error { return cmdRender([]string{"-o", out, dir}) }},
		{"diff", func(dir, file, out string) error { return cmdDiff([]string{dir, dir}) }},
		{"patch", func(dir, file, out string) error { return cmdPatch([]string{dir, dir, "-o", out}) }},
		{"apply", func(dir, file, out string) error { return cmdApply([]string{dir, file}) }},
		{"export", func(dir, file, out string) error { return cmdExport([]string{dir, out}) }},
		{"prune", func(dir, file, out string) error { return cmdPrune([]string{"--bounds", "0,0,1,1", dir}) }},
		{"move", func(dir, file, out string) error { return cmdMove([]string{"--by", "16,0,0", "--dry-run", dir}) }},
		{"origin", func(dir, file, out string) error { return cmdOrigin([]string{"--zero", file}) }},
		{"paste", func(dir, file, out string) error { return cmdPaste([]string{"--at", "0,0,0", file, dir}) }},
		{"extract", func(dir, file, out string) error {
			return cmdExtract([]string{"--min", "0,0,0", "--max", "1,1,1", dir, out})
		}},
		{"convert", func(dir, file, out string) error { return cmdConvert([]string{dir, out}) }},
	}
	for _, j := range junk {
		for _, c := range cmds {
			t.Run(j.name+"/"+c.name, func(t *testing.T) {
				dir := t.TempDir()
				file := filepath.Join(dir, "overworld.pile")
				if err := os.WriteFile(file, j.content, 0o644); err != nil {
					t.Fatal(err)
				}
				out := filepath.Join(t.TempDir(), "out")
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("panicked on %s: %v", j.name, r)
					}
				}()
				if err := c.run(dir, file, out); err == nil {
					t.Fatalf("%s reported success on a file that is %s", c.name, j.name)
				}
			})
		}
	}
}

// TestCommandsHonourTheDecodeCeiling: the README tells a reader to reach for
// pile.MaxDecodedBytes when opening worlds they did not write, and the CLI is
// the first thing they point at such a world. Every command that decodes chunk
// content has to have the same dial, and it has to actually reach the reader.
func TestCommandsHonourTheDecodeCeiling(t *testing.T) {
	dir := t.TempDir()
	var cols []format.Column
	for x := range int32(4096) {
		cols = append(cols, solidColumn(t, x, 0))
	}
	writeWorldAt(t, filepath.Join(dir, "overworld.pile"), cols)
	file := filepath.Join(dir, "overworld.pile")
	out := filepath.Join(t.TempDir(), "out")

	cmds := []struct {
		name string
		run  func(args ...string) error
	}{
		{"verify", func(a ...string) error { return cmdVerify(append(a, file)) }},
		{"stats", func(a ...string) error { return cmdStats(append(a, file)) }},
		{"upgrade", func(a ...string) error { return cmdUpgrade(append(a, file)) }},
		{"mode", func(a ...string) error { return cmdMode(append(a, file, "indexed")) }},
		{"render", func(a ...string) error { return cmdRender(append(a, "-o", out+".png", dir)) }},
		{"diff", func(a ...string) error { return cmdDiff(append(a, dir, dir)) }},
		{"prune", func(a ...string) error { return cmdPrune(append(a, "--dry-run", "--bounds", "0,0,1,1", dir)) }},
		{"move", func(a ...string) error { return cmdMove(append(a, "--by", "16,0,0", "--dry-run", dir)) }},
		{"export", func(a ...string) error { return cmdExport(append(a, dir, out)) }},
		{"extract", func(a ...string) error {
			return cmdExtract(append(a, "--min", "0,0,0", "--max", "1,1,1", dir, out+".pile"))
		}},
		{"convert", func(a ...string) error { return cmdConvert(append(a, dir, out+"-mcdb")) }},
	}
	for _, c := range cmds {
		t.Run(c.name, func(t *testing.T) {
			err := c.run("--max-decoded", "65536")
			if err == nil {
				t.Fatal("a 4096-column world passed a 64 KiB ceiling: --max-decoded is not reaching the reader")
			}
			if !errors.Is(err, format.ErrDecodeBudget) {
				t.Fatalf("refused, but not as a budget refusal: %v", err)
			}
			if errors.Is(err, format.ErrCorrupt) {
				t.Fatalf("a budget refusal must not claim the file is corrupt: %v", err)
			}
		})
	}
	// And with no ceiling the same world is fine, so the flag is a policy dial
	// and not a new validity rule.
	if err := cmdVerify([]string{file}); err != nil {
		t.Fatalf("the world does not verify without a ceiling: %v", err)
	}
}

// TestApplyRefusesAHostilePatch: a .pilepatch is a file somebody sends you, and
// its header fields are counts and lengths this tool reads before it has the
// bytes they promise.
func TestApplyRefusesAHostilePatch(t *testing.T) {
	dir := t.TempDir()
	writeWorldAt(t, filepath.Join(dir, "overworld.pile"), []format.Column{solidColumn(t, 0, 0)})
	patches := map[string][]byte{
		"empty":              {},
		"magic only":         []byte("PILP"),
		"bad version":        append([]byte("PILP"), 9, 0),
		"huge dim count":     append([]byte("PILP"), 2, 0, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01),
		"huge removed count": append([]byte("PILP"), 2, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01),
		"huge world length": append([]byte("PILP"), 2, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
			0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01),
	}
	for name, raw := range patches {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "x.pilepatch")
			if err := os.WriteFile(p, raw, 0o644); err != nil {
				t.Fatal(err)
			}
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panicked: %v", r)
				}
			}()
			err := cmdApply([]string{"--force", "--no-backup", dir, p})
			runtime.ReadMemStats(&after)
			if err == nil {
				t.Fatal("a malformed patch was applied")
			}
			if grew := int64(after.HeapSys) - int64(before.HeapSys); grew > 1<<28 {
				t.Fatalf("refusing a %d-byte patch reserved %d bytes", len(raw), grew)
			}
		})
	}
}

// TestOriginRefusesAnUnrepresentableAnchor: --set takes three integers and the
// anchor is three int32s on the wire. A value that does not fit used to be
// narrowed silently, so the tool reported an anchor it had not set.
func TestOriginRefusesAnUnrepresentableAnchor(t *testing.T) {
	dir := t.TempDir()
	writeWorldAt(t, filepath.Join(dir, "overworld.pile"), []format.Column{solidColumn(t, 0, 0)})
	sp := filepath.Join(dir, "s.pile")
	if err := cmdExtract([]string{"--min", "0,0,0", "--max", "3,3,3", dir, sp}); err != nil {
		t.Fatal(err)
	}
	if err := cmdOrigin([]string{"--set", "4294967296,0,0", sp}); err == nil {
		t.Fatal("an anchor that does not fit an int32 was accepted")
	}
	// The structure must be unchanged and still loadable.
	s, err := pile.LoadStructure(sp)
	if err != nil {
		t.Fatalf("the structure was damaged by the refused origin: %v", err)
	}
	if got := s.Data().Origin; got != [3]int32{} {
		t.Fatalf("the origin moved to %v", got)
	}
	// An anchor that does fit still works.
	if err := cmdOrigin([]string{"--set", "-2147483648,7,2147483647", sp}); err != nil {
		t.Fatalf("a representable anchor was refused: %v", err)
	}
}

// TestPileFilesRefusesADirectoryOfNonPileFiles: `pile verify some/dir` globs
// *.pile, and a directory holding none must say so rather than reporting
// success over an empty list.
func TestPileFilesRefusesADirectoryOfNonPileFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cmdVerify([]string{dir}); err == nil {
		t.Fatal("verifying a directory with no pile files reported success")
	}
	if err := cmdVerify([]string{filepath.Join(dir, "nope")}); err == nil {
		t.Fatal("verifying a path that does not exist reported success")
	}
}

// TestPasteRefusesAnUnaddressablePosition: --at is three integers, and the
// chunk keys the paste writes are int32. A position outside that range used to
// be narrowed silently: the structure landed at coordinates nobody asked for,
// and two of its columns could collide onto one key.
func TestPasteRefusesAnUnaddressablePosition(t *testing.T) {
	src := t.TempDir()
	writeWorldAt(t, filepath.Join(src, "overworld.pile"), []format.Column{solidColumn(t, 0, 0)})
	sp := filepath.Join(src, "s.pile")
	if err := cmdExtract([]string{"--min", "0,0,0", "--max", "31,3,31", src, sp}); err != nil {
		t.Fatal(err)
	}
	for _, at := range []string{
		"9223372036854775807,0,0",
		"-9223372036854775808,0,0",
		"0,0,34359738368",
		"68719476736,0,0",
	} {
		t.Run(at, func(t *testing.T) {
			dst := t.TempDir()
			if err := cmdPaste([]string{"--at", at, sp, dst}); err == nil {
				t.Fatal("an unaddressable paste position was accepted")
			}
			m, _ := filepath.Glob(filepath.Join(dst, "*.pile"))
			if len(m) != 0 {
				t.Fatalf("the refused paste still wrote %v", m)
			}
		})
	}
	// A position at the edge of the addressable range still works.
	dst := t.TempDir()
	if err := cmdPaste([]string{"--at", "34359738000,0,0", sp, dst}); err != nil {
		t.Fatalf("an addressable paste position was refused: %v", err)
	}
}

// emptyColumn is a stored column with nothing in it: what a converted world
// carries by the thousand where a map's spawn area was pre-generated and never
// built on.
func emptyColumn(t testing.TB, x, z int32) format.Column {
	t.Helper()
	return format.Column{X: x, Z: z, Col: &chunk.Column{Chunk: chunk.New(hostileReg(t), cube.Range{0, 15})}}
}

// TestRenderIgnoresEmptyColumnsWhenSizingTheImage.
//
// The bounding box was measured over every stored column while the render loop
// drew only non-air surfaces, so a column that contributes no pixel still set
// the image's width. Converted Bedrock worlds carry those in bulk: a Skywars
// map arrived with 5 041 empty chunks at the origin and the map itself at chunk
// X 5 405, which measured 88 192 pixels wide and was refused outright -- and
// had the ceiling been higher it would have written a 412 MB image that was
// blank apart from one corner.
func TestRenderIgnoresEmptyColumnsWhenSizingTheImage(t *testing.T) {
	dir := t.TempDir()
	cols := []format.Column{solidColumn(t, 5405, 0), solidColumn(t, 5406, 0)}
	// The empty pad, far enough away that including it breaks the ceiling.
	for x := int32(-35); x <= 35; x++ {
		cols = append(cols, emptyColumn(t, x, 0))
	}
	writeWorldAt(t, filepath.Join(dir, "overworld.pile"), cols)

	out := filepath.Join(dir, "map.png")
	if err := cmdRender([]string{"-o", out, dir}); err != nil {
		t.Fatalf("empty columns 87,000 blocks from the map made it unrenderable: %v", err)
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cfg, err := png.DecodeConfig(f)
	if err != nil {
		t.Fatal(err)
	}
	// Two columns of content and nothing else: the pad contributes no margin.
	if cfg.Width != 32 || cfg.Height != 16 {
		t.Errorf("image is %dx%d, want 32x16: the empty pad still sized it",
			cfg.Width, cfg.Height)
	}
}

// TestRenderReportsAWorldWithNothingToDraw: once the box is measured over
// drawing columns there may be none, and a blank PNG of no stated size is a
// worse answer than saying so.
func TestRenderReportsAWorldWithNothingToDraw(t *testing.T) {
	dir := t.TempDir()
	writeWorldAt(t, filepath.Join(dir, "overworld.pile"),
		[]format.Column{emptyColumn(t, 0, 0), emptyColumn(t, 1, 0)})
	err := cmdRender([]string{"-o", filepath.Join(dir, "map.png"), dir})
	if err == nil {
		t.Fatal("a world of nothing but air produced an image")
	}
	if !strings.Contains(err.Error(), "all 2 columns are air") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

// TestPruneEmptyKeepsEverythingThatIsNotAir.
//
// Bedrock writes a chunk record for every chunk that has ever entered
// simulation, so a converted minigame map arrives mostly air: one Skywars map
// held 10 225 columns of which 68 contained a block. They cost 700 bytes on
// disk, where they are identical and compress away, and 54 MB in memory once a
// server loads them, which is the reason --empty exists.
//
// Dropping a chunk is not the same as never storing one -- an absent chunk
// sends the server to its generator -- so this is opt-in, and everything that
// is not literally air has to survive it.
func TestPruneEmptyKeepsEverythingThatIsNotAir(t *testing.T) {
	reg := hostileReg(t)
	stone := reg.BlockRuntimeID(nil) + 1

	// An empty column carrying a block entity: the blocks are gone but the
	// entry is not recoverable from anything else in the file.
	withBE := emptyColumn(t, 2, 0)
	withBE.Col.BlockEntities = []chunk.BlockEntity{
		{Pos: cube.Pos{32, 1, 0}, Data: map[string]any{"id": "minecraft:chest"}},
	}
	// A waterlogged-only section: layer 0 is uniform air and layer 1 holds the
	// block. Two storages, so it is not empty -- the case the format spends a
	// rule on, and the one a naive "is the surface air" test would delete.
	waterlogged := emptyColumn(t, 3, 0)
	waterlogged.Col.Chunk.SetBlock(0, 1, 0, 1, stone)
	// An empty column carrying chunk user data.
	withUD := emptyColumn(t, 4, 0)
	withUD.UserData = []byte("arena-corner")

	dir := t.TempDir()
	writeWorldAt(t, filepath.Join(dir, "overworld.pile"), []format.Column{
		solidColumn(t, 0, 0),
		emptyColumn(t, 1, 0), // the only one that may go
		withBE, waterlogged, withUD,
	})

	if err := cmdPrune([]string{"--empty", "--no-backup", dir}); err != nil {
		t.Fatal(err)
	}
	wf, err := pile.LoadWorldFiles(dir, reg)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int32]bool{}
	for _, c := range wf.Dim(world.Overworld).Columns {
		got[c.X] = true
	}
	for _, x := range []int32{0, 2, 3, 4} {
		if !got[x] {
			t.Errorf("column %d was dropped and holds content", x)
		}
	}
	if got[1] {
		t.Error("the air-only column survived --empty")
	}
	if len(got) != 4 {
		t.Errorf("kept %d columns, want 4", len(got))
	}
}

// TestPruneRequiresAFilter: --bounds stopped being mandatory when --empty
// arrived, so the two must not have left a form that drops nothing silently.
func TestPruneRequiresAFilter(t *testing.T) {
	dir := t.TempDir()
	writeWorldAt(t, filepath.Join(dir, "overworld.pile"), []format.Column{solidColumn(t, 0, 0)})
	if err := cmdPrune([]string{"--no-backup", dir}); err == nil {
		t.Fatal("prune with neither --bounds nor --empty was accepted")
	}
}

// TestRenderBoundsPicksOneOfSeveralBuilds.
//
// A converted world is not always one map. A CubeCraft BedWars export turned
// out to hold five separate builds spread over 20 752 blocks of Z -- two of
// them quad maps, one a garden, none of them air -- so the bounding box was
// genuine and --empty had nothing to drop. Without a way to say which one you
// meant, the only answer the command could give was that the world was too
// large.
func TestRenderBoundsPicksOneOfSeveralBuilds(t *testing.T) {
	dir := t.TempDir()
	writeWorldAt(t, filepath.Join(dir, "overworld.pile"), []format.Column{
		solidColumn(t, 0, 0),
		solidColumn(t, 1, 0),
		solidColumn(t, 0, 1000), // a second build, 16 000 blocks away
	})

	// Whole world: refused, and the message has to name the way out.
	err := cmdRender([]string{"-o", filepath.Join(dir, "all.png"), dir})
	if err == nil {
		t.Fatal("a world spanning 16 000 blocks was rendered")
	}
	if !strings.Contains(err.Error(), "--bounds") {
		t.Fatalf("the refusal does not mention --bounds: %v", err)
	}

	out := filepath.Join(dir, "one.png")
	if err := cmdRender([]string{"--bounds", "-16,-16,64,64", "-o", out, dir}); err != nil {
		t.Fatalf("--bounds did not isolate the near build: %v", err)
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cfg, err := png.DecodeConfig(f)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Width != 32 || cfg.Height != 16 {
		t.Errorf("image is %dx%d, want 32x16", cfg.Width, cfg.Height)
	}

	// A box with nothing in it says so rather than writing an empty image.
	err = cmdRender([]string{"--bounds", "5000,5000,5100,5100", "-o", filepath.Join(dir, "n.png"), dir})
	if err == nil || !strings.Contains(err.Error(), "nothing to render") {
		t.Fatalf("an empty box gave %v", err)
	}
}

// TestPruneEmptyKeepsColumnsAtTheEdgeOfTheWorld.
//
// prune shares its box filter with render, and the box that means "everywhere"
// was written with int32 bounds while the columns are compared in block
// coordinates. A column at chunk X math.MinInt32 begins at block
// -34 359 738 368, which is below math.MinInt32, so the filter placed it
// outside the everywhere-box: prune --empty, given no --bounds at all, would
// have deleted it for being out of range.
func TestPruneEmptyKeepsColumnsAtTheEdgeOfTheWorld(t *testing.T) {
	dir := t.TempDir()
	writeWorldAt(t, filepath.Join(dir, "overworld.pile"), []format.Column{
		solidColumn(t, math.MinInt32, 0),
		solidColumn(t, math.MaxInt32, 0),
		emptyColumn(t, 0, 0),
	})
	if err := cmdPrune([]string{"--empty", "--no-backup", dir}); err != nil {
		t.Fatal(err)
	}
	wf, err := pile.LoadWorldFiles(dir, hostileReg(t))
	if err != nil {
		t.Fatal(err)
	}
	got := map[int32]bool{}
	for _, c := range wf.Dim(world.Overworld).Columns {
		got[c.X] = true
	}
	if !got[math.MinInt32] || !got[math.MaxInt32] {
		t.Errorf("a column at the edge of the world was pruned; kept %v", got)
	}
	if got[0] {
		t.Error("the air column survived")
	}
}

// TestRenderRefusalListsTheBuildsItFound.
//
// "pick one with --bounds" is half an answer if nothing says which boxes exist:
// the coordinates are in the file, and finding them otherwise means writing a
// program, which is what it took the first time a five-build CubeCraft export
// turned up here.
func TestRenderRefusalListsTheBuildsItFound(t *testing.T) {
	dir := t.TempDir()
	cols := []format.Column{
		solidColumn(t, 0, 0), solidColumn(t, 1, 0), solidColumn(t, 0, 1),
		solidColumn(t, 0, 1000), // a second build, far along Z
		solidColumn(t, 900, 0),  // a third, far along X
	}
	writeWorldAt(t, filepath.Join(dir, "overworld.pile"), cols)

	err := cmdRender([]string{"-o", filepath.Join(dir, "m.png"), dir})
	if err == nil {
		t.Fatal("a world spanning 16 000 blocks was rendered")
	}
	msg := err.Error()
	if !strings.Contains(msg, "3 separate places") {
		t.Errorf("the refusal did not count the builds:\n%s", msg)
	}
	// The largest is listed first, and every line must be a box that works.
	for _, want := range []string{"--bounds 0,0,31,31", "--bounds 0,16000,15,16015", "--bounds 14400,0,14415,15"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal is missing %q:\n%s", want, msg)
		}
	}
	// Feeding a listed box back in has to render, which is the whole promise.
	out := filepath.Join(dir, "one.png")
	if err := cmdRender([]string{"--bounds", "0,0,31,31", "-o", out, dir}); err != nil {
		t.Fatalf("a box the refusal suggested did not render: %v", err)
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cfg, _ := png.DecodeConfig(f)
	if cfg.Width != 32 || cfg.Height != 32 {
		t.Errorf("the suggested box rendered %dx%d, want 32x32", cfg.Width, cfg.Height)
	}
}
