package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/df-mc/dragonfly/server/world"
	"github.com/oriumgames/pile"
	"github.com/oriumgames/pile/format"
)

// editRoundTrip writes d as JSON and applies it, the way --apply does.
func editRoundTrip(t *testing.T, dir string, d editData, extra ...string) error {
	t.Helper()
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(t.TempDir(), "edit.json")
	if err := os.WriteFile(f, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return cmdEdit(append(append([]string{}, extra...), "--apply", f, dir))
}

func editRead(t *testing.T, dir string) editData {
	t.Helper()
	p, err := pile.Open(dir, pile.ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	return readEditData(p)
}

// TestEditRoundTrip drives the whole shape: read, change every kind of field,
// apply, read back.
//
// The typed structs are the point. NBT distinguishes an int32 from an int64 and
// JSON has one number type, so a round trip through a raw NBT map turns every
// integer property into a float64 -- which is how a custom block with a numeric
// property crashed a server earlier in this project's life. Settings and markers
// go through fields with declared types, so the types survive.
func TestEditRoundTrip(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	buildWorldA(t, dir)

	before := editRead(t, dir)
	if before.Settings.Name != "A" {
		t.Fatalf("fixture settings did not survive the build: %+v", before.Settings)
	}

	d := before
	d.Settings.Name = "Edited"
	d.Settings.Spawn = [3]int{10, 70, -20}
	d.Settings.Time = 12345
	d.Settings.TickRange = 9
	d.Markers = []exportMarker{
		{Name: "arena", Kind: "pvp", Min: &[3]float64{-10, 60, -10}, Max: &[3]float64{10, 70, 10}},
		{Name: "hub", Kind: "spawn", Pos: &[3]float64{1, 65, 2}},
	}
	d.Border = &editBorder{Min: [2]int32{-500, -500}, Max: [2]int32{500, 500}}
	d.UserData = json.RawMessage(`{"stage":"beta"}`)

	if err := editRoundTrip(t, dir, d); err != nil {
		t.Fatal(err)
	}

	after := editRead(t, dir)
	if after.Settings.Name != "Edited" || after.Settings.Spawn != [3]int{10, 70, -20} ||
		after.Settings.Time != 12345 || after.Settings.TickRange != 9 {
		t.Errorf("settings did not round trip: %+v", after.Settings)
	}
	if after.Border == nil || after.Border.Min != [2]int32{-500, -500} || after.Border.Max != [2]int32{500, 500} {
		t.Errorf("border did not round trip: %+v", after.Border)
	}
	// Compared as content: the blob is shown inline, so marshalling and an
	// editor both reflow it. What must survive is what it says.
	if !sameJSON(after.UserData, []byte(`{"stage":"beta"}`)) {
		t.Errorf("user data did not round trip: %s", after.UserData)
	}
	byName := map[string]exportMarker{}
	for _, m := range after.Markers {
		byName[m.Name] = m
	}
	if len(byName) != 2 {
		t.Fatalf("markers did not round trip: %+v", after.Markers)
	}
	if a := byName["arena"]; a.Min == nil || a.Max == nil || *a.Min != [3]float64{-10, 60, -10} {
		t.Errorf("the area marker came back as %+v", a)
	}
	if h := byName["hub"]; h.Pos == nil || *h.Pos != [3]float64{1, 65, 2} || h.Min != nil || h.Max != nil {
		t.Errorf("the point marker came back as %+v", h)
	}

	// The chunks are not this command's business and must be untouched.
	q, err := pile.Open(dir, pile.Registry(reg), pile.ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for range q.Columns(world.Overworld) {
		n++
	}
	_ = q.Close()
	if n != 3 {
		t.Errorf("the world has %d columns after an edit, want 3", n)
	}
}

// TestEditRemovesWhatTheEditorRemoved: the editor is shown the whole list, so
// deleting a marker there has to delete the marker. Merging instead would make
// removal impossible through the one interface the command offers.
func TestEditRemovesWhatTheEditorRemoved(t *testing.T) {
	dir := t.TempDir()
	buildWorldA(t, dir)

	d := editRead(t, dir)
	d.Markers = []exportMarker{
		{Name: "a", Kind: "x", Pos: &[3]float64{0, 0, 0}},
		{Name: "b", Kind: "x", Pos: &[3]float64{1, 1, 1}},
	}
	d.Border = &editBorder{Min: [2]int32{-1, -1}, Max: [2]int32{1, 1}}
	if err := editRoundTrip(t, dir, d); err != nil {
		t.Fatal(err)
	}

	d = editRead(t, dir)
	d.Markers = d.Markers[:1]
	d.Border = nil
	if err := editRoundTrip(t, dir, d); err != nil {
		t.Fatal(err)
	}

	after := editRead(t, dir)
	if len(after.Markers) != 1 {
		t.Errorf("removing a marker from the JSON left %d behind", len(after.Markers))
	}
	if after.Border != nil {
		t.Errorf("removing the border from the JSON left %+v", after.Border)
	}
}

// TestEditRefusedLeavesTheWorldAlone: the §7 schemas are checked when the world
// is written, so a bad edit has to fail with nothing changed rather than half
// applied.
func TestEditRefusedLeavesTheWorldAlone(t *testing.T) {
	dir := t.TempDir()
	buildWorldA(t, dir)

	good := editRead(t, dir)
	good.Settings.Name = "before the bad edit"
	if err := editRoundTrip(t, dir, good); err != nil {
		t.Fatal(err)
	}

	bad := editRead(t, dir)
	bad.Settings.Name = "should not survive"
	// An area whose corners are the wrong way round: one region with two
	// encodings, which §7.3 refuses rather than repairs.
	bad.Markers = append(bad.Markers, exportMarker{
		Name: "inverted", Kind: "x",
		Min: &[3]float64{10, 10, 10}, Max: &[3]float64{0, 0, 0},
	})
	err := editRoundTrip(t, dir, bad, "--no-backup")
	if err == nil {
		t.Fatal("an inverted area was accepted")
	}
	if !strings.Contains(err.Error(), "exceeds max") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}

	after := editRead(t, dir)
	if after.Settings.Name != "before the bad edit" {
		t.Errorf("a refused edit changed the world: name is %q", after.Settings.Name)
	}
	for _, m := range after.Markers {
		if m.Name == "inverted" {
			t.Error("a refused edit left its marker behind")
		}
	}
}

// TestEditTakesASnapshot: metadata is small and easy to get wrong, so the
// default has to leave something to go back to.
func TestEditTakesASnapshot(t *testing.T) {
	dir := t.TempDir()
	buildWorldA(t, dir)

	d := editRead(t, dir)
	d.Settings.Name = "changed"
	if err := editRoundTrip(t, dir, d); err != nil {
		t.Fatal(err)
	}
	p, err := pile.Open(dir, pile.ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	names, err := p.Snapshots()
	_ = p.Close()
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, n := range names {
		if n == "pre-edit" {
			found = true
		}
	}
	if !found {
		t.Errorf("no pre-edit snapshot; snapshots = %v", names)
	}
}

func TestEditorCommand(t *testing.T) {
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "")
	if name, _ := editorCommand(); name == "" {
		t.Error("no editor chosen when neither VISUAL nor EDITOR is set")
	}
	// A value may carry arguments: "code --wait" is the common case, and an
	// editor that returns before the file is saved discards the edit silently.
	t.Setenv("EDITOR", "code --wait")
	name, args := editorCommand()
	if name != "code" || len(args) != 1 || args[0] != "--wait" {
		t.Errorf("editorCommand = %q %v, want code [--wait]", name, args)
	}
	t.Setenv("VISUAL", "mate")
	if name, _ := editorCommand(); name != "mate" {
		t.Errorf("VISUAL did not win over EDITOR: got %q", name)
	}
}

// TestEditLeavesUntouchedUserDataAlone: a blob the edit did not change must come
// out of this command as the same bytes it went in as.
//
// It is shown inline so it can be edited, and marshalling reflows it, so the
// naive implementation rewrites an application's own bytes -- and moves the
// world's ContentHash -- because somebody renamed a marker. The blob is
// therefore written back only when its content actually changed.
func TestEditLeavesUntouchedUserDataAlone(t *testing.T) {
	dir := t.TempDir()
	buildWorldA(t, dir)

	// Deliberately compact, which is not how MarshalIndent would render it.
	const blob = `{"a":1,"b":[2,3],"c":{"d":"e"}}`
	p, err := pile.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	p.SetUserData([]byte(blob))
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	d := editRead(t, dir)
	d.Settings.Name = "renamed, nothing to do with the blob"
	if err := editRoundTrip(t, dir, d); err != nil {
		t.Fatal(err)
	}

	q, err := pile.Open(dir, pile.ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	got := string(q.UserData())
	_ = q.Close()
	if got != blob {
		t.Errorf("an untouched user data blob was rewritten:\n got  %s\n want %s", got, blob)
	}
}

// TestEditWritesChangedUserData is the other half: the preservation above must
// not turn into never writing the blob at all.
func TestEditWritesChangedUserData(t *testing.T) {
	dir := t.TempDir()
	buildWorldA(t, dir)

	p, err := pile.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	p.SetUserData([]byte(`{"stage":"alpha"}`))
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	d := editRead(t, dir)
	d.UserData = json.RawMessage(`{"stage":"beta"}`)
	if err := editRoundTrip(t, dir, d); err != nil {
		t.Fatal(err)
	}
	if got := editRead(t, dir).UserData; !sameJSON(got, []byte(`{"stage":"beta"}`)) {
		t.Errorf("a changed user data blob was not written: %s", got)
	}
}

// TestMetadataDivergence: a world's dimension files each carry a copy of the
// metadata, and the provider reads the overworld's. Nothing in the format
// requires the copies to agree -- a reader only ever sees one file -- so a
// divergence is valid, loads, and silently ignores every copy but one. verify
// is the only place that sees all of them at once.
func TestMetadataDivergence(t *testing.T) {
	meta := func(settings, markers string) *format.Meta {
		return &format.Meta{Settings: []byte(settings), Markers: []byte(markers)}
	}
	for _, c := range []struct {
		name  string
		files map[string]*format.Meta
		want  []string
	}{
		{"a lone file cannot diverge", map[string]*format.Meta{"overworld.pile": meta("a", "b")}, nil},
		{
			"identical copies are silent",
			map[string]*format.Meta{
				"overworld.pile": meta("a", "b"),
				"nether.pile":    meta("a", "b"),
			},
			nil,
		},
		{
			"compared against the overworld, not the first alphabetically",
			map[string]*format.Meta{
				"overworld.pile": meta("a", "b"),
				"end.pile":       meta("a", "b"),
				"nether.pile":    meta("CHANGED", "b"),
			},
			[]string{"nether.pile differs from overworld.pile: settings"},
		},
		{
			"every differing field is named",
			map[string]*format.Meta{
				"overworld.pile": meta("a", "b"),
				"nether.pile":    meta("x", "y"),
			},
			[]string{"nether.pile differs from overworld.pile: settings, markers"},
		},
		{
			"with no overworld the first file is the reference",
			map[string]*format.Meta{
				"end.pile":    meta("a", "b"),
				"nether.pile": meta("x", "b"),
			},
			[]string{"nether.pile differs from end.pile: settings"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := metadataDivergence(c.files)
			if len(got) != len(c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("got %q, want %q", got[i], c.want[i])
				}
			}
		})
	}
}
