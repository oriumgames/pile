package main

import (
	"flag"

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

// addDecodeLimit registers --max-decoded on a flag set.
func addDecodeLimit(fs *flag.FlagSet) decodeLimit {
	return decodeLimit{max: fs.Int64("max-decoded", 0,
		"refuse a file whose decode would exceed this many bytes of live state (0: the format's own ceiling)")}
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
