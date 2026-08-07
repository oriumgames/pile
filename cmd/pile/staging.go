package main

import (
	"fmt"
	"os"
)

// createStaged creates a file that must not already exist.
//
// It is the command-line copy of the library's createExclusive, and exists for
// the same reason: os.Create is O_WRONLY|O_CREATE|O_TRUNC, which follows a
// symlink at the target. Every one of these tools stages a file at a
// predictable name beside the world it is rewriting — "<file>.upgrade",
// "<file>.mode", "<file>.tmp" — so in a directory another user can write to,
// that name can be pre-created as a symlink pointing anywhere this process can
// write, and the conversion goes through it. O_EXCL refuses to follow one. A
// stale file from a crashed run is removed first, deliberately and by this
// process, rather than being silently written through.
//
// The library keeps its own copy because this one is in package main and
// cannot be imported; the two are the same function and must stay so.
func createStaged(path string) (*os.File, error) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("pile: clear stale temporary file: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, fmt.Errorf("pile: create %s: %w", path, err)
	}
	return f, nil
}

// preserveMode copies an existing destination's permission bits onto the file
// staged to replace it, so an atomic replace does not quietly widen them.
func preserveMode(tmp, dst string) error {
	fi, err := os.Lstat(dst)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("pile: stat %s: %w", dst, err)
	}
	if !fi.Mode().IsRegular() {
		return nil
	}
	if err := os.Chmod(tmp, fi.Mode().Perm()); err != nil {
		return fmt.Errorf("pile: preserve mode of %s: %w", dst, err)
	}
	return nil
}
