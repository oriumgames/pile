package format

import (
	"fmt"
	"runtime"
	"sync"

	"github.com/cespare/xxhash/v2"
	"github.com/klauspost/compress/zstd"
)

// Shared zstd machinery.
//
// klauspost sizes a decoder's working set by its configured concurrency and
// holds it for the decoder's lifetime, so a process-wide decoder built with
// GOMAXPROCS concurrency keeps one full window-sized history buffer per core
// from its first use until the process exits: eight cores decoding whole solid
// bodies retained 195 MiB of it. DecodeAll is one-shot and carries nothing
// between calls, so none of that is ever read again.
//
// Decoders are therefore built with concurrency 1 and lent from pools. Only as
// many exist as there are decodes actually running, and the collector reclaims
// the idle ones. Concurrency 1 without the pool would serialise every decode
// in the process, which is why the pool is here and not just the option.
//
// Encoders stay shared instead of pooled. They are built late, so a process
// that only reads never constructs one, but rebuilding a CompressionBest
// encoder costs a megabyte of allocation and several milliseconds, and a
// server saves far too often to pay that per save.

// Decoded-size ceilings. A hostile frame can declare a huge content size and
// zstd preallocates it, so the cap must be low enough that a few concurrent
// hostile inputs cannot exhaust memory, yet above any legitimate payload:
// a solid body holds a whole (small-world) dimension, an indexed frame holds
// one chunk record, palette segment, metadata blob or directory.
const (
	maxDecodedBody = 512 << 20
	// maxDecodedFrame covers record, palette and metadata frames, which each
	// hold a single chunk's worth of data.
	maxDecodedFrame = 64 << 20
	// maxDecodedDirectory is larger: one directory describes every chunk in
	// the file, and it bounds maxDirEntries.
	maxDecodedDirectory = 512 << 20
	// maxZstdWindow bounds the window a frame may ask a decoder to hold. The
	// decoded-size ceilings bound the output, not the memory needed to produce
	// it, so without this a small frame can still demand an arbitrary window.
	// Pile's own frames never need more: a record is one chunk and a body is
	// written in one pass.
	maxZstdWindow = 8 << 20
)

// lazyEncoder serves one encoder configuration process-wide, building the
// encoder on first use.
//
// Encoders are shared rather than pooled: EncodeAll is safe for concurrent
// use, and rebuilding a CompressionBest encoder costs about a megabyte of
// allocation, which a server that saves regularly would pay on every save.
// Building it late is what keeps a reader cheap, since a process that only
// opens and reads worlds never constructs one at all.
type lazyEncoder struct {
	level zstd.EncoderLevel
	conc  int
	// dict is the shared compression dictionary, or nil for none. It is never
	// mutated after the encoder is registered.
	dict []byte
	once sync.Once
	enc  *zstd.Encoder
}

// encodeAll compresses src, appending the result to dst.
func (p *lazyEncoder) encodeAll(src, dst []byte) []byte {
	p.once.Do(func() {
		opts := []zstd.EOption{
			zstd.WithEncoderLevel(p.level),
			zstd.WithEncoderConcurrency(p.conc),
		}
		if p.dict != nil {
			opts = append(opts, zstd.WithEncoderDict(p.dict))
		}
		e, err := zstd.NewWriter(nil, opts...)
		if err != nil {
			// Static options, and any dictionary was parsed when the encoder
			// was registered, so no caller input reaches this.
			panic("pile: create zstd encoder: " + err.Error())
		}
		p.enc = e
	})
	return p.enc.EncodeAll(src, dst)
}

// decPool lends decoders bounded at one decoded-size ceiling.
type decPool struct {
	maxMemory uint64
	dict      []byte
	pool      sync.Pool
}

// decodeAll decompresses src, appending the result to dst. A decoder is
// returned to the pool even when the input was rejected: DecodeAll releases
// its block decoder on every path, so a failed decode leaves nothing behind.
func (p *decPool) decodeAll(src, dst []byte) ([]byte, error) {
	d, _ := p.pool.Get().(*zstd.Decoder)
	if d == nil {
		var err error
		if d, err = p.newDecoder(); err != nil {
			panic("pile: create zstd decoder: " + err.Error())
		}
	}
	out, err := d.DecodeAll(src, dst)
	p.pool.Put(d)
	return out, err
}

func (p *decPool) newDecoder() (*zstd.Decoder, error) {
	opts := []zstd.DOption{
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxWindow(maxZstdWindow),
		zstd.WithDecoderMaxMemory(p.maxMemory),
	}
	if p.dict != nil {
		opts = append(opts, zstd.WithDecoderDicts(p.dict))
	}
	return zstd.NewReader(nil, opts...)
}

type encKey struct {
	level zstd.EncoderLevel
	fast  bool
}

var (
	encMu    sync.Mutex
	encoders = map[encKey]*lazyEncoder{}

	decMu    sync.Mutex
	decPools = map[uint64]*decPool{}
)

// zstdLevel maps a CompressionLevel to a zstd encoder level.
func zstdLevel(l CompressionLevel) zstd.EncoderLevel {
	switch l {
	case CompressionFast:
		return zstd.SpeedFastest
	case CompressionBest:
		return zstd.SpeedBestCompression
	default:
		return zstd.SpeedDefault
	}
}

// sharedEncoder returns the process-wide encoder for a compression level.
// fast enables multi-threaded compression (output no longer deterministic).
func sharedEncoder(level CompressionLevel, fast bool) *lazyEncoder {
	k := encKey{level: zstdLevel(level), fast: fast}
	encMu.Lock()
	defer encMu.Unlock()
	if e, ok := encoders[k]; ok {
		return e
	}
	conc := 1
	if fast {
		conc = runtime.GOMAXPROCS(0)
	}
	e := &lazyEncoder{level: k.level, conc: conc}
	encoders[k] = e
	return e
}

// sharedDecoderFor returns the decoder pool bounding decoded output at
// maxMemory. Keying on the ceiling shares one pool between uses whose limits
// genuinely agree (a solid body and an indexed directory) without ever letting
// a stricter ceiling be served by a decoder that would allocate past it.
func sharedDecoderFor(maxMemory uint64) *decPool {
	decMu.Lock()
	defer decMu.Unlock()
	if p, ok := decPools[maxMemory]; ok {
		return p
	}
	p := &decPool{maxMemory: maxMemory}
	decPools[maxMemory] = p
	return p
}

// sharedDecoder returns the decoder pool for whole solid bodies.
func sharedDecoder() *decPool { return sharedDecoderFor(maxDecodedBody) }

// sharedFrameDecoder returns the decoder pool for indexed frames, which are
// far smaller than a whole body and capped accordingly.
func sharedFrameDecoder() *decPool { return sharedDecoderFor(maxDecodedFrame) }

// sharedDirectoryDecoder returns the decoder pool for indexed directory
// frames, which are bounded separately from record frames.
func sharedDirectoryDecoder() *decPool { return sharedDecoderFor(maxDecodedDirectory) }

// dictCodec holds the codecs for one shared dictionary at one compression
// level.
//
// Every open indexed handle used to build a private dictionary encoder and
// decoder, measured at 11 MiB per handle that writes a frame, paid again for
// each dimension of a world and again for the handle a compaction opens over
// the file it just wrote. Keying on the dictionary makes that cost belong to
// the dictionary instead of to the handle, and the encoder is built only when
// a frame is actually compressed: opening a dictionary world read-only now
// costs one parsed dictionary and no encoder at all.
type dictCodec struct {
	enc *lazyEncoder
	dec *decPool
}

type dictKey struct {
	level zstd.EncoderLevel
	size  int
	hash  uint64
}

var (
	dictMu     sync.Mutex
	dictCodecs = map[dictKey]*dictCodec{}
)

// sharedDictCodec returns the codecs for a dictionary at a compression level.
// The map it memoizes into is keyed by dictionary content, so it grows with
// the number of distinct dictionaries a process meets (one per compacted
// file), not with the number of handles open on them.
func sharedDictCodec(dict []byte, level CompressionLevel) (*dictCodec, error) {
	k := dictKey{level: zstdLevel(level), size: len(dict), hash: xxhash.Sum64(dict)}
	dictMu.Lock()
	defer dictMu.Unlock()
	if c, ok := dictCodecs[k]; ok {
		return c, nil
	}
	dec := &decPool{maxMemory: maxDecodedFrame, dict: dict}
	// Build the first decoder eagerly rather than on first read: a dictionary
	// that does not parse has to be refused where it is installed, not at some
	// later read of a frame that already depends on it.
	d, err := dec.newDecoder()
	if err != nil {
		return nil, fmt.Errorf("pile: create dict decoder: %w", err)
	}
	dec.pool.Put(d)
	c := &dictCodec{enc: &lazyEncoder{level: k.level, conc: 1, dict: dict}, dec: dec}
	dictCodecs[k] = c
	return c, nil
}

// compressBody compresses a body with the shared encoder.
func compressBody(body []byte, level CompressionLevel, fast bool) []byte {
	return sharedEncoder(level, fast).encodeAll(body, make([]byte, 0, len(body)/4))
}
