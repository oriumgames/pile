package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/oriumgames/pile"
)

// exportData is the data.json schema accompanying an exported structure.
type exportData struct {
	// Dimension the structure was cut from: overworld, nether or end.
	Dimension string `json:"dimension"`
	// Origin is the world position of the structure's minimum corner; import
	// pastes it back there.
	Origin [3]int `json:"origin"`

	Settings exportSettings `json:"settings"`
	Markers  []exportMarker `json:"markers,omitempty"`

	// UserData holds the world's metadata blob: inline JSON when the blob is
	// valid JSON, base64 otherwise.
	UserData       json.RawMessage `json:"userData,omitempty"`
	UserDataBase64 string          `json:"userDataBase64,omitempty"`
}

type exportSettings struct {
	Name               string `json:"name"`
	Spawn              [3]int `json:"spawn"`
	Time               int64  `json:"time"`
	TimeCycle          bool   `json:"timeCycle"`
	RainTime           int64  `json:"rainTime"`
	Raining            bool   `json:"raining"`
	ThunderTime        int64  `json:"thunderTime"`
	Thundering         bool   `json:"thundering"`
	WeatherCycle       bool   `json:"weatherCycle"`
	RequiredSleepTicks int64  `json:"requiredSleepTicks"`
	CurrentTick        int64  `json:"currentTick"`
	DefaultGameMode    int    `json:"defaultGameMode"`
	Difficulty         int    `json:"difficulty"`
	TickRange          int32  `json:"tickRange"`
}

type exportMarker struct {
	Name string      `json:"name"`
	Kind string      `json:"kind"`
	Pos  *[3]float64 `json:"pos,omitempty"`
	// Min and Max make the marker an area (§7.3). Omitted for a point, so an
	// exported file shows what the marker is rather than padding every one
	// with zeroed corners.
	Min   *[3]float64    `json:"min,omitempty"`
	Max   *[3]float64    `json:"max,omitempty"`
	Extra map[string]any `json:"extra,omitempty"`
}

func cmdExport(args []string) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	dimFlag := fs.String("dim", "overworld", "dimension to export")
	limit := addDecodeLimit(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return errors.New("usage: pile export <world-dir> <out-dir> [--dim overworld]")
	}
	dim, err := parseDim(*dimFlag)
	if err != nil {
		return err
	}
	p, err := pile.Open(fs.Arg(0), append(limit.providerOpts(), pile.ReadOnly())...)
	if err != nil {
		return err
	}
	defer p.Close()

	min, max, ok := pile.WorldBounds(p, dim)
	if !ok {
		return fmt.Errorf("world %s has no blocks in %s", fs.Arg(0), *dimFlag)
	}
	s, err := pile.ExtractStructure(p, dim, min, max)
	if err != nil {
		return err
	}

	outDir := fs.Arg(1)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	if err := s.Save(filepath.Join(outDir, "structure.pile")); err != nil {
		return err
	}

	set := p.Settings()
	gm, _ := world.GameModeID(set.DefaultGameMode)
	diff, _ := world.DifficultyID(set.Difficulty)
	data := exportData{
		Dimension: *dimFlag,
		Origin:    [3]int{min.X(), min.Y(), min.Z()},
		Settings: exportSettings{
			Name:               set.Name,
			Spawn:              [3]int{set.Spawn.X(), set.Spawn.Y(), set.Spawn.Z()},
			Time:               set.Time,
			TimeCycle:          set.TimeCycle,
			RainTime:           set.RainTime,
			Raining:            set.Raining,
			ThunderTime:        set.ThunderTime,
			Thundering:         set.Thundering,
			WeatherCycle:       set.WeatherCycle,
			RequiredSleepTicks: set.RequiredSleepTicks,
			CurrentTick:        set.CurrentTick,
			DefaultGameMode:    gm,
			Difficulty:         diff,
			TickRange:          set.TickRange,
		},
	}
	for _, m := range p.Markers() {
		em := exportMarker{Name: m.Name, Kind: m.Kind, Pos: m.Pos, Extra: m.Extra}
		if m.Bounds != nil {
			lo, hi := m.Bounds.Min, m.Bounds.Max
			em.Min, em.Max = &lo, &hi
		}
		data.Markers = append(data.Markers, em)
	}
	if ud := p.UserData(); len(ud) > 0 {
		if json.Valid(ud) {
			data.UserData = ud
		} else {
			data.UserDataBase64 = base64.StdEncoding.EncodeToString(ud)
		}
	}
	blob, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outDir, "data.json"), append(blob, '\n'), 0o644); err != nil {
		return err
	}
	d := s.Dimensions()
	fmt.Printf("exported %dx%dx%d structure + data.json to %s\n", d[0], d[1], d[2], outDir)
	return nil
}

func cmdImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	limit := addDecodeLimit(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return errors.New("usage: pile import <export-dir> <world-dir>")
	}
	srcDir, dstDir := fs.Arg(0), fs.Arg(1)
	if isPile(dstDir) {
		return fmt.Errorf("destination %s already contains a pile world; refusing to write into it", dstDir)
	}

	blob, err := os.ReadFile(filepath.Join(srcDir, "data.json"))
	if err != nil {
		return err
	}
	var data exportData
	if err := json.Unmarshal(blob, &data); err != nil {
		return fmt.Errorf("parse data.json: %w", err)
	}
	dim, err := parseDim(data.Dimension)
	if err != nil {
		return err
	}
	s, err := pile.LoadStructure(filepath.Join(srcDir, "structure.pile"), limit.structureOpts()...)
	if err != nil {
		return err
	}

	p, err := pile.Open(dstDir, limit.providerOpts()...)
	if err != nil {
		return err
	}
	if err := s.PasteInto(p, dim, cube.Pos{data.Origin[0], data.Origin[1], data.Origin[2]}); err != nil {
		_ = p.Close()
		return err
	}

	es := data.Settings
	set := p.Settings()
	set.Name = es.Name
	set.Spawn = cube.Pos{es.Spawn[0], es.Spawn[1], es.Spawn[2]}
	set.Time = es.Time
	set.TimeCycle = es.TimeCycle
	set.RainTime = es.RainTime
	set.Raining = es.Raining
	set.ThunderTime = es.ThunderTime
	set.Thundering = es.Thundering
	set.WeatherCycle = es.WeatherCycle
	set.RequiredSleepTicks = es.RequiredSleepTicks
	set.CurrentTick = es.CurrentTick
	if gm, ok := world.GameModeByID(es.DefaultGameMode); ok {
		set.DefaultGameMode = gm
	}
	if d, ok := world.DifficultyByID(es.Difficulty); ok {
		set.Difficulty = d
	}
	set.TickRange = es.TickRange
	p.SaveSettings(set)

	for _, m := range data.Markers {
		mk := pile.Marker{Name: m.Name, Kind: m.Kind, Pos: m.Pos, Extra: m.Extra}
		if m.Min != nil && m.Max != nil {
			mk.Bounds = &pile.Bounds{Min: *m.Min, Max: *m.Max}
		}
		p.SetMarker(mk)
	}
	switch {
	case len(data.UserData) > 0:
		p.SetUserData(data.UserData)
	case data.UserDataBase64 != "":
		ud, err := base64.StdEncoding.DecodeString(data.UserDataBase64)
		if err != nil {
			_ = p.Close()
			return fmt.Errorf("decode userDataBase64: %w", err)
		}
		p.SetUserData(ud)
	}
	if err := p.Close(); err != nil {
		return err
	}
	fmt.Printf("imported %s into %s\n", srcDir, dstDir)
	return nil
}
