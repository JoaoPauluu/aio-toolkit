package plugin

import (
	"fmt"
	"hash/fnv"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// LoadError reports a plugin folder that could not be loaded.
type LoadError struct {
	Dir   string `json:"dir"`
	Error string `json:"error"`
}

// Registry holds the currently known plugins. It scans one or more root
// directories; a plugin in a later root overrides one with the same id in an
// earlier root (so a mounted /plugins folder can override built-ins).
type Registry struct {
	roots []string

	mu      sync.RWMutex
	plugins map[string]*Manifest
	errors  []LoadError
	print   uint64
}

func NewRegistry(roots []string) *Registry {
	r := &Registry{roots: roots, plugins: map[string]*Manifest{}}
	r.Reload()
	return r
}

// Reload rescans all roots.
func (r *Registry) Reload() {
	plugins := map[string]*Manifest{}
	var errs []LoadError
	for _, root := range r.roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			if !os.IsNotExist(err) {
				errs = append(errs, LoadError{Dir: root, Error: err.Error()})
			}
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || e.Name()[0] == '.' || e.Name()[0] == '_' {
				continue
			}
			dir := filepath.Join(root, e.Name())
			if _, err := os.Stat(filepath.Join(dir, ManifestName)); err != nil {
				continue
			}
			m, err := Load(dir)
			if err != nil {
				errs = append(errs, LoadError{Dir: dir, Error: err.Error()})
				continue
			}
			plugins[m.ID] = m
		}
	}
	fp := r.fingerprint()
	r.mu.Lock()
	r.plugins, r.errors, r.print = plugins, errs, fp
	r.mu.Unlock()
	log.Printf("plugins: loaded %d (%d errors)", len(plugins), len(errs))
	for _, e := range errs {
		log.Printf("plugins: %s: %s", e.Dir, e.Error)
	}
}

// fingerprint hashes names, sizes and mtimes of every manifest so changes
// can be detected cheaply without re-parsing.
func (r *Registry) fingerprint() uint64 {
	h := fnv.New64a()
	for _, root := range r.roots {
		entries, _ := os.ReadDir(root)
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			fi, err := os.Stat(filepath.Join(root, e.Name(), ManifestName))
			if err != nil {
				continue
			}
			fmt.Fprintf(h, "%s/%s|%d|%d;", root, e.Name(), fi.Size(), fi.ModTime().UnixNano())
		}
	}
	return h.Sum64()
}

// Watch polls for manifest changes and reloads when something changed.
func (r *Registry) Watch(interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for range t.C {
		fp := r.fingerprint()
		r.mu.RLock()
		changed := fp != r.print
		r.mu.RUnlock()
		if changed {
			r.Reload()
		}
	}
}

func (r *Registry) Get(id string) *Manifest {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.plugins[id]
}

// List returns plugins sorted by category, then name.
func (r *Registry) List() ([]*Manifest, []LoadError) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Manifest, 0, len(r.plugins))
	for _, m := range r.plugins {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Category != out[j].Category {
			return out[i].Category < out[j].Category
		}
		return out[i].Name < out[j].Name
	})
	return out, append([]LoadError{}, r.errors...)
}
