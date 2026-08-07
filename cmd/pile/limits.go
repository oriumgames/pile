package main

import (
	"flag"
	"fmt"
	"strconv"
	"strings"

	"github.com/oriumgames/pile"
	"github.com/oriumgames/pile/format"
)

// decodeLimit is the --max-decoded flag: the ceiling on the live decoded state
// one file may produce, in bytes.
//
// Every command that decodes chunk content takes it, because every command's
// argument can be a file somebody sent you and the format's own ceilings are
// set at what it can represent rather than at what a workstation wants to
// spend. A file refused under it fails with format.ErrDecodeBudget, which does
// not wrap format.ErrCorrupt: the file is bigger than you asked for, not
// broken.
//
// Zero, the default, is the format's ceiling. There is deliberately no
// non-zero default: picking one would make the tool refuse worlds an operator
// legitimately has, and would do it silently.
type decodeLimit struct{ max *int64 }

// addDecodeLimit registers --max-decoded on a flag set. The value takes a
// size suffix because the number is otherwise unreadable: nobody types
// 268435456 and sees a quarter of a gigabyte, and a ceiling nobody can read is
// a ceiling nobody checks.
func addDecodeLimit(fs *flag.FlagSet) decodeLimit {
	d := decodeLimit{max: new(int64)}
	fs.Var(sizeValue{d.max}, "max-decoded",
		"refuse a file whose decode would exceed this much live state, e.g. 256MiB (0: the format's own ceiling)")
	return d
}

// sizeValue parses a byte count with an optional binary or decimal suffix.
type sizeValue struct{ n *int64 }

func (v sizeValue) String() string {
	if v.n == nil || *v.n == 0 {
		return "0"
	}
	return strconv.FormatInt(*v.n, 10)
}

func (v sizeValue) Set(s string) error {
	n, err := parseSize(s)
	if err != nil {
		return err
	}
	*v.n = n
	return nil
}

func parseSize(s string) (int64, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, fmt.Errorf("empty size")
	}
	mult := int64(1)
	for _, suffix := range []struct {
		name string
		mult int64
	}{
		{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30},
		{"KB", 1000}, {"MB", 1000 * 1000}, {"GB", 1000 * 1000 * 1000},
		{"K", 1 << 10}, {"M", 1 << 20}, {"G", 1 << 30},
	} {
		if rest, ok := cutSuffixFold(t, suffix.name); ok {
			t, mult = rest, suffix.mult
			break
		}
	}
	n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a size", s)
	}
	if n < 0 {
		return 0, fmt.Errorf("%q is negative", s)
	}
	if n > (1<<62)/mult {
		return 0, fmt.Errorf("%q overflows", s)
	}
	return n * mult, nil
}

func cutSuffixFold(s, suffix string) (string, bool) {
	if len(s) < len(suffix) || !strings.EqualFold(s[len(s)-len(suffix):], suffix) {
		return s, false
	}
	return s[:len(s)-len(suffix)], true
}

// providerOpts returns the provider options the limit implies.
func (d decodeLimit) providerOpts() []pile.Option {
	if d.max == nil || *d.max <= 0 {
		return nil
	}
	return []pile.Option{pile.MaxDecodedBytes(*d.max)}
}

// structureOpts returns the structure options the limit implies.
func (d decodeLimit) structureOpts() []pile.StructureOption {
	if d.max == nil || *d.max <= 0 {
		return nil
	}
	return []pile.StructureOption{pile.StructureMaxDecodedBytes(*d.max)}
}

// readOpts returns the codec read options the limit implies.
func (d decodeLimit) readOpts() []format.ReadOption {
	if d.max == nil || *d.max <= 0 {
		return nil
	}
	return []format.ReadOption{format.MaxDecodedBytes(*d.max)}
}

// value returns the raw ceiling, for callers that carry it in a struct field.
func (d decodeLimit) value() int64 {
	if d.max == nil {
		return 0
	}
	return *d.max
}
