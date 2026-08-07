package format

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/cespare/xxhash/v2"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
	"github.com/klauspost/compress/dict"
	"github.com/klauspost/compress/zstd"
)

// Dictionary training thresholds: below these, a shared dictionary is not
// worth its overhead.
const (
	dictMinSamples = 16
	dictMinBytes   = 64 << 10
)

// ErrNoColumn is returned by IndexedWorld.Column for positions that have no
// stored column.
var ErrNoColumn = errors.New("pile: no column at position")

// IndexedWorld is an append-only, random-access pile world file for large
// worlds. Records are self-contained per-chunk zstd frames referencing global
// palette segments; a checkpoint appends pending palette segments, updated
// metadata, a directory and a footer. Until a checkpoint's footer is fully
// written, the previous checkpoint remains authoritative, so torn writes lose
// at most the work since the last checkpoint.
//
// Unlike solid mode, indexed files are not deterministic (they are
// history-dependent) and do not deduplicate sections across chunks: the
// trade for O(1) column access with directory-plus-palette memory. Compact
// rewrites the live set into a fresh, garbage-free file.
type IndexedWorld struct {
	mu   sync.Mutex
	f    *os.File
	path string
	reg  world.BlockRegistry
	opts Options

	// hdrBytes is the file's 16-byte header, authenticated by every
	// checkpoint hash.
	hdrBytes []byte

	readOnly   bool
	closed     bool
	end        int64
	generation uint64
	compressed bool
	// headerDamaged records that the physical header disagreed with the
	// directory prologue, so the file was opened from the directory's copy.
	headerDamaged bool
	// headerFlags and headerBlockVersion mirror the file header. Each
	// checkpoint records them, so recovery can validate (and report) a
	// header that no longer matches the checkpoints it belongs to.
	headerFlags        uint32
	headerBlockVersion int32
	// prevFooterOff is the offset of the newest durable footer, chained into
	// the next footer for recovery sanity checks.
	prevFooterOff int64

	// Shared-dictionary state (built at Compact; nil = no dictionary).
	dict    []byte
	dictRef frameRef
	dictEnc *zstd.Encoder
	dictDec *zstd.Decoder

	// Directory: live record frames by chunk position.
	dir       map[[2]int32]dirEntry
	liveBytes int64

	// dirRef is the current directory frame (live overhead, not garbage).
	dirRef frameRef

	// Metadata blobs, duplicated into a meta frame on checkpoint.
	metaRef            frameRef
	settings, userData []byte
	markers, border    []byte
	// paletteErr holds the first rejected palette entry seen while resolving
	// a column, since the palette callbacks cannot return an error.
	paletteErr error
	metaDirty  bool

	// Global block palette: segments on disk plus pending entries.
	blockSegs       []frameRef
	blockIdx        map[uint32]uint32
	stateIdx        map[string]uint32 // canonical state string → index (unknown states)
	rids            []uint32          // palette index → runtime ID
	unknown         []int32           // palette index → UnknownStates index or -1
	unkStates       []BlockState
	identity        []uint32 // identity remap for canonicalBlob
	pendingBlock    writer
	pendingBlkN     int
	pendingOverride []paletteOverride

	// Global biome palette.
	biomeSegs    []frameRef
	biomeIdx     map[string]uint32
	biomeIDs     []uint32
	biomeUnknown []int32
	biomeUnkName []string
	biomeNames   []string
	biomeIdent   []uint32
	pendingBiome writer
	pendingBioN  int

	recordsDirty bool
	// recovered records that the newest checkpoint did not validate and an
	// older one was adopted: content stored since then is gone, which a
	// caller should be able to see rather than infer.
	recovered bool
	// footerPending marks a footer that was written but whose durability
	// sync has not succeeded yet, so a retried checkpoint re-syncs instead of
	// reporting success. pendingFooterOff is that footer's offset.
	footerPending    bool
	pendingFooterOff int64
}

// frameRef locates a stored frame and authenticates it: every frame the
// directory references carries its own hash, so corruption in a palette
// segment, the metadata or the dictionary is detected rather than silently
// changing world content.
// buildDictionary trains a shared compression dictionary. The upstream
// builder panics rather than erroring on some sample sets (highly repetitive
// input, for instance), and compaction must never take a server down, so the
// panic is contained and treated as "no dictionary": the file is simply
// compacted without one.
func buildDictionary(samples [][]byte, level CompressionLevel) (d []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			d, err = nil, fmt.Errorf("pile: dictionary training failed: %v", r)
		}
	}()
	return dict.BuildZstdDict(samples, dict.Options{
		MaxDictSize: 16 << 10,
		HashBytes:   6,
		ZstdDictID:  0x504C4531, // fixed for reproducible compaction
		ZstdLevel:   zstdLevel(level),
	})
}

// paletteOverride records a pending palette entry whose block version
// differs from the segment's own.
type paletteOverride struct {
	index   uint64
	version int32
}

type frameRef struct {
	off    int64
	length uint32
	hash   uint64
}

type dirEntry struct {
	off    int64
	length uint32
	hash   uint64
}

// CreateIndexed creates a new indexed world file at path. The file is valid
// (empty, checkpointed) when CreateIndexed returns.
func CreateIndexed(path string, reg world.BlockRegistry, opts Options) (*IndexedWorld, error) {
	// Validate before touching the filesystem: a rejected option must not
	// leave a file behind, least of all an empty one that the next call
	// cannot create over.
	if opts.Dimension > maxDimension {
		return nil, fmt.Errorf("pile: dimension %d is reserved", uint8(opts.Dimension))
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, fmt.Errorf("pile: create indexed world: %w", err)
	}
	w, err := newIndexedWorld(f, path, reg, opts, false)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	w.compressed = opts.Compression != CompressionNone

	hdr := &writer{}
	hdr.raw(headerMagic[:])
	hdr.u16(Version)
	hdr.u8(KindWorld)
	hdr.u8(ModeIndexed)
	flags := uint32(0)
	if !w.compressed {
		flags |= FlagUncompressed
	}
	// Unlike a solid write, this is a layout decision rather than a statement
	// about content: it is fixed here and every record obeys it, because
	// changing it later would mean rewriting every record's light bitset.
	// §2.3 scopes the "clear when nothing carries light" rule to mode 0 for
	// exactly this reason.
	if opts.StoreLight {
		flags |= FlagStoreLight
	}
	flags |= uint32(opts.Dimension) << dimensionShift
	hdr.u32(flags)
	hdr.i32(chunk.CurrentBlockVersion)
	if _, err := f.Write(hdr.bytes()); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("pile: write header: %w", err)
	}
	w.hdrBytes = append([]byte(nil), hdr.bytes()...)
	w.headerFlags, w.headerBlockVersion = flags, chunk.CurrentBlockVersion
	w.end = headerSize
	w.recordsDirty = true // force the initial checkpoint
	if err := w.checkpointLocked(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return w, nil
}

// OpenIndexed opens an existing indexed world file, recovering to the last
// valid checkpoint if the file ends in a torn write.
func OpenIndexed(path string, reg world.BlockRegistry, readOnly bool) (*IndexedWorld, error) {
	mode := os.O_RDWR
	if readOnly {
		mode = os.O_RDONLY
	}
	f, err := os.OpenFile(path, mode, 0)
	if err != nil {
		return nil, err
	}
	w, err := newIndexedWorld(f, path, reg, Options{Compression: CompressionDefault}, readOnly)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := w.load(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return w, nil
}

func newIndexedWorld(f *os.File, path string, reg world.BlockRegistry, opts Options, readOnly bool) (*IndexedWorld, error) {
	reg.Finalize()
	return &IndexedWorld{
		f: f, path: path, reg: reg, opts: opts, readOnly: readOnly,
		dir:      map[[2]int32]dirEntry{},
		blockIdx: map[uint32]uint32{},
		stateIdx: map[string]uint32{},
		biomeIdx: map[string]uint32{},
	}, nil
}

// setDict installs a shared compression dictionary: all frames appended from
// now on are compressed against it, and frames referencing it decode through
// a dictionary-aware decoder.
func (w *IndexedWorld) setDict(dict []byte) error {
	enc, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstdLevel(w.opts.Compression)),
		zstd.WithEncoderConcurrency(1),
		zstd.WithEncoderDict(dict))
	if err != nil {
		return fmt.Errorf("pile: create dict encoder: %w", err)
	}
	dec, err := zstd.NewReader(nil,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxWindow(maxZstdWindow),
		zstd.WithDecoderMaxMemory(maxDecodedFrame),
		zstd.WithDecoderDicts(dict))
	if err != nil {
		enc.Close()
		return fmt.Errorf("pile: create dict decoder: %w", err)
	}
	if w.dictEnc != nil {
		w.dictEnc.Close()
	}
	if w.dictDec != nil {
		w.dictDec.Close()
	}
	w.dict, w.dictEnc, w.dictDec = dict, enc, dec
	return nil
}

// installDict writes a freshly trained dictionary as a frame (compressed
// without itself) and activates it for all subsequent frames.
func (w *IndexedWorld) installDict(d []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	ref, _, err := w.appendFrame(d) // dictEnc is nil here: plain compression
	if err != nil {
		return err
	}
	w.dictRef = ref
	if err := w.setDict(d); err != nil {
		return err
	}
	w.recordsDirty = true
	return nil
}

// HasDict reports whether the file uses a shared compression dictionary.
func (w *IndexedWorld) HasDict() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dict != nil
}

// load reads header, footer (with recovery) and all shared segments.
func (w *IndexedWorld) load() error {
	st, err := w.f.Stat()
	if err != nil {
		return err
	}
	w.end = st.Size()
	if w.end < headerSize+footerSize {
		return corruptf("indexed file too short (%d bytes)", w.end)
	}
	hdr := make([]byte, headerSize)
	if _, err := w.f.ReadAt(hdr, 0); err != nil {
		return err
	}
	if [4]byte(hdr[0:4]) != headerMagic {
		return corruptf("bad header magic")
	}
	if v := binary.LittleEndian.Uint16(hdr[4:6]); v != Version {
		return ErrUnsupportedVersion
	}
	// Magic and version identify the file; the remaining header fields are
	// validated against the directory prologue, which the checkpoint hash
	// authenticates and which therefore survives damage to these 16 bytes.
	w.hdrBytes = append([]byte(nil), hdr...)
	w.headerFlags = binary.LittleEndian.Uint32(hdr[8:12])
	w.headerBlockVersion = int32(binary.LittleEndian.Uint32(hdr[12:16]))
	w.compressed = w.headerFlags&FlagUncompressed == 0

	if err := w.adoptCheckpoint(); err != nil {
		// Only the magic and version have to survive in the physical header,
		// so kind and mode are not consulted until the directory has had its
		// chance. Once recovery has failed they are the best explanation
		// available: a solid or structure file opened as an indexed world has
		// no directory to find, and that is a clearer answer than corruption.
		if hdr[7] == ModeSolid {
			return ErrUnsupportedMode
		}
		if hdr[6] == KindStructure {
			return corruptf("file kind %d is not a world", hdr[6])
		}
		return err
	}
	// The directory prologue is authoritative and has now been applied; make
	// sure the physical header agrees, and record it when it does not.
	if img := headerImage(KindWorld, ModeIndexed, w.headerFlags, w.headerBlockVersion); !bytes.Equal(w.hdrBytes, img) {
		w.headerDamaged = true
		// Adopt the canonical image as the preimage for everything written
		// from here on. The directory prologue is authoritative, so a
		// checkpoint written after recovery must be verifiable from it;
		// hashing against the damaged bytes instead would tie the world to
		// its own corruption and make repairing the header invalidate the
		// newest data.
		w.hdrBytes = img
	}
	w.opts.StoreLight = w.headerFlags&FlagStoreLight != 0
	return nil
}

// headerImage builds the canonical 16-byte header for a set of semantic
// fields. It is the checkpoint hash's header preimage, so a checkpoint can be
// verified from the directory alone.
func headerImage(kind, mode uint8, flags uint32, blockVersion int32) []byte {
	h := &writer{}
	h.raw(headerMagic[:])
	h.u16(Version)
	h.u8(kind)
	h.u8(mode)
	h.u32(flags)
	h.i32(blockVersion)
	return h.bytes()
}

// prologueHeaderImage rebuilds a header image from a stored directory frame's
// prologue, trying the frame both raw and decompressed because the physical
// header (which records whether frames are compressed) may be the damaged
// part.
func prologueHeaderImage(stored []byte) ([]byte, bool) {
	parse := func(b []byte) ([]byte, bool) {
		r := &reader{b: b}
		kind, err := r.u8()
		if err != nil {
			return nil, false
		}
		mode, err := r.u8()
		if err != nil {
			return nil, false
		}
		flags, err := r.u32()
		if err != nil {
			return nil, false
		}
		version, err := r.u32()
		if err != nil {
			return nil, false
		}
		if kind != KindWorld || mode != ModeIndexed {
			return nil, false
		}
		return headerImage(kind, mode, flags, int32(version)), true
	}
	if img, ok := parse(stored); ok {
		return img, true
	}
	body, err := sharedFrameDecoder().DecodeAll(stored, nil)
	if err != nil {
		return nil, false
	}
	return parse(body)
}

// HeaderDamaged reports that the file's physical header did not match the
// semantic fields the directory carries, so the world was opened from the
// directory's authoritative copy. Rewriting the file (Compact) repairs it.
func (w *IndexedWorld) HeaderDamaged() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.headerDamaged
}

// verifyRecords checks the stored hash of every live record frame.
func (w *IndexedWorld) verifyRecords() error {
	for k, e := range w.dir {
		stored := make([]byte, e.length)
		if _, err := w.f.ReadAt(stored, e.off); err != nil {
			return fmt.Errorf("pile: read record (%d,%d): %w", k[0], k[1], err)
		}
		if xxhash.Sum64(stored) != e.hash {
			return fmt.Errorf("%w: record (%d,%d)", ErrChecksum, k[0], k[1])
		}
	}
	return nil
}

// resetLoadedState clears everything loadDirectory populates so a failed
// checkpoint candidate leaves no partial state behind.
func (w *IndexedWorld) resetLoadedState() {
	w.dir = map[[2]int32]dirEntry{}
	w.liveBytes = 0
	w.blockSegs, w.biomeSegs = nil, nil
	w.blockIdx = map[uint32]uint32{}
	w.stateIdx = map[string]uint32{}
	w.biomeIdx = map[string]uint32{}
	w.rids, w.identity = nil, nil
	w.unknown, w.unkStates = nil, nil
	w.biomeIDs, w.biomeNames, w.biomeIdent = nil, nil, nil
	// The unknown-biome mapping is indexed by palette reference, so leaving it
	// behind would reinterpret the next directory's references through the
	// previous one's names.
	w.biomeUnknown, w.biomeUnkName = nil, nil
	w.metaRef, w.dirRef = frameRef{}, frameRef{}
	w.footerPending = false
	w.recovered = false
	w.settings, w.userData, w.markers, w.border = nil, nil, nil, nil
	if w.dictEnc != nil {
		w.dictEnc.Close()
	}
	if w.dictDec != nil {
		w.dictDec.Close()
	}
	w.dict, w.dictRef, w.dictEnc, w.dictDec = nil, frameRef{}, nil, nil
}

// adoptCheckpoint walks footer candidates newest-first and adopts the first
// whose directory (including palette segments, metadata and dictionary)
// validates completely, so a torn or rotten checkpoint falls back to the
// previous one instead of failing the open.
func (w *IndexedWorld) adoptCheckpoint() error {
	cands := w.footerCandidates()
	// Follow each candidate's chain as well: a file whose recent checkpoints
	// all reference a damaged shared frame is still recoverable from an
	// older link, which a bounded scan of the tail would never reach.
	seen := map[int64]bool{}
	for _, c := range cands {
		seen[c.off] = true
	}
	for i := 0; i < len(cands); i++ {
		prev := cands[i].prev
		for prev != 0 && !seen[prev] {
			seen[prev] = true
			c, ok := w.tryFooter(prev)
			if !ok {
				break
			}
			cands = append(cands, c)
			prev = c.prev
		}
	}
	err := error(corruptf("no valid checkpoint footer found"))
	for _, c := range cands {
		w.resetLoadedState()
		if lerr := w.loadDirectory(c.dirRef); lerr != nil {
			err = lerr
			continue
		}
		// A checkpoint is only adopted if every record it references is
		// intact: otherwise a damaged newest checkpoint would be preferred
		// over an older one whose copy of that record is still good.
		if lerr := w.verifyRecords(); lerr != nil {
			err = lerr
			continue
		}
		w.generation = c.gen
		w.dirRef = c.dirRef
		w.prevFooterOff = c.off
		// Recovery means the checkpoint at the end of the file was not the
		// one adopted, whatever the reason: a torn tail, a footer that fails
		// its hash, or a checkpoint whose frames are damaged. Anything else
		// under-reports the loss to the caller.
		w.recovered = c.off != w.end-footerSize
		return nil
	}
	w.resetLoadedState()
	return err
}

// footerCand is a structurally valid footer candidate.
type footerCand struct {
	off    int64
	dirRef frameRef
	gen    uint64
	prev   int64
}

// footerCandidates returns candidates newest-first: the footer at EOF, then
// candidates found scanning backwards (bounded), for torn-write recovery.
func (w *IndexedWorld) footerCandidates() []footerCand {
	var out []footerCand
	seen := map[int64]bool{}
	add := func(off int64) {
		if seen[off] {
			return
		}
		seen[off] = true
		if c, ok := w.tryFooter(off); ok {
			out = append(out, c)
		}
	}
	add(w.end - footerSize)

	const window = 64 << 10
	const maxCandidates = 64
	for hi := w.end; hi > headerSize && len(out) < maxCandidates; {
		lo := max(hi-window, headerSize)
		buf := make([]byte, hi-lo)
		if _, err := w.f.ReadAt(buf, lo); err != nil {
			break
		}
		for i := len(buf) - 4; i >= 0 && len(out) < maxCandidates; i-- {
			if [4]byte(buf[i:i+4]) != footerMagic {
				continue
			}
			// Magic sits at the end of the footer.
			add(lo + int64(i) - (footerSize - 4))
		}
		// Overlap windows by the footer size so a footer spanning the
		// boundary is still found.
		hi = lo + footerSize
		if lo == headerSize {
			break
		}
	}
	return out
}

// tryFooter validates a footer candidate at off: magic, directory bounds and
// hash, and the previous-footer chain field.
func (w *IndexedWorld) tryFooter(off int64) (footerCand, bool) {
	if off < headerSize || off+footerSize > w.end {
		return footerCand{}, false
	}
	buf := make([]byte, footerSize)
	if _, err := w.f.ReadAt(buf, off); err != nil {
		return footerCand{}, false
	}
	if [4]byte(buf[footerSize-4:]) != footerMagic {
		return footerCand{}, false
	}
	wantHash := binary.LittleEndian.Uint64(buf[0:8])
	dirOffU := binary.LittleEndian.Uint64(buf[8:16])
	dirLenU := binary.LittleEndian.Uint64(buf[16:24])
	gen := binary.LittleEndian.Uint64(buf[24:32])
	prevU := binary.LittleEndian.Uint64(buf[32:40])
	// Bounds without overflow: compare by subtraction against the footer's
	// own offset, and cap the frame size explicitly before any allocation.
	if dirOffU > uint64(off) || dirLenU > maxFrameLen || dirLenU == 0 || dirLenU > uint64(off)-dirOffU {
		return footerCand{}, false
	}
	dirOff, dirLen, prev := int64(dirOffU), int64(dirLenU), int64(prevU)
	if dirOff < headerSize {
		return footerCand{}, false
	}
	// Chain sanity: the previous footer must be older than this one (or 0
	// for the first checkpoint).
	if prev != 0 && (prev < headerSize || prev >= off) {
		return footerCand{}, false
	}
	frame := make([]byte, dirLen)
	if _, err := w.f.ReadAt(frame, dirOff); err != nil {
		return footerCand{}, false
	}
	if checkpointHash(w.hdrBytes, frame, buf[8:]) != wantHash {
		// The physical header may be the damaged part: the directory carries
		// its own copy of the semantic fields, so rebuild the preimage from
		// there and retry before rejecting the checkpoint.
		img, ok := prologueHeaderImage(frame)
		if !ok || checkpointHash(img, frame, buf[8:]) != wantHash {
			return footerCand{}, false
		}
	}
	return footerCand{
		off:    off,
		dirRef: frameRef{off: dirOff, length: uint32(dirLen), hash: wantHash},
		gen:    gen,
		prev:   prev,
	}, true
}

// loadDirectory decodes the directory frame and all referenced segments.
func (w *IndexedWorld) loadDirectory(ref frameRef) error {
	body, err := w.readDirFrame(ref)
	if err != nil {
		return fmt.Errorf("pile: read directory: %w", err)
	}
	r := &reader{b: body}
	kind, err := r.u8()
	if err != nil {
		return err
	}
	mode, err := r.u8()
	if err != nil {
		return err
	}
	dirFlags, err := r.u32()
	if err != nil {
		return err
	}
	dirVersion, err := r.u32()
	if err != nil {
		return err
	}
	if kind != KindWorld || mode != ModeIndexed {
		return corruptf("directory describes kind %d mode %d, want world/indexed", kind, mode)
	}
	if dirFlags&^knownFlags != 0 {
		return ErrUnknownFlags
	}
	if dirFlags&(FlagDefaultBiome|FlagStats) != 0 || dirFlags>>defaultBiomeShift != 0 {
		return corruptf("directory flags 0x%08X are not valid for an indexed file", dirFlags)
	}
	if d := Dimension((dirFlags & dimensionMask) >> dimensionShift); d > maxDimension {
		return corruptf("dimension %d is reserved", uint8(d))
	}
	// The directory is authoritative: it is covered by the checkpoint hash,
	// so it survives damage to the 16 physical header bytes.
	w.headerFlags, w.headerBlockVersion = dirFlags, int32(dirVersion)
	w.compressed = dirFlags&FlagUncompressed == 0
	w.opts.StoreLight = dirFlags&FlagStoreLight != 0

	validRef := func(ref frameRef, what string) error {
		if ref.length == 0 {
			// Absent references are canonical: all three fields zero, so
			// equivalent directories cannot differ in bytes or hash.
			if ref.off != 0 || ref.hash != 0 {
				return corruptf("absent %s reference must be all-zero, got off=%d hash=%d", what, ref.off, ref.hash)
			}
			return nil
		}
		if ref.off < headerSize || uint64(ref.length) > maxFrameLen || int64(ref.length) > w.end-ref.off {
			return corruptf("%s frame [%d,+%d) out of file bounds", what, ref.off, ref.length)
		}
		stored := make([]byte, ref.length)
		if _, err := w.f.ReadAt(stored, ref.off); err != nil {
			return corruptf("read %s frame: %v", what, err)
		}
		if xxhash.Sum64(stored) != ref.hash {
			return fmt.Errorf("%w: %s frame at %d", ErrChecksum, what, ref.off)
		}
		return nil
	}

	readRef := func() (frameRef, error) { return parseFrameRef(r) }
	metaRef, err := readRef()
	if err != nil {
		return err
	}
	dictRef, err := readRef()
	if err != nil {
		return err
	}
	w.dictRef = dictRef
	if err := validRef(w.dictRef, "dictionary"); err != nil {
		return err
	}
	// A dictionary only means anything to a compressed file, so an
	// uncompressed one carrying a reference has two ways to say the same
	// thing, and the second one is unreadable.
	if !w.compressed && w.dictRef.length != 0 {
		return corruptf("uncompressed indexed file references a dictionary")
	}
	if w.dictRef.length > 0 {
		// The dictionary frame itself is compressed without the dictionary.
		dict, err := w.readFrame(w.dictRef)
		if err != nil {
			return fmt.Errorf("pile: read dictionary frame: %w", err)
		}
		if err := w.setDict(dict); err != nil {
			return err
		}
	}

	w.metaRef = metaRef
	if err := validRef(w.metaRef, "meta"); err != nil {
		return err
	}
	if w.metaRef.length > 0 {
		mb, err := w.readFrame(w.metaRef)
		if err != nil {
			return fmt.Errorf("pile: read meta frame: %w", err)
		}
		mr := &reader{b: mb}
		s, u, m, b, _, err := readMetaBlobs(mr, 0)
		if err != nil {
			return err
		}
		w.settings, w.userData = cloneBytes(s), cloneBytes(u)
		w.markers, w.border = cloneBytes(m), cloneBytes(b)
	}

	readSegs := func(what string) ([]frameRef, error) {
		return w.parseSegRefs(r, what, validRef)
	}
	if w.blockSegs, err = readSegs("block palette segment"); err != nil {
		return err
	}
	if w.biomeSegs, err = readSegs("biome palette segment"); err != nil {
		return err
	}
	return w.finishDirectory(r)
}

// parseSegRefs reads a palette segment reference list. It is a method rather
// than a closure so the rules it enforces can be exercised directly: repeating
// a reference multiplies its entries into the cumulative palette from a tiny
// file, and the frames have to plausibly fit.
func (w *IndexedWorld) parseSegRefs(r *reader, what string, validRef func(frameRef, string) error) ([]frameRef, error) {
	{
		n, err := r.count(1<<20, "segment")
		if err != nil {
			return nil, err
		}
		if n > r.remaining()/2+1 {
			return nil, corruptf("segment count %d exceeds input", n)
		}
		segs := make([]frameRef, 0, n)
		seen := make(map[frameRef]struct{}, n)
		var total uint64
		for range n {
			off, err := r.uvarint()
			if err != nil {
				return nil, err
			}
			length, err := r.uvarint()
			if err != nil {
				return nil, err
			}
			if length > maxFrameLen {
				return nil, corruptf("%s frame length %d exceeds limit %d", what, length, uint64(maxFrameLen))
			}
			if length == 0 {
				return nil, corruptf("%s frame has zero length", what)
			}
			hb, err := r.take(8)
			if err != nil {
				return nil, err
			}
			ref := frameRef{off: int64(off), length: uint32(length), hash: binary.LittleEndian.Uint64(hb)}
			if err := validRef(ref, what); err != nil {
				return nil, err
			}
			// Repeating a segment reference would multiply its entries into
			// the cumulative palette from a tiny file.
			if _, dup := seen[ref]; dup {
				return nil, corruptf("duplicate %s reference at offset %d", what, ref.off)
			}
			seen[ref] = struct{}{}
			// The stored frames must plausibly fit in the file.
			total += uint64(ref.length)
			if total > uint64(w.end) {
				return nil, corruptf("%s frames total %d bytes, file is %d", what, total, w.end)
			}
			segs = append(segs, ref)
		}
		return segs, nil
	}
}

// finishDirectory reads the chunk entry table that follows the segment lists.
func (w *IndexedWorld) finishDirectory(r *reader) error {

	chunkN, err := r.count(maxDirEntries, "directory chunk")
	if err != nil {
		return err
	}
	var px, pz, poff int64
	var prevKey uint64
	for i := range chunkN {
		dx, err := r.svarint()
		if err != nil {
			return err
		}
		dz, err := r.svarint()
		if err != nil {
			return err
		}
		doff, err := r.svarint()
		if err != nil {
			return err
		}
		length, err := r.uvarint()
		if err != nil {
			return err
		}
		hb, err := r.take(8)
		if err != nil {
			return err
		}
		px, pz, poff = px+dx, pz+dz, poff+doff
		// Accumulate in 64 bits and range-check: narrowing a wrapped sum would
		// let two different byte sequences denote the same chunk position.
		if px < math.MinInt32 || px > math.MaxInt32 || pz < math.MinInt32 || pz > math.MaxInt32 {
			return corruptf("directory entry position (%d,%d) out of int32 range", px, pz)
		}
		// Entries are Morton-ordered and positions are unique, so keys
		// strictly ascend. Anything else is either a duplicate (no defined
		// meaning) or a reordering (a second encoding of one directory).
		if key := mortonKey(int32(px), int32(pz)); i > 0 && key <= prevKey {
			return corruptf("directory entry (%d,%d) is out of order or duplicated", px, pz)
		} else {
			prevKey = key
		}
		if length > maxFrameLen {
			return corruptf("directory entry (%d,%d) length %d exceeds limit %d", px, pz, length, uint64(maxFrameLen))
		}
		e := dirEntry{off: poff, length: uint32(length), hash: binary.LittleEndian.Uint64(hb)}
		if e.off < headerSize || e.off+int64(e.length) > w.end {
			return corruptf("directory entry (%d,%d) out of file bounds", px, pz)
		}
		w.dir[[2]int32{int32(px), int32(pz)}] = e
		w.liveBytes += int64(e.length)
	}
	if r.remaining() != 0 {
		return corruptf("%d trailing bytes in directory", r.remaining())
	}

	// Load palette segments. Each segment records the Minecraft block version
	// its states were written at, so states written before a game upgrade are
	// still upgraded correctly after one.
	for _, seg := range w.blockSegs {
		body, err := w.readFrame(seg)
		if err != nil {
			return fmt.Errorf("pile: read block palette segment: %w", err)
		}
		sr := &reader{b: body}
		segVersion, err := sr.u32()
		if err != nil {
			return err
		}
		segRids, segUnknown, segStates, err := decodeBlockPalette(sr, w.reg, int32(segVersion))
		if err != nil {
			return err
		}
		if len(w.rids)+len(segRids) > maxPalette {
			return corruptf("cumulative block palette exceeds %d entries", maxPalette)
		}
		for i, rid := range segRids {
			idx := uint32(len(w.rids))
			w.rids = append(w.rids, rid)
			w.identity = append(w.identity, idx)
			if segUnknown[i] >= 0 {
				st := segStates[segUnknown[i]]
				if st.Version == 0 {
					st.Version = int32(segVersion)
				}
				w.unknown = append(w.unknown, int32(len(w.unkStates)))
				w.unkStates = append(w.unkStates, st)
				// The same key builder the writer uses: a state stored at the
				// writer's own version and one stored at no version in
				// particular are one entry, and keying them apart here would
				// duplicate it on the next store after a reopen.
				w.stateIdx[preservedStateKey(st.Name, st.Properties, st.Version)] = idx
			} else {
				w.unknown = append(w.unknown, -1)
				if _, ok := w.blockIdx[rid]; !ok {
					w.blockIdx[rid] = idx
				}
				// Index the entry by its state as well as by the runtime ID
				// that produced it. A registry may number one state twice, and
				// after a reopen the other alias would otherwise look new and
				// be appended a second time.
				if name, props, ok := w.reg.RuntimeIDToState(rid); ok {
					key := preservedStateKey(name, props, 0)
					if _, seen := w.stateIdx[key]; !seen {
						w.stateIdx[key] = idx
					}
				}
			}
		}
	}
	for _, seg := range w.biomeSegs {
		body, err := w.readFrame(seg)
		if err != nil {
			return fmt.Errorf("pile: read biome palette segment: %w", err)
		}
		sr := &reader{b: body}
		segIDs, segUnknown, segNames, err := decodeBiomePalette(sr)
		if err != nil {
			return err
		}
		if len(w.biomeIDs)+len(segIDs) > maxPalette {
			return corruptf("cumulative biome palette exceeds %d entries", maxPalette)
		}
		sr2 := &reader{b: body}
		n, err := sr2.count(maxPalette, "biome palette")
		if err != nil {
			return err
		}
		for i := range n {
			name, err := sr2.str()
			if err != nil {
				return err
			}
			idx := uint32(len(w.biomeNames))
			w.biomeNames = append(w.biomeNames, name)
			w.biomeIdent = append(w.biomeIdent, idx)
			w.biomeIDs = append(w.biomeIDs, segIDs[i])
			if segUnknown[i] >= 0 {
				w.biomeUnknown = append(w.biomeUnknown, int32(len(w.biomeUnkName)))
				w.biomeUnkName = append(w.biomeUnkName, segNames[segUnknown[i]])
			} else {
				w.biomeUnknown = append(w.biomeUnknown, -1)
			}
			if _, ok := w.biomeIdx[name]; !ok {
				w.biomeIdx[name] = idx
			}
		}
	}
	return nil
}

// parseFrameRef reads one frame reference from a directory.
//
// The length is checked before it is narrowed to the 32-bit field that carries
// it. A value past that field would otherwise wrap into a small one and be
// accepted, so a file a conforming reader rejects would read here as a
// different file, which is the one failure a bounds check exists to prevent.
func parseFrameRef(r *reader) (frameRef, error) {
	off, err := r.uvarint()
	if err != nil {
		return frameRef{}, err
	}
	length, err := r.uvarint()
	if err != nil {
		return frameRef{}, err
	}
	hb, err := r.take(8)
	if err != nil {
		return frameRef{}, err
	}
	if length > maxFrameLen {
		return frameRef{}, corruptf("frame length %d exceeds limit %d", length, uint64(maxFrameLen))
	}
	if off > uint64(math.MaxInt64) {
		return frameRef{}, corruptf("frame offset %d is out of range", off)
	}
	return frameRef{off: int64(off), length: uint32(length), hash: binary.LittleEndian.Uint64(hb)}, nil
}

// readDirFrame reads the directory frame, which is bounded separately from
// record frames because it describes the whole file.
func (w *IndexedWorld) readDirFrame(ref frameRef) ([]byte, error) {
	buf := make([]byte, ref.length)
	if _, err := w.f.ReadAt(buf, ref.off); err != nil {
		return nil, err
	}
	// The compression bit lives in the physical header, which may itself be
	// damaged; the directory carries the authoritative header fields, so it
	// has to be readable without trusting that bit. Try the form the header
	// claims, then the other.
	first, second := true, false
	if w.compressed {
		first, second = false, true
	}
	for _, compressed := range [2]bool{first, second} {
		body, ok := decodeDirBody(buf, compressed)
		if !ok {
			continue
		}
		// The prologue inside the frame says how the frame is stored, and it
		// is the authority. Accepting a form it contradicts would let one
		// directory be written two ways, and a reader that trusted the flag
		// instead of guessing would reject what this one accepted.
		flags, ok := prologueFlags(body)
		if !ok {
			continue
		}
		if (flags&FlagUncompressed == 0) != compressed {
			return nil, corruptf("directory frame is stored %s but its prologue says %s",
				storageWord(compressed), storageWord(flags&FlagUncompressed == 0))
		}
		return body, nil
	}
	return nil, corruptf("directory frame is neither valid raw nor valid compressed data")
}

func storageWord(compressed bool) string {
	if compressed {
		return "compressed"
	}
	return "raw"
}

// prologueFlags reads the flags word out of a decoded directory frame.
func prologueFlags(body []byte) (uint32, bool) {
	if len(body) < 10 {
		return 0, false
	}
	return binary.LittleEndian.Uint32(body[2:6]), true
}

// readFrame reads and decompresses a frame.
func (w *IndexedWorld) readFrame(ref frameRef) ([]byte, error) {
	buf := make([]byte, ref.length)
	if _, err := w.f.ReadAt(buf, ref.off); err != nil {
		return nil, err
	}
	if !w.compressed {
		// Same ceiling as the compressed path: the limit is a validity rule
		// about the frame, so one encoding of it cannot be legal while the
		// other is not.
		if len(buf) > maxDecodedFrame {
			return nil, corruptf("frame is %d bytes, limit %d", len(buf), maxDecodedFrame)
		}
		return buf, nil
	}
	dec := sharedFrameDecoder()
	if w.dictDec != nil {
		dec = w.dictDec
	}
	out, err := dec.DecodeAll(buf, nil)
	if err != nil {
		return nil, corruptf("decompress frame: %v", err)
	}
	return out, nil
}

// appendFrame compresses and appends a frame at EOF, returning its location
// and the stored bytes.
func (w *IndexedWorld) appendFrame(body []byte) (frameRef, []byte, error) {
	return w.appendFrameWith(body, false, maxDecodedFrame)
}

// appendFrameWith appends a frame; plain forces compression without the
// shared dictionary (required for the directory frame, which is read before
// the dictionary it references can be loaded, and for the dictionary itself).
func (w *IndexedWorld) appendFrameWith(body []byte, plain bool, limit int) (frameRef, []byte, error) {
	// Every frame the reader decompresses is capped, and the stored length is
	// carried in a uint32. A writer that passes either ceiling produces a file
	// it cannot reopen, which in indexed mode means silently rolling back to
	// an older checkpoint.
	if len(body) > limit {
		return frameRef{}, nil, fmt.Errorf("pile: frame is %d bytes, limit %d", len(body), limit)
	}
	stored := body
	if w.compressed {
		if w.dictEnc != nil && !plain {
			stored = w.dictEnc.EncodeAll(body, make([]byte, 0, len(body)/2))
		} else {
			stored = sharedEncoder(w.opts.Compression, false).EncodeAll(body, make([]byte, 0, len(body)/2))
		}
	}
	if uint64(len(stored)) > maxFrameLen {
		return frameRef{}, nil, fmt.Errorf("pile: stored frame is %d bytes, limit %d", len(stored), uint64(maxFrameLen))
	}
	ref := frameRef{off: w.end, length: uint32(len(stored)), hash: xxhash.Sum64(stored)}
	if _, err := w.f.WriteAt(stored, w.end); err != nil {
		// A partial write leaves the file longer than the logical end while
		// w.end stays put, so a later, shorter frame plus its footer would sit
		// inside the abandoned prefix and leave stale bytes past the footer.
		// A completed save must end at its own ELIP, or reopening it reports
		// recovery from an EOF nothing damaged.
		w.truncateToEnd()
		return frameRef{}, nil, fmt.Errorf("pile: append frame: %w", err)
	}
	w.end += int64(len(stored))
	return ref, stored, nil
}

// truncateToEnd discards anything written past the logical end of the file.
// It is best effort: if the truncation itself fails there is nothing further
// to be done here, and the recovery scan still bounds what a reader will
// believe.
func (w *IndexedWorld) truncateToEnd() {
	if st, err := w.f.Stat(); err == nil && st.Size() > w.end {
		_ = w.f.Truncate(w.end)
	}
}

// addState assigns (or returns) the palette index of a preserved unknown
// state, queueing new entries for the next checkpoint's segment.
func (w *IndexedWorld) addState(bs BlockState) uint32 {
	version := normaliseStateVersion(bs.Version)
	key := preservedStateKey(bs.Name, bs.Properties, bs.Version)
	if idx, ok := w.stateIdx[key]; ok {
		return idx
	}
	if err := w.reservePaletteEntry(checkState(bs.Name, bs.Properties)); err != nil {
		return 0
	}
	idx := uint32(len(w.rids))
	w.stateIdx[key] = idx
	w.rids = append(w.rids, placeholderRid(w.reg))
	w.unknown = append(w.unknown, int32(len(w.unkStates)))
	w.unkStates = append(w.unkStates, bs)
	w.identity = append(w.identity, idx)
	w.pendingBlock.str(bs.Name)
	encodeProps(&w.pendingBlock, bs.Properties)
	if version != 0 {
		// This state is expressed at a different version than the segment it
		// lands in (a preserved unknown state carried through compaction).
		w.pendingOverride = append(w.pendingOverride, paletteOverride{
			index: uint64(w.pendingBlkN), version: version,
		})
	}
	w.pendingBlkN++
	return idx
}

// addBlock assigns (or returns) the palette index of a runtime ID, queueing
// new entries for the next checkpoint's segment.
func (w *IndexedWorld) addBlock(rid uint32) uint32 {
	if idx, ok := w.blockIdx[rid]; ok {
		return idx
	}
	name, props, found := w.reg.RuntimeIDToState(rid)
	if !found {
		name, props = "minecraft:air", nil
	}
	// Key by the state, not by the runtime ID. A registry may number the same
	// state twice, and the palette holds one entry per unique state: the solid
	// builder merges such aliases, so indexed mode must too or the same
	// content would encode differently depending on which writer produced it.
	key := preservedStateKey(name, props, 0)
	if idx, ok := w.stateIdx[key]; ok {
		w.blockIdx[rid] = idx
		return idx
	}
	if err := w.reservePaletteEntry(checkState(name, props)); err != nil {
		return 0
	}
	idx := uint32(len(w.rids))
	w.blockIdx[rid] = idx
	w.stateIdx[key] = idx
	w.rids = append(w.rids, rid)
	w.unknown = append(w.unknown, -1)
	w.identity = append(w.identity, idx)
	w.pendingBlock.str(name)
	encodeProps(&w.pendingBlock, props)
	w.pendingBlkN++
	return idx
}

// reservePaletteEntry records the first reason a new block palette entry
// cannot be admitted, without mutating anything. Solid mode validates states
// when it finalizes its palette; indexed mode appends entries as they arrive
// and has no such moment, so the same rules are applied at admission. The
// error is sticky and surfaces from Store, because addBlock and addState sit
// behind resolveColumn's callback signature and cannot return one.
func (w *IndexedWorld) reservePaletteEntry(err error) error {
	if err == nil && len(w.rids) >= maxPalette {
		err = fmt.Errorf("pile: %d block states exceeds limit %d", len(w.rids)+1, maxPalette)
	}
	return w.noteErr(err)
}

// noteErr records a rejection without asserting anything about capacity. The
// block and biome palettes have separate limits and separate references, so a
// biome must not be refused because the block palette is full.
func (w *IndexedWorld) noteErr(err error) error {
	if err != nil && w.paletteErr == nil {
		w.paletteErr = err
	}
	return err
}

// addBiome assigns (or returns) the palette index of a biome name.
func (w *IndexedWorld) addBiome(name string) uint32 {
	if idx, ok := w.biomeIdx[name]; ok {
		return idx
	}
	if err := w.noteErr(checkString(name, "biome name")); err != nil {
		return 0
	}
	if !strings.Contains(name, ":") {
		w.noteErr(fmt.Errorf("pile: biome name %q is not namespaced", name))
		return 0
	}
	if len(w.biomeNames) >= maxPalette {
		w.noteErr(fmt.Errorf("pile: %d biomes exceeds limit %d", len(w.biomeNames)+1, maxPalette))
		return 0
	}
	idx := uint32(len(w.biomeNames))
	w.biomeIdx[name] = idx
	w.biomeNames = append(w.biomeNames, name)
	w.biomeIdent = append(w.biomeIdent, idx)
	// A name the runtime cannot resolve decodes as the fallback, and the entry
	// records the original so a read before the next checkpoint sees it too.
	// Appending -1 unconditionally left the mapping empty until the segment had
	// been written and loaded back, so a read-and-rewrite in between replaced
	// the biome with the fallback.
	id, unknown := fallbackBiomeID(), int32(-1)
	if b, ok := lookupBiome(name); ok {
		id = uint32(b.EncodeBiome())
	} else {
		unknown = int32(len(w.biomeUnkName))
		w.biomeUnkName = append(w.biomeUnkName, name)
	}
	w.biomeIDs = append(w.biomeIDs, id)
	w.biomeUnknown = append(w.biomeUnknown, unknown)
	w.pendingBiome.str(name)
	w.pendingBioN++
	return idx
}

// paletteSnapshot records how far the palettes had grown, so a failed Store
// can wind them back. Every palette structure is append-only within a
// checkpoint, so lengths are enough to undo one; the maps are the exception
// and their new keys are removed by name.
type paletteSnapshot struct {
	rids, identity       int
	unknown, unkStates   int
	blockKeys            []uint32
	stateKeys            []string
	biomeKeys            []string
	biomeIDs, biomeIdent int
	biomeNames           int
	biomeUnknown         int
	biomeUnkName         int
	pendingBlockLen      int
	pendingBlkN          int
	pendingOverride      int
	pendingBiomeLen      int
	pendingBioN          int
}

func (w *IndexedWorld) snapshotPalettes() paletteSnapshot {
	return paletteSnapshot{
		rids: len(w.rids), identity: len(w.identity),
		unknown: len(w.unknown), unkStates: len(w.unkStates),
		blockKeys: slices.Collect(maps.Keys(w.blockIdx)),
		stateKeys: slices.Collect(maps.Keys(w.stateIdx)),
		biomeKeys: slices.Collect(maps.Keys(w.biomeIdx)),
		biomeIDs:  len(w.biomeIDs), biomeIdent: len(w.biomeIdent),
		biomeNames: len(w.biomeNames), biomeUnknown: len(w.biomeUnknown),
		biomeUnkName:    len(w.biomeUnkName),
		pendingBlockLen: w.pendingBlock.len(), pendingBlkN: w.pendingBlkN,
		pendingOverride: len(w.pendingOverride),
		pendingBiomeLen: w.pendingBiome.len(), pendingBioN: w.pendingBioN,
	}
}

func (w *IndexedWorld) restorePalettes(s paletteSnapshot) {
	w.rids = w.rids[:s.rids]
	w.identity = w.identity[:s.identity]
	w.unknown = w.unknown[:s.unknown]
	w.unkStates = w.unkStates[:s.unkStates]
	keepKeys(w.blockIdx, s.blockKeys)
	keepKeys(w.stateIdx, s.stateKeys)
	keepKeys(w.biomeIdx, s.biomeKeys)
	w.biomeIDs = w.biomeIDs[:s.biomeIDs]
	w.biomeIdent = w.biomeIdent[:s.biomeIdent]
	w.biomeNames = w.biomeNames[:s.biomeNames]
	w.biomeUnknown = w.biomeUnknown[:s.biomeUnknown]
	w.biomeUnkName = w.biomeUnkName[:s.biomeUnkName]
	w.pendingBlock.b = w.pendingBlock.b[:s.pendingBlockLen]
	w.pendingBlkN = s.pendingBlkN
	w.pendingOverride = w.pendingOverride[:s.pendingOverride]
	w.pendingBiome.b = w.pendingBiome.b[:s.pendingBiomeLen]
	w.pendingBioN = s.pendingBioN
}

// keepKeys deletes every key of m that is not in keep.
func keepKeys[K comparable, V any](m map[K]V, keep []K) {
	if len(m) == len(keep) {
		return
	}
	set := make(map[K]struct{}, len(keep))
	for _, k := range keep {
		set[k] = struct{}{}
	}
	for k := range m {
		if _, ok := set[k]; !ok {
			delete(m, k)
		}
	}
}

// Store encodes and appends a column, replacing any previous record at its
// position. The column is read, not retained; the caller keeps ownership.
func (w *IndexedWorld) Store(c Column) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.readOnly {
		return ErrReadOnlyFile
	}
	if w.closed {
		return errors.New("pile: indexed world is closed")
	}
	if err := validateColumn(c); err != nil {
		return err
	}
	if _, exists := w.dir[[2]int32{c.X, c.Z}]; !exists && len(w.dir) >= maxChunks {
		return fmt.Errorf("pile: indexed world already holds %d chunks, the format limit", maxChunks)
	}
	cr := extractColumnRaw(c, w.opts.SkipBiomes, w.opts.StoreLight, placeholderRid(w.reg))
	if cr.err != nil {
		return fmt.Errorf("pile: encode chunk (%d,%d): %w", c.X, c.Z, cr.err)
	}
	// Resolving a column appends to the cumulative and pending palettes as it
	// goes, and the first entry it cannot admit is only reported afterwards.
	// A rejected Store must leave nothing behind: entries added before the bad
	// one are valid but unreferenced, and would still reach the next
	// checkpoint, so an empty world that refused a column would not encode
	// like an empty world that never saw one.
	snap := w.snapshotPalettes()
	// Any failure from here on has to wind the palettes back, not just the
	// one the palette itself reported: a record that is too large, a frame
	// that will not append, or a directory that is full all leave the same
	// unreferenced entries behind, and they would still reach the next
	// checkpoint.
	committed := false
	defer func() {
		if !committed {
			w.restorePalettes(snap)
		}
	}()
	// Indexed palettes are first-seen order with no frequency sorting, so
	// nothing there consumes a reference count.
	ci := resolveColumn(&cr, w.addBlock, w.addBiome, w.addState, func(uint32) {}, func(uint32) {})
	if w.paletteErr != nil {
		err := w.paletteErr
		w.paletteErr = nil
		return fmt.Errorf("pile: chunk (%d,%d): %w", c.X, c.Z, err)
	}
	cb := buildColBlobs(&ci, w.identity, w.biomeIdent, 0, false)
	body := &writer{b: make([]byte, 0, 8<<10)}
	encodeRecordBody(body, &ci, &cb, w.identity, w.opts.StoreLight, func(bw *writer, blob []byte) {
		bw.raw(blob)
	})
	if n := body.len(); n > maxDecodedFrame {
		// The reader caps a frame's decompressed size; refuse to write a
		// record this process could not read back.
		return fmt.Errorf("pile: chunk (%d,%d) encodes to %d bytes, limit %d", c.X, c.Z, n, maxDecodedFrame)
	}
	ref, stored, err := w.appendFrame(body.bytes())
	if err != nil {
		return err
	}
	key := [2]int32{c.X, c.Z}
	if _, ok := w.dir[key]; !ok && len(w.dir) >= maxDirEntries {
		return fmt.Errorf("pile: indexed world already holds %d chunks (limit %d)", len(w.dir), maxDirEntries)
	}
	if prev, ok := w.dir[key]; ok {
		w.liveBytes -= int64(prev.length)
	}
	w.dir[key] = dirEntry{off: ref.off, length: ref.length, hash: xxhash.Sum64(stored)}
	w.liveBytes += int64(ref.length)
	w.recordsDirty = true
	committed = true
	return nil
}

// Column reads and decodes the column at a position. Returns ErrNoColumn if
// none is stored.
func (w *IndexedWorld) Column(x, z int32) (Column, error) {
	w.mu.Lock()
	body, rids, biomeIDs, unknown, unkStates, storeLight, err := w.fetchRecordLocked(x, z)
	w.mu.Unlock()
	if err != nil {
		return Column{}, err
	}
	return w.applyRecordBody(body, rids, biomeIDs, unknown, unkStates, storeLight, x, z)
}

// columnHeld is Column with w.mu held for the entire call (used by Compact so
// no Store can interleave).
func (w *IndexedWorld) columnHeld(x, z int32) (Column, error) {
	body, rids, biomeIDs, unknown, unkStates, storeLight, err := w.fetchRecordLocked(x, z)
	if err != nil {
		return Column{}, err
	}
	return w.applyRecordBody(body, rids, biomeIDs, unknown, unkStates, storeLight, x, z)
}

// fetchRecordLocked reads, verifies and decompresses a record frame. Caller
// holds w.mu.
func (w *IndexedWorld) fetchRecordLocked(x, z int32) (body []byte, rids, biomeIDs []uint32, unknown []int32, unkStates []BlockState, storeLight bool, err error) {
	e, ok := w.dir[[2]int32{x, z}]
	if !ok {
		return nil, nil, nil, nil, nil, false, ErrNoColumn
	}
	stored := make([]byte, e.length)
	if _, err := w.f.ReadAt(stored, e.off); err != nil {
		return nil, nil, nil, nil, nil, false, err
	}
	if xxhash.Sum64(stored) != e.hash {
		return nil, nil, nil, nil, nil, false, fmt.Errorf("%w: record (%d,%d) checksum mismatch", ErrChecksum, x, z)
	}
	if w.compressed {
		dec := sharedFrameDecoder()
		if w.dictDec != nil {
			dec = w.dictDec
		}
		body, err = dec.DecodeAll(stored, nil)
		if err != nil {
			return nil, nil, nil, nil, nil, false, corruptf("decompress record (%d,%d): %v", x, z, err)
		}
	} else {
		// The ceiling is a rule about the record, so an uncompressed one past
		// it is just as invalid as a compressed one that decodes past it.
		if len(stored) > maxDecodedFrame {
			return nil, nil, nil, nil, nil, false,
				corruptf("record (%d,%d) is %d bytes, limit %d", x, z, len(stored), maxDecodedFrame)
		}
		body = stored
	}
	return body, w.rids, w.biomeIDs, w.unknown, w.unkStates, w.opts.StoreLight, nil
}

// applyRecordBody decodes a fetched record body into a Column.
func (w *IndexedWorld) applyRecordBody(body []byte, rids, biomeIDs []uint32, unknown []int32, unkStates []BlockState, storeLight bool, x, z int32) (Column, error) {
	r := &reader{b: body}
	// Indexed mode never elides a biome section, so it names no default; a
	// section absent because biomes were skipped still takes the version-stable
	// fallback rather than a numeric id.
	col, err := decodeRecordBody(r, w.reg, rids, biomeIDs, w.reg.AirRuntimeID(), fallbackBiomeID(), false, storeLight, -1, unknown, unkStates,
		w.biomeUnknown, w.biomeUnkName,
		func(r *reader) (decBlob, error) { return decodeOneBlob(r) }, x, z)
	if err != nil {
		return Column{}, err
	}
	if r.remaining() != 0 {
		return Column{}, corruptf("%d trailing bytes in record (%d,%d)", r.remaining(), x, z)
	}
	return col, nil
}

// RecordID returns an opaque identity for the record currently stored at a
// position, changing whenever the record is replaced. Callers that decode a
// column outside the lock can re-check it before publishing the result.
func (w *IndexedWorld) RecordID(x, z int32) (uint64, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	e, ok := w.dir[[2]int32{x, z}]
	if !ok {
		return 0, false
	}
	return uint64(e.off) ^ e.hash, true
}

// Positions returns all stored column positions.
func (w *IndexedWorld) Positions() [][2]int32 {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([][2]int32, 0, len(w.dir))
	for k := range w.dir {
		out = append(out, k)
	}
	return out
}

// ChunkCount returns the number of stored columns.
func (w *IndexedWorld) ChunkCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.dir)
}

// Meta returns the metadata blobs.
func (w *IndexedWorld) Meta() (settings, userData, markers, border []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return cloneBytes(w.settings), cloneBytes(w.userData), cloneBytes(w.markers), cloneBytes(w.border)
}

// metaFrameLen returns the encoded size of a metadata frame, so the aggregate
// can be checked where the blobs arrive rather than where they are written.
func metaFrameLen(blobs ...[]byte) int {
	w := &writer{}
	for _, b := range blobs {
		w.blob(b)
	}
	return w.len()
}

// SetMeta replaces the metadata blobs, persisted on the next checkpoint. It
// rejects blobs the decoder would refuse, so a checkpoint can never write a
// file that fails to reopen (which would silently roll back to an older
// checkpoint, losing chunks stored since).
func (w *IndexedWorld) SetMeta(settings, userData, markers, border []byte) error {
	// Refuse where a store would. Reporting success on a world that can never
	// persist the change leaves Meta answering with something no reader will
	// ever see.
	w.mu.Lock()
	readOnly, closed := w.readOnly, w.closed
	w.mu.Unlock()
	if readOnly {
		return ErrReadOnlyFile
	}
	if closed {
		return errors.New("pile: indexed world is closed")
	}
	for _, b := range []struct {
		p    []byte
		what string
		nbt  bool
	}{
		{settings, "world settings blob", true},
		{userData, "world user data", false},
		{markers, "markers blob", true},
		{border, "border blob", true},
	} {
		if err := checkBlob(b.p, b.what); err != nil {
			return err
		}
		// checkBlob only bounds the length. The reader also demands canonical
		// NBT of the designated blobs, and a checkpoint that fails to reopen
		// rolls the world back to an older one, losing every chunk stored
		// since, so the check belongs here rather than at write time.
		if b.nbt && len(b.p) > 0 {
			if err := validateNBT(b.p); err != nil {
				return fmt.Errorf("pile: %s: %w", b.what, err)
			}
		}
	}
	if err := checkMetaSchemas(settings, markers, border); err != nil {
		return err
	}
	// The four blobs go into one frame, and the frame has its own ceiling: four
	// individually legal 16 MiB blobs already exceed it. Checking only the
	// parts leaves SetMeta reporting success and every later checkpoint
	// failing, which is the rollback this call exists to prevent.
	if n := metaFrameLen(settings, userData, markers, border); n > maxDecodedFrame {
		return fmt.Errorf("pile: metadata frame is %d bytes, limit %d", n, maxDecodedFrame)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.settings, w.userData = cloneBytes(settings), cloneBytes(userData)
	w.markers, w.border = cloneBytes(markers), cloneBytes(border)
	w.metaDirty = true
	return nil
}

// Dimension reports which dimension this file describes.
func (w *IndexedWorld) Dimension() Dimension {
	w.mu.Lock()
	defer w.mu.Unlock()
	return Dimension((w.headerFlags & dimensionMask) >> dimensionShift)
}

// Recovered reports whether opening the file had to fall back to an older
// checkpoint because the newest one did not validate. When true, everything
// stored after that checkpoint is gone: worth surfacing to an operator.
func (w *IndexedWorld) Recovered() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.recovered
}

// Generation returns the checkpoint generation.
func (w *IndexedWorld) Generation() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.generation
}

// GarbageRatio reports the fraction of file bytes that are dead (overwritten
// records, superseded directories and torn tails).
func (w *IndexedWorld) GarbageRatio() float64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	total := w.end - headerSize
	if total <= 0 {
		return 0
	}
	live := w.liveBytes
	for _, s := range w.blockSegs {
		live += int64(s.length)
	}
	for _, s := range w.biomeSegs {
		live += int64(s.length)
	}
	live += int64(w.metaRef.length)
	live += int64(w.dictRef.length)
	live += int64(w.dirRef.length) + footerSize // current checkpoint overhead
	if live > total {
		return 0
	}
	return float64(total-live) / float64(total)
}

// Checkpoint appends pending palette segments, metadata, a directory and a
// footer, making everything stored so far durable.
func (w *IndexedWorld) Checkpoint() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.readOnly {
		return ErrReadOnlyFile
	}
	return w.checkpointLocked()
}

func (w *IndexedWorld) checkpointLocked() error {
	if w.footerPending {
		// A previous checkpoint wrote its footer but failed to sync it.
		// Retry the sync, then fall through: the dirty flags stay set, so if
		// anything was stored after that footer a fresh checkpoint covering
		// it is written now (a redundant checkpoint when nothing changed is
		// harmless; silently dropping a later store would not be).
		if err := w.f.Sync(); err != nil {
			return fmt.Errorf("pile: sync footer: %w", err)
		}
		w.footerPending = false
		w.prevFooterOff = w.pendingFooterOff
	}
	if !w.recordsDirty && !w.metaDirty && w.pendingBlkN == 0 && w.pendingBioN == 0 {
		return nil
	}
	// Flush pending palette segments.
	if w.pendingBlkN > 0 {
		seg := &writer{}
		seg.u32(uint32(chunk.CurrentBlockVersion)) // states below are at this version
		seg.uvarint(uint64(w.pendingBlkN))
		seg.raw(w.pendingBlock.bytes())
		seg.uvarint(uint64(len(w.pendingOverride)))
		var prevIdx uint64
		for _, o := range w.pendingOverride {
			seg.uvarint(o.index - prevIdx)
			seg.i32(o.version)
			prevIdx = o.index
		}
		ref, _, err := w.appendFrame(seg.bytes())
		if err != nil {
			return err
		}
		w.blockSegs = append(w.blockSegs, ref)
		w.pendingBlock = writer{}
		w.pendingBlkN = 0
		w.pendingOverride = nil
	}
	if w.pendingBioN > 0 {
		seg := &writer{}
		seg.uvarint(uint64(w.pendingBioN))
		seg.raw(w.pendingBiome.bytes())
		ref, _, err := w.appendFrame(seg.bytes())
		if err != nil {
			return err
		}
		w.biomeSegs = append(w.biomeSegs, ref)
		w.pendingBiome = writer{}
		w.pendingBioN = 0
	}
	if w.metaDirty || (w.metaRef.length == 0 && (len(w.settings) > 0 || len(w.userData) > 0 || len(w.markers) > 0 || len(w.border) > 0)) {
		mb := &writer{}
		mb.blob(w.settings)
		mb.blob(w.userData)
		mb.blob(w.markers)
		mb.blob(w.border)
		ref, _, err := w.appendFrame(mb.bytes())
		if err != nil {
			return err
		}
		w.metaRef = ref
		w.metaDirty = false
	}

	// Directory: a self-describing prologue (the semantic header fields, so a
	// checkpoint does not depend on the physical header surviving), then
	// entries sorted by Morton key with positions and offsets delta-coded.
	d := &writer{}
	d.u8(KindWorld)
	d.u8(ModeIndexed)
	d.u32(w.headerFlags)
	d.i32(w.headerBlockVersion)
	writeRef := func(ref frameRef) {
		d.uvarint(uint64(ref.off))
		d.uvarint(uint64(ref.length))
		d.u64(ref.hash)
	}
	writeRef(w.metaRef)
	writeRef(w.dictRef)
	writeSegs := func(segs []frameRef) {
		d.uvarint(uint64(len(segs)))
		for _, s := range segs {
			writeRef(s)
		}
	}
	writeSegs(w.blockSegs)
	writeSegs(w.biomeSegs)

	keys := make([][2]int32, 0, len(w.dir))
	for k := range w.dir {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b [2]int32) int {
		ka, kb := mortonKey(a[0], a[1]), mortonKey(b[0], b[1])
		switch {
		case ka < kb:
			return -1
		case ka > kb:
			return 1
		default:
			return 0
		}
	})
	d.uvarint(uint64(len(keys)))
	var px, pz, poff int64
	for _, k := range keys {
		e := w.dir[k]
		d.svarint(int64(k[0]) - px)
		d.svarint(int64(k[1]) - pz)
		d.svarint(e.off - poff)
		d.uvarint(uint64(e.length))
		d.u64(e.hash)
		px, pz, poff = int64(k[0]), int64(k[1]), e.off
	}

	if len(d.bytes()) > maxDecodedDirectory {
		return fmt.Errorf("pile: directory is %d bytes, limit %d", len(d.bytes()), maxDecodedDirectory)
	}
	dirRef, stored, err := w.appendFrameWith(d.bytes(), true, maxDecodedDirectory)
	if err != nil {
		return err
	}
	w.dirRef = dirRef

	// Durability ordering: everything the footer references must be on disk
	// before the footer itself, or a crash could persist a footer whose
	// records are torn.
	if err := w.f.Sync(); err != nil {
		return fmt.Errorf("pile: sync frames: %w", err)
	}

	w.generation++
	tail := &writer{}
	tail.u64(uint64(dirRef.off))
	tail.u64(uint64(dirRef.length))
	tail.u64(w.generation)
	tail.u64(uint64(w.prevFooterOff))
	tail.raw(footerMagic[:])
	ftr := &writer{}
	ftr.u64(checkpointHash(w.hdrBytes, stored, tail.bytes()))
	ftr.raw(tail.bytes())
	footerPos := w.end
	if _, err := w.f.WriteAt(ftr.bytes(), w.end); err != nil {
		return fmt.Errorf("pile: write footer: %w", err)
	}
	w.end += footerSize
	// The checkpoint only counts as taken once its footer is durable: keep
	// the dirty state until then so a failed sync is retried rather than
	// reported as a successful save.
	w.footerPending, w.pendingFooterOff = true, footerPos
	if err := w.f.Sync(); err != nil {
		return fmt.Errorf("pile: sync footer: %w", err)
	}
	w.footerPending = false
	w.recordsDirty = false
	w.prevFooterOff = footerPos
	return nil
}

// Compact rewrites the live set into a fresh file (atomic rename), removing
// all garbage and canonicalizing palettes, then reopens the world from it.
// The world is locked for the duration: concurrent Store/Column calls block
// until compaction finishes instead of being silently discarded.
func (w *IndexedWorld) Compact() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.readOnly {
		return ErrReadOnlyFile
	}
	if err := w.checkpointLocked(); err != nil {
		return err
	}
	keys := make([][2]int32, 0, len(w.dir))
	for k := range w.dir {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b [2]int32) int {
		ka, kb := mortonKey(a[0], a[1]), mortonKey(b[0], b[1])
		switch {
		case ka < kb:
			return -1
		case ka > kb:
			return 1
		default:
			return 0
		}
	})
	opts, path := w.opts, w.path
	// Compaction rewrites the file, so it has to carry the header forward:
	// w.opts came from the caller and never held the dimension the file
	// records.
	opts.Dimension = Dimension((w.headerFlags & dimensionMask) >> dimensionShift)

	// Train a shared dictionary from the live record bodies when there is
	// enough material for it to pay off.
	var trained []byte
	if opts.Compression != CompressionNone && len(keys) >= dictMinSamples {
		samples := make([][]byte, 0, len(keys))
		total := 0
		for _, k := range keys {
			e := w.dir[k]
			body, err := w.readFrame(frameRef{off: e.off, length: e.length})
			if err != nil {
				continue
			}
			samples = append(samples, body)
			total += len(body)
		}
		if len(samples) >= dictMinSamples && total >= dictMinBytes {
			if d, err := buildDictionary(samples, opts.Compression); err == nil {
				trained = d
			}
		}
	}

	tmp := path + ".compact"
	_ = os.Remove(tmp)
	nw, err := CreateIndexed(tmp, w.reg, opts)
	if err != nil {
		return err
	}
	fail := func(err error) error {
		_ = nw.Close()
		_ = os.Remove(tmp)
		return err
	}
	if trained != nil {
		if err := nw.installDict(trained); err != nil {
			return fail(err)
		}
	}
	for _, k := range keys {
		col, err := w.columnHeld(k[0], k[1])
		if err != nil {
			return fail(fmt.Errorf("pile: compact: read (%d,%d): %w", k[0], k[1], err))
		}
		if err := nw.Store(col); err != nil {
			return fail(err)
		}
	}
	if err := nw.SetMeta(w.settings, w.userData, w.markers, w.border); err != nil {
		return fail(err)
	}
	if err := nw.Checkpoint(); err != nil {
		return fail(err)
	}
	if err := nw.closeFile(); err != nil {
		return fail(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fail(err)
	}
	// The rename has happened: the old inode is unlinked, so the handle must
	// be replaced before anything else can be written through it, whatever
	// the directory sync reports.
	syncErr := syncDirOf(path)

	_ = w.f.Close()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		w.closed = true // no usable handle; refuse further writes
		return errors.Join(syncErr, err)
	}
	w.f = f
	w.resetLoadedState()
	w.prevFooterOff = 0
	w.footerPending = false
	if err := w.load(); err != nil {
		return errors.Join(syncErr, err)
	}
	return syncErr
}

// Close checkpoints pending state and closes the file.
func (w *IndexedWorld) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	if !w.readOnly {
		if err := w.checkpointLocked(); err != nil {
			_ = w.f.Close()
			return err
		}
	}
	return w.closeFileLocked()
}

func (w *IndexedWorld) closeFile() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	return w.closeFileLocked()
}

func (w *IndexedWorld) closeFileLocked() error {
	if w.dictEnc != nil {
		w.dictEnc.Close()
	}
	if w.dictDec != nil {
		w.dictDec.Close()
	}
	return w.f.Close()
}

// decodeDirBody returns a directory frame body interpreted as raw or
// compressed data, reporting whether that interpretation is plausible.
func decodeDirBody(buf []byte, compressed bool) ([]byte, bool) {
	if !compressed {
		// A raw directory starts with the prologue's kind byte, which is the
		// only value a valid uncompressed directory can begin with.
		if len(buf) == 0 || (buf[0] != KindWorld && buf[0] != KindStructure) {
			return nil, false
		}
		if len(buf) > maxDecodedDirectory {
			return nil, false
		}
		return buf, true
	}
	out, err := sharedDirectoryDecoder().DecodeAll(buf, nil)
	if err != nil {
		return nil, false
	}
	return out, true
}

// syncDirOf fsyncs the directory containing path, making a rename into it
// durable.
func syncDirOf(path string) error {
	dir := filepath.Dir(path)
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

// ErrReadOnlyFile is returned when writing to a read-only indexed world.
var ErrReadOnlyFile = errors.New("pile: indexed world is read-only")
