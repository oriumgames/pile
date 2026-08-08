package pile

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/df-mc/dragonfly/server/world"
	"github.com/oriumgames/pile/format"
)

// The filesystem behaviour audit, as tests.
//
// Four things have to be deliberate rather than accidental: path
// traversal, symlinks on atomic rename, permission bits, and temp-file naming.
// Each is pinned here, including the ones whose answer is "this is what it
// does and it is on purpose" — a documented decision with no test is a
// decision that changes the next time someone edits the line.
//
// Each case below states what it covers; what none of them cover is a second
// process on the same world directory, which is out of scope by contract.

// symlinkOrSkip creates a symlink, skipping on filesystems that have none.
func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("this filesystem has no symlinks: %v", err)
	}
}

// mustWorld builds a saved solid-mode world at dir with one column.
func mustWorld(t *testing.T, dir string) {
	t.Helper()
	reg := testRegistry(t)
	p, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, testColumn(t, reg, world.ChunkPos{0, 0})); err != nil {
		t.Fatal(err)
	}
	if err := p.Save(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestStagedNamesAreNeverWrittenThrough drives every path that stages a file
// under a predictable name, with that name pre-created as a symlink pointing
// at a file the process can write. None of them may write through it.
//
// The unit test on createExclusive proves the helper refuses; this proves the
// helper is what each path actually calls, which is the half that has gone
// missing before.
func TestStagedNamesAreNeverWrittenThrough(t *testing.T) {
	reg := testRegistry(t)
	for _, tc := range []struct {
		name string
		// stagedName is the predictable name, relative to the world directory.
		stagedName string
		run        func(t *testing.T, dir string)
	}{
		{
			name:       "Provider.Save",
			stagedName: "overworld.pile.tmp",
			run: func(t *testing.T, dir string) {
				p, err := Open(dir)
				if err != nil {
					t.Fatal(err)
				}
				if err := p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, testColumn(t, reg, world.ChunkPos{0, 0})); err != nil {
					t.Fatal(err)
				}
				if err := p.Save(); err != nil {
					t.Fatal(err)
				}
				_ = p.Close()
			},
		},
		{
			name:       "WorldFiles.Write",
			stagedName: "overworld.pile.tmp",
			run: func(t *testing.T, dir string) {
				mustWorld(t, dir)
				wf, err := LoadWorldFiles(dir, reg)
				if err != nil {
					t.Fatal(err)
				}
				if err := wf.Write(dir, reg); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:       "Structure.Save",
			stagedName: "s.pilestruct.tmp",
			run: func(t *testing.T, dir string) {
				s := testStructure(t)
				if err := s.Save(filepath.Join(dir, "s.pilestruct")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:       "Provider.Rollback",
			stagedName: "overworld.pile.rollback",
			run: func(t *testing.T, dir string) {
				p, err := Open(dir)
				if err != nil {
					t.Fatal(err)
				}
				defer p.Close()
				if err := p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, testColumn(t, reg, world.ChunkPos{0, 0})); err != nil {
					t.Fatal(err)
				}
				if err := p.Snapshot("s"); err != nil {
					t.Fatal(err)
				}
				if err := p.Rollback("s"); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:       "IndexedWorld.Compact",
			stagedName: "overworld.pile.compact",
			run: func(t *testing.T, dir string) {
				path := filepath.Join(dir, "overworld.pile")
				w, err := format.CreateIndexed(path, reg, format.Options{Compression: format.CompressionDefault})
				if err != nil {
					t.Fatal(err)
				}
				if err := w.Compact(); err != nil {
					t.Fatal(err)
				}
				_ = w.Close()
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			// The target lives outside the world directory, the way an
			// attacker's would: nothing this operation does should reach it.
			outside := filepath.Join(t.TempDir(), "victim")
			if err := os.WriteFile(outside, []byte("untouched"), 0o600); err != nil {
				t.Fatal(err)
			}
			symlinkOrSkip(t, outside, filepath.Join(dir, tc.stagedName))
			tc.run(t, dir)
			got, err := os.ReadFile(outside)
			if err != nil {
				t.Fatalf("the symlink target vanished: %v", err)
			}
			if string(got) != "untouched" {
				t.Fatalf("%s wrote through a symlink at %s: target now %d bytes", tc.name, tc.stagedName, len(got))
			}
		})
	}
}

// TestSaveReplacesASymlinkDestination: the destination of an atomic replace
// may itself be a symlink. rename(2) replaces the link, it does not follow it,
// so a save cannot be redirected by planting one at the world file's own name.
//
// The consequence is deliberate and worth stating: a dimension file that an
// operator symlinked onto another disk becomes a real file at its original
// location on the first solid-mode save. Append mode is the other way round
// (see TestAppendModeWritesThroughASymlinkedDimensionFile), because it opens
// the file in place instead of replacing it.
func TestSaveReplacesASymlinkDestination(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	// A dangling link, so following it would have to create the target: the
	// question is not only whether an existing file survives but whether the
	// save can be made to write outside the world directory at all.
	outside := filepath.Join(t.TempDir(), "victim")
	symlinkOrSkip(t, outside, filepath.Join(dir, "overworld.pile"))

	p, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, testColumn(t, reg, world.ChunkPos{0, 0})); err != nil {
		t.Fatal(err)
	}
	if err := p.Save(); err != nil {
		t.Fatal(err)
	}
	_ = p.Close()

	if _, err := os.Lstat(outside); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the save wrote through the destination symlink: %v", err)
	}
	fi, err := os.Lstat(filepath.Join(dir, "overworld.pile"))
	if err != nil {
		t.Fatal(err)
	}
	if !fi.Mode().IsRegular() {
		t.Fatalf("the destination is still %v, want a regular file", fi.Mode())
	}
}

// TestAppendModeWritesThroughASymlinkedDimensionFile pins the deliberate
// asymmetry. An indexed world is appended to in place — that is the whole
// point of the mode — so the path is opened, not replaced, and a symlink there
// is followed like any other. That is the caller's own file, named by the
// caller, and refusing symlinked world files would break the ordinary reason
// to have one (a dimension parked on another disk).
func TestAppendModeWritesThroughASymlinkedDimensionFile(t *testing.T) {
	reg := testRegistry(t)
	dir, store := t.TempDir(), t.TempDir()
	real := filepath.Join(store, "overworld.pile")
	w, err := format.CreateIndexed(real, reg, format.Options{Compression: format.CompressionDefault})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(real)
	if err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, real, filepath.Join(dir, "overworld.pile"))

	p, err := Open(dir, AppendMode())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.StoreColumn(world.ChunkPos{0, 0}, world.Overworld, testColumn(t, reg, world.ChunkPos{0, 0})); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(real)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() <= before.Size() {
		t.Fatalf("append mode did not write through the symlink: %d -> %d", before.Size(), after.Size())
	}
	if fi, err := os.Lstat(filepath.Join(dir, "overworld.pile")); err != nil {
		t.Fatal(err)
	} else if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("append mode replaced the symlink instead of following it")
	}
}

// TestSnapshotNamesCannotEscape: a snapshot name is the one caller-supplied
// string that becomes a path component, so it is the one place a traversal can
// start. Every operation that takes one must reject the same set.
func TestSnapshotNamesCannotEscape(t *testing.T) {
	bad := []string{
		"", ".", "..", "../evil", "a/b", `a\b`, "/abs", "./x", "..",
		strings.Repeat("../", 4) + "etc",
	}
	dir := t.TempDir()
	mustWorld(t, dir)
	p, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for _, name := range bad {
		for _, op := range []struct {
			what string
			run  func(string) error
		}{
			{"Snapshot", p.Snapshot},
			{"Rollback", p.Rollback},
			{"DeleteSnapshot", p.DeleteSnapshot},
		} {
			if err := op.run(name); err == nil {
				t.Errorf("%s(%q) was accepted", op.what, name)
			}
		}
	}
	// Nothing above may have created anything outside snapshots/.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "overworld.pile" && e.Name() != snapshotsDirName {
			t.Errorf("a rejected snapshot name left %q behind", e.Name())
		}
	}
}

// TestCreatedFilesAndDirectoriesHaveTheIntendedMode pins the permission bits.
// Files are staged 0644 and directories 0755, both less the process umask,
// which is the conventional answer for a server's data directory: readable by
// the operator's tooling, writable only by the owner.
func TestCreatedFilesAndDirectoriesHaveTheIntendedMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not meaningful here")
	}
	mask := os.FileMode(umask(t))
	dir := t.TempDir()
	world := filepath.Join(dir, "w")
	mustWorld(t, world)
	p, err := Open(world)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Snapshot("s"); err != nil {
		t.Fatal(err)
	}
	_ = p.Close()

	for _, tc := range []struct {
		path string
		want os.FileMode
	}{
		{filepath.Join(world, "overworld.pile"), 0o644 &^ mask},
		{filepath.Join(world, snapshotsDirName), 0o755 &^ mask},
		{filepath.Join(world, snapshotsDirName, "s"), 0o755 &^ mask},
		{filepath.Join(world, snapshotsDirName, "s", "overworld.pile"), 0o644 &^ mask},
	} {
		fi, err := os.Stat(tc.path)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != tc.want {
			t.Errorf("%s has mode %v, want %v", tc.path, got, tc.want)
		}
	}
}

// TestSavePreservesAnExistingFilesMode: a save replaces the destination inode,
// so without help the file that lands carries the staging mode rather than the
// one it replaced, and a world an operator had closed to 0600 quietly became
// world-readable on its first save.
func TestSavePreservesAnExistingFilesMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not meaningful here")
	}
	reg := testRegistry(t)
	t.Run("solid save", func(t *testing.T) {
		dir := t.TempDir()
		mustWorld(t, dir)
		path := filepath.Join(dir, "overworld.pile")
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
		p, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := p.StoreColumn(world.ChunkPos{1, 1}, world.Overworld, testColumn(t, reg, world.ChunkPos{1, 1})); err != nil {
			t.Fatal(err)
		}
		if err := p.Save(); err != nil {
			t.Fatal(err)
		}
		_ = p.Close()
		assertMode(t, path, 0o600)
	})
	t.Run("indexed compaction", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "overworld.pile")
		w, err := format.CreateIndexed(path, reg, format.Options{Compression: format.CompressionDefault})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := w.Compact(); err != nil {
			t.Fatal(err)
		}
		_ = w.Close()
		assertMode(t, path, 0o600)
	})
	t.Run("structure save", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "s.pilestruct")
		s := testStructure(t)
		if err := s.Save(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := s.Save(path); err != nil {
			t.Fatal(err)
		}
		assertMode(t, path, 0o600)
	})
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != want {
		t.Fatalf("%s has mode %v after the replace, want %v", path, got, want)
	}
}

// TestNoStagingFileSurvivesAFailedSave: a staging name left on disk is not
// only litter. It is the next save's O_EXCL target, and it is a file whose
// contents are a half-written world sitting beside a good one.
func TestNoStagingFileSurvivesAFailedSave(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	mustWorld(t, dir)
	p, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.StoreColumn(world.ChunkPos{0, 0}, world.Nether, testColumn(t, reg, world.ChunkPos{0, 0})); err != nil {
		t.Fatal(err)
	}
	// A directory at the destination makes the rename fail with everything
	// before it having succeeded, which is the only way to reach the failure
	// path without a fault-injecting filesystem.
	if err := os.MkdirAll(filepath.Join(dir, "nether.pile"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := p.Save(); err == nil {
		t.Fatal("the save reported success over an unwritable destination")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Fatalf("a failed save left %q behind", e.Name())
		}
	}
	_ = p.Close()
}

// TestOpenRefusesADimensionFileThatIsADirectory: a world directory is not
// necessarily one this process created. Every entry it reads by name may be
// something other than a file, and the answer must be an error rather than a
// panic or a silently empty world.
func TestOpenRefusesADimensionFileThatIsADirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "overworld.pile"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil {
		t.Fatal("a directory named overworld.pile opened as a world")
	} else if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reported as missing rather than as the wrong kind of file: %v", err)
	}
}

// testStructure builds a minimal saveable structure.
func testStructure(t *testing.T) *Structure {
	t.Helper()
	data, err := format.NewStructureData([3]int32{1, 1, 1})
	if err != nil {
		t.Fatal(err)
	}
	return newStructure(data, StructureRegistry(testRegistry(t)))
}
