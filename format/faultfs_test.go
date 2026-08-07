package format

import (
	"errors"
	"fmt"
	"os"
	"slices"
)

// An injectable filesystem for the durability suite.
//
// §5.6 claims that a torn write leaves a file that opens at either the old
// checkpoint or the new one, never something in between and never something
// unopenable. Nothing about a real filesystem lets a test fail or truncate a
// chosen write, so the claim could only ever be checked against hand-built
// fixtures. These two models close that:
//
//   - crashFile is a write-through wrapper that lets the first n bytes of a
//     chosen write reach the disk and then refuses every further write, sync
//     and truncate, which is what a process dying mid-write leaves behind.
//   - a recorded trace (recordFile) plus replayImage synthesises the other
//     half of a crash: writes issued since the last successful fsync may reach
//     the platter in any subset, or not at all. That is the model the
//     "fsync before the footer" ordering exists to defend against, and a
//     prefix model cannot express it.

var errCrash = errors.New("faultfs: simulated crash")

// fsOp is one mutating filesystem operation. Reads and Stat are not recorded:
// they never change the file, and counting them would make the operation index
// depend on how a decoder happens to read.
type fsOp struct {
	kind string // "write", "writeat", "sync", "truncate"
	off  int64
	data []byte // the bytes offered (writes only)
}

func (o fsOp) n() int { return len(o.data) }

func (o fsOp) String() string {
	switch o.kind {
	case "write", "writeat":
		return fmt.Sprintf("%s off=%d len=%d", o.kind, o.off, len(o.data))
	default:
		return o.kind
	}
}

// crashFile wraps a real file and stops it dead at a chosen operation.
//
// crashAt is an index into the sequence of mutating operations. When the
// counter reaches it, `partial` bytes of that write land and everything
// afterwards — including the truncate that appendFrame attempts on a short
// write — is refused, because a process that has died does not get to tidy up.
// When transient is set the process does not die: the one operation fails and
// everything afterwards works, which is the other thing a filesystem does (a
// full disk, an I/O error on one sector) and the case the writer's own
// clean-up is supposed to handle.
type crashFile struct {
	f         *os.File
	crashAt   int // -1 to record without crashing
	partial   int
	transient bool
	// failSyncs refuses every fsync while leaving writes alone: the disk that
	// takes the bytes but will not promise they are durable.
	failSyncs bool

	n    int
	dead bool
	ops  []fsOp
}

func newCrashFile(f *os.File, crashAt, partial int) *crashFile {
	return &crashFile{f: f, crashAt: crashAt, partial: partial}
}

func newTransientFile(f *os.File, failAt, partial int) *crashFile {
	return &crashFile{f: f, crashAt: failAt, partial: partial, transient: true}
}

// recordFile is a crashFile that never crashes: it captures the trace.
func recordFile(f *os.File) *crashFile { return newCrashFile(f, -1, 0) }

// step advances the operation counter and reports how many bytes of the
// operation are allowed through and whether this is where the process dies.
func (c *crashFile) step(op fsOp) (allow int, crash bool) {
	if c.dead {
		return 0, true
	}
	i := c.n
	c.n++
	c.ops = append(c.ops, op)
	if i != c.crashAt {
		return op.n(), false
	}
	c.dead = !c.transient
	if c.partial > op.n() {
		return op.n(), true
	}
	return c.partial, true
}

func (c *crashFile) Write(p []byte) (int, error) {
	off, _ := c.f.Seek(0, 1)
	allow, crash := c.step(fsOp{kind: "write", off: off, data: slices.Clone(p)})
	var n int
	var err error
	if allow > 0 {
		n, err = c.f.Write(p[:allow])
	}
	if crash && err == nil {
		err = errCrash
	}
	return n, err
}

func (c *crashFile) WriteAt(p []byte, off int64) (int, error) {
	allow, crash := c.step(fsOp{kind: "writeat", off: off, data: slices.Clone(p)})
	var n int
	var err error
	if allow > 0 {
		n, err = c.f.WriteAt(p[:allow], off)
	}
	if crash && err == nil {
		err = errCrash
	}
	return n, err
}

func (c *crashFile) Sync() error {
	_, crash := c.step(fsOp{kind: "sync"})
	if crash || c.failSyncs {
		return errCrash
	}
	return c.f.Sync()
}

func (c *crashFile) Truncate(size int64) error {
	if _, crash := c.step(fsOp{kind: "truncate", off: size}); crash {
		return errCrash
	}
	return c.f.Truncate(size)
}

// ReadAt and Stat are not faulted: a crash model is about what reaches the
// disk, and a read that fails is a different (already covered) story.
func (c *crashFile) ReadAt(p []byte, off int64) (int, error) { return c.f.ReadAt(p, off) }
func (c *crashFile) Stat() (os.FileInfo, error)              { return c.f.Stat() }

// Close always closes the real handle, dead or not: the test has to be able to
// reopen the file the crash left behind.
func (c *crashFile) Close() error { return c.f.Close() }

// writeOps returns the indices of the recorded operations that offered bytes.
func (c *crashFile) writeOps() []int {
	var out []int
	for i, op := range c.ops {
		if op.n() > 0 {
			out = append(out, i)
		}
	}
	return out
}

// replayImage builds the file image a crash would leave when the writes named
// by keep are the only ones that reached the disk.
//
// A dropped write is not a hole in a shorter file: a later write at a higher
// offset extends the file and the gap reads as zeros, which is what a
// filesystem gives back for a sparse region. Modelling it any other way would
// make the test kinder than reality.
func replayImage(base []byte, ops []fsOp, keep func(i int) bool) []byte {
	img := slices.Clone(base)
	for i, op := range ops {
		switch op.kind {
		case "write", "writeat":
			if !keep(i) {
				continue
			}
			end := int(op.off) + len(op.data)
			for len(img) < end {
				img = append(img, 0)
			}
			copy(img[op.off:], op.data)
		case "truncate":
			if !keep(i) {
				continue
			}
			if int(op.off) < len(img) {
				img = img[:op.off]
			}
		}
	}
	return img
}

// syncGroups splits a recorded trace into the runs of writes that were in
// flight together: everything issued after one successful fsync and before the
// next. A crash may lose any subset of a group, and never anything from an
// earlier one.
func syncGroups(ops []fsOp) [][]int {
	var groups [][]int
	cur := []int{}
	for i, op := range ops {
		switch op.kind {
		case "write", "writeat":
			cur = append(cur, i)
		case "sync":
			groups = append(groups, cur)
			cur = []int{}
		}
	}
	groups = append(groups, cur)
	return groups
}
