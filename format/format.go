// Package format implements the Pile v2 world file format: a compact,
// deterministic, single-file world container designed around dragonfly's
// chunk types.
package format

import (
	"errors"
	"fmt"

	"github.com/cespare/xxhash/v2"
	"github.com/df-mc/dragonfly/server/world"
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

	// dimensionMask covers bits 5-7, which name the dimension the file
	// describes. A world file is one dimension, and nothing else in the file
	// says which: without this the answer lives in the file name or in
	// unconstrained settings NBT, where two implementations spell it
	// differently and neither round trips. Three bits cost nothing now and
	// cannot be added after the format is frozen.
	dimensionMask  = uint32(0b111) << dimensionShift
	dimensionShift = 5

	// knownFlags is the set of flags this version understands. Unknown flag
	// bits cause decoding to fail: they may change the meaning of the payload.
	knownFlags = FlagStoreLight | FlagStats | FlagDefaultBiome |
		FlagUncompressed | dimensionMask | 0xFFFF0000 // high 16 bits: default biome reference

	defaultBiomeShift = 16
)

// Dimension names the dimension a world file describes. It is stored in flag
// bits 5-7, so Overworld is zero and a file that never thought about
// dimensions is an overworld, which is what such a file has always been.
type Dimension uint8

const (
	Overworld Dimension = 0
	Nether    Dimension = 1
	End       Dimension = 2
	// Values 3-7 are reserved and MUST NOT be written. Readers reject them
	// rather than ignoring them, so the encoding stays available.
	maxDimension = End
)

// DimensionOf maps a dragonfly dimension onto the header field. Custom
// dimensions have no encoding of their own, so they record themselves as the
// overworld and rely on their file name, which is what identified them before
// the field existed.
//
// It lives here rather than in a caller so that every writer answers the
// question the same way: a world file names its dimension, and a path that
// forgets to set it silently claims to be the overworld.
func DimensionOf(dim world.Dimension) Dimension {
	id, _ := world.DimensionID(dim)
	switch id {
	case 1:
		return Nether
	case 2:
		return End
	}
	return Overworld
}

func (d Dimension) String() string {
	switch d {
	case Overworld:
		return "overworld"
	case Nether:
		return "nether"
	case End:
		return "end"
	}
	return fmt.Sprintf("dimension(%d)", uint8(d))
}

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
	// Dimension names the dimension an indexed world describes. Solid writes
	// take it from WorldData.Dimension instead, where it round trips with the
	// rest of the world; this field is read only by CreateIndexed, which has
	// no WorldData to take it from.
	Dimension Dimension
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
	// maxStringLen is the largest an NBT string length can express. The
	// format's own string primitive is bounded by the same number so one
	// concept has one ceiling rather than two that differ by a byte.
	maxStringLen = 1<<16 - 1
	maxBlobLen   = 16 << 20
	// maxChunks bounds chunk records in a solid body and entries in an
	// indexed directory. It is the largest value a u32 holds, so a reader that
	// keeps the count in one cannot be handed a legal file it must truncate.
	// 2^32-1 chunks is 2^36 blocks square: far beyond any reachable world, and
	// not a limit a growing server can hit.
	maxChunks  = 1<<32 - 1
	maxPalette = 1 << 20
	maxBlobs   = 1 << 24
	// maxPerChunk bounds entities, block entities and scheduled ticks per
	// chunk independently.
	maxPerChunk = 1 << 20
	// maxSectionCnt covers the full int16 block-Y domain dragonfly can
	// address (-32768..32767), which is 4096 sections.
	maxSectionCnt = 4096
	// minSectionIdx and maxSectionIdx bound a chunk's vertical placement.
	// Block Y is an int16 everywhere in the chunk API, so section indices run
	// -2048..2047 and a record naming anything outside that describes blocks
	// no implementation can address. The count limit alone does not catch it:
	// a one-section chunk based at 2^40 is small and still unrepresentable.
	minSectionIdx = -2048
	maxSectionIdx = 2047
	// maxDecodedStorages bounds how many paletted storages one file may decode
	// into. Every stored layer costs a single blob reference on the wire and
	// about a hundred bytes of live objects, so the byte ceilings bound the
	// input without bounding the result: a chunk may declare 4096 sections of
	// 255 layers each, and a few hundred kilobytes of repeated references then
	// materialise tens of gigabytes. A real 10,000-chunk overworld uses about
	// half a million, so this leaves an order of magnitude of headroom.
	maxDecodedStorages = 1 << 22
	// maxNBTElements bounds how many values one NBT blob may decode into. The
	// structural walk bounds declared lengths against the bytes that remain,
	// which is enough for arrays but not for lists of compounds: an empty
	// compound costs one byte and allocates a map, so a 16 MiB blob can ask
	// for sixteen million of them.
	maxNBTElements = 1 << 20
	// maxPrealloc caps any capacity hint taken from input. Bounding a count by
	// the bytes that remain is not enough on its own when the decoded element
	// is much larger than the bytes that produce it: the guard has to bound the
	// allocation, not the count. Growing past this costs one reallocation.
	maxPrealloc = 4096
	// maxLayers is one below Bedrock's byte-encoded sub chunk storage count.
	// Dragonfly addresses a layer with a uint8 and grows its storage slice
	// with `for uint8(len(storages)) <= layer`, so a sub chunk holding 256
	// layers makes that comparison wrap to zero and append without end. A
	// 256th layer is therefore not merely unusual, it is unreachable: nothing
	// can read it back and any write to the sub chunk hangs. Accepting one
	// from a file would hand a hostile world a way to wedge the server, so the
	// limit is what can actually be addressed.
	maxLayers = 255
	// MaxLayers is maxLayers for callers outside this package: a mover or an
	// extractor that walks layers has to walk all of them, and a lower cap of
	// its own would silently drop everything above it.
	MaxLayers = maxLayers
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
