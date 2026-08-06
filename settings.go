package pile

import (
	"fmt"
	"math"

	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/oriumgames/pile/format"
)

// settingsToNBT encodes the complete world.Settings as a deterministic NBT
// blob for the file's meta block. Keys the running build does not know are
// carried over from extra, so an older server does not erase settings a newer
// one wrote.
func settingsToNBT(s *world.Settings, extra map[string]any) ([]byte, error) {
	// world.Settings embeds a mutex and is mutated by dragonfly's tick loop;
	// snapshot under the lock.
	s.Lock()
	defer s.Unlock()
	gameMode, _ := world.GameModeID(s.DefaultGameMode)
	difficulty, _ := world.DifficultyID(s.Difficulty)
	for i, v := range []int{s.Spawn.X(), s.Spawn.Y(), s.Spawn.Z()} {
		if v < math.MinInt32 || v > math.MaxInt32 {
			return nil, fmt.Errorf("pile: spawn coordinate %d (axis %d) does not fit the format's 32-bit field", v, i)
		}
	}
	m := map[string]any{
		"name":               s.Name,
		"spawnX":             int32(s.Spawn.X()),
		"spawnY":             int32(s.Spawn.Y()),
		"spawnZ":             int32(s.Spawn.Z()),
		"time":               s.Time,
		"timeCycle":          s.TimeCycle,
		"rainTime":           s.RainTime,
		"raining":            s.Raining,
		"thunderTime":        s.ThunderTime,
		"thundering":         s.Thundering,
		"weatherCycle":       s.WeatherCycle,
		"requiredSleepTicks": s.RequiredSleepTicks,
		"currentTick":        s.CurrentTick,
		"defaultGameMode":    int32(gameMode),
		"difficulty":         int32(difficulty),
		"tickRange":          s.TickRange,
	}
	for k, v := range extra {
		if _, known := m[k]; !known {
			m[k] = v
		}
	}
	return format.MarshalNBT(m)
}

// settingsFromNBT decodes a settings blob. Missing fields keep their default
// values; a nil/empty blob returns defaults.
// settingsFromNBT decodes a settings blob, also returning the keys this build
// does not recognise so a later save can preserve them.
func settingsFromNBT(b []byte) (*world.Settings, map[string]any, error) {
	s := defaultSettings()
	if len(b) == 0 {
		return s, nil, nil
	}
	m, err := format.UnmarshalNBT(b)
	if err != nil {
		return nil, nil, err
	}
	if v, ok := m["name"].(string); ok {
		s.Name = v
	}
	var spawn cube.Pos
	if x, ok := m["spawnX"].(int32); ok {
		spawn[0] = int(x)
	}
	if y, ok := m["spawnY"].(int32); ok {
		spawn[1] = int(y)
	}
	if z, ok := m["spawnZ"].(int32); ok {
		spawn[2] = int(z)
	}
	s.Spawn = spawn
	if v, ok := m["time"].(int64); ok {
		s.Time = v
	}
	if v, ok := m["timeCycle"].(byte); ok {
		s.TimeCycle = v != 0
	}
	if v, ok := m["rainTime"].(int64); ok {
		s.RainTime = v
	}
	if v, ok := m["raining"].(byte); ok {
		s.Raining = v != 0
	}
	if v, ok := m["thunderTime"].(int64); ok {
		s.ThunderTime = v
	}
	if v, ok := m["thundering"].(byte); ok {
		s.Thundering = v != 0
	}
	if v, ok := m["weatherCycle"].(byte); ok {
		s.WeatherCycle = v != 0
	}
	if v, ok := m["requiredSleepTicks"].(int64); ok {
		s.RequiredSleepTicks = v
	}
	if v, ok := m["currentTick"].(int64); ok {
		s.CurrentTick = v
	}
	if v, ok := m["defaultGameMode"].(int32); ok {
		if gm, ok := world.GameModeByID(int(v)); ok {
			s.DefaultGameMode = gm
		}
	}
	if v, ok := m["difficulty"].(int32); ok {
		if d, ok := world.DifficultyByID(int(v)); ok {
			s.Difficulty = d
		}
	}
	if v, ok := m["tickRange"].(int32); ok {
		s.TickRange = v
	}
	known := map[string]bool{
		"name": true, "spawnX": true, "spawnY": true, "spawnZ": true, "time": true,
		"timeCycle": true, "rainTime": true, "raining": true, "thunderTime": true,
		"thundering": true, "weatherCycle": true, "requiredSleepTicks": true,
		"currentTick": true, "defaultGameMode": true, "difficulty": true, "tickRange": true,
	}
	var extra map[string]any
	for k, v := range m {
		if !known[k] {
			if extra == nil {
				extra = map[string]any{}
			}
			extra[k] = v
		}
	}
	return s, extra, nil
}

// defaultSettings mirrors dragonfly's defaults for a fresh world.
func defaultSettings() *world.Settings {
	return &world.Settings{
		Name:            "World",
		DefaultGameMode: world.GameModeSurvival,
		Difficulty:      world.DifficultyNormal,
		TimeCycle:       true,
		WeatherCycle:    true,
		TickRange:       6,
	}
}
