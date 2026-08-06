package format

// Bounded structural validation for little-endian NBT. gophertunnel's decoder
// allocates list and array backing storage from declared lengths before
// reading elements, so a few hostile bytes can request a multi-gigabyte
// allocation that no recover() can catch; it also recurses per nesting level,
// so deeply nested input can exhaust the stack. Every blob is therefore walked
// here first: each declared length must fit the bytes that remain, and nesting
// is capped. The walk allocates nothing and is a single linear pass.

// maxNBTDepth bounds compound/list nesting.
const maxNBTDepth = 64

// nbtWalker is a bounds-checked cursor over an NBT blob.
type nbtWalker struct {
	b   []byte
	off int
}

func (w *nbtWalker) remaining() int { return len(w.b) - w.off }

func (w *nbtWalker) skip(n int) error {
	if n < 0 || w.remaining() < n {
		return corruptf("nbt: truncated (want %d bytes, have %d)", n, w.remaining())
	}
	w.off += n
	return nil
}

func (w *nbtWalker) u8() (byte, error) {
	if w.remaining() < 1 {
		return 0, corruptf("nbt: truncated tag")
	}
	v := w.b[w.off]
	w.off++
	return v, nil
}

func (w *nbtWalker) u16() (int, error) {
	if w.remaining() < 2 {
		return 0, corruptf("nbt: truncated length")
	}
	v := int(w.b[w.off]) | int(w.b[w.off+1])<<8
	w.off += 2
	return v, nil
}

func (w *nbtWalker) i32() (int32, error) {
	if w.remaining() < 4 {
		return 0, corruptf("nbt: truncated length")
	}
	v := int32(uint32(w.b[w.off]) | uint32(w.b[w.off+1])<<8 |
		uint32(w.b[w.off+2])<<16 | uint32(w.b[w.off+3])<<24)
	w.off += 4
	return v, nil
}

// name skips a tag name (u16 length + bytes).
func (w *nbtWalker) name() error {
	n, err := w.u16()
	if err != nil {
		return err
	}
	return w.skip(n)
}

// minPayloadSize is the smallest number of bytes a payload of the given tag
// type can occupy. Used to bound element counts before iterating.
func minPayloadSize(t byte) (int, bool) {
	switch t {
	case tagByte:
		return 1, true
	case tagShort:
		return 2, true
	case tagInt, tagFloat:
		return 4, true
	case tagLong, tagDouble:
		return 8, true
	case tagString:
		return 2, true // length prefix only
	case tagByteArray, tagIntArray, tagLongArray:
		return 4, true // length prefix only
	case tagList:
		return 5, true // element type + count
	case tagCompound:
		return 1, true // TAG_End
	}
	return 0, false
}

// validateNBT checks that a blob is a structurally sound little-endian NBT
// compound whose declared lengths all fit within it.
func validateNBT(b []byte) error {
	w := &nbtWalker{b: b}
	t, err := w.u8()
	if err != nil {
		return err
	}
	if t != tagCompound {
		return corruptf("nbt: root tag type %d is not a compound", t)
	}
	if err := w.name(); err != nil {
		return err
	}
	if err := w.payload(tagCompound, 0); err != nil {
		return err
	}
	if w.remaining() != 0 {
		return corruptf("nbt: %d trailing bytes", w.remaining())
	}
	return nil
}

// payload validates one tag payload of the given type.
func (w *nbtWalker) payload(t byte, depth int) error {
	if depth > maxNBTDepth {
		return corruptf("nbt: nesting deeper than %d", maxNBTDepth)
	}
	switch t {
	case tagByte:
		return w.skip(1)
	case tagShort:
		return w.skip(2)
	case tagInt, tagFloat:
		return w.skip(4)
	case tagLong, tagDouble:
		return w.skip(8)
	case tagString:
		n, err := w.u16()
		if err != nil {
			return err
		}
		return w.skip(n)
	case tagByteArray, tagIntArray, tagLongArray:
		n, err := w.i32()
		if err != nil {
			return err
		}
		if n < 0 {
			return corruptf("nbt: negative array length %d", n)
		}
		elem := 1
		if t == tagIntArray {
			elem = 4
		} else if t == tagLongArray {
			elem = 8
		}
		if int64(n)*int64(elem) > int64(w.remaining()) {
			return corruptf("nbt: array of %d elements exceeds remaining input", n)
		}
		return w.skip(int(n) * elem)
	case tagList:
		et, err := w.u8()
		if err != nil {
			return err
		}
		n, err := w.i32()
		if err != nil {
			return err
		}
		if n < 0 {
			return corruptf("nbt: negative list length %d", n)
		}
		if n == 0 {
			return nil
		}
		minSize, ok := minPayloadSize(et)
		if !ok {
			return corruptf("nbt: unknown list element type %d", et)
		}
		// Bound the count before iterating: the declared elements must be
		// able to fit in what remains.
		if int64(n)*int64(minSize) > int64(w.remaining()) {
			return corruptf("nbt: list of %d elements (type %d) exceeds remaining input", n, et)
		}
		for range n {
			if err := w.payload(et, depth+1); err != nil {
				return err
			}
		}
		return nil
	case tagCompound:
		for {
			ct, err := w.u8()
			if err != nil {
				return err
			}
			if ct == tagEnd {
				return nil
			}
			if _, ok := minPayloadSize(ct); !ok {
				return corruptf("nbt: unknown tag type %d", ct)
			}
			if err := w.name(); err != nil {
				return err
			}
			if err := w.payload(ct, depth+1); err != nil {
				return err
			}
		}
	}
	return corruptf("nbt: unknown tag type %d", t)
}
