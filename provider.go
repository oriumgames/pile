package pile

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
	"github.com/df-mc/goleveldb/leveldb"
	"github.com/google/uuid"
	"github.com/klauspost/compress/zstd"
)

// CompressionLevel represents the compression level for saving worlds.
type CompressionLevel int

const (
	// CompressionLevelNone disables compression.
	CompressionLevelNone CompressionLevel = iota
	// CompressionLevelFast uses fast compression (level 1).
	CompressionLevelFast
	// CompressionLevelDefault uses default compression (level 3).
	CompressionLevelDefault
	// CompressionLevelBest uses best compression (level 9).
	CompressionLevelBest
)

// Provider implements world.Provider for the Pile world format.
// Pile is a single-file world format designed for small worlds.
// Note: Pile loads the entire world into memory, so it's only suitable for small worlds.
type Provider struct {
	mu       sync.RWMutex
	dir      string
	settings *world.Settings

	// Separate worlds for each dimension
	overworld *World
	nether    *World
	end       *World

	// Player spawn positions
	playerSpawns map[uuid.UUID]cube.Pos

	dirty            bool             // Track if we need to save
	compressionLevel CompressionLevel // Compression level for saves

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
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create pile directory: %w", err)
	}

	p := &Provider{
		dir:              dir,
		settings:         defaultSettings(),
		playerSpawns:     make(map[uuid.UUID]cube.Pos),
		compressionLevel: compressionLevel,
	}

	// Try to load existing worlds
	if err := p.load(); err != nil && !errors.Is(err, os.ErrNotExist) {
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

// Settings returns the world settings.
func (p *Provider) Settings() *world.Settings {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.settings
}

// SaveSettings saves the world settings.
func (p *Provider) SaveSettings(s *world.Settings) {
	p.mu.Lock()
	p.settings = s
	p.dirty = true
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
func (p *Provider) StoreColumn(pos world.ChunkPos, dim world.Dimension, col *chunk.Column) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	w := p.worldForDim(dim)
	if w == nil {
		w = newWorld(dim.Range())
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
func (p *Provider) SavePlayerSpawnPosition(id uuid.UUID, pos cube.Pos) error {
	p.mu.Lock()
	p.playerSpawns[id] = pos
	p.dirty = true
	p.mu.Unlock()
	return nil
}

// Close saves all pending changes and closes the provider.
func (p *Provider) Close() error {
	// Stop background saver to avoid concurrent writes during shutdown.
	p.DisableBackgroundSaves()

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.dirty {
		return p.saveInternal()
	}
	return nil
}

// Save forces a save of all worlds.
func (p *Provider) Save() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.saveInternal()
}

// ChunkCount returns the total number of chunks across all dimensions.
func (p *Provider) ChunkCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()

	count := 0
	if p.overworld != nil {
		count += len(p.overworld.chunks)
	}
	if p.nether != nil {
		count += len(p.nether.chunks)
	}
	if p.end != nil {
		count += len(p.end.chunks)
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
	return len(w.chunks)
}

// IsDirty returns whether the provider has unsaved changes.
func (p *Provider) IsDirty() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.dirty
}

// worldForDim returns the world for the given dimension.
func (p *Provider) worldForDim(dim world.Dimension) *World {
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

// setWorldForDim sets the world for the given dimension.
func (p *Provider) setWorldForDim(dim world.Dimension, w *World) {
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
func (p *Provider) load() error {
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

		w, err := readWorld(f, dim.Range())
		f.Close()
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}

		p.setWorldForDim(dim, w)
	}

	// Load settings from metadata if available
	if p.overworld != nil && len(p.overworld.UserData) > 0 {
		settings := &Settings{}
		if err := decodeSettings(p.overworld.UserData, settings); err == nil {
			// Successfully loaded settings - convert to world.Settings
			p.settings = settingsFromInternal(settings)
		}
	}

	return nil
}

// saveInternal saves all worlds to disk. Must be called with lock held.
func (p *Provider) saveInternal() error {
	dims := []struct {
		dim   world.Dimension
		world *World
	}{
		{world.Overworld, p.overworld},
		{world.Nether, p.nether},
		{world.End, p.end},
	}

	// Encode settings into overworld metadata
	if p.overworld != nil {
		settings := settingsToInternal(p.settings)
		p.overworld.UserData = encodeSettings(settings)
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
			if err := writeWorldStreaming(f, d.world, p.compressionLevel); err != nil {
				_ = f.Close() // Ignore error on cleanup path
				return fmt.Errorf("write(streaming) %s: %w", path, err)
			}
		} else {
			// Legacy path: Buffer entire world before writing.
			if err := writeWorldWithCompression(f, d.world, p.compressionLevel); err != nil {
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

// readWorld reads a Pile world from a reader.
func readWorld(r io.Reader, dimRange cube.Range) (*World, error) {
	// Read magic number
	var magic uint32
	if err := binary.Read(r, binary.BigEndian, &magic); err != nil {
		return nil, fmt.Errorf("read magic: %w", err)
	}
	if magic != MagicNumber {
		return nil, fmt.Errorf("invalid magic number: got 0x%08X, want 0x%08X", magic, MagicNumber)
	}

	// Read version
	var version int16
	if err := binary.Read(r, binary.BigEndian, &version); err != nil {
		return nil, fmt.Errorf("read version: %w", err)
	}
	if version > CurrentVersion {
		return nil, fmt.Errorf("unsupported version: %d (max supported: %d)", version, CurrentVersion)
	}

	// Read compression type
	var compression uint8
	if err := binary.Read(r, binary.BigEndian, &compression); err != nil {
		return nil, fmt.Errorf("read compression: %w", err)
	}

	// Read data length (unused but required for format compatibility)
	_, err := readVarInt(r)
	if err != nil {
		return nil, fmt.Errorf("read data length: %w", err)
	}

	// Read and optionally decompress data
	var dataReader io.Reader = r
	if compression == CompressionZstd {
		decoder, err := zstd.NewReader(r)
		if err != nil {
			return nil, fmt.Errorf("create zstd decoder: %w", err)
		}
		defer decoder.Close()
		dataReader = decoder
	}

	// Read world data
	return decodeWorld(dataReader, dimRange)
}

// writeWorld writes a Pile world to a writer.
func writeWorld(w io.Writer, world *World) error {
	return writeWorldWithCompression(w, world, CompressionLevelDefault)
}

// writeWorldWithCompression writes a Pile world to a writer with a specific compression level.
func writeWorldWithCompression(w io.Writer, world *World, compressionLevel CompressionLevel) error {
	buf := newBuffer()

	// Encode world data
	encodeWorld(buf, world)
	data := buf.Bytes()

	// Compress based on compression level
	compression := CompressionNone
	compressedData := data

	if compressionLevel != CompressionLevelNone && len(data) > 1024 {
		// Map compression level to zstd level
		var zstdLevel zstd.EncoderLevel
		switch compressionLevel {
		case CompressionLevelFast:
			zstdLevel = zstd.SpeedFastest
		case CompressionLevelDefault:
			zstdLevel = zstd.SpeedDefault
		case CompressionLevelBest:
			zstdLevel = zstd.SpeedBestCompression
		default:
			zstdLevel = zstd.SpeedDefault
		}

		encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstdLevel))
		if err == nil {
			compressed := encoder.EncodeAll(data, make([]byte, 0, len(data)))
			if len(compressed) < len(data) {
				compression = CompressionZstd
				compressedData = compressed
			}
			encoder.Close()
		}
	}

	// Write header
	if err := binary.Write(w, binary.BigEndian, uint32(MagicNumber)); err != nil {
		return fmt.Errorf("write magic: %w", err)
	}
	if err := binary.Write(w, binary.BigEndian, int16(CurrentVersion)); err != nil {
		return fmt.Errorf("write version: %w", err)
	}
	if err := binary.Write(w, binary.BigEndian, uint8(compression)); err != nil {
		return fmt.Errorf("write compression: %w", err)
	}
	if err := writeVarInt(w, int64(len(data))); err != nil {
		return fmt.Errorf("write data length: %w", err)
	}

	// Write data
	if _, err := w.Write(compressedData); err != nil {
		return fmt.Errorf("write data: %w", err)
	}

	return nil
}

// defaultSettings returns default world settings.
func defaultSettings() *world.Settings {
	return &world.Settings{
		Name:            "Pile World",
		Spawn:           cube.Pos{0, 64, 0},
		Time:            6000,
		TimeCycle:       true,
		WeatherCycle:    true,
		DefaultGameMode: world.GameModeSurvival,
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

// writeWorldStreaming writes a Pile world to a writer using a streaming approach.
// It writes the world header first, followed by world data streamed chunk-by-chunk.
// For compressed output, a streaming Zstd encoder is used.
// Note: The uncompressed data length in the header is written as a placeholder and not validated by the decoder.
func writeWorldStreaming(w io.Writer, world *World, compressionLevel CompressionLevel) error {
	// Determine compression mode.
	compression := CompressionNone
	var dataWriter io.Writer = w
	var zstdWriter *zstd.Encoder

	if compressionLevel != CompressionLevelNone {
		compression = CompressionZstd
		// Map compression level to zstd level
		var zstdLevel zstd.EncoderLevel
		switch compressionLevel {
		case CompressionLevelFast:
			zstdLevel = zstd.SpeedFastest
		case CompressionLevelDefault:
			zstdLevel = zstd.SpeedDefault
		case CompressionLevelBest:
			zstdLevel = zstd.SpeedBestCompression
		default:
			zstdLevel = zstd.SpeedDefault
		}
		enc, err := zstd.NewWriter(w, zstd.WithEncoderLevel(zstdLevel))
		if err != nil {
			return fmt.Errorf("create zstd encoder: %w", err)
		}
		zstdWriter = enc
		dataWriter = enc
	}

	// Write header.
	if err := binary.Write(w, binary.BigEndian, uint32(MagicNumber)); err != nil {
		if zstdWriter != nil {
			_ = zstdWriter.Close()
		}
		return fmt.Errorf("write magic: %w", err)
	}
	if err := binary.Write(w, binary.BigEndian, int16(world.Version)); err != nil {
		if zstdWriter != nil {
			_ = zstdWriter.Close()
		}
		return fmt.Errorf("write version: %w", err)
	}
	if err := binary.Write(w, binary.BigEndian, uint8(compression)); err != nil {
		if zstdWriter != nil {
			_ = zstdWriter.Close()
		}
		return fmt.Errorf("write compression: %w", err)
	}
	// Placeholder for uncompressed data length (decoder does not validate).
	if err := writeVarInt(w, 0); err != nil {
		if zstdWriter != nil {
			_ = zstdWriter.Close()
		}
		return fmt.Errorf("write data length: %w", err)
	}

	// Stream world data.
	// 1) Fixed world header (min/max sections, user data, chunk count)
	hdr := newBuffer()
	hdr.WriteInt32(world.MinSection)
	hdr.WriteInt32(world.MaxSection)
	hdr.WriteBytes(world.UserData)
	chunks := world.Chunks()
	hdr.WriteVarInt(int64(len(chunks)))
	if _, err := dataWriter.Write(hdr.Bytes()); err != nil {
		if zstdWriter != nil {
			_ = zstdWriter.Close()
		}
		return fmt.Errorf("write world header: %w", err)
	}

	// 2) Each chunk in sequence
	for _, c := range chunks {
		cb := newBuffer()
		encodeChunk(cb, c, world.MinSection, world.MaxSection)
		if _, err := dataWriter.Write(cb.Bytes()); err != nil {
			if zstdWriter != nil {
				_ = zstdWriter.Close()
			}
			return fmt.Errorf("write chunk (%d,%d): %w", c.X, c.Z, err)
		}
	}

	// Finalize compression stream, if any.
	if zstdWriter != nil {
		if err := zstdWriter.Close(); err != nil {
			return fmt.Errorf("close zstd stream: %w", err)
		}
	}
	return nil
}
