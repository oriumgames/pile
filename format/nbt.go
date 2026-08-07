package format

import (
	"fmt"
	"reflect"
	"slices"

	"github.com/sandertv/gophertunnel/minecraft/nbt"
)

// Pile stores NBT blobs in little-endian (Bedrock disk) encoding. Decoding
// uses gophertunnel's decoder. Encoding uses the deterministic encoder below:
// gophertunnel encodes Go maps in iteration order, which is random, and the
// pile format guarantees byte-identical output for identical content, so
// compound keys must be emitted in sorted order.

// NBT tag types (shared by all NBT encodings).
const (
	tagEnd       = 0
	tagByte      = 1
	tagShort     = 2
	tagInt       = 3
	tagLong      = 4
	tagFloat     = 5
	tagDouble    = 6
	tagByteArray = 7
	tagString    = 8
	tagList      = 9
	tagCompound  = 10
	tagIntArray  = 11
	tagLongArray = 12
)

// MarshalNBT encodes a compound deterministically in little-endian NBT: keys
// are emitted in sorted order, so identical content yields identical bytes.
// It accepts the Go types produced by UnmarshalNBT plus bool and int for
// convenience when maps are built in code.
func MarshalNBT(m map[string]any) ([]byte, error) { return marshalNBT(m) }

// UnmarshalNBT decodes a little-endian NBT compound.
func UnmarshalNBT(b []byte) (map[string]any, error) { return unmarshalNBT(b) }

// marshalNBT encodes a compound deterministically in little-endian NBT with an
// empty root name. It accepts the Go types produced by gophertunnel's decoder
// (so decode → encode round trips preserve tag types exactly) plus bool and
// int for convenience when maps are built in code.
func marshalNBT(m map[string]any) ([]byte, error) {
	w := &writer{b: make([]byte, 0, 128)}
	w.u8(tagCompound)
	w.u16(0) // empty root name
	var n nbtBudget
	if err := nbtCompound(w, m, 0, &n); err != nil {
		return nil, err
	}
	return w.bytes(), nil
}

// nbtBudget counts the containers a value encodes into, so the writer refuses
// what the reader refuses. §8 requires writers to reject content their own
// readers would, and the container ceiling was the one place that was left to
// the reader alone: the metadata blobs are re-validated after marshalling, but
// a block entity's or an entity's NBT went to the wire unchecked, so a runtime
// value carrying more than the ceiling produced a file this same release
// cannot read back. The accounting matches nbtvalidate.go exactly — every
// container nested inside another is charged one, the root is not.
type nbtBudget struct{ n int }

func (b *nbtBudget) charge(n int) error {
	b.n += n
	if b.n > maxNBTElements {
		return fmt.Errorf("pile: nbt decodes into more than %d containers", maxNBTElements)
	}
	return nil
}

// unmarshalNBT decodes a little-endian NBT compound. The blob is structurally
// validated first (bounded lengths and nesting, see nbtvalidate.go) because
// gophertunnel's decoder allocates from declared lengths before reading and
// recurses per nesting level; the recover is a second line of defence.
func unmarshalNBT(b []byte) (m map[string]any, err error) {
	if err := validateNBT(b); err != nil {
		return nil, err
	}
	defer func() {
		if r := recover(); r != nil {
			m, err = nil, corruptf("decode nbt: %v", r)
		}
	}()
	if err := nbt.UnmarshalEncoding(b, &m, nbt.LittleEndian); err != nil {
		return nil, corruptf("decode nbt: %v", err)
	}
	return m, nil
}

// nbtString writes an NBT string: uint16 length + bytes.
func nbtString(w *writer, s string) error {
	if len(s) > 0xFFFF {
		return fmt.Errorf("pile: nbt string longer than 65535 bytes")
	}
	w.u16(uint16(len(s)))
	w.raw([]byte(s))
	return nil
}

// nbtTagType returns the tag type a value encodes as.
func nbtTagType(v any) (byte, error) {
	switch v.(type) {
	case byte, int8, bool:
		return tagByte, nil
	case int16, uint16:
		return tagShort, nil
	case int32, uint32:
		return tagInt, nil
	case int64, uint64, int:
		return tagLong, nil
	case float32:
		return tagFloat, nil
	case float64:
		return tagDouble, nil
	case string:
		return tagString, nil
	case map[string]any:
		return tagCompound, nil
	case []any, []string, []float32, []float64, []int16, []map[string]any,
		[]byte, []int32, []int64:
		// Slices encode as lists and fixed-size arrays as NBT arrays, exactly
		// as gophertunnel does, so pile and dragonfly's own mcdb provider
		// write identical NBT for the same decoded value.
		return tagList, nil
	case nil:
		return 0, fmt.Errorf("pile: cannot encode nil nbt value")
	default:
		// gophertunnel decodes NBT byte/int/long arrays into fixed-size Go
		// arrays ([N]byte, [N]int32, [N]int64; entity UUIDs are [4]int32).
		if t, ok := arrayTagType(v); ok {
			return t, nil
		}
		return 0, fmt.Errorf("pile: cannot encode nbt value of type %T", v)
	}
}

// arrayTagType maps fixed-size Go arrays to NBT array tags.
func arrayTagType(v any) (byte, bool) {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Array {
		return 0, false
	}
	switch rv.Type().Elem().Kind() {
	case reflect.Uint8:
		return tagByteArray, true
	case reflect.Int32:
		return tagIntArray, true
	case reflect.Int64:
		return tagLongArray, true
	}
	return 0, false
}

// nbtPayload writes the payload of a value (no tag type, no name).
func nbtPayload(w *writer, v any, depth int, n *nbtBudget) error {
	// The check lives here rather than only in nbtCompound: a chain of nested
	// lists never reaches a compound, so it slipped past the limit and
	// produced a file this same release refuses to read.
	if depth > maxNBTDepth {
		return fmt.Errorf("pile: nbt nested deeper than %d", maxNBTDepth)
	}
	switch x := v.(type) {
	case byte:
		w.u8(x)
	case int8:
		w.u8(uint8(x))
	case bool:
		if x {
			w.u8(1)
		} else {
			w.u8(0)
		}
	case int16:
		w.u16(uint16(x))
	case uint16:
		w.u16(x)
	case int32:
		w.u32(uint32(x))
	case uint32:
		w.u32(x)
	case int64:
		w.u64(uint64(x))
	case uint64:
		w.u64(x)
	case int:
		w.u64(uint64(int64(x)))
	case float32:
		w.f32(x)
	case float64:
		w.f64(x)
	case string:
		return nbtString(w, x)
	case []byte:
		w.u8(emptyListType(len(x), tagByte))
		w.i32(int32(len(x)))
		w.raw(x)
	case []int32:
		w.u8(emptyListType(len(x), tagInt))
		w.i32(int32(len(x)))
		for _, e := range x {
			w.u32(uint32(e))
		}
	case []int64:
		w.u8(emptyListType(len(x), tagLong))
		w.i32(int32(len(x)))
		for _, e := range x {
			w.u64(uint64(e))
		}
	case map[string]any:
		return nbtCompound(w, x, depth, n)
	case []float32:
		w.u8(emptyListType(len(x), tagFloat))
		w.i32(int32(len(x)))
		for _, e := range x {
			w.f32(e)
		}
	case []float64:
		w.u8(emptyListType(len(x), tagDouble))
		w.i32(int32(len(x)))
		for _, e := range x {
			w.f64(e)
		}
	case []int16:
		w.u8(emptyListType(len(x), tagShort))
		w.i32(int32(len(x)))
		for _, e := range x {
			w.u16(uint16(e))
		}
	case []string:
		w.u8(emptyListType(len(x), tagString))
		w.i32(int32(len(x)))
		for _, e := range x {
			if err := nbtString(w, e); err != nil {
				return err
			}
		}
	case []map[string]any:
		w.u8(emptyListType(len(x), tagCompound))
		w.i32(int32(len(x)))
		// Every element is a compound and therefore a map, which is what the
		// reader charges for a list of compounds.
		if err := n.charge(len(x)); err != nil {
			return err
		}
		for _, e := range x {
			if err := nbtCompound(w, e, depth+1, n); err != nil {
				return err
			}
		}
	case []any:
		if len(x) == 0 {
			w.u8(tagEnd)
			w.i32(0)
			return nil
		}
		et, err := nbtTagType(x[0])
		if err != nil {
			return err
		}
		w.u8(et)
		w.i32(int32(len(x)))
		// A list of compounds or of lists decodes into one container per
		// element; a list of scalars decodes into one slice and costs nothing.
		if et == tagCompound || et == tagList {
			if err := n.charge(len(x)); err != nil {
				return err
			}
		}
		for _, e := range x {
			t, err := nbtTagType(e)
			if err != nil {
				return err
			}
			if t != et {
				return fmt.Errorf("pile: mixed element types in nbt list (%d vs %d)", t, et)
			}
			if err := nbtPayload(w, e, depth+1, n); err != nil {
				return err
			}
		}
	default:
		rv := reflect.ValueOf(v)
		if rv.Kind() == reflect.Array {
			ln := rv.Len()
			switch rv.Type().Elem().Kind() {
			case reflect.Uint8:
				w.i32(int32(ln))
				for i := range ln {
					w.u8(uint8(rv.Index(i).Uint()))
				}
				return nil
			case reflect.Int32:
				w.i32(int32(ln))
				for i := range ln {
					w.u32(uint32(rv.Index(i).Int()))
				}
				return nil
			case reflect.Int64:
				w.i32(int32(ln))
				for i := range ln {
					w.u64(uint64(rv.Index(i).Int()))
				}
				return nil
			}
		}
		return fmt.Errorf("pile: cannot encode nbt value of type %T", v)
	}
	return nil
}

// emptyListType returns the element type byte for a list: an empty list is
// always TAG_End, because a decoded empty list loses its element type and any
// other choice would make encode/decode/encode change the bytes.
func emptyListType(n int, elem byte) byte {
	if n == 0 {
		return tagEnd
	}
	return elem
}

// nbtCompound writes a compound payload with keys in sorted order.
func nbtCompound(w *writer, m map[string]any, depth int, n *nbtBudget) error {
	// The decoder refuses input nested deeper than maxNBTDepth, so refuse to
	// write it: the value would be unreadable by this same release.
	if depth > maxNBTDepth {
		return fmt.Errorf("pile: nbt nested deeper than %d", maxNBTDepth)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		t, err := nbtTagType(m[k])
		if err != nil {
			return fmt.Errorf("%w (key %q)", err, k)
		}
		// A compound or list field is a container of its own, charged the same
		// as an element of a list of compounds. The root compound is not
		// charged: it is the blob rather than something nested in it.
		if t == tagCompound || t == tagList {
			if err := n.charge(1); err != nil {
				return err
			}
		}
		w.u8(t)
		if err := nbtString(w, k); err != nil {
			return err
		}
		if err := nbtPayload(w, m[k], depth+1, n); err != nil {
			return err
		}
	}
	w.u8(tagEnd)
	return nil
}
