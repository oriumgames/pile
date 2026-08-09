package main

import (
	"errors"
	"flag"
	"fmt"
	"sort"
	"strings"

	"github.com/df-mc/dragonfly/server/world"
	"github.com/oriumgames/pile"
	"github.com/oriumgames/pile/format"
)

// cmdReplace rewrites block states throughout a pile world.
//
// The case it exists for is a converted world holding blocks from a behaviour
// pack: pile convert --permissive brings them across with their real
// identifiers, which is right when the server will register them and useless
// when it will not. Turning them into air, or into something that looks close
// enough, is the other half of that story.
func cmdReplace(args []string) error {
	fs := flag.NewFlagSet("replace", flag.ContinueOnError)
	from := fs.String("from", "", "block identifier to replace, e.g. cubecraft:portal_side")
	unresolved := fs.Bool("unresolved", false, "replace every state this build's registry does not know")
	to := fs.String("to", "minecraft:air", "block to put in its place")
	dryRun := fs.Bool("dry-run", false, "report what would change without writing")
	noBackup := fs.Bool("no-backup", false, "skip the snapshots/pre-replace backup")
	limit := addDecodeLimit(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || (*from == "" && !*unresolved) {
		return errors.New("usage: pile replace (--from name | --unresolved) [--to name] " +
			"[--dry-run] [--no-backup] <world>")
	}
	dir := fs.Arg(0)

	// The world's palette is readable without a registry, which is what makes
	// this work on exactly the worlds that need it: an identifier the registry
	// cannot resolve is still an identifier the file states plainly.
	states, err := pile.WorldBlockStates(dir, limit.readOpts()...)
	if err != nil {
		return err
	}

	// The process-wide registry: world.NewBlockRegistry panics on Finalize in
	// dragonfly v0.11.1 (Clone resets bitSize and the block hashes collide), so
	// a clone is not available. Harmless for a CLI, which finalizes once and
	// exits; a library caller doing this repeatedly would want the clone.
	reg := world.DefaultBlockRegistry
	// Every state in the file is registered before the registry is closed, so
	// the load below resolves all of them and no preserved-state sidecar is
	// involved. Replacing a placeholder through the sidecar would mean editing
	// two representations of one block and getting both right; this way there
	// is one.
	var unknown []format.BlockState
	for _, s := range states {
		added, err := pile.RegisterBlockState(reg, s)
		if err != nil {
			return err
		}
		if added {
			unknown = append(unknown, s)
		}
	}
	reg.Finalize()

	target, ok := reg.StateToRuntimeID(parseState(*to))
	if !ok {
		return fmt.Errorf("--to %s is not a block this build knows; "+
			"give properties as name[k=v,k=v] if it needs them", *to)
	}

	// Which states go. --from names an identifier and takes all of its states:
	// a caller who wants one state of one block is better served by saying so
	// in a later flag than by having to spell every state out now.
	victims := map[uint32]format.BlockState{}
	for _, s := range pickStates(states, unknown, *from, *unresolved) {
		rid, ok := reg.StateToRuntimeID(s.Name, s.Properties)
		if !ok || rid == target {
			continue
		}
		victims[rid] = s
	}
	if len(victims) == 0 {
		fmt.Println("nothing matched; the world is unchanged")
		return nil
	}

	wf, err := pile.LoadWorldFiles(dir, reg, limit.providerOpts()...)
	if err != nil {
		return err
	}
	counts := map[string]int{}
	for i := range wf.Dims {
		for _, c := range wf.Dims[i].Columns {
			replaceInColumn(c, victims, target, counts)
		}
	}
	if len(counts) == 0 {
		fmt.Println("the states are in the palette but no block uses them; the world is unchanged")
		return nil
	}

	total := 0
	for _, n := range sortedCountKeys(counts) {
		fmt.Printf("  %-56s %d blocks\n", n, counts[n])
		total += counts[n]
	}
	verb := "replaced"
	if *dryRun {
		verb = "would replace"
	}
	fmt.Printf("%s %d blocks with %s\n", verb, total, *to)
	if *dryRun {
		return nil
	}
	if !*noBackup {
		if err := wf.Backup(dir, "pre-replace"); err != nil {
			return err
		}
	}
	return wf.Write(dir, reg)
}

// pickStates returns the states the flags select.
func pickStates(all, unknown []format.BlockState, from string, unresolved bool) []format.BlockState {
	if unresolved {
		return unknown
	}
	name, props := parseState(from)
	var out []format.BlockState
	for _, s := range all {
		if s.Name != name {
			continue
		}
		// A bare identifier takes every state of that block; one written with
		// properties takes only the state that matches them.
		if props != nil && !sameProps(s.Properties, props) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// replaceInColumn swaps every victim runtime ID for the target, counting what
// it changed.
//
// A section whose palette holds no victim is skipped without reading a single
// position, which is what keeps this cheap: the blocks being replaced are
// usually a handful of identifiers in a world of thousands of sections.
func replaceInColumn(c format.Column, victims map[uint32]format.BlockState, target uint32, counts map[string]int) {
	for _, sub := range c.Col.Chunk.Sub() {
		if sub.Empty() {
			continue
		}
		for l, st := range sub.Layers() {
			pal := st.Palette()
			hit := false
			for i := range pal.Len() {
				if _, ok := victims[pal.Value(uint16(i))]; ok {
					hit = true
					break
				}
			}
			if !hit {
				continue
			}
			for x := range byte(16) {
				for y := range byte(16) {
					for z := range byte(16) {
						rid := sub.Block(x, y, z, uint8(l))
						s, ok := victims[rid]
						if !ok {
							continue
						}
						sub.SetBlock(x, y, z, uint8(l), target)
						counts[stateLabel(s)]++
					}
				}
			}
		}
	}
}

// parseState splits "name[k=v,k=v]" into its parts. Properties are read as
// strings; the registry lookup below is what decides whether they name a real
// state, and a value whose type is wrong simply does not match one.
func parseState(s string) (string, map[string]any) {
	open := strings.IndexByte(s, '[')
	if open < 0 || !strings.HasSuffix(s, "]") {
		return s, nil
	}
	name := s[:open]
	props := map[string]any{}
	for _, kv := range strings.Split(s[open+1:len(s)-1], ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		props[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return name, props
}

// sameProps compares a state's properties against ones given on the command
// line, which arrive as strings whatever the state stores.
func sameProps(have map[string]any, want map[string]any) bool {
	if len(have) != len(want) {
		return false
	}
	for k, w := range want {
		h, ok := have[k]
		if !ok || fmt.Sprint(h) != fmt.Sprint(w) {
			return false
		}
	}
	return true
}

// stateLabel renders a state the way pile check prints one.
func stateLabel(s format.BlockState) string {
	if len(s.Properties) == 0 {
		return s.Name
	}
	keys := make([]string, 0, len(s.Properties))
	for k := range s.Properties {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, s.Properties[k]))
	}
	return s.Name + "[" + strings.Join(parts, ",") + "]"
}

// sortedCountKeys orders the report. identity.go has a sortedKeys of its own
// over a different value type; one generic helper for two call sites in two
// files would be indirection for its own sake.
func sortedCountKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
