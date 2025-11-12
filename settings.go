package pile

import (
	"bytes"
	"fmt"

	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/sandertv/gophertunnel/minecraft/nbt"
)

// settingsToInternal converts world.Settings to internal Settings.
func settingsToInternal(s *world.Settings) *Settings {
	gameModeID, _ := world.GameModeID(s.DefaultGameMode)
	difficultyID, _ := world.DifficultyID(s.Difficulty)

	return &Settings{
		Name:            s.Name,
		Spawn:           s.Spawn,
		Time:            s.Time,
		TimeCycle:       s.TimeCycle,
		RainTime:        s.RainTime,
		Raining:         s.Raining,
		ThunderTime:     s.ThunderTime,
		Thundering:      s.Thundering,
		WeatherCycle:    s.WeatherCycle,
		CurrentTick:     s.CurrentTick,
		DefaultGameMode: int32(gameModeID),
		Difficulty:      int32(difficultyID),
	}
}

// settingsFromInternal converts internal Settings to world.Settings.
func settingsFromInternal(s *Settings) *world.Settings {
	gameMode, _ := world.GameModeByID(int(s.DefaultGameMode))
	difficulty, _ := world.DifficultyByID(int(s.Difficulty))

	return &world.Settings{
		Name:            s.Name,
		Spawn:           s.Spawn,
		Time:            s.Time,
		TimeCycle:       s.TimeCycle,
		RainTime:        s.RainTime,
		Raining:         s.Raining,
		ThunderTime:     s.ThunderTime,
		Thundering:      s.Thundering,
		WeatherCycle:    s.WeatherCycle,
		CurrentTick:     s.CurrentTick,
		DefaultGameMode: gameMode,
		Difficulty:      difficulty,
	}
}

// Settings is an internal representation of world settings for serialization.
type Settings struct {
	Name            string
	Spawn           cube.Pos
	Time            int64
	TimeCycle       bool
	RainTime        int64
	Raining         bool
	ThunderTime     int64
	Thundering      bool
	WeatherCycle    bool
	CurrentTick     int64
	DefaultGameMode int32
	Difficulty      int32
}

// encodeSettings encodes world settings to bytes.
func encodeSettings(s *Settings) []byte {
	buf := new(bytes.Buffer)
	// Use NBT encoding for settings
	data := map[string]any{
		"name":            s.Name,
		"spawnX":          int32(s.Spawn.X()),
		"spawnY":          int32(s.Spawn.Y()),
		"spawnZ":          int32(s.Spawn.Z()),
		"time":            s.Time,
		"timeCycle":       s.TimeCycle,
		"rainTime":        s.RainTime,
		"raining":         s.Raining,
		"thunderTime":     s.ThunderTime,
		"thundering":      s.Thundering,
		"weatherCycle":    s.WeatherCycle,
		"currentTick":     s.CurrentTick,
		"defaultGameMode": int32(s.DefaultGameMode),
		"difficulty":      int32(s.Difficulty),
	}

	_ = nbt.NewEncoder(buf).Encode(data)
	return buf.Bytes()
}

// decodeSettings decodes world settings from bytes.
func decodeSettings(data []byte, s *Settings) error {
	if len(data) == 0 {
		return fmt.Errorf("no settings data")
	}

	var m map[string]any
	if err := nbt.NewDecoder(bytes.NewReader(data)).Decode(&m); err != nil {
		return err
	}

	if name, ok := m["name"].(string); ok {
		s.Name = name
	}
	if x, ok := m["spawnX"].(int32); ok {
		if y, ok := m["spawnY"].(int32); ok {
			if z, ok := m["spawnZ"].(int32); ok {
				s.Spawn = cube.Pos{int(x), int(y), int(z)}
			}
		}
	}
	if t, ok := m["time"].(int64); ok {
		s.Time = t
	}
	if tc, ok := m["timeCycle"].(uint8); ok {
		s.TimeCycle = tc != 0
	}
	if rt, ok := m["rainTime"].(int64); ok {
		s.RainTime = rt
	}
	if r, ok := m["raining"].(uint8); ok {
		s.Raining = r != 0
	}
	if tt, ok := m["thunderTime"].(int64); ok {
		s.ThunderTime = tt
	}
	if t, ok := m["thundering"].(uint8); ok {
		s.Thundering = t != 0
	}
	if wc, ok := m["weatherCycle"].(uint8); ok {
		s.WeatherCycle = wc != 0
	}
	if ct, ok := m["currentTick"].(int64); ok {
		s.CurrentTick = ct
	}
	if gm, ok := m["defaultGameMode"].(int32); ok {
		s.DefaultGameMode = gm
	}
	if d, ok := m["difficulty"].(int32); ok {
		s.Difficulty = d
	}

	return nil
}
