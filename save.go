package pile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/df-mc/dragonfly/server/world"
	"github.com/oriumgames/pile/format"
)

// Save synchronously writes all dirty dimensions. Unchanged dimensions are
// not rewritten unless world metadata changed (metadata is duplicated into
// every dimension file). A failure from an earlier background save is
// surfaced here (and cleared). Encoding and file I/O run outside the
// provider lock, so a save does not stall the world.
func (p *Provider) Save() error {
	if p.conf.readOnly {
		return ErrReadOnly
	}
	err := p.saveNow()
	p.mu.Lock()
	if last := p.lastSaveErr; last != nil {
		p.lastSaveErr = nil
		err = errors.Join(last, err)
	}
	p.mu.Unlock()
	return err
}

// SaveAsync schedules a background save. Multiple calls coalesce; the last
// scheduled save always observes the latest state. No-op when read-only.
//
// It reports nothing, because there is nobody to report to. A background save
// that fails is remembered and returned by the next Save or Close, so the
// failure is not lost — but only the most recent one is kept, and a process
// that only ever calls SaveAsync never observes any of them. Anything that
// must know a save succeeded has to call Save.
func (p *Provider) SaveAsync() {
	// This guard has no input of its own and no test can give it one: every
	// method that could set a dirty flag refuses a read-only provider first,
	// so a read-only save finds nothing to write and produces no file either
	// way. It is kept as the second line of that defence rather than removed,
	// because what stands behind it is ten other guards and not an argument.
	// It has no distinguishing input and so no control that can fail: every
	// method able to set a dirty flag refuses read-only first, so a read-only
	// save finds nothing to write either way. Kept as the second line of a
	// property whose first line is those ten guards.
	if p.conf.readOnly {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.closing {
		return
	}
	p.savePending = true
	if !p.saveRunning {
		p.saveRunning = true
		go p.runSaver()
	}
}

// runSaver drains coalesced SaveAsync requests.
func (p *Provider) runSaver() {
	for {
		p.mu.Lock()
		if p.closed || p.closing || !p.savePending {
			p.saveRunning = false
			p.mu.Unlock()
			return
		}
		p.savePending = false
		p.mu.Unlock()
		if err := p.saveNow(); err != nil {
			p.mu.Lock()
			p.lastSaveErr = err // surfaced by the next Save/Close
			p.mu.Unlock()
		}
	}
}

// saveJob is one dimension file snapshot to be written outside the lock.
type saveJob struct {
	ds   *dimState
	path string
	data *format.WorldData
}

// saveNow snapshots dirty state under p.mu and performs encoding and file
// I/O outside it. saveMu serializes concurrent saves. Stored columns are
// never mutated after StoreColumn (encoding does not compact in place), so
// snapshotting pointer copies is safe against concurrent loads and stores.
func (p *Provider) saveNow() error {
	p.saveMu.Lock()
	defer p.saveMu.Unlock()
	return p.saveHeld()
}

// saveHeld performs a save with saveMu already held, so callers that must
// stay serialised with the write phase (Snapshot) can bracket more work.
func (p *Provider) saveHeld() error {
	p.mu.Lock()
	if p.dir == "" { // in-memory provider: nothing to persist
		p.mu.Unlock()
		return nil
	}
	settings, err := settingsToNBT(p.settings, p.settingsExtra)
	if err != nil {
		p.mu.Unlock()
		return fmt.Errorf("pile: encode settings: %w", err)
	}
	userData := cloneBytes(p.userData)

	if p.conf.appendMode {
		type checkpointJob struct {
			ds *dimState
			iw *format.IndexedWorld
		}
		var jobs []checkpointJob
		for _, ds := range p.dims {
			if ds.iw == nil {
				continue
			}
			if p.metaDirty || ds.dirty {
				if p.metaDirty {
					if err := ds.iw.SetMeta(settings, userData); err != nil {
						p.mu.Unlock()
						return err
					}
				}
				jobs = append(jobs, checkpointJob{ds: ds, iw: ds.iw})
				ds.dirty = false
			}
		}
		p.metaDirty = false
		p.mu.Unlock()
		var firstErr error
		for _, j := range jobs {
			if err := j.iw.Checkpoint(); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				p.mu.Lock()
				j.ds.dirty = true
				p.mu.Unlock()
			}
		}
		return firstErr
	}

	var jobs []saveJob
	for _, ds := range p.dims {
		if len(ds.cols) == 0 && !ds.onDisk {
			continue
		}
		if !ds.dirty && !p.metaDirty && ds.onDisk {
			continue
		}
		cols := make([]format.Column, 0, len(ds.cols))
		for _, c := range ds.cols {
			cols = append(cols, c)
		}
		jobs = append(jobs, saveJob{
			ds:   ds,
			path: p.dimPath(ds.dim),
			data: &format.WorldData{
				Settings: settings, UserData: userData, Columns: cols,
			},
		})
		ds.dirty = false
	}
	p.metaDirty = false
	p.mu.Unlock()

	var firstErr error
	for _, j := range jobs {
		if err := p.writeFile(j.path, j.data); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			p.mu.Lock()
			j.ds.dirty = true // retry on the next save
			p.mu.Unlock()
			continue
		}
		p.mu.Lock()
		j.ds.onDisk = true
		p.mu.Unlock()
	}
	return firstErr
}

// syncDir fsyncs a directory so a rename into it is durable: on many
// filesystems the rename is not persisted until the directory itself is
// synced, so a crash could otherwise resurrect the pre-rename file.
func syncDir(dir string) error {
	if dir == "" {
		dir = "."
	}
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("pile: open directory for sync: %w", err)
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("pile: sync directory %s: %w", dir, err)
	}
	return nil
}

// SaveAs writes the provider's complete current state (including
// base-provider columns for instances) as a fresh world at dir. The provider
// itself is unchanged; an in-memory instance stays in memory. Works on
// read-only providers: it writes a copy, not the source.
func (p *Provider) SaveAs(dir string) error {
	p.mu.Lock()
	settings, err := settingsToNBT(p.settings, p.settingsExtra)
	if err != nil {
		p.mu.Unlock()
		return fmt.Errorf("pile: encode settings: %w", err)
	}
	userData := cloneBytes(p.userData)
	dims := make([]world.Dimension, 0, len(p.dims))
	for _, ds := range p.dims {
		dims = append(dims, ds.dim)
	}
	p.mu.Unlock()
	if p.base != nil {
		p.base.mu.Lock()
		for _, ds := range p.base.dims {
			if !containsDim(dims, ds.dim) {
				dims = append(dims, ds.dim)
			}
		}
		p.base.mu.Unlock()
	}

	wrote := false
	for _, dim := range dims {
		cols, err := p.snapshotDim(dim)
		if err != nil {
			return err
		}
		if len(cols) == 0 {
			continue
		}
		wrote = true
		d := &format.WorldData{
			Settings: settings, UserData: userData, Columns: cols,
		}
		id, _ := world.DimensionID(dim)
		name := fmt.Sprintf("dim%d.pile", id)
		switch id {
		case 0:
			name = "overworld.pile"
		case 1:
			name = "nether.pile"
		case 2:
			name = "end.pile"
		}
		if err := p.writeFile(filepath.Join(dir, name), d); err != nil {
			return err
		}
	}
	if !wrote {
		// A world with metadata but no columns must still be representable,
		// or SaveAs would silently produce nothing to reopen.
		return p.writeFile(filepath.Join(dir, "overworld.pile"), &format.WorldData{
			Settings: settings, UserData: userData})
	}
	return nil
}

func containsDim(dims []world.Dimension, dim world.Dimension) bool {
	return slices.Contains(dims, dim)
}

// writeFile writes one dimension file atomically: temp file, fsync, rename.
func (p *Provider) writeFile(path string, d *format.WorldData) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("pile: create world directory: %w", err)
	}
	tmp := path + ".tmp"
	f, err := createExclusive(tmp)
	if err != nil {
		return fmt.Errorf("pile: create %s: %w", tmp, err)
	}
	opts := format.Options{
		Compression:     p.conf.compression,
		SkipBiomes:      p.conf.skip&SkipBiomes != 0,
		StoreLight:      p.conf.storeLight,
		Stats:           true,
		FastCompression: p.conf.fastSave,
	}
	if err := format.WriteWorld(f, d, p.conf.registry, opts); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("pile: sync %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("pile: close %s: %w", tmp, err)
	}
	if err := preserveMode(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("pile: rename %s: %w", tmp, err)
	}
	return syncDir(filepath.Dir(path))
}

// compactThreshold is the garbage ratio past which Close compacts an
// append-mode dimension file automatically.
const compactThreshold = 0.5

// Close saves pending changes (unless read-only) and marks the provider
// closed. It waits for any in-flight background save to finish first. In
// append mode, dimension files past the garbage threshold are compacted.
//
// A Close that fails does not close the provider: the error is returned, the
// state stays dirty, and Close may be retried once the underlying problem
// (a full disk, say) is resolved.
func (p *Provider) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	if p.closing {
		p.mu.Unlock()
		return errors.New("pile: Close already in progress")
	}
	p.closing = true
	readOnly := p.conf.readOnly
	p.mu.Unlock()

	// Stop accepting background saves while closing, but stay open so a
	// failure is recoverable.
	unwind := func(err error) error {
		p.mu.Lock()
		p.closing = false
		p.mu.Unlock()
		return err
	}

	if !readOnly {
		err := p.saveNow()
		p.mu.Lock()
		if last := p.lastSaveErr; last != nil {
			p.lastSaveErr = nil
			err = errors.Join(last, err)
		}
		p.mu.Unlock()
		if err != nil {
			return unwind(err)
		}
	}
	if p.conf.appendMode {
		p.saveMu.Lock()
		defer p.saveMu.Unlock()
		p.mu.Lock()
		defer p.mu.Unlock()
		var firstErr error
		for _, ds := range p.dims {
			if ds.iw == nil {
				continue
			}
			if !readOnly && ds.iw.GarbageRatio() > compactThreshold {
				if cerr := ds.iw.Compact(); cerr != nil && firstErr == nil {
					firstErr = cerr
				}
			}
			if cerr := ds.iw.Close(); cerr != nil && firstErr == nil {
				firstErr = cerr
			}
			ds.iw = nil
		}
		// The indexed files are closed either way, so the provider is closed
		// even when a failure is reported.
		p.closed, p.closing = true, false
		return firstErr
	}
	p.mu.Lock()
	p.closed, p.closing = true, false
	p.mu.Unlock()
	return nil
}
