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

	// FrozenVersion is the format version whose bytes are frozen. While
	// Version equals it, the bytes a writer produces for given content are
	// fixed and may not move: the golden suite and both conformance vector
	// suites refuse to regenerate their fixtures at all, and -format-change
	// does not lift that refusal.
	//
	// Moving the bytes therefore takes three steps, in this order:
	//
	//  1. set Version to the next number (leave FrozenVersion where it is,
	//     which is what lifts the lock: a version that is not the frozen one
	//     may still move);
	//  2. make the change and re-run the fixture suites with -update, which
	//     rewrites the goldens, the vectors and their manifests;
	//  3. freeze the new version by setting FrozenVersion to it as well.
	//
	// Between steps 1 and 3 the format is unfrozen and the older guard
	// applies instead: -update refuses to bless a changed fixture without
	// -format-change. There is no forward-compatibility lane (format.md
	// §2.1), so a reader of one version refuses another version's files.
	FrozenVersion = 2

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

	// ErrDecodeBudget reports that a decode was stopped because it would have
	// passed the ceiling the caller set with MaxDecodedBytes.
	//
	// It deliberately does **not** wrap ErrCorrupt, which is the one documented
	// exception to the sentence above. Every other decode error is the reader
	// saying the file is invalid; this one is the reader saying the file is
	// larger than this caller is willing to decode, which is a statement about
	// the caller and not about the file. A file refused under it may be
	// perfectly conforming, and another caller — or the same caller with a
	// wider ceiling — will decode it. Callers that quarantine or delete files
	// on ErrCorrupt must not do so on this.
	ErrDecodeBudget = errors.New("pile: decode exceeds the caller's budget")
)

// ReadOption configures a decode. Options are caller policy, never format:
// passing none decodes exactly what §8 permits, byte for byte and file for
// file, which is what every reader did before options existed.
type ReadOption func(*readConfig)

type readConfig struct {
	// maxDecodedBytes is the caller's ceiling in decoded bytes. Zero (the zero
	// value, and so the default) means §8's own.
	maxDecodedBytes int64
}

// MaxDecodedBytes bounds the live decoded state one decode may produce, in
// bytes, measured under the cost model of columnBytes and storageBytes below.
//
// It exists because §8's ceilings are set at what the format can represent and
// not at what a deployment wants to spend: a legal 1,161-byte file decodes into
// 1.12 GiB of columns, and the §8 column ceiling sits four times higher again.
// No single constant serves both a lobby server opening maps it did not write
// and an operator storing a genuine four-million-column world, so the choice
// belongs to the caller.
//
//   - n <= 0 selects §8's own ceiling, which is the default and is exactly the
//     behaviour of a reader that never heard of this option.
//   - Any other value is **clamped downward** to §8's ceiling. A caller can
//     tighten the limit and cannot loosen it: a reader that accepted what a
//     conforming reader must refuse would fork the format just as surely as one
//     that refused what it must accept.
//
// A decode stopped by this ceiling fails with ErrDecodeBudget, which does not
// wrap ErrCorrupt. The file is not being called invalid.
//
// What it charges is decoded columns and decoded section storages — the two
// quantities §8 bounds by count and the two that dominate a decode's live
// footprint. It does not charge transient allocation inside an NBT blob, which
// has its own §8 ceiling (containers per blob) and is released before the
// decode returns. ReadMeta decodes neither columns nor storages, so it accepts
// the option for uniformity and the ceiling cannot bind there.
func MaxDecodedBytes(n int64) ReadOption {
	return func(c *readConfig) { c.maxDecodedBytes = n }
}

func newReadConfig(opts []ReadOption) readConfig {
	var c readConfig
	for _, o := range opts {
		if o != nil {
			o(&c)
		}
	}
	return c
}

// decodedByteCeiling resolves the caller's ceiling against §8's, which is the
// one direction the clamp works in.
func (c readConfig) decodedByteCeiling() int64 {
	if c.maxDecodedBytes <= 0 || c.maxDecodedBytes > decodedBytesCeiling {
		return decodedBytesCeiling
	}
	return c.maxDecodedBytes
}

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
	// maxChunks bounds the columns one file decodes into: chunk records in a
	// solid body, entries in an indexed directory. It used to be 2^32-1, the
	// largest value a u32 holds, which bounded the field width and not the
	// decode: a chunk record can be eleven bytes and need mark no section
	// present, so a 1,161-byte file declared a million of them and decoded into
	// 1.12 GiB
	// of live columns, and the ceiling itself allowed forty-eight thousand
	// times that before the 512 MiB body limit bound instead. 2^22 is where a
	// column count stops being decodable at all rather than merely large: a
	// solid file holds every column at once by design, so 4,194,304 of them is
	// already about four gigabytes of live objects, and an indexed directory
	// has been capped at the same number all along (maxDirEntries), so the two
	// modes now state one world-size ceiling instead of two that differ by
	// three orders of magnitude. It is 2048x2048 chunks, or 32,768 blocks
	// square, against roughly ten thousand chunks for a real overworld.
	//
	// It is also the number a column already costs elsewhere: every column
	// holding a single block consumes one of the 4,194,304 decoded storages of
	// maxDecodedStorages, so a world with more content-bearing columns than
	// this was already invalid. What this ceiling adds is the empty ones.
	maxChunks  = 1 << 22
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
	// maxDictLen bounds a stored dictionary. The window ceiling of §2.5 does
	// not cover it: a decoder retains a dictionary's whole content, outside
	// the window, and pins the backing array it was handed. Compaction trains
	// dictionaries capped at 16 KiB, so a legitimate one is far below this,
	// and a frame ceiling of 64 MiB would otherwise let a file pin that much
	// per open handle in both an encoder and a decoder.
	maxDictLen = 1 << 20
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
	// maxDirEntries bounds an indexed directory: the same column ceiling as
	// maxChunks, under the name the directory code reads it by. The two were
	// separate numbers while a solid body's ceiling was the width of a u32;
	// they are one ceiling now, and the alias stays only so a reader of
	// finishDirectory sees which limit is being applied.
	maxDirEntries = maxChunks
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

// The cost model behind MaxDecodedBytes. These are not validity rules and not
// wire constants: they convert the two quantities §8 bounds by count into the
// bytes a caller actually cares about, so that one number can express a policy
// over both. They are rounded, and deliberately so — a budget is a policy dial,
// not a measurement, and a caller that needs a byte-exact accounting of a
// decode should measure its own process rather than trust a per-unit constant.
const (
	// columnBytes is what one decoded column costs in live objects: a recRaw, a
	// chunk.Chunk and a Column. SECURITY.md measures 1,048,576 empty columns at
	// 1.12 GiB retained, which is about 1,150 bytes each; §8 rounds the same
	// figure to "about a kilobyte".
	columnBytes = 1024
	// storageBytes is what one decoded section storage costs. §8's note on
	// maxDecodedStorages puts a stored layer at "about a hundred bytes of live
	// objects".
	storageBytes = 128
	// decodedBytesCeiling is the most a decode can cost under this model while
	// still obeying §8, and so the value a caller's ceiling is clamped to.
	//
	// The trailing columnBytes is one column of headroom, and it is load
	// bearing rather than decorative. An indexed handle spends this ceiling
	// once on its directory and then gives each record decode whatever is left
	// (see IndexedWorld.recordBudget), so without the headroom a directory at
	// the entry ceiling would leave a remainder one column short of what §8
	// still permits a single record to reach. The default ceiling has to be
	// unreachable, not nearly unreachable: the zero value must accept exactly
	// the files a reader without this option accepts.
	decodedBytesCeiling = int64(maxChunks)*columnBytes + int64(maxDecodedStorages)*storageBytes + columnBytes
)
