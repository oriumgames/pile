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

// TestBlockStatesNeedsNoRegistry: the palette is names and properties, so
// listing it must not depend on resolving any of them. That is the whole point
// of the function -- a world whose blocks come from a behaviour pack cannot be
// read by a program that does not implement the pack, and its palette is
// exactly what tells you what to implement.
func TestBlockStatesNeedsNoRegistry(t *testing.T) {
	reg := testRegistry(t)
	file := encode(t, testWorld(t, reg), reg, CompressionNone)

	states, err := BlockStates(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) == 0 {
		t.Fatal("no block states reported for a world that has blocks")
	}
	// Every entry must be usable without a registry: a name, and a version
	// resolved from the palette's own rather than left as the zero that means
	// "ask the palette".
	for _, st := range states {
		if st.Name == "" {
			t.Errorf("a state came back with no name: %+v", st)
		}
		if st.Version == 0 {
			t.Errorf("%s came back with version 0, which means \"the palette's own\" and is not an answer", st.Name)
		}
	}

	// It must agree with the resolving path about what is in the file: every
	// state UnresolvedStates names has to appear here too, or one of the two is
	// reading a different palette.
	unresolved, err := UnresolvedStates(file, reg)
	if err != nil {
		t.Fatal(err)
	}
	named := map[string]bool{}
	for _, st := range states {
		named[st.Name] = true
	}
	for _, u := range unresolved {
		name, _, _ := strings.Cut(u, "[")
		if !named[name] {
			t.Errorf("UnresolvedStates reports %q, which BlockStates does not list", name)
		}
	}
}
