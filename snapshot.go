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
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".pile") {
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
	// Remove dimension files the snapshot does not contain.
	keep := map[string]bool{}
	for _, f := range files {
		keep[f.dst] = true
	}
	for _, ds := range p.dims {
		path := p.dimPath(ds.dim)
		if !keep[path] {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				cleanup()
				return err
			}
		}
	}
	for _, f := range files {
		if err := preserveMode(f.tmp, f.dst); err != nil {
			cleanup()
			return fmt.Errorf("pile: rollback %s: %w", name, err)
		}
		if err := os.Rename(f.tmp, f.dst); err != nil {
			cleanup()
			return fmt.Errorf("pile: rollback %s: %w", name, err)
		}
	}
	if err := syncDir(p.dir); err != nil {
		return err
	}
	return p.loadFromDisk()
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
