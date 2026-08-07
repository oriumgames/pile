package pile

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// snapshotsDirName is the directory inside a world holding its snapshots.
const snapshotsDirName = "snapshots"

// validateSnapshotName rejects names that would escape the snapshots
// directory or collide with dimension files.
func validateSnapshotName(name string) error {
	if name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return fmt.Errorf("pile: invalid snapshot name %q", name)
	}
	return nil
}

// Snapshot saves the current state and copies the world's files into
// snapshots/<name>, replacing any snapshot of the same name. Not supported on
// read-only or in-memory providers.
func (p *Provider) Snapshot(name string) error {
	if p.conf.readOnly {
		return ErrReadOnly
	}
	if p.dir == "" {
		return errors.New("pile: in-memory provider cannot snapshot; use SaveAs")
	}
	if err := validateSnapshotName(name); err != nil {
		return err
	}
	// Hold saveMu across both the save and the copies: a concurrent save
	// writes files outside p.mu, so copying without it could mix dimensions
	// from different generations into one snapshot.
	p.saveMu.Lock()
	defer p.saveMu.Unlock()
	if err := p.saveHeld(); err != nil {
		return err
	}
	p.mu.Lock()
	if last := p.lastSaveErr; last != nil {
		p.lastSaveErr = nil
		p.mu.Unlock()
		return last
	}
	p.mu.Unlock()

	p.mu.Lock()
	defer p.mu.Unlock()
	dst := filepath.Join(p.dir, snapshotsDirName, name)
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for _, ds := range p.dims {
		if !ds.onDisk {
			continue
		}
		src := p.dimPath(ds.dim)
		if _, err := os.Stat(src); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := copyFile(src, filepath.Join(dst, filepath.Base(src))); err != nil {
			return fmt.Errorf("pile: snapshot %s: %w", name, err)
		}
	}
	return nil
}

// Snapshots lists the world's snapshot names.
func (p *Provider) Snapshots() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(p.dir, snapshotsDirName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// DeleteSnapshot removes a snapshot.
func (p *Provider) DeleteSnapshot(name string) error {
	if p.conf.readOnly {
		return ErrReadOnly
	}
	if err := validateSnapshotName(name); err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(p.dir, snapshotsDirName, name))
}

// Rollback replaces the world's current state with a snapshot. All unsaved
// and saved changes since the snapshot are discarded; the provider reloads
// from the restored files.
func (p *Provider) Rollback(name string) error {
	if p.conf.readOnly {
		return ErrReadOnly
	}
	if err := validateSnapshotName(name); err != nil {
		return err
	}
	src := filepath.Join(p.dir, snapshotsDirName, name)
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("pile: snapshot %q: %w", name, err)
	}

	// Serialize with any in-flight save: a save writing the old files while
	// rollback replaces them would corrupt the restored state.
	p.saveMu.Lock()
	defer p.saveMu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	// Release any open indexed files before replacing them.
	for _, ds := range p.dims {
		if ds.iw != nil {
			_ = ds.iw.Close()
			ds.iw = nil
		}
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	// Stage every snapshot file beside its destination first, so a failure or
	// crash part-way cannot leave some dimensions restored and others
	// missing: only after all copies are durable are the files renamed in.
	type staged struct{ tmp, dst string }
	var files []staged
	cleanup := func() {
		for _, f := range files {
			_ = os.Remove(f.tmp)
		}
	}
	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".pile") {
			// Not a regular file: a directory, a device node, or a FIFO, which
			// copyFile's os.Open would block on until somebody wrote to it. A
			// snapshots directory arrives with the world it belongs to and is
			// not necessarily something this process produced.
			continue
		}
		dst := filepath.Join(p.dir, e.Name())
		tmp := dst + ".rollback"
		if err := copyFile(filepath.Join(src, e.Name()), tmp); err != nil {
			cleanup()
			return fmt.Errorf("pile: rollback %s: %w", name, err)
		}
		files = append(files, staged{tmp: tmp, dst: dst})
	}
	// Move the current dimension files aside rather than deleting them, and
	// keep them until the restored world has been read back.
	//
	// This used to remove the files the snapshot does not contain, rename the
	// snapshot's copies over the rest, and then reload — so a snapshot
	// directory holding a file that is not a world (or a dimension file this
	// build cannot open, or one whose chunk data is broken) replaced a working
	// world with it, the reload failed, and the world was gone: not only from
	// the provider but from the directory, permanently. `snapshots/` travels
	// inside a world somebody hands you, and Rollback is the operator-facing
	// grief-recovery command, so the two meet.
	type parked struct{ path, aside string }
	var park []parked
	var placed []string
	// undo puts the directory back the way it was: remove what was renamed in,
	// then move the parked originals home in reverse order.
	undo := func() {
		for i := len(placed) - 1; i >= 0; i-- {
			_ = os.Remove(placed[i])
		}
		for i := len(park) - 1; i >= 0; i-- {
			_ = os.Remove(park[i].path)
			_ = os.Rename(park[i].aside, park[i].path)
		}
	}
	for _, ds := range p.dims {
		path := p.dimPath(ds.dim)
		aside := path + ".rollbackold"
		_ = os.Remove(aside)
		if err := os.Rename(path, aside); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			undo()
			cleanup()
			return fmt.Errorf("pile: rollback %s: %w", name, err)
		}
		park = append(park, parked{path: path, aside: aside})
	}
	for _, f := range files {
		if err := preserveMode(f.tmp, f.dst); err != nil {
			undo()
			cleanup()
			return fmt.Errorf("pile: rollback %s: %w", name, err)
		}
		if err := os.Rename(f.tmp, f.dst); err != nil {
			undo()
			cleanup()
			return fmt.Errorf("pile: rollback %s: %w", name, err)
		}
		placed = append(placed, f.dst)
	}
	if err := syncDir(p.dir); err != nil {
		undo()
		return err
	}
	if err := p.loadFromDisk(); err != nil {
		// The snapshot does not load. Put the world back exactly as it was and
		// report the snapshot, not the world, as the problem.
		undo()
		_ = syncDir(p.dir)
		if rerr := p.loadFromDisk(); rerr != nil {
			return errors.Join(fmt.Errorf("pile: rollback %s: %w", name, err),
				fmt.Errorf("pile: restoring the world after the failed rollback also failed: %w", rerr))
		}
		return fmt.Errorf("pile: rollback %s: snapshot does not load; the world is unchanged: %w", name, err)
	}
	for _, pk := range park {
		_ = os.Remove(pk.aside)
	}
	return syncDir(p.dir)
}

// copyFile copies src to dst, fsyncing the destination.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := createExclusive(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
