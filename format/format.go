// Package format implements the Pile v2 world file format: a compact,
// deterministic, single-file world container designed around dragonfly's
// chunk types.
package format

import (
	"errors"
	"fmt"

	"github.com/cespare/xxhash/v2"
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

// checkpointHash authenticates a file's semantic header and footer control
// words alongside its stored payload. Hashing the payload alone would leave
// blockVersion, the feature flags (which carry the default-biome reference)
// and the indexed control fields unprotected: a single flipped bit there
// changes how the payload decodes while every integrity check still passes.
//
// preimage = header (16 bytes) || stored payload || footer bytes 8..end
// (the control words and magic, i.e. everything but the hash field itself).
func checkpointHash(header, payload, footerTail []byte) uint64 {
	h := xxhash.New()
	_, _ = h.Write(header)
	_, _ = h.Write(payload)
	_, _ = h.Write(footerTail)
	return h.Sum64()
}

// CheckpointHash exposes the file authentication hash so tooling (and tests
// that rewrite files) can recompute it: it covers the header, the stored
// payload and the footer's control words.
func CheckpointHash(header, payload, footerTail []byte) uint64 {
	return checkpointHash(header, payload, footerTail)
}

// corruptf returns an error wrapping ErrCorrupt with a formatted description.
func corruptf(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrCorrupt}, args...)...)
}

// Decoder limits. These are validity rules: a file exceeding them is
// rejected, so they can only ever be raised by a format revision. They are
// therefore set at what the underlying models can represent rather than at
// what deployments are expected to use. Allocation safety comes from bounding
// every count by the input bytes that remain (see reader.count and the
// per-structure checks), not from these ceilings.
const (
	maxStringLen = 64 << 10
	maxBlobLen   = 16 << 20
	// maxChunks bounds chunk records in a solid body and entries in an
	// indexed directory. 2^32 chunks is 2^36 blocks square: far beyond any
	// reachable world, and not a limit a growing server can hit.
	maxChunks  = 1 << 32
	maxPalette = 1 << 20
	maxBlobs   = 1 << 24
	// maxPerChunk bounds entities, block entities and scheduled ticks per
	// chunk independently.
	maxPerChunk = 1 << 20
	// maxSectionCnt covers the full int16 block-Y domain dragonfly can
	// address (-32768..32767), which is 4096 sections.
	maxSectionCnt = 4096
	// maxLayers matches Bedrock's sub chunk storage count, which is encoded
	// as a byte on disk.
	maxLayers = 256
	// maxDirEntries bounds an indexed directory. The directory is one frame
	// and every entry costs at least a few bytes, so the ceiling that the
	// design can actually reach is set by the directory decode limit, not by
	// maxChunks: advertising more would be a promise the layout cannot keep.
	maxDirEntries = 1 << 22
	// maxStructureSize bounds one dimension of a structure in blocks. The
	// decoder enforces it per component, so the writer must use the same
	// value or it can emit files it cannot read back.
	maxStructureSize = 1 << 20
	// maxStructureCells bounds a structure's 16-cube cell grid. Unlike a
	// world, a structure is a single in-memory object, so this is both a
	// wire validity rule and an allocation bound: 2^20 cells spans a
	// 1024-block cube.
	maxStructureCells = 1 << 20
	// maxFrameLen is the largest stored frame an indexed file can reference.
	// Lengths are held in a uint32, so 2^32-1 is the representable maximum.
	maxFrameLen = 1<<32 - 1
	// lightArrayLen is the byte length of one 16-cube light nibble array.
	lightArrayLen = 2048
)
