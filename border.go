package pile

import (
	"sync"
	"time"

	"github.com/oriumgames/pile/format"
)

// Border is an application-level world border hint stored in world metadata:
// the inclusive XZ block bounds of the playable area. Pile does not enforce
// it; game code reads it to clamp movement, and tools (move, prune) translate
// or respect it.
type Border struct {
	Min [2]int32 // minimum X, Z
	Max [2]int32 // maximum X, Z
}

// borderToNBT encodes a border blob ({min: [2]int32, max: [2]int32}).
func borderToNBT(b *Border) ([]byte, error) {
	if b == nil {
		return nil, nil
	}
	// Fixed-size arrays encode as NBT array tags (slices encode as lists),
	// which is what the border schema specifies.
	return format.MarshalNBT(map[string]any{
		"min": [2]int32{b.Min[0], b.Min[1]},
		"max": [2]int32{b.Max[0], b.Max[1]},
	})
}

// borderFromNBT decodes a border blob; nil for empty or unknown shapes.
func borderFromNBT(blob []byte) *Border {
	if len(blob) == 0 {
		return nil
	}
	m, err := format.UnmarshalNBT(blob)
	if err != nil {
		return nil
	}
	pair := func(v any) ([2]int32, bool) {
		switch p := v.(type) {
		case []int32:
			if len(p) == 2 {
				return [2]int32{p[0], p[1]}, true
			}
		case [2]int32:
			return p, true
		}
		return [2]int32{}, false
	}
	mn, ok1 := pair(m["min"])
	mx, ok2 := pair(m["max"])
	if !ok1 || !ok2 {
		return nil
	}
	return &Border{Min: mn, Max: mx}
}

// Border returns the world border, or nil when none is set.
func (p *Provider) Border() *Border {
	p.mu.Lock()
	defer p.mu.Unlock()
	return borderFromNBT(p.border)
}

// SetBorder stores the world border; nil clears it. Persisted on the next
// save.
func (p *Provider) SetBorder(b *Border) error {
	if p.conf.readOnly {
		return ErrReadOnly
	}
	blob, err := borderToNBT(b)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.border = blob
	p.metaDirty = true
	p.mu.Unlock()
	return nil
}

// AutoSave starts a background ticker that schedules a coalesced save every
// interval. The returned stop function halts it; it also stops on Close.
//
// It saves through SaveAsync, so a failed autosave is reported by the next
// Save or Close and not at the moment it happens, and only the most recent
// failure is kept. A process that autosaves and ignores Close's return value
// never learns that any of them failed.
func (p *Provider) AutoSave(interval time.Duration) (stop func()) {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				p.mu.Lock()
				closed := p.closed
				p.mu.Unlock()
				if closed {
					return
				}
				p.SaveAsync()
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}
