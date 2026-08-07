//go:build windows

package pile

import (
	"errors"
	"testing"
)

func umask(t *testing.T) uint32 { return 0 }

func mkfifo(path string) error { return errors.New("no FIFOs on windows") }
