package format

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func TestUnresolvedStates(t *testing.T) {
	reg := testRegistry(t)
	d := testWorld(t, reg)
	file := encode(t, d, reg, CompressionNone)

	// A clean file resolves fully.
	unresolved, err := UnresolvedStates(file, reg)
	if err != nil {
		t.Fatal(err)
	}
	if len(unresolved) != 0 {
		t.Fatalf("clean file reports unresolved states: %v", unresolved)
	}

	// Corrupt a palette entry name in the uncompressed body (equal length so
	// offsets stay valid), then fix the body hash in the footer.
	bad := bytes.Clone(file)
	body := bad[headerSize : len(bad)-footerSize]
	idx := bytes.Index(body, []byte("minecraft:dirt"))
	if idx < 0 {
		t.Fatal("dirt not found in body")
	}
	copy(body[idx:], "minecraft:d1rt")
	binary.LittleEndian.PutUint64(bad[len(bad)-footerSize:],
		checkpointHash(bad[:headerSize], body, bad[len(bad)-footerSize+8:]))

	unresolved, err = UnresolvedStates(bad, reg)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range unresolved {
		if strings.Contains(s, "minecraft:d1rt") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected minecraft:d1rt in unresolved states, got %v", unresolved)
	}

	// The corrupted file still decodes (placeholder block), not errors.
	if _, err := ReadWorld(bad, reg); err != nil {
		t.Fatalf("file with unknown state failed to decode: %v", err)
	}
}
