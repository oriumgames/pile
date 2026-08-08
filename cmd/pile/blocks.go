package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"os"

	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/goleveldb/leveldb"
	"github.com/df-mc/goleveldb/leveldb/opt"
	"github.com/oriumgames/pile"
	"github.com/oriumgames/pile/format"
	"github.com/sandertv/gophertunnel/minecraft/nbt"
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
		return errors.New("usage: pile blocks <mcdb-world|dir|file.pile> [--custom] [--quiet] [--max-decoded n]")
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

// scanPalettes walks every sub-chunk in the world and collects the distinct
// states of each block identifier.
//
// The database is opened READ-ONLY. goleveldb's default is read-write and
// rewrites the manifest and journal on open even when nothing is stored, which
// is a change to somebody's world that listing its blocks must not make.
func scanPalettes(dir string) (map[string][]map[string]any, error) {
	db, err := leveldb.OpenFile(dir+"/db", &opt.Options{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", dir, err)
	}
	defer func() { _ = db.Close() }()

	states := map[string][]map[string]any{}
	seen := map[string]bool{}
	it := db.NewIterator(nil, nil)
	defer it.Release()
	for it.Next() {
		k := it.Key()
		// Sub-chunk keys: eight bytes of position, the tag, and the index.
		// Thirteen-byte keys carry a dimension between the two; those are the
		// nether and the end, which have their own palettes and are included.
		if !((len(k) == 10 && k[8] == 0x2F) || (len(k) == 14 && k[12] == 0x2F)) {
			continue
		}
		entries, err := subChunkPalette(it.Value())
		if err != nil {
			// A sub-chunk this cannot walk is skipped rather than fatal: the
			// point is to report what is there, and one odd sub-chunk should
			// not cost the answer for the whole world.
			continue
		}
		for _, e := range entries {
			key := e.Name + "\x00" + fmt.Sprint(e.State)
			if seen[key] {
				continue
			}
			seen[key] = true
			states[e.Name] = append(states[e.Name], e.State)
		}
	}
	if err := it.Error(); err != nil {
		return nil, err
	}
	if len(states) == 0 {
		return nil, errors.New("no block palettes found; is this a Bedrock world?")
	}
	return states, nil
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

// blockEntry is one palette entry as stored on disk.
type blockEntry struct {
	Name    string         `nbt:"name"`
	State   map[string]any `nbt:"states"`
	Version int32          `nbt:"version"`
}

// subChunkPalette returns every palette entry of a stored sub-chunk without
// decoding its indices. The layout is version 8 or 9: a version byte, a storage
// count, an index byte for version 9, then per storage a header, the packed
// indices, a palette count, and that many NBT compounds.
func subChunkPalette(v []byte) ([]blockEntry, error) {
	if len(v) < 2 {
		return nil, fmt.Errorf("sub-chunk of %d bytes", len(v))
	}
	ver, storages := v[0], int(v[1])
	b := v[2:]
	if ver == 9 {
		if len(b) < 1 {
			return nil, errors.New("version 9 sub-chunk with no index byte")
		}
		b = b[1:]
	}
	var out []blockEntry
	for s := range storages {
		if len(b) < 1 {
			return nil, fmt.Errorf("storage %d: truncated header", s)
		}
		bits := int(b[0] >> 1)
		b = b[1:]
		if bits == 0 || bits > 32 {
			return nil, fmt.Errorf("storage %d: %d bits per entry", s, bits)
		}
		per := 32 / bits
		words := (4096 + per - 1) / per
		if len(b) < words*4+4 {
			return nil, fmt.Errorf("storage %d: truncated indices", s)
		}
		b = b[words*4:]
		count := int32(binary.LittleEndian.Uint32(b[:4]))
		b = b[4:]
		if count < 0 {
			return nil, fmt.Errorf("storage %d: palette count %d", s, count)
		}
		r := bytes.NewReader(b)
		dec := nbt.NewDecoderWithEncoding(r, nbt.LittleEndian)
		for i := range count {
			var e blockEntry
			if err := dec.Decode(&e); err != nil {
				return nil, fmt.Errorf("storage %d entry %d: %w", s, i, err)
			}
			out = append(out, e)
		}
		b = b[len(b)-r.Len():]
	}
	return out, nil
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
