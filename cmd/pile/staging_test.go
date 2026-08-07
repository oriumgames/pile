package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCreateStagedRefusesAnExistingPath: the command-line tools stage under
// names that are trivially predictable from the file they are rewriting, in
// whatever directory that file happens to live in. os.Create would follow a
// symlink planted at one of them; this must not.
func TestCreateStagedRefusesAnExistingPath(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "world.pile.upgrade")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("this filesystem has no symlinks: %v", err)
	}
	f, err := createStaged(link)
	if err != nil {
		t.Fatalf("staging over a symlink failed for the wrong reason: %v", err)
	}
	_, _ = f.WriteString("staged")
	_ = f.Close()
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "original" {
		t.Fatalf("the symlink target was written through: %q", got)
	}
}

// TestPreserveModeKeepsTheDestinationBits: a tool that rewrites a world file
// replaces its inode, so without this the file it leaves behind carries the
// staging mode rather than the one the operator set.
func TestPreserveModeKeepsTheDestinationBits(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "world.pile")
	if err := os.WriteFile(dst, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(dir, "world.pile.upgrade")
	f, err := createStaged(tmp)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if err := preserveMode(tmp, dst); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("staged file has mode %v, want -rw-------", fi.Mode().Perm())
	}
	// A destination that does not exist leaves the staging mode alone, and one
	// that is not a regular file is not something to copy bits from.
	if err := preserveMode(tmp, filepath.Join(dir, "absent")); err != nil {
		t.Fatalf("a missing destination was an error: %v", err)
	}
}
