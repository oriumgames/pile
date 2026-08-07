// Package pile provides a compact single-file world provider for dragonfly
// built on the pile v2 format.
package pile

import (
	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
	"github.com/google/uuid"
	"github.com/oriumgames/pile/format"
)

// Compression levels, re-exported from the format package.
const (
	CompressionNone    = format.CompressionNone
	CompressionFast    = format.CompressionFast
	CompressionDefault = format.CompressionDefault
	CompressionBest    = format.CompressionBest
)

// SkipMask selects data categories to drop when storing columns.
type SkipMask uint32

const (
	// SkipEntities drops all entities.
	SkipEntities SkipMask = 1 << iota
	// SkipBlockEntities drops all block entities.
	SkipBlockEntities
	// SkipScheduledTicks drops all scheduled block updates.
	SkipScheduledTicks
	// SkipBiomes stores no biome data; loading yields biome 0 everywhere.
	SkipBiomes
	// SkipChunkUserData drops per-chunk user data.
	SkipChunkUserData
)

// SpawnStore is an optional external store for player spawn positions. Pile
// keeps no player data in its files; without a SpawnStore the provider
// reports no spawn for every player.
type SpawnStore interface {
	LoadPlayerSpawn(id uuid.UUID) (pos cube.Pos, exists bool, err error)
	SavePlayerSpawn(id uuid.UUID, pos cube.Pos) error
}

// Option configures a Provider.
type Option func(*config)

type config struct {
	readOnly     bool
	appendMode   bool
	storeLight   bool
	fastSave     bool
	cacheColumns int
	compression  format.CompressionLevel
	registry     world.BlockRegistry
	spawns       SpawnStore

	maxDecoded int64

	skip      SkipMask
	loadSkip  SkipMask
	filterEnt func(chunk.Entity) bool
	filterBE  func(chunk.BlockEntity) bool
	filterCol func(world.ChunkPos, world.Dimension) bool
}

func defaultConfig() config {
	return config{compression: format.CompressionBest, registry: world.DefaultBlockRegistry}
}

// readOpts translates the provider's decode policy into format ReadOptions.
// It returns nil when there is no policy to state, so the default path calls
// the readers with no options at all rather than with an option carrying the
// zero value — the two are defined to mean the same thing, and this way the
// default does not depend on that definition holding.
func (c config) readOpts() []format.ReadOption {
	if c.maxDecoded <= 0 {
		return nil
	}
	return []format.ReadOption{format.MaxDecodedBytes(c.maxDecoded)}
}

// ReadOnly opens the provider in read-only mode: all mutating operations are
// no-ops and the files are never written.
func ReadOnly() Option { return func(c *config) { c.readOnly = true } }

// StoreLight stores baked light data in saved files. Never needed for
// correctness (dragonfly recalculates light on chunk load regardless); useful
// only for consumers that skip that recalculation.
func StoreLight() Option { return func(c *config) { c.storeLight = true } }

// FastSaves compresses saves with multiple threads. Saves get faster; solid
// files are no longer byte-deterministic across runs, which weakens the
// "identical content = identical file" property.
func FastSaves() Option { return func(c *config) { c.fastSave = true } }

// CacheColumns keeps up to n decoded columns per dimension in an LRU cache
// (append mode only), with Morton-neighbour readahead. 0 disables caching.
func CacheColumns(n int) Option { return func(c *config) { c.cacheColumns = n } }

// AppendMode opens the world as indexed/append files instead of solid ones:
// stores append immediately, saves are cheap checkpoints, columns are decoded
// on demand and memory stays at directory-plus-palette level. Use for large
// or frequently saved worlds. Garbage accumulates between compactions;
// Close compacts automatically past a garbage threshold. Solid files cannot
// be opened in append mode (convert with `pile mode` first) and vice versa.
func AppendMode() Option { return func(c *config) { c.appendMode = true } }

// Compression sets the zstd level used for saves. Default: CompressionBest.
func Compression(level format.CompressionLevel) Option {
	return func(c *config) { c.compression = level }
}

// Registry sets the block registry used for encoding and decoding. It is
// finalized on open. Default: world.DefaultBlockRegistry.
func Registry(reg world.BlockRegistry) Option {
	return func(c *config) {
		if reg != nil {
			c.registry = reg
		}
	}
}

// MaxDecodedBytes bounds the live decoded state a world file may produce, in
// bytes. 0 (the default) is the format's own ceiling, which is where §8 of the
// specification sets it: near what the format can represent, not near what a
// deployment wants to spend. A legal 1,161-byte file decodes into 1.12 GiB, and
// the ceiling above it is four times higher again.
//
// Set it when the worlds being opened are not worlds you wrote. A value above
// the format's ceiling is clamped down to it, so this can tighten the limit and
// cannot loosen it.
//
// A file refused under this ceiling is not a corrupt file. The error satisfies
// errors.Is(err, format.ErrDecodeBudget) and deliberately does not satisfy
// errors.Is(err, format.ErrCorrupt): it says the file is larger than this
// provider was told to decode, not that anything is wrong with it.
//
// In append mode the limit is per open world file rather than per column read:
// it covers the directory the file keeps resident plus one decoded column.
func MaxDecodedBytes(n int64) Option { return func(c *config) { c.maxDecoded = n } }

// WithSpawnStore plugs an external player spawn store into the provider.
func WithSpawnStore(s SpawnStore) Option { return func(c *config) { c.spawns = s } }

// Skip drops whole data categories when columns are stored. Dropped data
// never reaches the file, so output stays deterministic regardless of the
// filtered content.
func Skip(m SkipMask) Option { return func(c *config) { c.skip |= m } }

// LoadSkip drops data categories when columns are loaded, even if present in
// the file. Useful for template worlds whose entities are spawned by code.
func LoadSkip(m SkipMask) Option { return func(c *config) { c.loadSkip |= m } }

// FilterEntity keeps only entities for which f returns true when storing.
func FilterEntity(f func(chunk.Entity) bool) Option {
	return func(c *config) { c.filterEnt = f }
}

// FilterBlockEntity keeps only block entities for which f returns true when
// storing.
func FilterBlockEntity(f func(chunk.BlockEntity) bool) Option {
	return func(c *config) { c.filterBE = f }
}

// FilterColumn refuses entire columns when storing: columns for which f
// returns false are silently not persisted.
func FilterColumn(f func(world.ChunkPos, world.Dimension) bool) Option {
	return func(c *config) { c.filterCol = f }
}
