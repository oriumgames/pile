package pile

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// StructureLibrary is a set of structures loaded from a directory, looked up
// by file basename (without the .pile extension).
type StructureLibrary struct {
	m map[string]*Structure
}

// LoadStructureLibrary loads every *.pile structure in a directory. Options
// apply to each structure. Non-structure pile files fail the load: a library
// directory holds structures only.
func LoadStructureLibrary(dir string, opts ...StructureOption) (*StructureLibrary, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.pile"))
	if err != nil {
		return nil, err
	}
	lib := &StructureLibrary{m: map[string]*Structure{}}
	for _, path := range matches {
		s, err := LoadStructure(path, opts...)
		if err != nil {
			return nil, fmt.Errorf("pile: library %s: %w", path, err)
		}
		name := strings.TrimSuffix(filepath.Base(path), ".pile")
		lib.m[name] = s
	}
	return lib, nil
}

// Get returns a structure by name.
func (l *StructureLibrary) Get(name string) (*Structure, bool) {
	s, ok := l.m[name]
	return s, ok
}

// Names returns all structure names, sorted.
func (l *StructureLibrary) Names() []string {
	names := make([]string, 0, len(l.m))
	for n := range l.m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Len returns the number of structures.
func (l *StructureLibrary) Len() int { return len(l.m) }
