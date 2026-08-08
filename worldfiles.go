package pile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/oriumgames/pile/format"
)

// DimPath returns the file path of a dimension inside a world directory:
// overworld.pile, nether.pile, end.pile (dim<id>.pile for custom dimensions).
func DimPath(dir string, dim world.Dimension) string {
	id, _ := world.DimensionID(dim)
	name := fmt.Sprintf("dim%d.pile", id)
	switch id {
	case 0:
		name = "overworld.pile"
	case 1:
		name = "nether.pile"
	case 2:
		name = "end.pile"
	}
	return filepath.Join(dir, name)
}

// worldDimensions lists every dimension that has a file in dir: the three
// vanilla dimensions plus any registered custom dimension with a
// dim<id>.pile file. Files for unregistered dimension IDs are an error, not
// silently ignored: skipping them would regenerate and overwrite their
// content.
func worldDimensions(dir string) ([]world.Dimension, error) {
	dims := []world.Dimension{world.Overworld, world.Nether, world.End}
	matches, err := filepath.Glob(filepath.Join(dir, "dim*.pile"))
	if err != nil {
		return nil, err
	}
	for _, m := range matches {
		var id int
		if _, err := fmt.Sscanf(filepath.Base(m), "dim%d.pile", &id); err != nil {
			continue
		}
		if id <= 2 {
			continue // vanilla dimensions use their named files
		}
		dim, ok := world.DimensionByID(id)
		if !ok {
			return nil, fmt.Errorf("pile: %s belongs to unregistered dimension id %d; register the dimension before opening the world", m, id)
		}
		dims = append(dims, dim)
	}
	return dims, nil
}

// FileMode reads the mode byte of a pile file header: format.ModeSolid or
// format.ModeIndexed.
func FileMode(path string) (uint8, error) {
	mode, _, err := fileHeader(path)
	return mode, err
}

// fileHeader reads a pile file's mode byte and flags.
func fileHeader(path string) (mode uint8, flags uint32, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	hdr := make([]byte, 12)
	if _, err := f.ReadAt(hdr, 0); err != nil {
		return 0, 0, fmt.Errorf("pile: %s: %w", path, err)
	}
	if string(hdr[0:4]) != "PILE" {
		return 0, 0, fmt.Errorf("pile: %s is not a pile file", path)
	}
	return hdr[7], uint32(hdr[8]) | uint32(hdr[9])<<8 | uint32(hdr[10])<<16 | uint32(hdr[11])<<24, nil
}

// DimFile is one dimension file of a world, loaded for offline tooling.
type DimFile struct {
	Dim world.Dimension
	// Indexed reports the file's mode, preserved when writing back.
	Indexed bool
	// StoreLight reports whether the source file stored baked light,
	// preserved when writing back.
	StoreLight bool
	Columns    []format.Column
}

// WorldFiles is a world directory loaded for offline tooling (move, diff,
// patch, prune). It holds every dimension's columns plus the world metadata
// blobs.
type WorldFiles struct {
	Dims []DimFile

	Settings []byte
	UserData []byte
	Markers  []byte
}

// LoadWorldFiles loads all dimension files of a world directory, regardless
// of their mode. Intended for offline tools; servers use Open.
//
// Options are accepted so a tool reading a world it did not write can set
// MaxDecodedBytes; only the decode policy is read, since everything else an
// Option carries is about a running provider. It has no ceiling by default,
// which is the same position Open is in and for the same reason.
func LoadWorldFiles(dir string, reg world.BlockRegistry, opts ...Option) (*WorldFiles, error) {
	conf := defaultConfig()
	for _, o := range opts {
		o(&conf)
	}
	if reg == nil {
		reg = world.DefaultBlockRegistry
	}
	reg.Finalize()
	ropts := conf.readOpts()
	wf := &WorldFiles{}
	haveMeta := false
	dims, err := worldDimensions(dir)
	if err != nil {
		return nil, err
	}
	for _, dim := range dims {
		path := DimPath(dir, dim)
		mode, flags, err := fileHeader(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, err
		}
		df := DimFile{
			Dim:        dim,
			Indexed:    mode == format.ModeIndexed,
			StoreLight: flags&format.FlagStoreLight != 0,
		}
		if df.Indexed {
			iw, err := format.OpenIndexed(path, reg, true, ropts...)
			if err != nil {
				return nil, fmt.Errorf("pile: open %s: %w", path, err)
			}
			for _, k := range iw.Positions() {
				c, err := iw.Column(k[0], k[1])
				if err != nil {
					_ = iw.Close()
					return nil, fmt.Errorf("pile: read %s (%d,%d): %w", path, k[0], k[1], err)
				}
				df.Columns = append(df.Columns, c)
			}
			if !haveMeta {
				wf.Settings, wf.UserData, wf.Markers = iw.Meta()
				haveMeta = true
			}
			_ = iw.Close()
		} else {
			file, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			d, err := format.ReadWorld(file, reg, ropts...)
			if err != nil {
				return nil, fmt.Errorf("pile: read %s: %w", path, err)
			}
			df.Columns = d.Columns
			if !haveMeta {
				wf.Settings, wf.UserData, wf.Markers = d.Settings, d.UserData, d.Markers
				haveMeta = true
			}
		}
		wf.Dims = append(wf.Dims, df)
	}
	if len(wf.Dims) == 0 {
		return nil, fmt.Errorf("pile: no pile world at %s", dir)
	}
	return wf, nil
}

// Write writes every dimension back into dir. All dimensions are encoded and
// fsynced to temporary files first and only then renamed into place, so a
// failure while encoding or writing any dimension leaves the whole world
// untouched. The renames themselves are not a single atomic transaction: a
// crash inside the rename phase can leave a world with some dimensions
// updated (each individual file is always intact and consistent).
func (wf *WorldFiles) Write(dir string, reg world.BlockRegistry) error {
	if reg == nil {
		reg = world.DefaultBlockRegistry
	}
	reg.Finalize()

	type staged struct{ tmp, path string }
	var files []staged
	cleanup := func() {
		for _, f := range files {
			_ = os.Remove(f.tmp)
		}
	}
	for _, df := range wf.Dims {
		path := DimPath(dir, df.Dim)
		tmp := path + ".tmp"
		if err := wf.writeDimTemp(tmp, df, reg); err != nil {
			cleanup()
			return err
		}
		files = append(files, staged{tmp: tmp, path: path})
	}
	for _, f := range files {
		if err := preserveMode(f.tmp, f.path); err != nil {
			cleanup()
			return err
		}
		if err := os.Rename(f.tmp, f.path); err != nil {
			cleanup()
			return err
		}
	}
	if len(files) > 0 {
		return syncDir(dir)
	}
	return nil
}

// writeDimTemp encodes one dimension into a temporary file and fsyncs it.
func (wf *WorldFiles) writeDimTemp(tmp string, df DimFile, reg world.BlockRegistry) error {
	_ = os.Remove(tmp)
	if df.Indexed {
		iw, err := format.CreateIndexed(tmp, reg, format.Options{
			Compression: format.CompressionDefault, StoreLight: df.StoreLight,
		})
		if err != nil {
			return err
		}
		for _, c := range df.Columns {
			if err := iw.Store(c); err != nil {
				_ = iw.Close()
				_ = os.Remove(tmp)
				return err
			}
		}
		if err := iw.SetMeta(wf.Settings, wf.UserData, wf.Markers); err != nil {
			_ = iw.Close()
			_ = os.Remove(tmp)
			return err
		}
		if err := iw.Close(); err != nil {
			_ = os.Remove(tmp)
			return err
		}
		return nil
	} else {
		d := &format.WorldData{
			Settings: wf.Settings, UserData: wf.UserData, Markers: wf.Markers,
			Columns: df.Columns,
		}
		f, err := createExclusive(tmp)
		if err != nil {
			return err
		}
		err = format.WriteWorld(f, d, reg, format.Options{Compression: format.CompressionBest, StoreLight: df.StoreLight, Stats: true})
		if err2 := f.Sync(); err == nil {
			err = err2
		}
		if err2 := f.Close(); err == nil {
			err = err2
		}
		if err != nil {
			_ = os.Remove(tmp)
			return err
		}
	}
	return nil
}

// Backup copies the world's current dimension files into snapshots/<name>,
// replacing any previous backup of that name.
func (wf *WorldFiles) Backup(dir, name string) error {
	dst := filepath.Join(dir, snapshotsDirName, name)
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for _, df := range wf.Dims {
		src := DimPath(dir, df.Dim)
		if err := copyFile(src, filepath.Join(dst, filepath.Base(src))); err != nil {
			return fmt.Errorf("pile: backup %s: %w", name, err)
		}
	}
	return nil
}

// Dim returns the loaded data for a dimension, or nil.
func (wf *WorldFiles) Dim(dim world.Dimension) *DimFile {
	for i := range wf.Dims {
		if wf.Dims[i].Dim == dim {
			return &wf.Dims[i]
		}
	}
	return nil
}

// WorldBounds computes the chunk-granular bounding box [lo, hi] of all
// non-empty sections of a dimension. ok is false when the dimension holds no
// blocks.
func WorldBounds(p *Provider, dim world.Dimension) (lo, hi cube.Pos, ok bool) {
	first := true
	for pos, col := range p.Columns(dim) {
		r := col.Chunk.Range()
		for i, sub := range col.Chunk.Sub() {
			if sub.Empty() {
				continue
			}
			x0, z0 := int(pos[0])*16, int(pos[1])*16
			y0 := r[0] + i*16
			if first {
				lo, hi = cube.Pos{x0, y0, z0}, cube.Pos{x0 + 15, y0 + 15, z0 + 15}
				first = false
				continue
			}
			lo = cube.Pos{min(lo.X(), x0), min(lo.Y(), y0), min(lo.Z(), z0)}
			hi = cube.Pos{max(hi.X(), x0+15), max(hi.Y(), y0+15), max(hi.Z(), z0+15)}
		}
	}
	return lo, hi, !first
}
