package main

import (
	"errors"
	"flag"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"os"

	"github.com/df-mc/dragonfly/server/world"
	"github.com/oriumgames/pile"
	"github.com/oriumgames/pile/format"
)

// cmdBlocks lists the block identifiers a leveldb world uses, and the property
// values each of them takes.
//
// It exists for a chicken-and-egg. Converting a world whose blocks come from a
// behaviour pack needs a registry that knows those blocks; knowing which blocks
// to register needs to read the world; and reading the world through dragonfly
// needs the registry, because its decoder refuses a state it cannot resolve
// rather than substituting anything. The way out is to read the palettes
// without decoding anything, which needs no registry at all.
//
// The property schema in the output is not decoration. A custom block is
// announced to the client once per identifier, and the client generates the
// state list itself from the declared properties -- so a block registered with
// four states and declared with none leaves the client's palette shorter than
// the server's, and every block after it renders as something else.
func cmdBlocks(args []string) error {
	fs := flag.NewFlagSet("blocks", flag.ContinueOnError)
	customOnly := fs.Bool("custom", false, "list only identifiers outside the minecraft: namespace")
	quiet := fs.Bool("quiet", false, "print only identifiers, one per line")
	limit := addDecodeLimit(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: pile blocks [--custom] [--quiet] [--max-decoded n] <mcdb-world|dir|file.pile>")
	}
	target := fs.Arg(0)
	var states map[string][]map[string]any
	var err error
	fromMCDB := IsMCDBDir(target)
	if fromMCDB {
		states, err = scanPalettes(target)
	} else {
		states, err = pilePalettes(target, limit)
	}
	if err != nil {
		return err
	}
	names := slices.Sorted(maps.Keys(states))

	custom, customStates := 0, 0
	for _, name := range names {
		vanilla := strings.HasPrefix(name, "minecraft:")
		if !vanilla {
			custom++
			customStates += len(states[name])
		}
		if *customOnly && vanilla {
			continue
		}
		if *quiet {
			fmt.Println(name)
			continue
		}
		fmt.Printf("%-44s %3d state", name, len(states[name]))
		if len(states[name]) != 1 {
			fmt.Print("s")
		}
		if schema := schemaOf(states[name]); schema != "" {
			fmt.Printf("   %s", schema)
		}
		fmt.Println()
	}
	if !*quiet {
		total := 0
		for _, s := range states {
			total += len(s)
		}
		fmt.Printf("\n%d identifiers, %d states\n", len(names), total)
		if custom > 0 && fromMCDB {
			fmt.Printf("%d identifiers and %d states are outside minecraft: and must be registered "+
				"before this world can be converted\n", custom, customStates)
		} else if custom > 0 {
			fmt.Printf("%d identifiers and %d states are outside minecraft: and need a registry "+
				"that knows them; `pile check --allow %s` reports only the rest\n",
				custom, customStates, namespaceHint(states))
		}
	}
	return nil
}

// IsMCDBDir reports whether dir looks like a leveldb world. It is the same
// question IsMCDB answers in the library, asked without importing it here.
func IsMCDBDir(dir string) bool { return isMcdb(dir) }

// scanPalettes groups the world's block states by identifier. The scan itself
// is pile.MCDBBlockStates, shared with the permissive converter: this used to
// hold its own copy, and when the copy turned out to mishandle uniform storages
// it under-reported by a factor of six while the converter it was meant to
// inform failed on a block the report never mentioned.
func scanPalettes(dir string) (map[string][]map[string]any, error) {
	states, err := pile.MCDBBlockStates(dir)
	if err != nil {
		return nil, err
	}
	out := map[string][]map[string]any{}
	for _, s := range states {
		out[s.Name] = append(out[s.Name], s.Properties)
	}
	return out, nil
}

// schemaOf renders the property values an identifier's states take, which is
// what a custom block has to declare through block.Permutable.States() for a
// client to rebuild the same state list the server holds.
func schemaOf(states []map[string]any) string {
	values := map[string][]string{}
	for _, st := range states {
		for k, v := range st {
			s := fmt.Sprint(v)
			if !slices.Contains(values[k], s) {
				values[k] = append(values[k], s)
			}
		}
	}
	if len(values) == 0 {
		return ""
	}
	keys := slices.Sorted(maps.Keys(values))
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v := values[k]
		sort.Strings(v)
		parts = append(parts, fmt.Sprintf("%s=%s", k, strings.Join(v, ",")))
	}
	return strings.Join(parts, "  ")
}

// pilePalettes reads the block palette of a pile world or file. Like the mcdb
// path it needs no registry: format.BlockStates reports the palette as it is
// stored, which is names and properties.
func pilePalettes(target string, limit decodeLimit) (map[string][]map[string]any, error) {
	files, err := pileFiles(target)
	if err != nil {
		return nil, err
	}
	states := map[string][]map[string]any{}
	seen := map[string]bool{}
	add := func(list []format.BlockState) {
		for _, st := range list {
			key := st.Name + "\x00" + fmt.Sprint(st.Properties)
			if seen[key] {
				continue
			}
			seen[key] = true
			states[st.Name] = append(states[st.Name], st.Properties)
		}
	}
	for _, f := range files {
		mode, err := pile.FileMode(f)
		if err != nil {
			return nil, err
		}
		if mode == format.ModeIndexed {
			w, err := format.OpenIndexed(f, world.DefaultBlockRegistry, true, limit.readOpts()...)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", f, err)
			}
			list, err := w.BlockStates()
			_ = w.Close()
			if err != nil {
				return nil, fmt.Errorf("%s: %w", f, err)
			}
			add(list)
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		list, err := format.BlockStates(data, limit.readOpts()...)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		add(list)
	}
	if len(states) == 0 {
		return nil, errors.New("no block palette found")
	}
	return states, nil
}

// namespaceHint picks a non-vanilla namespace to name in the hint, so the
// suggested command is one a reader can paste rather than adapt.
func namespaceHint(states map[string][]map[string]any) string {
	spaces := map[string]bool{}
	for name := range states {
		if ns, _, ok := strings.Cut(name, ":"); ok && ns != "minecraft" {
			spaces[ns] = true
		}
	}
	return strings.Join(slices.Sorted(maps.Keys(spaces)), ",")
}
