package pile

import (
	"errors"
	"fmt"
	"iter"
	"os"
	"sync"

	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
	"github.com/df-mc/goleveldb/leveldb"
	"github.com/google/uuid"
	"github.com/oriumgames/pile/format"
)

// ErrReadOnly is returned by the mutating operations that can report an error
// at all — Save, SetBorder, SetChunkUserData, Snapshot, DeleteSnapshot and
// Rollback — when the provider was opened with ReadOnly. The mutators with no
// error to return (StoreColumn, SaveSettings, SetUserData, SetMarker) are
// silent no-ops instead, as ReadOnly's own documentation says; IsReadOnly is
// how a caller tells the two situations apart before it relies on a write.
var ErrReadOnly = errors.New("pile: provider is read-only")

// The errors a caller branches on, re-exported so that branching on them does
// not mean importing the codec and dragonfly's leveldb package alongside this
// one. They are the same values, so errors.Is against either name works.
var (
	// ErrNotFound reports that no column is stored at a position. LoadColumn
	// returns it, under the name world.Provider's contract requires: it is
	// dragonfly's leveldb.ErrNotFound, and every provider signals a missing
	// chunk with it whatever it is backed by.
	ErrNotFound = leveldb.ErrNotFound
	// ErrCorrupt reports that a file is not a valid pile file. Every decode
	// failure wraps it except ErrDecodeBudget, which is the point of the two
	// being separate.
	ErrCorrupt = format.ErrCorrupt
	// ErrDecodeBudget reports that a decode was stopped by the ceiling set
	// with MaxDecodedBytes. It deliberately does not wrap ErrCorrupt: the file
	// is larger than this provider was told to decode, not broken, and a
	// caller that quarantines corrupt worlds must not quarantine this one.
	ErrDecodeBudget = format.ErrDecodeBudget
)

// Provider implements world.Provider backed by pile files, one per dimension
// (overworld.pile, nether.pile, end.pile). The whole world is held in memory;
// saves are full deterministic rewrites (temp file + atomic rename), so a
// saved file never contains garbage.
type Provider struct {
	conf config
	dir  string
	// base is an optional read-only fallback provider (the template an
	// instance was created from). Columns not stored locally are served from
	// it; local stores shadow it copy-on-write.
	base *Provider

	mu sync.Mutex
	// saveMu serializes save work (encode + file I/O), which runs outside
	// p.mu so a save never stalls column loads/stores.
	saveMu   sync.Mutex
	settings *world.Settings
	// settingsExtra holds settings keys this build does not know, preserved
	// verbatim so an older server cannot erase a newer one's fields.
	settingsExtra map[string]any
	userData      []byte
	markers       []Marker
	border        []byte
	dims          map[int]*dimState

	metaDirty bool
	closed    bool
	// closing marks a Close in progress: background saves stop, but the
	// provider stays usable if the close fails.
	closing     bool
	savePending bool
	saveRunning bool
	// lastSaveErr holds a failed background save's error until the next
	// synchronous Save or Close surfaces it.
	lastSaveErr error
	// iterErr holds the first error a Columns iteration hit, until a caller
	// retrieves it with IterError.
	iterErr error
}

// dimState holds one dimension's columns: an in-memory map in solid mode, an
// IndexedWorld handle in append mode.
type dimState struct {
	dim   world.Dimension
	cols  map[[2]int32]format.Column
	dirty bool
	// onDisk is true when a file for this dimension exists.
	onDisk bool

	// Append mode state.
	iw *format.IndexedWorld
	// meta is the bounded LRU of per-chunk user data and preserved
	// unknown-state sidecars, so overwrites carry them across without
	// re-reading the previous record every time.
	meta *metaCache
	// cache is the optional decoded-column LRU (append mode).
	cache *columnCache
}

// sidecar bundles a column's preserved unknown-state data.
type sidecar struct {
	unknown    []format.UnknownBlock
	ticks      []format.UnknownTick
	states     []format.BlockState
	bioUnknown []format.UnknownBlock
	bioNames   []string
}

// Compile-time interface check.
var _ world.Provider = (*Provider)(nil)

// Open creates a provider for the world directory dir, loading any existing
// pile files in it. The directory is created on first save when absent.
func Open(dir string, opts ...Option) (*Provider, error) {
	conf := defaultConfig()
	for _, o := range opts {
		o(&conf)
	}
	conf.registry.Finalize()

	p := &Provider{
		conf:     conf,
		dir:      dir,
		settings: defaultSettings(),
		dims:     make(map[int]*dimState),
	}
	if err := p.loadFromDisk(); err != nil {
		return nil, err
	}
	return p, nil
}

// loadFromDisk (re)initialises the provider's state from its directory.
func (p *Provider) loadFromDisk() error {
	p.dims = make(map[int]*dimState)
	p.settings = defaultSettings()
	p.userData, p.markers, p.border = nil, nil, nil

	dims, err := worldDimensions(p.dir)
	if err != nil {
		return err
	}
	var meta *format.Meta
	for _, dim := range dims {
		id, _ := world.DimensionID(dim)
		ds := &dimState{dim: dim, cols: make(map[[2]int32]format.Column), meta: newMetaCache(p.conf.cacheColumns), cache: newColumnCache(p.conf.cacheColumns)}
		p.dims[id] = ds

		path := p.dimPath(dim)
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			continue
		}

		if p.conf.appendMode {
			iw, err := format.OpenIndexed(path, p.conf.registry, p.conf.readOnly, p.conf.readOpts()...)
			if errors.Is(err, format.ErrUnsupportedMode) {
				return fmt.Errorf("pile: %s is a solid file; convert it with `pile mode %s indexed` before using AppendMode", path, p.dir)
			} else if err != nil {
				return fmt.Errorf("pile: open %s: %w", path, err)
			}
			ds.iw = iw
			ds.onDisk = true
			if id == 0 || meta == nil {
				s, u, m, b := iw.Meta()
				meta = &format.Meta{Settings: s, UserData: u, Markers: m, Border: b}
			}
			continue
		}

		file, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("pile: open %s: %w", path, err)
		}
		d, err := format.ReadWorld(file, p.conf.registry, p.conf.readOpts()...)
		if errors.Is(err, format.ErrUnsupportedMode) {
			return fmt.Errorf("pile: %s is an indexed file; open the world with pile.AppendMode()", path)
		} else if err != nil {
			return fmt.Errorf("pile: read %s: %w", path, err)
		}
		ds.onDisk = true
		for _, c := range d.Columns {
			ds.cols[[2]int32{c.X, c.Z}] = c
		}
		// World metadata is duplicated in every dimension file; the overworld
		// wins, otherwise the first file found.
		if id == 0 || meta == nil {
			meta = &format.Meta{Settings: d.Settings, UserData: d.UserData, Markers: d.Markers, Border: d.Border}
		}
	}
	if meta != nil {
		s, extra, err := settingsFromNBT(meta.Settings)
		if err != nil {
			return fmt.Errorf("pile: decode settings: %w", err)
		}
		p.settingsExtra = extra
		m, err := markersFromNBT(meta.Markers)
		if err != nil {
			return fmt.Errorf("pile: decode markers: %w", err)
		}
		p.settings, p.userData, p.markers, p.border = s, meta.UserData, m, meta.Border
	}
	return nil
}

// dimPath returns the file path for a dimension.
func (p *Provider) dimPath(dim world.Dimension) string {
	return DimPath(p.dir, dim)
}

// Dir returns the world directory.
func (p *Provider) Dir() string { return p.dir }

// IsReadOnly reports whether the provider was opened read-only.
func (p *Provider) IsReadOnly() bool { return p.conf.readOnly }

// Settings returns the world settings.
//
// The pointer is the provider's own, not a copy: world.Provider's contract has
// dragonfly take it at startup, mutate it as the world runs and hand it back to
// SaveSettings at shutdown. A caller that changes settings outside that cycle
// must still call SaveSettings, because nothing observes a write through this
// pointer and the world would not be marked dirty.
func (p *Provider) Settings() *world.Settings {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.settings
}

// SaveSettings stores the world settings, persisted on the next save.
func (p *Provider) SaveSettings(s *world.Settings) {
	if p.conf.readOnly {
		return
	}
	p.mu.Lock()
	p.settings = s
	p.metaDirty = true
	p.mu.Unlock()
}

// LoadColumn returns a copy of the stored column at pos, falling back to the
// base provider for instances. If none exists, the error matches ErrNotFound
// (dragonfly's leveldb.ErrNotFound) as the world.Provider contract requires.
func (p *Provider) LoadColumn(pos world.ChunkPos, dim world.Dimension) (*chunk.Column, error) {
	if err := checkDim(dim); err != nil {
		return nil, err
	}
	var col *chunk.Column
	if p.conf.appendMode {
		key := [2]int32{pos[0], pos[1]}
		p.mu.Lock()
		ds := p.dim(dim)
		iw := ds.iw
		if e, ok := ds.cache.Get(key); ok {
			ds.meta.Put(key, chunkMeta{ud: e.ud, side: e.side})
			cloned := cloneColumn(e.col)
			p.mu.Unlock()
			col = cloned
		} else {
			p.mu.Unlock()
			if iw == nil {
				return nil, leveldb.ErrNotFound
			}
			// Capture the record's identity before decoding: a StoreColumn
			// that lands while the record is being applied must not be
			// undone by publishing this now-stale result into the caches.
			id, _ := iw.RecordID(pos[0], pos[1])
			fc, err := iw.Column(pos[0], pos[1])
			if errors.Is(err, format.ErrNoColumn) {
				return nil, leveldb.ErrNotFound
			} else if err != nil {
				return nil, err
			}
			side := sidecarOf(fc)
			p.mu.Lock()
			now, still := iw.RecordID(pos[0], pos[1])
			fresh := still && now == id
			if fresh {
				ds.meta.Put(key, chunkMeta{ud: fc.UserData, side: side})
			}
			if ds.cache != nil {
				if fresh {
					// The cache owns fc.Col; hand the caller a copy.
					ds.cache.Put(key, cacheEntry{col: fc.Col, ud: fc.UserData, side: side})
				}
				col = cloneColumn(fc.Col)
			} else {
				col = fc.Col
			}
			p.mu.Unlock()
			if ds.cache != nil && fresh {
				p.readahead(dim, pos)
			}
		}
	} else {
		p.mu.Lock()
		ds := p.dim(dim)
		c, ok := ds.cols[[2]int32{pos[0], pos[1]}]
		if ok {
			// Clone while holding the lock: a concurrent save compacts stored
			// chunks in place, so reading them unlocked races.
			col = cloneColumn(c.Col)
		}
		p.mu.Unlock()
		switch {
		case ok:
		case p.base != nil:
			bc, err := p.base.LoadColumn(pos, dim)
			if err != nil {
				return nil, err
			}
			col = bc
		default:
			return nil, leveldb.ErrNotFound
		}
	}
	if p.conf.loadSkip&SkipEntities != 0 {
		col.Entities = nil
	}
	if p.conf.loadSkip&SkipBlockEntities != 0 {
		col.BlockEntities = nil
	}
	if p.conf.loadSkip&SkipScheduledTicks != 0 {
		col.ScheduledBlocks = nil
	}
	adaptColumnRange(col, p.conf.registry, dim.Range())
	return col, nil
}

// adaptColumnRange re-bases a column onto the dimension's vertical range when
// the stored range differs (e.g. a file saved under an older world height),
// clipping content outside it. Without this, dragonfly panics on block access
// because its sub chunk indexing trusts the dimension range.
func adaptColumnRange(col *chunk.Column, reg world.BlockRegistry, rng cube.Range) {
	if col.Chunk.Range() == rng {
		return
	}
	col.Chunk = format.AdaptRange(col.Chunk, reg, rng)
	keepBE := col.BlockEntities[:0]
	for _, be := range col.BlockEntities {
		if be.Pos.Y() >= rng[0] && be.Pos.Y() <= rng[1] {
			keepBE = append(keepBE, be)
		}
	}
	col.BlockEntities = keepBE
	keepST := col.ScheduledBlocks[:0]
	for _, st := range col.ScheduledBlocks {
		if st.Pos.Y() >= rng[0] && st.Pos.Y() <= rng[1] {
			keepST = append(keepST, st)
		}
	}
	col.ScheduledBlocks = keepST
}

// StoreColumn stores a copy of col at pos, applying the configured skip mask
// and filters. Read-only providers ignore the call.
func (p *Provider) StoreColumn(pos world.ChunkPos, dim world.Dimension, col *chunk.Column) error {
	return p.storeColumn(pos, dim, col, nil)
}

// storeColumn is StoreColumn with an optional explicit unknown-state sidecar
// (used when the caller brings its own, e.g. pasting a structure that carries
// preserved states). A nil sidecar inherits the previous column's.
func (p *Provider) storeColumn(pos world.ChunkPos, dim world.Dimension, col *chunk.Column, side *sidecar) error {
	if err := checkDim(dim); err != nil {
		return err
	}
	if p.conf.readOnly {
		return nil
	}
	if p.conf.filterCol != nil && !p.conf.filterCol(pos, dim) {
		return nil
	}
	c := cloneColumn(col)
	safeCompact(c.Chunk, p.conf.registry.AirRuntimeID())
	if p.conf.skip&SkipEntities != 0 {
		c.Entities = nil
	} else if p.conf.filterEnt != nil {
		c.Entities = filterSlice(c.Entities, p.conf.filterEnt)
	}
	if p.conf.skip&SkipBlockEntities != 0 {
		c.BlockEntities = nil
	} else if p.conf.filterBE != nil {
		c.BlockEntities = filterSlice(c.BlockEntities, p.conf.filterBE)
	}
	if p.conf.skip&SkipScheduledTicks != 0 {
		c.ScheduledBlocks = nil
	}

	if p.conf.appendMode {
		return p.storeAppend(pos, dim, c, side)
	}
	p.mu.Lock()
	ds := p.dim(dim)
	key := [2]int32{pos[0], pos[1]}
	prev, had := ds.cols[key]
	userData := prev.UserData
	unknown, unkTicks, unkStates := prev.Unknown, prev.UnknownTicks, prev.UnknownStates
	bioUnknown, bioNames := prev.UnknownBiomes, prev.UnknownBiomeNames
	p.mu.Unlock()
	if side != nil {
		unknown, unkTicks, unkStates = side.unknown, side.ticks, side.states
		bioUnknown, bioNames = side.bioUnknown, side.bioNames
	} else if !had && p.base != nil {
		// First shadow of a template chunk: inherit its metadata and any
		// preserved unknown states.
		userData = p.base.ChunkUserData(pos, dim)
		bs := p.base.columnSidecar(pos, dim)
		unknown, unkTicks, unkStates = bs.unknown, bs.ticks, bs.states
		bioUnknown, bioNames = bs.bioUnknown, bs.bioNames
	}
	if p.conf.skip&SkipChunkUserData != 0 {
		userData = nil
	}
	p.mu.Lock()
	// The sidecar is carried as-is: encoding re-validates every entry against
	// the placeholder block, so positions the server overwrote drop out.
	ds.cols[key] = format.Column{
		X: pos[0], Z: pos[1], Col: c, UserData: userData,
		Unknown: unknown, UnknownTicks: unkTicks, UnknownStates: unkStates,
		UnknownBiomes: bioUnknown, UnknownBiomeNames: bioNames,
	}
	ds.dirty = true
	p.mu.Unlock()
	return nil
}

// columnSidecar returns a stored column's preserved unknown-state data.
func (p *Provider) columnSidecar(pos world.ChunkPos, dim world.Dimension) sidecar {
	key := [2]int32{pos[0], pos[1]}
	if p.conf.appendMode {
		p.mu.Lock()
		ds := p.dim(dim)
		if m, ok := ds.meta.Get(key); ok {
			p.mu.Unlock()
			return m.side
		}
		iw := ds.iw
		p.mu.Unlock()
		if iw == nil {
			return sidecar{}
		}
		id, ok := iw.RecordID(pos[0], pos[1])
		if !ok {
			return sidecar{}
		}
		fc, err := iw.Column(pos[0], pos[1])
		if err != nil {
			return sidecar{}
		}
		side := sidecarOf(fc)
		p.publishMeta(ds, iw, key, id, chunkMeta{ud: fc.UserData, side: side})
		return side
	}
	p.mu.Lock()
	c, ok := p.dim(dim).cols[key]
	p.mu.Unlock()
	if ok {
		return sidecarOf(c)
	}
	if p.base != nil {
		return p.base.columnSidecar(pos, dim)
	}
	return sidecar{}
}

// publishMeta records a decoded record's per-chunk metadata, but only if the
// record it came from is still the current one.
//
// Decoding happens outside p.mu, so a store may have replaced the record in
// the meantime. Publishing anyway would put a stale sidecar where the newer
// one belongs, and the next store with no explicit sidecar would inherit it:
// the same recheck LoadColumn and readahead already do.
func (p *Provider) publishMeta(ds *dimState, iw *format.IndexedWorld, key [2]int32, id uint64, m chunkMeta) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if now, still := iw.RecordID(key[0], key[1]); !still || now != id {
		return
	}
	ds.meta.Put(key, m)
}

// sidecarOf collects a column's preserved-state fields.
func sidecarOf(c format.Column) sidecar {
	return sidecar{
		unknown: c.Unknown, ticks: c.UnknownTicks, states: c.UnknownStates,
		bioUnknown: c.UnknownBiomes, bioNames: c.UnknownBiomeNames,
	}
}

// filterSlice keeps elements for which keep returns true.
func filterSlice[T any](in []T, keep func(T) bool) []T {
	out := in[:0]
	for _, e := range in {
		if keep(e) {
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// checkDim rejects dimensions that are not registered with dragonfly: their
// ID would alias the overworld and silently merge worlds.
func checkDim(dim world.Dimension) error {
	if _, ok := world.DimensionID(dim); !ok {
		return fmt.Errorf("pile: dimension %v is not registered", dim)
	}
	// The record layout stores a section index and count, so a range that is
	// not 16-block aligned cannot be reproduced exactly. Refuse it here
	// rather than silently re-basing every column.
	if r := dim.Range(); !format.AlignedRange(r) {
		return fmt.Errorf("pile: dimension %v has vertical range %v; pile requires 16-block aligned ranges", dim, r)
	}
	return nil
}

// insertColumn stores a column without cloning it. Callers hand over
// ownership; used by Builder and internal transfers.
func (p *Provider) insertColumn(dim world.Dimension, c format.Column) {
	p.mu.Lock()
	ds := p.dim(dim)
	ds.cols[[2]int32{c.X, c.Z}] = c
	ds.dirty = true
	p.mu.Unlock()
}

// dim returns the state for a dimension, creating it for custom dimensions.
func (p *Provider) dim(dim world.Dimension) *dimState {
	id, _ := world.DimensionID(dim)
	ds, ok := p.dims[id]
	if !ok {
		ds = &dimState{dim: dim, cols: make(map[[2]int32]format.Column), meta: newMetaCache(p.conf.cacheColumns), cache: newColumnCache(p.conf.cacheColumns)}
		p.dims[id] = ds
	}
	return ds
}

// LoadPlayerSpawnPosition delegates to the configured SpawnStore; without
// one, no spawn exists. Pile files never contain player data.
func (p *Provider) LoadPlayerSpawnPosition(id uuid.UUID) (cube.Pos, bool, error) {
	if p.conf.spawns != nil {
		return p.conf.spawns.LoadPlayerSpawn(id)
	}
	return cube.Pos{}, false, nil
}

// SavePlayerSpawnPosition delegates to the configured SpawnStore, if any.
func (p *Provider) SavePlayerSpawnPosition(id uuid.UUID, pos cube.Pos) error {
	if p.conf.spawns == nil || p.conf.readOnly {
		return nil
	}
	return p.conf.spawns.SavePlayerSpawn(id, pos)
}

// ChunkCount returns the number of stored columns in a dimension, including
// base-provider columns not shadowed locally.
func (p *Provider) ChunkCount(dim world.Dimension) int {
	p.mu.Lock()
	if p.conf.appendMode {
		iw := p.dim(dim).iw
		p.mu.Unlock()
		if iw == nil {
			return 0
		}
		return iw.ChunkCount()
	}
	own := len(p.dim(dim).cols)
	p.mu.Unlock()
	if p.base == nil {
		return own
	}
	p.mu.Lock()
	keys := make(map[[2]int32]struct{}, own)
	for k := range p.dim(dim).cols {
		keys[k] = struct{}{}
	}
	p.mu.Unlock()
	for _, k := range p.base.dimKeys(dim) {
		keys[k] = struct{}{}
	}
	return len(keys)
}

// dimKeys returns the column keys of a dimension.
func (p *Provider) dimKeys(dim world.Dimension) [][2]int32 {
	p.mu.Lock()
	ds := p.dim(dim)
	if p.conf.appendMode {
		iw := ds.iw
		p.mu.Unlock()
		if iw == nil {
			return nil
		}
		return iw.Positions()
	}
	defer p.mu.Unlock()
	keys := make([][2]int32, 0, len(ds.cols))
	for k := range ds.cols {
		keys = append(keys, k)
	}
	return keys
}

// Columns iterates over copies of all stored columns of a dimension,
// including base-provider columns not shadowed locally.
//
// The set of positions is fixed when iteration starts, so storing a column at
// a new position during iteration is safe and not observed. Each column is
// produced when it is reached, one at a time: iterating does not require
// memory for the whole dimension, and a caller that stops early pays only for
// what it took. In append mode that means a column overwritten during the
// iteration may be yielded with its new content.
func (p *Provider) Columns(dim world.Dimension) iter.Seq2[world.ChunkPos, *chunk.Column] {
	return func(yield func(world.ChunkPos, *chunk.Column) bool) {
		if p.conf.appendMode {
			p.columnsAppend(dim, yield)
			return
		}
		p.mu.Lock()
		ds := p.dim(dim)
		own := make(map[[2]int32]struct{}, len(ds.cols))
		keys := make([][2]int32, 0, len(ds.cols))
		for k := range ds.cols {
			own[k] = struct{}{}
			keys = append(keys, k)
		}
		p.mu.Unlock()
		for _, k := range keys {
			// Clone under the lock and one at a time: a concurrent save
			// compacts stored chunks in place, so reading them unlocked races,
			// and cloning the whole dimension up front is the cost this
			// iterator exists to avoid.
			p.mu.Lock()
			c, ok := ds.cols[k]
			var col *chunk.Column
			if ok {
				col = cloneColumn(c.Col)
			}
			p.mu.Unlock()
			if !ok {
				continue
			}
			if !yield(world.ChunkPos{k[0], k[1]}, col) {
				return
			}
		}
		if p.base == nil {
			return
		}
		p.base.Columns(dim)(func(pos world.ChunkPos, col *chunk.Column) bool {
			if _, shadowed := own[[2]int32{pos[0], pos[1]}]; shadowed {
				return true
			}
			return yield(pos, col)
		})
		p.noteIterErr(p.base.IterError())
	}
}

// columnsAppend yields an append-mode dimension's columns, decoding one record
// per step.
//
// Positions is the directory's key list and costs nothing; decoding is what is
// expensive, and doing it all before the first result meant a caller looking
// for one column, or computing a bounding box it could answer from positions
// alone, held every column in the dimension at once.
func (p *Provider) columnsAppend(dim world.Dimension, yield func(world.ChunkPos, *chunk.Column) bool) {
	p.mu.Lock()
	iw := p.dim(dim).iw
	p.mu.Unlock()
	if iw == nil {
		return
	}
	for _, k := range iw.Positions() {
		c, err := iw.Column(k[0], k[1])
		if errors.Is(err, format.ErrNoColumn) {
			// Removed since the key list was taken; not data loss.
			continue
		} else if err != nil {
			// A record that cannot be read is data loss if skipped: stop and
			// report it rather than producing a short world.
			p.noteIterErr(fmt.Errorf("pile: read column (%d,%d): %w", k[0], k[1], err))
			return
		}
		if !yield(world.ChunkPos{c.X, c.Z}, c.Col) {
			return
		}
	}
}

// noteIterErr records the first error an iteration hit. An iterator cannot
// yield one, so callers for which a short iteration would mean data loss
// retrieve it with IterError afterwards.
func (p *Provider) noteIterErr(err error) {
	if err == nil {
		return
	}
	p.mu.Lock()
	if p.iterErr == nil {
		p.iterErr = err
	}
	p.mu.Unlock()
}

// IterError returns and clears the first error a Columns iteration hit. An
// iterator cannot report errors itself, so any caller for which a short
// iteration would mean data loss (conversion, export, backup) must check
// this after iterating.
func (p *Provider) IterError() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	err := p.iterErr
	p.iterErr = nil
	return err
}

// snapshotDim returns deep copies of all columns of a dimension, merged with
// the base provider (local columns win).
func (p *Provider) snapshotDim(dim world.Dimension) ([]format.Column, error) {
	if p.conf.appendMode {
		p.mu.Lock()
		iw := p.dim(dim).iw
		p.mu.Unlock()
		if iw == nil {
			return nil, nil
		}
		var out []format.Column
		for _, k := range iw.Positions() {
			c, err := iw.Column(k[0], k[1])
			if err != nil {
				// A record that cannot be read is data loss if skipped:
				// report it rather than producing a short world.
				return nil, fmt.Errorf("pile: read column (%d,%d): %w", k[0], k[1], err)
			}
			out = append(out, c)
		}
		return out, nil
	}
	p.mu.Lock()
	own := make(map[[2]int32]struct{}, len(p.dim(dim).cols))
	snapshot := make([]format.Column, 0, len(p.dim(dim).cols))
	for k, c := range p.dim(dim).cols {
		own[k] = struct{}{}
		snapshot = append(snapshot, format.Column{
			X: c.X, Z: c.Z, Col: cloneColumn(c.Col), UserData: cloneBytes(c.UserData),
			Unknown: c.Unknown, UnknownTicks: c.UnknownTicks, UnknownStates: c.UnknownStates,
			UnknownBiomes: c.UnknownBiomes, UnknownBiomeNames: c.UnknownBiomeNames,
		})
	}
	p.mu.Unlock()
	if p.base != nil {
		baseCols, err := p.base.snapshotDim(dim)
		if err != nil {
			return nil, err
		}
		for _, c := range baseCols {
			if _, shadowed := own[[2]int32{c.X, c.Z}]; !shadowed {
				snapshot = append(snapshot, c)
			}
		}
	}
	return snapshot, nil
}

// cloneBytes copies b, returning nil for empty input.
func cloneBytes(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
