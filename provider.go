package pile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
	"github.com/df-mc/goleveldb/leveldb"
	"github.com/google/uuid"
	"github.com/oriumgames/pile/format"
)

// CompressionLevel represents the compression level for saving worlds.
type CompressionLevel = format.CompressionLevel

const (
	// CompressionLevelNone disables compression.
	CompressionLevelNone = format.CompressionLevelNone
	// CompressionLevelFast uses fast compression (level 1).
	CompressionLevelFast = format.CompressionLevelFast
	// CompressionLevelDefault uses default compression (level 3).
	CompressionLevelDefault = format.CompressionLevelDefault
	// CompressionLevelBest uses best compression (level 9).
	CompressionLevelBest = format.CompressionLevelBest
)

// Provider implements world.Provider for the Pile world format.
// Pile is a single-file world format designed for small worlds.
// Note: Pile loads the entire world into memory, so it's only suitable for small worlds.
type Provider struct {
	mu       sync.RWMutex
	dir      string
	settings *world.Settings

	// Separate worlds for each dimension
	overworld *format.World
	nether    *format.World
	end       *format.World

	// Player spawn positions
	playerSpawns map[uuid.UUID]cube.Pos

	dirty            bool             // Track if we need to save
	compressionLevel CompressionLevel // Compression level for saves
	readOnly         bool             // When true, prevents all modifications

	// Background save subsystem
	saveCh         chan struct{} // Non-blocking save trigger channel
	stopCh         chan struct{} // Stop signal for background saver
	streamingSaves bool          // When true, use streaming write path (chunk-by-chunk)
}

// New creates a new Pile provider in the given directory.
// If no world files exist, new ones will be created on first save.
func New(dir string) (*Provider, error) {
	return NewWithCompression(dir, CompressionLevelDefault)
}

// NewWithCompression creates a new Pile provider with a specific compression level.
func NewWithCompression(dir string, compressionLevel CompressionLevel) (*Provider, error) {
	return newProvider(dir, compressionLevel, false)
}

// NewReadOnly creates a new read-only Pile provider in the given directory.
// All modification operations (StoreColumn, SetUserData, SavePlayerSpawnPosition) will be silently ignored.
// The provider will not create any files if they don't exist.
func NewReadOnly(dir string) (*Provider, error) {
	return NewReadOnlyWithCompression(dir, CompressionLevelDefault)
}

// NewReadOnlyWithCompression creates a new read-only Pile provider with a specific compression level.
// The compression level is only used if the provider is later converted to read-write mode.
func NewReadOnlyWithCompression(dir string, compressionLevel CompressionLevel) (*Provider, error) {
	return newProvider(dir, compressionLevel, true)
}

// newProvider is the internal constructor that all public constructors delegate to.
func newProvider(dir string, compressionLevel CompressionLevel, readOnly bool) (*Provider, error) {
	// Only create directory if not read-only
	if !readOnly {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("create pile directory: %w", err)
		}
	}

	p := &Provider{
		dir:              dir,
		settings:         defaultSettings(),
		playerSpawns:     make(map[uuid.UUID]cube.Pos),
		compressionLevel: compressionLevel,
		readOnly:         readOnly,
	}

	// Try to load existing worlds
	if err := p.load(readOnly); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("load pile worlds: %w", err)
	}

	return p, nil
}

// SetCompressionLevel sets the compression level for future saves.
func (p *Provider) SetCompressionLevel(level CompressionLevel) {
	p.mu.Lock()
	p.compressionLevel = level
	p.mu.Unlock()
}

// IsReadOnly returns true if the provider is in read-only mode.
func (p *Provider) IsReadOnly() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.readOnly
}

// Settings returns the world settings.
func (p *Provider) Settings() *world.Settings {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.settings
}

// SaveSettings sets the world settings.
// Pile doesn't store any settings data.
func (p *Provider) SaveSettings(s *world.Settings) {
	p.mu.Lock()
	p.settings = s
	p.mu.Unlock()
}

// LoadColumn loads a chunk column from the appropriate dimension.
func (p *Provider) LoadColumn(pos world.ChunkPos, dim world.Dimension) (*chunk.Column, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	w := p.worldForDim(dim)
	if w == nil {
		return nil, leveldb.ErrNotFound
	}

	c := w.Chunk(pos[0], pos[1])
	if c == nil {
		return nil, leveldb.ErrNotFound
	}

	// Convert Pile chunk to Dragonfly column
	return chunkToColumn(c, dim.Range())
}

// StoreColumn stores a chunk column to the appropriate dimension.
// Silently ignores the operation if the provider is read-only.
func (p *Provider) StoreColumn(pos world.ChunkPos, dim world.Dimension, col *chunk.Column) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.readOnly {
		return nil
	}

	w := p.worldForDim(dim)
	if w == nil {
		w = format.NewWorld(int32(dim.Range()[0]>>4), int32(dim.Range()[1]>>4))
		p.setWorldForDim(dim, w)
	}

	// Convert Dragonfly column to Pile chunk
	c, err := columnToChunk(col, pos[0], pos[1], dim.Range())
	if err != nil {
		return fmt.Errorf("convert column to pile chunk: %w", err)
	}

	w.SetChunk(c)
	p.dirty = true
	return nil
}

// LoadPlayerSpawnPosition loads a player's spawn position.
func (p *Provider) LoadPlayerSpawnPosition(id uuid.UUID) (cube.Pos, bool, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	pos, ok := p.playerSpawns[id]
	return pos, ok, nil
}

// SavePlayerSpawnPosition saves a player's spawn position.
// Silently ignores the operation if the provider is read-only.
func (p *Provider) SavePlayerSpawnPosition(id uuid.UUID, pos cube.Pos) error {
	p.mu.Lock()
	if p.readOnly {
		p.mu.Unlock()
		return nil
	}
	p.playerSpawns[id] = pos
	p.dirty = true
	p.mu.Unlock()
	return nil
}

// Close saves all pending changes and closes the provider.
// Does nothing if the provider is read-only.
func (p *Provider) Close() error {
	// Stop background saver to avoid concurrent writes during shutdown.
	p.DisableBackgroundSaves()

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.readOnly {
		return nil
	}

	if p.dirty {
		return p.saveInternal()
	}
	return nil
}

// Save forces a save of all worlds.
// Does nothing if the provider is read-only.
func (p *Provider) Save() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.readOnly {
		return nil
	}

	return p.saveInternal()
}

// ChunkCount returns the total number of chunks across all dimensions.
func (p *Provider) ChunkCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()

	count := 0
	if p.overworld != nil {
		count += p.overworld.ChunkCount()
	}
	if p.nether != nil {
		count += p.nether.ChunkCount()
	}
	if p.end != nil {
		count += p.end.ChunkCount()
	}
	return count
}

// DimensionChunkCount returns the number of chunks in a specific dimension.
func (p *Provider) DimensionChunkCount(dim world.Dimension) int {
	p.mu.RLock()
	defer p.mu.RUnlock()

	w := p.worldForDim(dim)
	if w == nil {
		return 0
	}
	return w.ChunkCount()
}

// IsDirty returns whether the provider has unsaved changes.
func (p *Provider) IsDirty() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.dirty
}

// worldForDim returns the world for the given dimension.
func (p *Provider) worldForDim(dim world.Dimension) *format.World {
	switch dim {
	case world.Overworld:
		return p.overworld
	case world.Nether:
		return p.nether
	case world.End:
		return p.end
	default:
		return nil
	}
}

// GetUserData returns the user data for the specified dimension.
func (p *Provider) GetUserData() []byte {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.overworld.UserData
}

// SetUserData sets the user data for the specified dimension.
// Silently ignores the operation if the provider is read-only.
func (p *Provider) SetUserData(d world.Dimension, data []byte) {
	if p.readOnly {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.overworld.SetUserData(data)
	p.dirty = true
}

// setWorldForDim sets the world for the given dimension.
func (p *Provider) setWorldForDim(dim world.Dimension, w *format.World) {
	switch dim {
	case world.Overworld:
		p.overworld = w
	case world.Nether:
		p.nether = w
	case world.End:
		p.end = w
	}
}

// dimensionFileName returns the file name for a dimension.
func dimensionFileName(dim world.Dimension) string {
	switch dim {
	case world.Overworld:
		return "overworld.pile"
	case world.Nether:
		return "nether.pile"
	case world.End:
		return "end.pile"
	default:
		return "unknown.pile"
	}
}

// load loads all world files from disk.
func (p *Provider) load(readOnly bool) error {
	dims := []world.Dimension{world.Overworld, world.Nether, world.End}

	for _, dim := range dims {
		path := filepath.Join(p.dir, dimensionFileName(dim))
		f, err := os.Open(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // File doesn't exist yet, skip
			}
			return fmt.Errorf("open %s: %w", path, err)
		}

		var w *format.World
		if readOnly {
			w, err = format.ReadOnly(f)
		} else {
			w, err = format.Read(f)
		}
		f.Close()
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}

		p.setWorldForDim(dim, w)
	}

	return nil
}

// saveInternal saves all worlds to disk. Must be called with lock held.
func (p *Provider) saveInternal() error {
	dims := []struct {
		dim   world.Dimension
		world *format.World
	}{
		{world.Overworld, p.overworld},
		{world.Nether, p.nether},
		{world.End, p.end},
	}

	for _, d := range dims {
		if d.world == nil {
			continue
		}

		path := filepath.Join(p.dir, dimensionFileName(d.dim))
		f, err := os.Create(path)
		if err != nil {
			return fmt.Errorf("create %s: %w", path, err)
		}

		// Streaming write path: Stream chunk-by-chunk to reduce peak memory usage.
		if p.streamingSaves {
			if err := format.WriteStreaming(f, d.world, p.compressionLevel); err != nil {
				_ = f.Close() // Ignore error on cleanup path
				return fmt.Errorf("write(streaming) %s: %w", path, err)
			}
		} else {
			// Legacy path: Buffer entire world before writing.
			if err := format.WriteWithCompression(f, d.world, p.compressionLevel); err != nil {
				_ = f.Close() // Ignore error on cleanup path
				return fmt.Errorf("write %s: %w", path, err)
			}
		}

		if err := f.Close(); err != nil {
			return fmt.Errorf("close %s: %w", path, err)
		}

		// Clear dirty flags after successful save
		d.world.ClearDirty()
	}

	p.dirty = false
	return nil
}

// defaultSettings returns default world settings.
func defaultSettings() *world.Settings {
	return &world.Settings{
		Name:            "Pile",
		Time:            6000,
		DefaultGameMode: world.GameModeAdventure,
		Difficulty:      world.DifficultyNormal,
	}
}

// SetStreamingSaves enables or disables streaming saves (chunk-by-chunk).
// When enabled, the provider streams chunks to disk instead of buffering the entire world.
func (p *Provider) SetStreamingSaves(enabled bool) {
	p.mu.Lock()
	p.streamingSaves = enabled
	p.mu.Unlock()
}

// EnableBackgroundSaves starts a background goroutine that coalesces save requests
// and writes the world to disk asynchronously.
func (p *Provider) EnableBackgroundSaves() {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Already running.
	if p.saveCh != nil && p.stopCh != nil {
		return
	}
	p.saveCh = make(chan struct{}, 1)
	p.stopCh = make(chan struct{})

	go p.runSaver()
}

// DisableBackgroundSaves stops the background save goroutine.
func (p *Provider) DisableBackgroundSaves() {
	p.mu.Lock()
	stop := p.stopCh
	// Set to nil to prevent double-close and mark as disabled
	p.stopCh = nil
	p.saveCh = nil
	p.mu.Unlock()

	// Signal goroutine to stop
	if stop != nil {
		close(stop)
	}
}

// SaveAsync schedules a background save and returns immediately.
// If the background saver is not enabled, this is a no-op.
func (p *Provider) SaveAsync() {
	p.mu.RLock()
	ch := p.saveCh
	p.mu.RUnlock()

	if ch == nil {
		return
	}
	// Non-blocking signal: coalesce multiple save requests.
	select {
	case ch <- struct{}{}:
	default:
	}
}

// runSaver processes asynchronous save requests.
func (p *Provider) runSaver() {
	for {
		select {
		case _, ok := <-p.saveCh:
			if !ok {
				return
			}
			// Coalesce multiple quick-fire requests into one save.
		coalesce:
			for {
				select {
				case <-p.saveCh:
					continue
				default:
					break coalesce
				}
			}
			// Perform save under lock to keep world state consistent.
			p.mu.Lock()
			_ = p.saveInternal()
			p.mu.Unlock()
		case <-p.stopCh:
			return
		}
	}
}
