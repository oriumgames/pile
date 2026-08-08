package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime/debug"

	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
	"github.com/oriumgames/pile"
	"github.com/oriumgames/pile/format"
)

// cmdHash prints the content identity of a world or a single file.
//
// The readme says a file hash is a map version, and until this command there
// was no way to obtain one: ContentHash existed, diff and patch used it
// internally, and nothing printed it. That made the format's headline property
// something a user had to write Go to reach.
func cmdHash(args []string) error {
	fs := flag.NewFlagSet("hash", flag.ContinueOnError)
	limit := addDecodeLimit(fs)
	quiet := fs.Bool("quiet", false, "print nothing; exit 1 if the arguments differ")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("usage: pile hash [--quiet] [--max-decoded n] <dir|file> [<dir|file>...]")
	}
	world.DefaultBlockRegistry.Finalize()
	reg := world.DefaultBlockRegistry

	var first map[string]uint64
	same := true
	for _, arg := range fs.Args() {
		hashes, err := hashTarget(arg, reg, limit)
		if err != nil {
			return fmt.Errorf("%s: %w", arg, err)
		}
		if first == nil {
			first = hashes
		} else if !sameHashes(first, hashes) {
			same = false
		}
		if *quiet {
			continue
		}
		if fs.NArg() > 1 {
			fmt.Printf("%s:\n", arg)
		}
		for _, name := range sortedKeys(hashes) {
			fmt.Printf("  %-10s %016x\n", name, hashes[name])
		}
	}
	if !same {
		// Exit 1 rather than an error: differing is an answer, not a failure,
		// and a script asking "are these the same map" wants a status code
		// rather than a message on stderr.
		os.Exit(1)
	}
	return nil
}

// hashTarget returns the content identity of every dimension of a world
// directory, or of a single file under the key "file".
//
// Identity is format.ContentHash, which is defined as the hash of the decoded
// content re-encoded canonically and uncompressed. That definition is what
// makes it comparable across everything that is not part of the content: the
// compressor, the compression level, and the file's mode. An indexed world and
// the solid world holding the same columns have the same identity here, which
// is the answer a user comparing a deployed map against a built one wants.
func hashTarget(path string, reg world.BlockRegistry, limit decodeLimit) (map[string]uint64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	out := map[string]uint64{}
	if !st.IsDir() {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		// A structure, or a solid world file, hashes as it stands.
		if m, err := format.ReadMeta(data, limit.readOpts()...); err == nil && m.Mode == format.ModeSolid {
			h, err := format.ContentHash(data, reg, limit.readOpts()...)
			if err != nil {
				return nil, err
			}
			out["file"] = h
			return out, nil
		}
		// An indexed file has no canonical bytes of its own (§5), so its
		// identity is the identity of the world it decodes into.
		return nil, errors.New("indexed files are hashed as part of their world directory, not on their own")
	}

	wf, err := pile.LoadWorldFiles(path, reg, limit.providerOpts()...)
	if err != nil {
		return nil, err
	}
	for _, df := range wf.Dims {
		h, err := canonicalDimHash(wf, df, reg)
		if err != nil {
			return nil, err
		}
		out[dimName(df.Dim)] = h
	}
	return out, nil
}

// canonicalDimHash re-encodes one dimension as a canonical solid file and
// hashes it, which is what ContentHash means. Going through the encoder rather
// than hashing the stored bytes is the whole point: an indexed file's bytes are
// history-dependent, and a solid file's depend on the compressor.
func canonicalDimHash(wf *pile.WorldFiles, df pile.DimFile, reg world.BlockRegistry) (uint64, error) {
	var buf bytes.Buffer
	d := &format.WorldData{
		Settings: wf.Settings, UserData: wf.UserData,
		Markers: wf.Markers,
		Columns: df.Columns,
	}
	if err := format.WriteWorld(&buf, d, reg, format.Options{Compression: format.CompressionNone}); err != nil {
		return 0, err
	}
	return format.ContentHash(buf.Bytes(), reg)
}

func sameHashes(a, b map[string]uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func sortedKeys(m map[string]uint64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func dimName(d world.Dimension) string {
	switch d {
	case world.Nether:
		return "nether"
	case world.End:
		return "end"
	}
	return "overworld"
}

// cmdVersion prints what a bug report needs: which pile, which wire format,
// and which dragonfly it was built against. For a format whose point is
// compatibility, "which versions" is the first question asked and was the one
// thing this tool could not answer.
func cmdVersion(args []string) error {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	fmt.Printf("pile          %s\n", moduleVersion("github.com/oriumgames/pile"))
	fmt.Printf("wire format   v%d", format.Version)
	if format.Version == format.FrozenVersion {
		fmt.Print(" (frozen)")
	}
	fmt.Println()
	fmt.Printf("block version %d\n", chunk.CurrentBlockVersion)
	fmt.Printf("dragonfly     %s\n", moduleVersion("github.com/df-mc/dragonfly"))
	return nil
}

// moduleVersion reports a dependency's version from the build info the
// toolchain embeds. A binary built with `go run` or from a checkout has none,
// so the fallback says so rather than printing something that looks like a
// version and is not.
func moduleVersion(path string) string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "(unknown: no build info)"
	}
	if bi.Main.Path == path && bi.Main.Version != "" {
		return bi.Main.Version
	}
	for _, dep := range bi.Deps {
		if dep.Path == path {
			if dep.Replace != nil {
				return dep.Replace.Version + " (replaced)"
			}
			return dep.Version
		}
	}
	return "(unknown: not in build info)"
}
