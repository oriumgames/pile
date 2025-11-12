package format

import (
	"encoding/binary"
	"fmt"
	"io"

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

// Read reads a Pile world from a reader.
func Read(r io.Reader) (*World, error) {
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
	return DecodeWorld(dataReader)
}

// Write writes a Pile world to a writer with default compression.
func Write(w io.Writer, world *World) error {
	return WriteWithCompression(w, world, CompressionLevelDefault)
}

// WriteWithCompression writes a Pile world to a writer with a specific compression level.
func WriteWithCompression(w io.Writer, world *World, compressionLevel CompressionLevel) error {
	buf := newBuffer()

	// Encode world data
	EncodeWorld(buf, world)
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

// WriteStreaming writes a Pile world to a writer using a streaming approach.
// It writes the world header first, followed by world data streamed chunk-by-chunk.
// For compressed output, a streaming Zstd encoder is used.
// Note: The uncompressed data length in the header is written as a placeholder and not validated by the decoder.
func WriteStreaming(w io.Writer, world *World, compressionLevel CompressionLevel) error {
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
		EncodeChunk(cb, c, world.MinSection, world.MaxSection)
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
