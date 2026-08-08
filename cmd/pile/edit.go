package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/oriumgames/pile"
)

// editData is everything pile edit puts in front of the editor: a world's
// metadata, and nothing that would make the file enormous.
//
// Chunks are deliberately absent. This is for changing a spawn point, renaming
// a marker or moving an area -- the things that are a nuisance to do any other
// way. Editing blocks as JSON would be a worse tool than the game.
// Every field is always present, including the empty ones. An editor is the
// only place a reader finds out what this command can change, and a key that
// vanishes when it holds nothing teaches that the world has no markers rather
// than that markers exist and this world has none. `null` and `[]` say the
// second, which is the true one.
//
// UserDataBase64 is the exception: it appears only when the blob is not JSON,
// because two keys for one value would invite setting both.
type editData struct {
	Settings exportSettings `json:"settings"`
	Markers  []exportMarker `json:"markers"`
	Border   *editBorder    `json:"border"`

	// UserData is the world's metadata blob: inline when it is valid JSON,
	// base64 otherwise, so an editor shows something readable when it can.
	UserData       json.RawMessage `json:"userData"`
	UserDataBase64 string          `json:"userDataBase64,omitempty"`
}

type editBorder struct {
	Min [2]int32 `json:"min"`
	Max [2]int32 `json:"max"`
}

func cmdEdit(args []string) error {
	fs := flag.NewFlagSet("edit", flag.ContinueOnError)
	print := fs.Bool("print", false, "write the JSON to stdout and make no changes")
	apply := fs.String("apply", "", "read the JSON from this file instead of opening an editor")
	noBackup := fs.Bool("no-backup", false, "skip the snapshot taken before writing")
	limit := addDecodeLimit(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: pile edit [--print] [--apply file.json] [--no-backup] [--max-decoded n] <world>")
	}
	dir := fs.Arg(0)

	p, err := pile.Open(dir, append(limit.providerOpts(), pile.ReadOnly())...)
	if err != nil {
		return err
	}
	before := readEditData(p)
	_ = p.Close()

	original, err := json.MarshalIndent(before, "", "  ")
	if err != nil {
		return err
	}
	original = append(original, '\n')
	if *print {
		_, err := os.Stdout.Write(original)
		return err
	}

	edited := original
	if *apply != "" {
		if edited, err = os.ReadFile(*apply); err != nil {
			return err
		}
	} else if edited, err = editInEditor(original); err != nil {
		return err
	}

	if bytes.Equal(bytes.TrimSpace(original), bytes.TrimSpace(edited)) {
		fmt.Println("no changes")
		return nil
	}
	var after editData
	dec := json.NewDecoder(bytes.NewReader(edited))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&after); err != nil {
		// Keep the edit rather than discarding it: the whole cost of a typo
		// should be fixing the typo.
		kept := filepath.Join(os.TempDir(), "pile-edit-failed.json")
		if werr := os.WriteFile(kept, edited, 0o600); werr == nil {
			return fmt.Errorf("%w\nyour edit was kept at %s; fix it and re-run with --apply %s", err, kept, kept)
		}
		return err
	}

	return applyEditData(dir, after, before, *noBackup, limit)
}

// readEditData collects a world's metadata into the editable form.
func readEditData(p *pile.Provider) editData {
	// Markers start as an empty slice rather than nil so the JSON shows [].
	d := editData{Settings: exportSettingsOf(p.Settings()), Markers: []exportMarker{}}
	for _, m := range p.Markers() {
		em := exportMarker{Name: m.Name, Kind: m.Kind, Pos: m.Pos, Extra: m.Extra}
		if m.Bounds != nil {
			lo, hi := m.Bounds.Min, m.Bounds.Max
			em.Min, em.Max = &lo, &hi
		}
		d.Markers = append(d.Markers, em)
	}
	if b := p.Border(); b != nil {
		d.Border = &editBorder{Min: b.Min, Max: b.Max}
	}
	if ud := p.UserData(); len(ud) > 0 {
		if json.Valid(ud) {
			d.UserData = ud
		} else {
			d.UserDataBase64 = base64.StdEncoding.EncodeToString(ud)
		}
	}
	if len(d.UserData) == 0 {
		// Explicit null rather than an omitted key, and rather than {}: an
		// empty object is a blob whose content is an empty object, which is not
		// the same as having none, and applying it would store two bytes where
		// there were none.
		d.UserData = json.RawMessage("null")
	}
	return d
}

// applyEditData writes the edited metadata back. Chunks are never read or
// rewritten by this: the provider is opened, the metadata replaced, and saved.
func applyEditData(dir string, d, before editData, noBackup bool, limit decodeLimit) error {
	p, err := pile.Open(dir, limit.providerOpts()...)
	if err != nil {
		return err
	}
	if !noBackup {
		// Metadata is small and easy to get wrong, and an edit that turns out
		// to be the wrong one has nothing to return to otherwise.
		if err := p.Snapshot("pre-edit"); err != nil {
			_ = p.Close()
			return fmt.Errorf("taking the pre-edit snapshot: %w", err)
		}
	}

	s := p.Settings()
	applyExportSettings(s, d.Settings)
	p.SaveSettings(s)

	// Markers are replaced wholesale rather than merged: the editor showed the
	// whole list, so deleting a line has to mean deleting a marker.
	for _, existing := range p.Markers() {
		p.RemoveMarker(existing.Name)
	}
	for _, m := range d.Markers {
		mk := pile.Marker{Name: m.Name, Kind: m.Kind, Pos: m.Pos, Extra: m.Extra}
		if m.Min != nil && m.Max != nil {
			mk.Bounds = &pile.Bounds{Min: *m.Min, Max: *m.Max}
		}
		p.SetMarker(mk)
	}

	if d.Border != nil {
		p.SetBorder(&pile.Border{Min: d.Border.Min, Max: d.Border.Max})
	} else {
		p.SetBorder(nil)
	}

	switch {
	case isJSONNull(d.UserData):
		p.SetUserData(nil)
	case len(d.UserData) > 0:
		// Write the bytes that were there if the edit did not change them.
		// MarshalIndent reformats an embedded RawMessage, so a user data blob
		// makes a round trip through this command as re-indented JSON -- which
		// would rewrite an application's own bytes, and move the world's
		// ContentHash, because somebody renamed a marker.
		if sameJSON(before.UserData, d.UserData) {
			p.SetUserData(before.UserData)
			break
		}
		p.SetUserData(d.UserData)
	case d.UserDataBase64 != "":
		b, err := base64.StdEncoding.DecodeString(d.UserDataBase64)
		if err != nil {
			_ = p.Close()
			return fmt.Errorf("userDataBase64: %w", err)
		}
		p.SetUserData(b)
	default:
		p.SetUserData(nil)
	}

	// Close is where the write happens, and where the §7 schemas are checked:
	// a marker with no position, an area whose corners are the wrong way round,
	// a double that is NaN. A refusal here means nothing was written.
	if err := p.Close(); err != nil {
		return fmt.Errorf("the edit was refused and the world is unchanged: %w", err)
	}
	fmt.Printf("%s updated\n", dir)
	return nil
}

// editInEditor writes the JSON to a temporary file, opens it in the user's
// editor, and returns what came back.
func editInEditor(content []byte) ([]byte, error) {
	f, err := os.CreateTemp("", "pile-edit-*.json")
	if err != nil {
		return nil, err
	}
	path := f.Name()
	defer func() { _ = os.Remove(path) }()
	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}

	name, args := editorCommand()
	cmd := exec.Command(name, append(args, path)...)
	// The editor owns the terminal for as long as it runs, which is what makes
	// this usable with vi rather than only with editors that open a window.
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s: %w (nothing was written)", name, err)
	}
	return os.ReadFile(path)
}

// editorCommand picks the editor: VISUAL, then EDITOR, then something that
// exists. A value may carry arguments, as "code --wait" does, and an editor
// that returns before the file is saved would silently discard the edit.
func editorCommand() (string, []string) {
	for _, env := range []string{"VISUAL", "EDITOR"} {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			parts := strings.Fields(v)
			return parts[0], parts[1:]
		}
	}
	if runtime.GOOS == "windows" {
		return "notepad", nil
	}
	for _, cand := range []string{"hx", "vim", "vi", "nano"} {
		if _, err := exec.LookPath(cand); err == nil {
			return cand, nil
		}
	}
	return "vi", nil
}

// exportSettingsOf and applyExportSettings convert between world.Settings and
// the JSON form. They are here rather than in export.go because edit needs both
// directions and export only ever needed one.
func exportSettingsOf(s *world.Settings) exportSettings {
	gm, _ := world.GameModeID(s.DefaultGameMode)
	diff, _ := world.DifficultyID(s.Difficulty)
	return exportSettings{
		Name:               s.Name,
		Spawn:              [3]int{s.Spawn.X(), s.Spawn.Y(), s.Spawn.Z()},
		Time:               s.Time,
		TimeCycle:          s.TimeCycle,
		RainTime:           s.RainTime,
		Raining:            s.Raining,
		ThunderTime:        s.ThunderTime,
		Thundering:         s.Thundering,
		WeatherCycle:       s.WeatherCycle,
		RequiredSleepTicks: s.RequiredSleepTicks,
		CurrentTick:        s.CurrentTick,
		DefaultGameMode:    gm,
		Difficulty:         diff,
		TickRange:          s.TickRange,
	}
}

// applyExportSettings is the other direction. A game mode or difficulty the
// runtime does not know is left alone rather than reset: an unrecognised number
// in the file should not silently move a world to survival.
func applyExportSettings(s *world.Settings, es exportSettings) {
	s.Name = es.Name
	s.Spawn = cube.Pos{es.Spawn[0], es.Spawn[1], es.Spawn[2]}
	s.Time = es.Time
	s.TimeCycle = es.TimeCycle
	s.RainTime = es.RainTime
	s.Raining = es.Raining
	s.ThunderTime = es.ThunderTime
	s.Thundering = es.Thundering
	s.WeatherCycle = es.WeatherCycle
	s.RequiredSleepTicks = es.RequiredSleepTicks
	s.CurrentTick = es.CurrentTick
	if gm, ok := world.GameModeByID(es.DefaultGameMode); ok {
		s.DefaultGameMode = gm
	}
	if d, ok := world.DifficultyByID(es.Difficulty); ok {
		s.Difficulty = d
	}
	s.TickRange = es.TickRange
}

// sameJSON reports whether two JSON documents carry the same content, ignoring
// the whitespace an editor or a marshaller may have moved around.
func sameJSON(a, b []byte) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	var ca, cb bytes.Buffer
	if json.Compact(&ca, a) != nil || json.Compact(&cb, b) != nil {
		return false
	}
	return bytes.Equal(ca.Bytes(), cb.Bytes())
}

// isJSONNull reports whether a raw message is the literal null, which is how
// the editable form says "this world has no user data".
func isJSONNull(b []byte) bool {
	return string(bytes.TrimSpace(b)) == "null"
}
