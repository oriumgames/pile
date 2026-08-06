//go:build race

package pile

// raceEnabled reports that the race detector is active; timing assertions in
// scale tests are skipped since the detector slows execution many-fold.
const raceEnabled = true
