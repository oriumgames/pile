package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/cespare/xxhash/v2"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/oriumgames/pile"
	"github.com/oriumgames/pile/format"
)

// Patch file layout ("PILP"):
//
//	magic   4 bytes "PILP"
//	version u16le = 2
//	dimN    uvarint
//	per dim:
//	  id        uvarint
//	  baseHash  u64le, xxHash64 over the base dimension's Morton-sorted
//	            canonical column encodings; apply refuses a target whose
//	            content hash differs (unless --force)
//	  removedN  uvarint, then removedN × (x svarint, z svarint)
//	  worldLen  uvarint, then a solid pile world holding the changed and
//	            added columns plus the new world metadata
var patchMagic = []byte("PILP")

// dimContentHash computes the base-identity hash of a dimension. A missing
// dimension hashes as an empty one, so patches spanning added or removed
// dimensions work.
func dimContentHash(df *pile.DimFile, reg world.BlockRegistry) (uint64, error) {
	var src []format.Column
	if df != nil {
		src = df.Columns
	}
	cols := make([]format.Column, len(src))
	copy(cols, src)
	sort.Slice(cols, func(i, j int) bool {
		return cols[i].X < cols[j].X || (cols[i].X == cols[j].X && cols[i].Z < cols[j].Z)
	})
	h := xxhash.New()
	for _, c := range cols {
		enc, err := canonicalColumnBytes(c, reg)
		if err != nil {
			return 0, err
		}
		var key [8]byte
		binary.LittleEndian.PutUint32(key[0:4], uint32(c.X))
		binary.LittleEndian.PutUint32(key[4:8], uint32(c.Z))
		_, _ = h.Write(key[:])
		_, _ = h.Write(enc)
	}
	return h.Sum64(), nil
}

func cmdPatch(args []string) error {
	fs := flag.NewFlagSet("patch", flag.ContinueOnError)
	out := fs.String("o", "", "output patch file (required)")
	limit := addDecodeLimit(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 || *out == "" {
		return errors.New("usage: pile patch -o <file.pilepatch> <old-world> <new-world>")
	}
	reg := world.DefaultBlockRegistry
	reg.Finalize()
	oldW, err := pile.LoadWorldFiles(fs.Arg(0), reg, limit.providerOpts()...)
	if err != nil {
		return err
	}
	newW, err := pile.LoadWorldFiles(fs.Arg(1), reg, limit.providerOpts()...)
	if err != nil {
		return err
	}
	diffs, err := diffWorlds(oldW, newW, reg)
	if err != nil {
		return err
	}

	var buf bytes.Buffer
	buf.Write(patchMagic)
	buf.Write([]byte{2, 0}) // version u16le
	writeUvarint(&buf, uint64(len(diffs)))
	total := 0
	for _, d := range diffs {
		id, _ := world.DimensionID(d.dim)
		writeUvarint(&buf, uint64(id))
		baseHash, err := dimContentHash(oldW.Dim(d.dim), reg)
		if err != nil {
			return err
		}
		var hb [8]byte
		binary.LittleEndian.PutUint64(hb[:], baseHash)
		buf.Write(hb[:])
		writeUvarint(&buf, uint64(len(d.removed)))
		for _, k := range d.removed {
			writeSvarint(&buf, int64(k[0]))
			writeSvarint(&buf, int64(k[1]))
		}
		// Collect changed + added columns from the new world.
		want := map[[2]int32]bool{}
		for _, k := range d.added {
			want[k] = true
		}
		for _, k := range d.modified {
			want[k] = true
		}
		wd := &format.WorldData{
			Settings: newW.Settings, UserData: newW.UserData,
		}
		if ndf := newW.Dim(d.dim); ndf != nil {
			for _, c := range ndf.Columns {
				if want[[2]int32{c.X, c.Z}] {
					wd.Columns = append(wd.Columns, c)
				}
			}
		}
		var world bytes.Buffer
		if err := format.WriteWorld(&world, wd, reg, format.Options{Compression: format.CompressionBest}); err != nil {
			return err
		}
		writeUvarint(&buf, uint64(world.Len()))
		buf.Write(world.Bytes())
		total += len(wd.Columns) + len(d.removed)
	}
	if total == 0 {
		return errors.New("worlds have no chunk differences; no patch written")
	}
	if err := os.WriteFile(*out, buf.Bytes(), 0o644); err != nil {
		return err
	}
	fmt.Printf("patch: %d chunk changes, %d bytes -> %s\n", total, buf.Len(), *out)
	return nil
}

func cmdApply(args []string) error {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	force := fs.Bool("force", false, "apply even if the target does not match the patch's base world")
	noBackup := fs.Bool("no-backup", false, "skip the snapshots/pre-apply backup")
	limit := addDecodeLimit(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return errors.New("usage: pile apply [--force] [--no-backup] <world> <file.pilepatch>")
	}
	dir, patchPath := fs.Arg(0), fs.Arg(1)
	reg := world.DefaultBlockRegistry
	reg.Finalize()

	raw, err := os.ReadFile(patchPath)
	if err != nil {
		return err
	}
	if len(raw) < 6 || !bytes.Equal(raw[0:4], patchMagic) {
		return fmt.Errorf("%s is not a pile patch", patchPath)
	}
	if v := binary.LittleEndian.Uint16(raw[4:6]); v != 2 {
		return fmt.Errorf("unsupported patch version %d", v)
	}
	r := bytes.NewReader(raw[6:])

	wf, err := pile.LoadWorldFiles(dir, reg, limit.providerOpts()...)
	if err != nil {
		return err
	}
	dimN, err := binary.ReadUvarint(r)
	if err != nil {
		return err
	}
	applied := 0
	for range dimN {
		id, err := binary.ReadUvarint(r)
		if err != nil {
			return err
		}
		dim, ok := world.DimensionByID(int(id))
		if !ok {
			return fmt.Errorf("patch references unknown dimension %d", id)
		}
		var hb [8]byte
		if _, err := readFull(r, hb[:]); err != nil {
			return err
		}
		wantBase := binary.LittleEndian.Uint64(hb[:])
		if !*force {
			// A missing dimension hashes as an empty one: skipping the check
			// there would let a delta patch build a partial dimension.
			got, err := dimContentHash(wf.Dim(dim), reg)
			if err != nil {
				return err
			}
			if got != wantBase {
				return fmt.Errorf("target world does not match the patch's base (dimension %d content hash mismatch); re-run with --force to apply anyway", id)
			}
		}
		removedN, err := binary.ReadUvarint(r)
		if err != nil {
			return err
		}
		removed := map[[2]int32]bool{}
		for range removedN {
			x, err := binary.ReadVarint(r)
			if err != nil {
				return err
			}
			z, err := binary.ReadVarint(r)
			if err != nil {
				return err
			}
			removed[[2]int32{int32(x), int32(z)}] = true
		}
		worldLen, err := binary.ReadUvarint(r)
		if err != nil {
			return err
		}
		if worldLen > uint64(r.Len()) {
			return fmt.Errorf("patch world length %d exceeds patch size", worldLen)
		}
		worldBytes := make([]byte, worldLen)
		if _, err := readFull(r, worldBytes); err != nil {
			return err
		}
		wd, err := format.ReadWorld(worldBytes, reg, limit.readOpts()...)
		if err != nil {
			return fmt.Errorf("patch world data: %w", err)
		}

		df := wf.Dim(dim)
		if df == nil {
			wf.Dims = append(wf.Dims, pile.DimFile{Dim: dim})
			df = &wf.Dims[len(wf.Dims)-1]
		}
		upsert := map[[2]int32]format.Column{}
		for _, c := range wd.Columns {
			upsert[[2]int32{c.X, c.Z}] = c
		}
		var kept []format.Column
		for _, c := range df.Columns {
			k := [2]int32{c.X, c.Z}
			if removed[k] {
				applied++
				continue
			}
			if _, ok := upsert[k]; ok {
				continue // replaced below
			}
			kept = append(kept, c)
		}
		for _, c := range upsert {
			kept = append(kept, c)
			applied++
		}
		df.Columns = kept
		wf.Settings, wf.UserData = wd.Settings, wd.UserData
	}
	if !*noBackup {
		if err := wf.Backup(dir, "pre-apply"); err != nil {
			return err
		}
	}
	if err := wf.Write(dir, reg); err != nil {
		return err
	}
	fmt.Printf("applied %d chunk changes to %s\n", applied, dir)
	return nil
}

func writeUvarint(buf *bytes.Buffer, v uint64) {
	var tmp [binary.MaxVarintLen64]byte
	buf.Write(tmp[:binary.PutUvarint(tmp[:], v)])
}

func writeSvarint(buf *bytes.Buffer, v int64) {
	var tmp [binary.MaxVarintLen64]byte
	buf.Write(tmp[:binary.PutVarint(tmp[:], v)])
}

func readFull(r *bytes.Reader, p []byte) (int, error) {
	n := 0
	for n < len(p) {
		m, err := r.Read(p[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}
