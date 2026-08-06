// Package format implements the Pile v2 world file format: a compact,
// deterministic, single-file world container designed around dragonfly's
// chunk types.
package format

import (
	"errors"
	"fmt"
)

// File magics. Stored as raw byte sequences, not integers, so the bytes "PILE"
// and "ELIP" appear literally at the start and end of every file.
var (
	headerMagic = [4]byte{'P', 'I', 'L', 'E'}
	footerMagic = [4]byte{'E', 'L', 'I', 'P'}
)

const (
	// Version is the pile format version written by this package.
	Version = 2

	headerSize = 16
	footerSize = 44
)

// File kinds.
const (
	KindWorld     = 0
	KindStructure = 1
)

// Body modes.
const (
	ModeSolid   = 0
	ModeIndexed = 1
)

// Header feature flags.
const (
	// FlagStoreLight indicates chunk records include baked light nibbles.
	// Light is never required for correctness; dragonfly recalculates light on
	// load.
	FlagStoreLight uint32 = 1 << 0
	// FlagStats indicates the meta block includes a stats compound.
	FlagStats uint32 = 1 << 1
	// Bit 2 is reserved: dictionary presence is signalled by the directory,
	// so the bit carries no meaning and MUST be zero. Accepting it would
	// spend the bit, since an old reader could not tell that a future file
	// using it needs a feature the reader lacks.
	// FlagDefaultBiome indicates bits 16-31 of the flags field hold a global
	// biome palette reference used as the world default. Sections whose biomes
	// are uniformly the default are not stored.
	FlagDefaultBiome uint32 = 1 << 3
	// FlagUncompressed indicates the body is stored without zstd compression.
	FlagUncompressed uint32 = 1 << 4

	// knownFlags is the set of flags this version understands. Unknown flag
	// bits cause decoding to fail: they may change the meaning of the payload.
	knownFlags = FlagStoreLight | FlagStats | FlagDefaultBiome |
		FlagUncompressed | 0xFFFF0000 // high 16 bits: default biome reference

	defaultBiomeShift = 16
)

// CompressionLevel selects the zstd effort used when writing a file.
type CompressionLevel int

const (
	// CompressionNone stores the body uncompressed.
	CompressionNone CompressionLevel = iota
	// CompressionFast uses zstd's fastest level.
	CompressionFast
	// CompressionDefault uses zstd's default level.
	CompressionDefault
	// CompressionBest uses zstd's best-ratio level. Solid saves are rare and
	// small; this is the recommended default for authored maps.
	CompressionBest
)

// Options configures WriteWorld.
type Options struct {
	// Compression selects the zstd level. The zero value is CompressionNone;
	// callers typically want CompressionBest.
	Compression CompressionLevel
	// SkipBiomes omits all biome data. Decoders yield biome 0 everywhere.
	SkipBiomes bool
	// StoreLight stores baked light nibbles per present section. Never needed
	// for correctness: dragonfly recalculates light on chunk load regardless,
	// so this only benefits consumers that skip that recalculation.
	StoreLight bool
	// Stats embeds a stats NBT compound in the meta block, readable via
	// ReadMeta without decoding chunk data.
	Stats bool
	// FastCompression compresses with multiple threads. Faster saves, but the
	// output is no longer byte-deterministic across runs.
	FastCompression bool
	// Workers bounds the number of goroutines used for parallel column
	// encode/decode. 0 uses GOMAXPROCS; 1 forces serial operation.
	Workers int
}

// Sentinel errors. Decoding errors wrap ErrCorrupt unless stated otherwise.
var (
	ErrCorrupt            = errors.New("pile: corrupt file")
	ErrUnsupportedVersion = errors.New("pile: unsupported format version")
	ErrUnsupportedMode    = errors.New("pile: unsupported body mode")
	ErrUnknownFlags       = errors.New("pile: unknown required feature flags")
	ErrChecksum           = errors.New("pile: body checksum mismatch")
)

// corruptf returns an error wrapping ErrCorrupt with a formatted description.
func corruptf(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrCorrupt}, args...)...)
}

// Decoder limits. These bound allocations while reading untrusted files.
const (
	maxStringLen  = 64 << 10
	maxBlobLen    = 16 << 20
	maxChunks     = 1 << 20
	maxPalette    = 1 << 20
	maxBlobs      = 1 << 24
	maxPerChunk   = 1 << 16 // entities, block entities, scheduled ticks each
	maxSectionCnt = 512
	maxLayers     = 4
	// maxFrameLen bounds any single stored frame in an indexed file.
	maxFrameLen = 1 << 32
)
