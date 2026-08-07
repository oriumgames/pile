package format

import (
	"runtime"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// Shared zstd machinery. klauspost encoders and decoders are safe for
// concurrent EncodeAll/DecodeAll use, so one instance per configuration
// serves the whole process instead of being constructed per save.

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

var (
	encMu    sync.Mutex
	encoders = map[encKey]*zstd.Encoder{}

	bodyDecOnce sync.Once
	bodyDec     *zstd.Decoder

	frameDecOnce sync.Once
	frameDec     *zstd.Decoder

	dirDecOnce sync.Once
	dirDec     *zstd.Decoder
)

// sharedDirectoryDecoder returns the process-wide decoder for indexed
// directory frames, which are bounded separately from record frames.
func sharedDirectoryDecoder() *zstd.Decoder {
	dirDecOnce.Do(func() {
		d, err := zstd.NewReader(nil,
			zstd.WithDecoderConcurrency(runtime.GOMAXPROCS(0)),
			zstd.WithDecoderMaxWindow(maxZstdWindow),
			zstd.WithDecoderMaxMemory(maxDecodedDirectory))
		if err != nil {
			panic("pile: create zstd directory decoder: " + err.Error())
		}
		dirDec = d
	})
	return dirDec
}

type encKey struct {
	level zstd.EncoderLevel
	fast  bool
}

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
func sharedEncoder(level CompressionLevel, fast bool) *zstd.Encoder {
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
	e, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(k.level),
		zstd.WithEncoderConcurrency(conc))
	if err != nil {
		panic("pile: create zstd encoder: " + err.Error()) // static options; cannot fail
	}
	encoders[k] = e
	return e
}

// sharedDecoder returns the process-wide decoder for whole solid bodies.
func sharedDecoder() *zstd.Decoder {
	bodyDecOnce.Do(func() {
		d, err := zstd.NewReader(nil,
			zstd.WithDecoderConcurrency(runtime.GOMAXPROCS(0)),
			zstd.WithDecoderMaxWindow(maxZstdWindow),
			zstd.WithDecoderMaxMemory(maxDecodedBody))
		if err != nil {
			panic("pile: create zstd decoder: " + err.Error())
		}
		bodyDec = d
	})
	return bodyDec
}

// sharedFrameDecoder returns the process-wide decoder for indexed frames,
// which are far smaller than a whole body and capped accordingly.
func sharedFrameDecoder() *zstd.Decoder {
	frameDecOnce.Do(func() {
		d, err := zstd.NewReader(nil,
			zstd.WithDecoderConcurrency(runtime.GOMAXPROCS(0)),
			zstd.WithDecoderMaxWindow(maxZstdWindow),
			zstd.WithDecoderMaxMemory(maxDecodedFrame))
		if err != nil {
			panic("pile: create zstd frame decoder: " + err.Error())
		}
		frameDec = d
	})
	return frameDec
}

// compressBody compresses a body with the shared encoder.
func compressBody(body []byte, level CompressionLevel, fast bool) []byte {
	return sharedEncoder(level, fast).EncodeAll(body, make([]byte, 0, len(body)/4))
}
