//go:build !windows

package pile

import (
	"syscall"
	"testing"
)

// umask reads the process umask by setting and restoring it. The tests that
// check permission bits have to subtract it: 0644 is what the code asks for,
// not what the kernel grants.
func umask(t *testing.T) uint32 {
	t.Helper()
	m := syscall.Umask(0)
	syscall.Umask(m)
	return uint32(m)
}

// mkfifo creates a named pipe. os.Open on one blocks until a writer appears,
// which is why the code that walks a snapshot directory has to skip it.
func mkfifo(path string) error { return syscall.Mkfifo(path, 0o644) }
