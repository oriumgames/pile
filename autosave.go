package pile

import (
	"sync"
	"time"
)

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
