package main

import (
	"cmp"
	"errors"
	"flag"
	"fmt"
	"github.com/df-mc/dragonfly/server/world"
	"slices"
	"strings"

	"github.com/oriumgames/pile"
)

func cmdConvert(args []string) error {
	fs := flag.NewFlagSet("convert", flag.ContinueOnError)
	permissive := fs.Bool("permissive", false,
		"register block states the registry does not know, so a world using a behaviour pack converts")
	limit := addDecodeLimit(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return errors.New("usage: pile convert [--permissive] <src> <dst> [--max-decoded n]")
	}
	src, dst := fs.Arg(0), fs.Arg(1)
	switch {
	case isMcdb(src):
		return convertMcdbToPile(src, dst, *permissive)
	case isPile(src):
		return convertPileToMcdb(src, dst, limit)
	default:
		return fmt.Errorf("%s is neither an mcdb world (level.dat) nor a pile world (*.pile)", src)
	}
}

func isMcdb(dir string) bool { return pile.IsMCDB(dir) }

func isPile(dir string) bool { return pile.IsPile(dir) }

// convertMcdbToPile is pile.ImportMCDB plus the line the CLI prints.
//
// Under --permissive the source's own palettes are scanned first and every
// state the registry does not know is registered as a placeholder, which is
// what lets a world built on a behaviour pack convert at all: dragonfly's chunk
// decoder fails outright on a state it cannot resolve, so one block from a pack
// stops the conversion at whichever chunk holds it.
func convertMcdbToPile(src, dst string, permissive bool) error {
	// The process-wide registry, not a clone. world.NewBlockRegistry would be
	// the better answer -- placeholders belong to one conversion rather than to
	// the process -- but in dragonfly v0.11.1 it panics on Finalize: Clone
	// resets bitSize to 0 and the block hashes then collide ("block Stairs...
	// already registered by block.Stairs..."). Until that is fixed upstream a
	// converter gets one shot at the registry, which is fine for a CLI whose
	// process ends with the conversion.
	reg := world.DefaultBlockRegistry
	if permissive {
		added, err := pile.RegisterMCDBStates(src, reg)
		if err != nil {
			return err
		}
		if added > 0 {
			fmt.Printf("registered %d block states this build does not know\n", added)
		}
	}
	reg.Finalize()
	n, err := pile.ImportMCDB(src, dst, pile.Registry(reg))
	if err != nil {
		if !permissive && strings.Contains(err.Error(), "cannot get runtime ID of block state") {
			return unresolvedConvertError(err, src)
		}
		return err
	}
	fmt.Printf("converted %d chunks: %s -> %s\n", n, src, dst)
	return nil
}

// convertPileToMcdb is pile.ExportMCDB plus the line the CLI prints.
func convertPileToMcdb(src, dst string, limit decodeLimit) error {
	n, err := pile.ExportMCDB(src, dst, limit.providerOpts()...)
	if err != nil {
		return err
	}
	fmt.Printf("converted %d chunks: %s -> %s\n", n, src, dst)
	return nil
}

// unresolvedConvertError explains a conversion that stopped on a block state
// the registry could not resolve.
//
// There are two quite different reasons for that and the same message used to
// be given for both. A behaviour pack's block is a block this build could never
// have known, and --permissive is the whole answer. A minecraft: block is one
// this build ought to know, and its absence means the world was written by a
// Minecraft newer than the dragonfly this is built against -- for which
// --permissive still converts, but leaves vanilla blocks showing as
// placeholders on the server, which is not what anybody wants from a lobby.
//
// Telling those apart needs the world's own palette, so it is scanned here, on
// the failure path only.
func unresolvedConvertError(cause error, src string) error {
	states, scanErr := pile.MCDBBlockStates(src)
	if scanErr != nil {
		// The scan is an explanation, not the outcome. If it fails, the
		// original error is still the answer.
		return fmt.Errorf("%w\n\nthis world uses blocks this build does not know; "+
			"re-run with --permissive to convert it anyway", cause)
	}
	reg := world.DefaultBlockRegistry
	reg.Finalize()

	vanilla, custom := map[string]int{}, map[string]int{}
	worldVersion := int32(0)
	for _, st := range states {
		// The world's own version is the highest any palette entry declares,
		// taken over every state rather than the unresolved ones: a world is
		// as new as its newest entry, and the entries that fail to resolve are
		// typically the oldest ones in it.
		worldVersion = max(worldVersion, st.Version)
		if _, ok := reg.StateToRuntimeID(st.Name, st.Properties); ok {
			continue
		}
		if strings.HasPrefix(st.Name, "minecraft:") {
			vanilla[st.Name]++
		} else {
			custom[st.Name]++
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%v\n\n", cause)
	fmt.Fprintf(&b, "%d block states in this world do not resolve against this build:\n",
		countStates(vanilla)+countStates(custom))
	if n := len(custom); n > 0 {
		fmt.Fprintf(&b, "  %d from behaviour packs (%d identifiers)\n", countStates(custom), n)
		for _, name := range topNames(custom, 6) {
			fmt.Fprintf(&b, "      %-42s %d states\n", name, custom[name])
		}
	}
	if n := len(vanilla); n > 0 {
		fmt.Fprintf(&b, "  %d vanilla minecraft: blocks (%d identifiers)\n", countStates(vanilla), n)
		for _, name := range topNames(vanilla, 6) {
			fmt.Fprintf(&b, "      %-42s %d states\n", name, vanilla[name])
		}
		fmt.Fprintf(&b, "\nVanilla identifiers this build's block list does not contain mean the world and\n"+
			"the block list are different ages, not that a pack is involved. This world's\n"+
			"palette is written at block version %s. Minecraft has since split blocks like\n"+
			"minecraft:carpet into minecraft:blue_carpet and friends, so the old spellings no\n"+
			"longer resolve.\n", blockVersionString(worldVersion))
		fmt.Fprintf(&b, "Opening the world once in a current Minecraft, or running it through chunker,\n"+
			"upgrades the states, and is the fix worth doing.\n")
	}
	fmt.Fprintf(&b, "\n--permissive converts it as it stands, keeping every identifier exactly as the\n"+
		"world spells it. `pile blocks %s` lists them without converting.", src)
	if len(vanilla) > 0 {
		fmt.Fprintf(&b, "\nBut a vanilla block kept as a placeholder still will not render or behave on the\n"+
			"server, which for a lobby is most of the point of the lobby.")
	}
	return errors.New(b.String())
}

func countStates(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

// topNames returns the identifiers with the most states, most first.
func topNames(m map[string]int, k int) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	slices.SortFunc(names, func(a, b string) int {
		if m[a] != m[b] {
			return cmp.Compare(m[b], m[a])
		}
		return cmp.Compare(a, b)
	})
	return names[:min(k, len(names))]
}

// blockVersionString renders a Bedrock block version, which packs four bytes.
func blockVersionString(v int32) string {
	if v == 0 {
		return "an unrecorded version"
	}
	return fmt.Sprintf("%d.%d.%d.%d", (v>>24)&0xFF, (v>>16)&0xFF, (v>>8)&0xFF, v&0xFF)
}
