package format

import "testing"

// TestAirOnlyLayerSeesEveryEntry.
//
// The trim that enforces §4.3's "writers MUST drop trailing all-air layers"
// asked whether the layer's palette was one entry long and that entry was air.
// A storage may hold air in more than one slot -- Bedrock writes one when an
// edit removes the last non-air block without rebuilding the palette -- and
// such a layer was called content, survived the trim, folded to a single air
// entry during global resolution, and went out as a uniform-air layer that this
// package's own reader refuses.
//
// One chunk in a converted 1 806-chunk skyblock lobby had it, which made the
// whole world unreadable: pile convert reported success and every command after
// it failed.
func TestAirOnlyLayerSeesEveryEntry(t *testing.T) {
	const air, stone = uint32(0), uint32(1)
	for _, c := range []struct {
		name string
		rs   rawBlockSec
		want bool
	}{
		{"one air entry", rawBlockSec{rids: []uint32{air}}, true},
		{"two air entries", rawBlockSec{rids: []uint32{air, air}}, true},
		{"five air entries", rawBlockSec{rids: []uint32{air, air, air, air, air}}, true},
		{"one stone entry", rawBlockSec{rids: []uint32{stone}}, false},
		{"air then stone", rawBlockSec{rids: []uint32{air, stone}}, false},
		{"stone then air", rawBlockSec{rids: []uint32{stone, air}}, false},

		// A preserved unresolved state stands in the palette as its
		// placeholder, which may itself be air. Dropping the layer would
		// discard the state it was preserving, so it is content wherever it
		// sits -- including in a slot that is not the first, which the
		// one-slot test could not have looked at.
		{"air with a preserved state in slot 0",
			rawBlockSec{rids: []uint32{air}, states: []int32{0}}, false},
		{"air with a preserved state in slot 1",
			rawBlockSec{rids: []uint32{air, air}, states: []int32{-1, 0}}, false},
		{"two air slots, neither preserved",
			rawBlockSec{rids: []uint32{air, air}, states: []int32{-1, -1}}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := airOnlyLayer(c.rs, air); got != c.want {
				t.Errorf("airOnlyLayer = %v, want %v", got, c.want)
			}
		})
	}
}
