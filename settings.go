package pile

import (
	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
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
