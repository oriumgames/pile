package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/df-mc/dragonfly/server/world"
	"github.com/oriumgames/pile"
)

// TestHashIsIndependentOfModeAndCompression is the claim the command exists to
// make, and the one that could quietly be false.
//
// The readme says a file hash is a map version. That is only useful if the hash
// depends on the content and nothing else -- not on the compressor, the
// compression level, or whether the world is stored solid or indexed. Hashing
// the stored bytes would fail all three; ContentHash is defined as decode and
// re-encode canonically for exactly this reason, and this test is what says the
// command actually delivers it.
func TestHashIsIndependentOfModeAndCompression(t *testing.T) {
	reg := testRegistry(t)
	limit := decodeLimit{}

	solid := t.TempDir()
	buildWorldA(t, solid)
	base, err := hashTarget(solid, reg, limit)
	if err != nil {
		t.Fatal(err)
	}
	if len(base) == 0 {
		t.Fatal("no dimension was hashed")
	}

	// The same content written at a different compression level.
	fast := t.TempDir()
	copyWorldWith(t, solid, fast, reg, pile.Compression(pile.CompressionFast))
	if got, err := hashTarget(fast, reg, limit); err != nil {
		t.Fatal(err)
	} else if !sameHashes(base, got) {
		t.Errorf("compression changed the identity: %v vs %v", base, got)
	}

	// And in indexed mode, whose bytes are history-dependent by design.
	indexed := t.TempDir()
	copyWorldWith(t, solid, indexed, reg, pile.AppendMode())
	if got, err := hashTarget(indexed, reg, limit); err != nil {
		t.Fatal(err)
	} else if !sameHashes(base, got) {
		t.Errorf("the file mode changed the identity: %v vs %v", base, got)
	}

	// A different world must not collide, or the test above would pass on a
	// hash that ignores content entirely.
	other := t.TempDir()
	buildWorldB(t, other)
	if got, err := hashTarget(other, reg, limit); err != nil {
		t.Fatal(err)
	} else if sameHashes(base, got) {
		t.Error("two different worlds hashed the same")
	}
}

// copyWorldWith rewrites a world into dst through a provider opened with opts,
// so the content is identical and the stored bytes are not.
func copyWorldWith(t *testing.T, src, dst string, reg world.BlockRegistry, opts ...pile.Option) {
	t.Helper()
	in, err := pile.Open(src, pile.Registry(reg), pile.ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close() }()
	out, err := pile.Open(dst, append(opts, pile.Registry(reg))...)
	if err != nil {
		t.Fatal(err)
	}
	for _, dim := range dimensions {
		for pos, col := range in.Columns(dim) {
			if err := out.StoreColumn(pos, dim, col); err != nil {
				t.Fatal(err)
			}
		}
		if err := in.IterError(); err != nil {
			t.Fatal(err)
		}
	}
	out.SaveSettings(in.Settings())
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestHashRefusesALoneIndexedFile: an indexed file has no canonical bytes of
// its own, so hashing one on its own would answer a question the format does
// not define. It has to say so rather than return something.
func TestHashRefusesALoneIndexedFile(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	buildWorldA(t, dir)
	indexed := t.TempDir()
	copyWorldWith(t, dir, indexed, reg, pile.AppendMode())

	_, err := hashTarget(filepath.Join(indexed, "overworld.pile"), reg, decodeLimit{})
	if err == nil {
		t.Fatal("a lone indexed file was hashed")
	}
	if !strings.Contains(err.Error(), "world directory") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

// TestSnapshotRoundTrip drives the four snapshot commands as a user would: take
// one, list it, change the world, roll back, and find the change gone.
//
// The rollback half matters more than it looks. The provider's Rollback used to
// delete the dimension files, rename the snapshot's over them, and only then
// read the result -- so a snapshot that did not load left the world
// permanently unopenable. This walks the whole path.
func TestSnapshotRoundTrip(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	buildWorldA(t, dir)
	before, err := hashTarget(dir, reg, decodeLimit{})
	if err != nil {
		t.Fatal(err)
	}

	if err := cmdSnapshot([]string{dir, "clean"}); err != nil {
		t.Fatal(err)
	}
	p, err := pile.Open(dir, pile.Registry(reg), pile.ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	names, err := p.Snapshots()
	_ = p.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "clean" {
		t.Fatalf("snapshots = %v, want [clean]", names)
	}

	// Change the world.
	buildWorldB(t, dir)
	changed, err := hashTarget(dir, reg, decodeLimit{})
	if err != nil {
		t.Fatal(err)
	}
	if sameHashes(before, changed) {
		t.Fatal("the fixture did not change the world, so the rollback proves nothing")
	}

	if err := cmdRollback([]string{dir, "clean"}); err != nil {
		t.Fatal(err)
	}
	after, err := hashTarget(dir, reg, decodeLimit{})
	if err != nil {
		t.Fatal(err)
	}
	if !sameHashes(before, after) {
		t.Errorf("rollback did not restore the world: %v, want %v", after, before)
	}

	// The rollback kept the pre-rollback state, so a wrong snapshot is
	// recoverable from.
	p, err = pile.Open(dir, pile.Registry(reg), pile.ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	names, err = p.Snapshots()
	_ = p.Close()
	if err != nil {
		t.Fatal(err)
	}
	var kept bool
	for _, n := range names {
		if n == "pre-rollback" {
			kept = true
		}
	}
	if !kept {
		t.Errorf("rollback did not keep the previous state; snapshots = %v", names)
	}

	if err := cmdDeleteSnapshot([]string{dir, "clean"}); err != nil {
		t.Fatal(err)
	}
	if err := cmdSnapshots([]string{dir}); err != nil {
		t.Fatal(err)
	}
}

func TestVersionCommand(t *testing.T) {
	if err := cmdVersion(nil); err != nil {
		t.Fatal(err)
	}
}
