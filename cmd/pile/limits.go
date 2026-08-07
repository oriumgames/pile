package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/oriumgames/pile"
	"github.com/oriumgames/pile/format"
)

// maxDecoded is the decode ceiling every command reads through, set by the
// global --max-decoded flag. Zero means the format's own §8 ceilings.
//
// The default is deliberately zero, i.e. unchanged behaviour. A CLI is pointed
// at a file its user named, and a built-in cap that refuses a legitimately
// large world would be a worse failure than the one it prevents: the user
// cannot tell a refusal-by-policy from a corrupt file without reading the
// error carefully, and there is no way for the tool to guess which worlds are
// meant to be enormous. What the flag buys is that `pile verify` on a file
// someone sent you can be bounded, which was previously impossible at any
// price. See readme.md, "Files from other people".
var maxDecoded int64

// readOpts returns the format-level read options every decode in this package
// should pass. Threading one helper rather than a flag per command means a new
// command cannot forget the ceiling by omission.
func readOpts() []format.ReadOption {
	if maxDecoded <= 0 {
		return nil
	}
	return []format.ReadOption{format.MaxDecodedBytes(maxDecoded)}
}

// providerOpts returns extra plus the ceiling, for the commands that go
// through pile.Open rather than the format package directly. It takes the
// caller's own options rather than being appended to them so that every
// pile.Open in this package reads as providerOpts(...) and one that forgot the
// ceiling is visible at a glance.
func providerOpts(extra ...pile.Option) []pile.Option {
	if maxDecoded <= 0 {
		return extra
	}
	return append(extra, pile.MaxDecodedBytes(maxDecoded))
}

// stripGlobalFlags consumes the global flags that may appear before the
// subcommand and returns what is left. They are handled here rather than by
// each command's own FlagSet because there are twenty commands and a ceiling
// that only some of them honour is worse than none: a user who sets it expects
// it to hold everywhere.
func stripGlobalFlags(args []string) ([]string, error) {
	for len(args) > 0 {
		arg := args[0]
		name, value, hasValue := strings.Cut(arg, "=")
		switch name {
		case "--max-decoded", "-max-decoded":
			if !hasValue {
				if len(args) < 2 {
					return nil, fmt.Errorf("%s needs a size, e.g. %s=256MiB", name, name)
				}
				value, args = args[1], args[1:]
			}
			n, err := parseSize(value)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			maxDecoded = n
			args = args[1:]
		default:
			return args, nil
		}
	}
	return args, nil
}

// parseSize reads a byte count with an optional binary or decimal suffix.
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
	if n <= 0 {
		return 0, fmt.Errorf("%q is not a positive size", s)
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
