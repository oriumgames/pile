package pile

import (
	"github.com/df-mc/dragonfly/server/world"
)

// NewMemory creates a provider with no backing files: a pure in-memory world.
// Save and Close are no-ops; use SaveAs to write it to disk explicitly.
func NewMemory(opts ...Option) *Provider {
	conf := defaultConfig()
	for _, o := range opts {
		o(&conf)
	}
	conf.registry.Finalize()
	return &Provider{
		conf:     conf,
		settings: defaultSettings(),
		dims:     map[int]*dimState{},
	}
}

// Template is a shared, read-only base world from which any number of
// independent instances can be created. The template's decoded state is
// shared by all instances copy-on-write: an instance only holds the columns
// it modified.
type Template struct {
	p *Provider
}

// OpenTemplate opens a world directory as a read-only template.
func OpenTemplate(dir string, opts ...Option) (*Template, error) {
	opts = append(opts, ReadOnly())
	p, err := Open(dir, opts...)
	if err != nil {
		return nil, err
	}
	return &Template{p: p}, nil
}

// Provider returns the template's own read-only provider, for serving the
// template world directly.
func (t *Template) Provider() *Provider { return t.p }

// Instance creates an in-memory world.Provider backed by the template.
// Loads fall through to the template until a column is stored; stores stay in
// the instance. Close discards the instance; SaveAs persists it as a new
// world. Options configure the instance (filters, compression for SaveAs).
func (t *Template) Instance(opts ...Option) *Provider {
	p := NewMemory(opts...)
	p.base = t.p

	t.p.mu.Lock()
	p.settings = cloneSettings(t.p.settings)
	p.userData = cloneBytes(t.p.userData)
	p.border = cloneBytes(t.p.border)
	p.markers = make([]Marker, len(t.p.markers))
	for i, m := range t.p.markers {
		m.Extra = deepCopyCompound(m.Extra)
		p.markers[i] = m
	}
	t.p.mu.Unlock()
	return p
}

// Close closes the underlying provider.
func (t *Template) Close() error { return t.p.Close() }

// cloneSettings copies a world.Settings value field by field (the struct
// embeds a mutex and must not be copied wholesale). The source is locked
// against dragonfly's tick loop.
func cloneSettings(s *world.Settings) *world.Settings {
	s.Lock()
	defer s.Unlock()
	return &world.Settings{
		Name:               s.Name,
		Spawn:              s.Spawn,
		Time:               s.Time,
		TimeCycle:          s.TimeCycle,
		RainTime:           s.RainTime,
		Raining:            s.Raining,
		ThunderTime:        s.ThunderTime,
		Thundering:         s.Thundering,
		WeatherCycle:       s.WeatherCycle,
		RequiredSleepTicks: s.RequiredSleepTicks,
		CurrentTick:        s.CurrentTick,
		DefaultGameMode:    s.DefaultGameMode,
		Difficulty:         s.Difficulty,
		TickRange:          s.TickRange,
	}
}
