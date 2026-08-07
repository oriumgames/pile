//go:build windows

package pile

import "testing"

func umask(t *testing.T) uint32 { return 0 }
