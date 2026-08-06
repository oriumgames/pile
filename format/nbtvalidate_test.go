package format

import "testing"

// TestNBTValidatorRejectsHostileLengths covers the allocation-amplification
// shapes: a list count that cannot fit, oversized arrays, and deep nesting.
func TestNBTValidatorRejectsHostileLengths(t *testing.T) {
	cases := map[string][]byte{
		// compound{ "x": list<compound> with 2147483647 elements }
		"huge compound list": {0x0a, 0, 0, 0x09, 0x01, 0, 'x', 0x0a, 0xff, 0xff, 0xff, 0x7f},
		// list of strings with a huge count
		"huge string list": {0x0a, 0, 0, 0x09, 0x01, 0, 'x', 0x08, 0xff, 0xff, 0xff, 0x7f},
		// int array claiming 2^30 entries
		"huge int array": {0x0a, 0, 0, 0x0b, 0x01, 0, 'x', 0x00, 0x00, 0x00, 0x40},
		// byte array longer than the blob
		"huge byte array": {0x0a, 0, 0, 0x07, 0x01, 0, 'x', 0xff, 0xff, 0xff, 0x7f},
		// negative list length
		"negative list": {0x0a, 0, 0, 0x09, 0x01, 0, 'x', 0x0a, 0xff, 0xff, 0xff, 0xff},
		// truncated string payload
		"truncated string": {0x0a, 0, 0, 0x08, 0x01, 0, 'x', 0xff, 0x00},
	}
	for name, b := range cases {
		if err := validateNBT(b); err == nil {
			t.Errorf("%s: validator accepted hostile blob", name)
		}
		if _, err := unmarshalNBT(b); err == nil {
			t.Errorf("%s: unmarshalNBT accepted hostile blob", name)
		}
	}

	// Deeply nested compounds must be rejected rather than recursing into
	// gophertunnel (which would exhaust the stack).
	deep := []byte{0x0a, 0, 0}
	for range maxNBTDepth + 10 {
		deep = append(deep, 0x0a, 0x01, 0, 'a')
	}
	for range maxNBTDepth + 11 {
		deep = append(deep, 0x00)
	}
	if err := validateNBT(deep); err == nil {
		t.Error("validator accepted excessively nested compound")
	}
}

// TestNBTValidatorAcceptsRealBlobs checks the validator does not reject
// anything the encoder produces.
func TestNBTValidatorAcceptsRealBlobs(t *testing.T) {
	blob, err := marshalNBT(map[string]any{
		"id": "minecraft:chest", "b": byte(1), "s": int16(2), "i": int32(3),
		"l": int64(4), "f": float32(5), "d": float64(6),
		"ba": []byte{1, 2, 3}, "ia": []int32{1, 2}, "la": []int64{9},
		"list":  []any{float32(1), float32(2)},
		"nest":  map[string]any{"deep": map[string]any{"x": int32(1)}},
		"comps": []map[string]any{{"a": int32(1)}, {"b": int32(2)}},
		"empty": []any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateNBT(blob); err != nil {
		t.Fatalf("validator rejected an encoder-produced blob: %v", err)
	}
	if _, err := unmarshalNBT(blob); err != nil {
		t.Fatal(err)
	}
}
