package community

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"aio/internal/events"
	"aio/internal/jobs"
	"aio/internal/plugin"
)

// PluginInfo summarises one plugin of a source.
type PluginInfo struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Icon        string   `json:"icon,omitempty"`
	Description string   `json:"description,omitempty"`
	Version     string   `json:"version,omitempty"`
	Author      string   `json:"author,omitempty"`
	Homepage    string   `json:"homepage,omitempty"`
	License     string   `json:"license,omitempty"`
	Run         []string `json:"run,omitempty"`
	Setup       []string `json:"setup,omitempty"`
	UI          bool     `json:"ui,omitempty"`
	Service     []string `json:"service,omitempty"`
	Schedules   []string `json:"schedules,omitempty"`
	Events      []string `json:"events,omitempty"`
	Provides    []string `json:"provides,omitempty"`
	Requires    []string `json:"requires,omitempty"`
	// Set in previews and listings:
	OverridesBuiltin bool   `json:"overrides_builtin,omitempty"`
	ShadowedByLocal  bool   `json:"shadowed_by_local,omitempty"`
	Conflict         string `json:"conflict,omitempty"`
}

func infoFor(m *plugin.Manifest) PluginInfo {
	pi := PluginInfo{ID: m.ID, Name: m.Name, Icon: m.Icon, Description: m.Description, Version: m.Version,
		Author: m.Author, Homepage: m.Homepage, License: m.License, Run: m.Run, Setup: m.Setup, UI: m.UI != "", Provides: m.Provides}
	if m.Service != nil {
		pi.Service = m.Service.Run
	}
	for _, s := range m.Schedule {
		if s.Every != "" {
			pi.Schedules = append(pi.Schedules, "every "+s.Every)
		} else {
			pi.Schedules = append(pi.Schedules, "cron "+s.Cron)
		}
	}
	for _, e := range m.Events {
		pi.Events = append(pi.Events, e.On)
	}
	for _, r := range m.Requires {
		if r.ID != "" {
			pi.Requires = append(pi.Requires, r.ID)
		} else {
			pi.Requires = append(pi.Requires, r.Source)
		}
	}
	return pi
}

// Prev is the version kept for rollback.
type Prev struct {
	Ref     string       `json:"ref"`
	Commit  string       `json:"commit"`
	Plugins []PluginInfo `json:"plugins"`
}

// Source is one installed repository or archive (it may hold several plugins).
type Source struct {
	ID          string       `json:"id"`
	Input       string       `json:"input"`
	Spec        Spec         `json:"spec"`
	Ref         string       `json:"ref"`    // resolved tag/branch of the installed version
	Commit      string       `json:"commit"` // installed version
	Latest      string       `json:"latest,omitempty"`
	LatestRef   string       `json:"latest_ref,omitempty"`
	Plugins     []PluginInfo `json:"plugins"`
	Enabled     bool         `json:"enabled"`
	AutoUpdate  bool         `json:"auto_update"`
	Dependency  bool         `json:"dependency,omitempty"` // installed because another plugin requires it
	InstalledAt time.Time    `json:"installed_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
	CheckedAt   time.Time    `json:"checked_at,omitempty"`
	Error       string       `json:"error,omitempty"`
	Warning     string       `json:"warning,omitempty"`
	Status      string       `json:"status,omitempty"` // installing | updating | checking (transient)
	Prev        *Prev        `json:"prev,omitempty"`
}

// UpdateAvailable reports whether a newer version was seen.
func (s *Source) UpdateAvailable() bool { return s.Latest != "" && s.Latest != s.Commit }

// Preview is a downloaded, validated but not yet installed source.
type Preview struct {
	Token    string       `json:"token"`
	Input    string       `json:"input"`
	Spec     Spec         `json:"spec"`
	WebURL   string       `json:"web_url"`
	Version  Version      `json:"version"`
	Plugins  []PluginInfo `json:"plugins"`
	Existing string       `json:"existing,omitempty"` // id of an installed source for the same repo

	dir       string
	manifests []*plugin.Manifest
	expires   time.Time
}

type Config struct {
	Dir      string // install root, e.g. /plugins/community
	MaxBytes int64
	Interval time.Duration // update check interval (0 = never)
}

type Manager struct {
	cfg      Config
	Fetcher  *Fetcher
	registry *plugin.Registry
	jobs     *jobs.Manager
	events   *events.Bus

	op sync.Mutex // serialises installs, updates, rollbacks and removals

	mu       sync.Mutex
	sources  []*Source
	previews map[string]*Preview
}

const stateFile = ".state.json"

func NewManager(cfg Config, reg *plugin.Registry, jm *jobs.Manager, bus *events.Bus) (*Manager, error) {
	for _, d := range []string{cfg.Dir, filepath.Join(cfg.Dir, ".prev"), filepath.Join(cfg.Dir, ".staging")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("community plugins folder: %w", err)
		}
	}
	// Leftovers from an interrupted install.
	if entries, _ := os.ReadDir(filepath.Join(cfg.Dir, ".staging")); len(entries) > 0 {
		for _, e := range entries {
			os.RemoveAll(filepath.Join(cfg.Dir, ".staging", e.Name()))
		}
	}
	f := NewFetcher(cfg.MaxBytes)
	f.TempDir = filepath.Join(cfg.Dir, ".staging")
	m := &Manager{cfg: cfg, Fetcher: f, registry: reg, jobs: jm, events: bus, previews: map[string]*Preview{}}
	if err := m.load(); err != nil {
		return nil, err
	}
	for _, s := range m.sources {
		s.Status = "" // interrupted operations
	}
	reg.SetCommunityRoot(cfg.Dir)
	reg.SetFilter(m.active)
	reg.Reload()
	return m, nil
}

func (m *Manager) load() error {
	b, err := os.ReadFile(filepath.Join(m.cfg.Dir, stateFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var st struct {
		Sources []*Source `json:"sources"`
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return fmt.Errorf("%s: %w", stateFile, err)
	}
	m.sources = st.Sources
	return nil
}

// saveLocked writes the state atomically. Caller holds m.mu.
func (m *Manager) saveLocked() error {
	b, err := json.MarshalIndent(map[string]any{"sources": m.sources}, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(m.cfg.Dir, stateFile+".tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(m.cfg.Dir, stateFile))
}

func (m *Manager) save() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.saveLocked(); err != nil {
		log.Printf("community: save state: %v", err)
	}
}

// active is the registry filter: community plugins of disabled or unknown
// sources are skipped.
func (m *Manager) active(p *plugin.Manifest) bool {
	if !p.Community {
		return true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.ownerLocked(p.ID); s != nil {
		return s.Enabled
	}
	return true // folder copied in by hand: treat like a local plugin
}

func (m *Manager) ownerLocked(pluginID string) *Source {
	for _, s := range m.sources {
		for _, p := range s.Plugins {
			if p.ID == pluginID {
				return s
			}
		}
	}
	return nil
}

func (m *Manager) get(id string) *Source {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sources {
		if s.ID == id {
			return s
		}
	}
	return nil
}

// List returns copies of all sources with override flags filled in.
func (m *Manager) List() []Source {
	m.mu.Lock()
	out := make([]Source, 0, len(m.sources))
	for _, s := range m.sources {
		c := *s
		c.Plugins = append([]PluginInfo{}, s.Plugins...)
		out = append(out, c)
	}
	m.mu.Unlock()
	for i := range out {
		for j := range out[i].Plugins {
			m.flag(&out[i].Plugins[j])
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].InstalledAt.Before(out[j].InstalledAt) })
	return out
}

// Get returns a copy of one source.
func (m *Manager) Get(id string) (Source, bool) {
	for _, s := range m.List() {
		if s.ID == id {
			return s, true
		}
	}
	return Source{}, false
}

// flag marks a plugin that overrides a built-in (a root scanned before the
// community dir) or is shadowed by a local plugin (a root scanned after it).
func (m *Manager) flag(pi *PluginInfo) {
	roots := m.registry.Roots()
	ci := -1
	for i, r := range roots {
		if r == m.cfg.Dir {
			ci = i
		}
	}
	if ci < 0 {
		return
	}
	has := func(root string) bool {
		_, err := os.Stat(filepath.Join(root, pi.ID, plugin.ManifestName))
		return err == nil
	}
	for _, r := range roots[:ci] {
		pi.OverridesBuiltin = pi.OverridesBuiltin || has(r)
	}
	for _, r := range roots[ci+1:] {
		pi.ShadowedByLocal = pi.ShadowedByLocal || has(r)
	}
}

func token() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// stage downloads version v of spec and validates the plugins in it.
// The returned dir is inside .staging and holds one folder per plugin id.
func (m *Manager) stage(ctx context.Context, spec Spec, v Version) (string, Version, []*plugin.Manifest, error) {
	archive, v, err := m.Fetcher.Download(ctx, spec, v)
	if err != nil {
		return "", v, nil, err
	}
	defer os.Remove(archive)
	dir := filepath.Join(m.cfg.Dir, ".staging", token())
	tree := dir + ".tree"
	if err := Extract(archive, tree, m.cfg.MaxBytes); err != nil {
		os.RemoveAll(tree)
		return "", v, nil, err
	}
	defer os.RemoveAll(tree)
	root := tree
	if spec.Subdir != "" {
		root = filepath.Join(tree, filepath.FromSlash(spec.Subdir))
		if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
			return "", v, nil, fmt.Errorf("folder %q not found in the repository", spec.Subdir)
		}
	}
	// One plugin at the root, or a pack: every direct subfolder with a manifest.
	var found []string
	if _, err := os.Stat(filepath.Join(root, plugin.ManifestName)); err == nil {
		found = []string{root}
	} else {
		for _, sub := range []string{root, filepath.Join(root, "plugins")} {
			entries, _ := os.ReadDir(sub)
			for _, e := range entries {
				if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
					if _, err := os.Stat(filepath.Join(sub, e.Name(), plugin.ManifestName)); err == nil {
						found = append(found, filepath.Join(sub, e.Name()))
					}
				}
			}
			if len(found) > 0 {
				break
			}
		}
	}
	if len(found) == 0 {
		return "", v, nil, fmt.Errorf("no %s found (put it at the repository root, in a subfolder per plugin, or point to the folder with …/tree/<ref>/<folder>)", plugin.ManifestName)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", v, nil, err
	}
	var list []*plugin.Manifest
	seen := map[string]bool{}
	for i, src := range found {
		// A manifest without an id takes its folder name; for a plugin at the
		// repository root that is the repo (or archive) name, not a temp dir.
		name := filepath.Base(src)
		if src == tree {
			name = defaultName(spec)
		}
		tmp := filepath.Join(dir, fmt.Sprintf(".%d", i))
		named := filepath.Join(tmp, name)
		os.MkdirAll(tmp, 0o755)
		if err := os.Rename(src, named); err != nil {
			os.RemoveAll(dir)
			return "", v, nil, err
		}
		mf, err := plugin.Load(named)
		if err != nil {
			os.RemoveAll(dir)
			return "", v, nil, err
		}
		if seen[mf.ID] {
			os.RemoveAll(dir)
			return "", v, nil, fmt.Errorf("two plugins use the id %q", mf.ID)
		}
		seen[mf.ID] = true
		dst := filepath.Join(dir, mf.ID)
		err = os.Rename(named, dst)
		os.RemoveAll(tmp)
		if err != nil {
			os.RemoveAll(dir)
			return "", v, nil, err
		}
		if mf, err = plugin.Load(dst); err != nil {
			os.RemoveAll(dir)
			return "", v, nil, err
		}
		list = append(list, mf)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	return dir, v, list, nil
}

// defaultName is the folder name (and so the default plugin id) for a
// plugin found at the root of a source.
func defaultName(s Spec) string {
	var n string
	switch {
	case s.Subdir != "":
		n = path.Base(s.Subdir)
	case s.Kind == Archive:
		n = path.Base(strings.SplitN(s.URL, "?", 2)[0])
		for _, ext := range []string{".zip", ".tar.gz", ".tgz"} {
			n = strings.TrimSuffix(strings.ToLower(n), ext)
		}
	default:
		n = path.Base(s.Repo)
	}
	n = strings.ToLower(n)
	n = strings.TrimPrefix(n, "aio-")
	var b strings.Builder
	for _, c := range n {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' {
			b.WriteRune(c)
		} else {
			b.WriteRune('-')
		}
	}
	n = strings.Trim(b.String(), "-._")
	if n == "" {
		n = "plugin"
	}
	return n
}

// Preview resolves and downloads a source so the user can review it.
func (m *Manager) Preview(ctx context.Context, input string) (*Preview, error) {
	spec, err := ParseSpec(input)
	if err != nil {
		return nil, err
	}
	v := Version{}
	if spec.Kind != Archive {
		if v, err = m.Fetcher.Resolve(ctx, spec); err != nil {
			return nil, err
		}
	}
	dir, v, list, err := m.stage(ctx, spec, v)
	if err != nil {
		return nil, err
	}
	p := &Preview{Token: token(), Input: strings.TrimSpace(input), Spec: spec, WebURL: spec.WebURL(), Version: v,
		dir: dir, manifests: list, expires: time.Now().Add(30 * time.Minute)}
	m.mu.Lock()
	for _, s := range m.sources {
		if s.Spec.SameRepo(spec) {
			p.Existing = s.ID
		}
	}
	for _, mf := range list {
		pi := infoFor(mf)
		if owner := m.ownerLocked(mf.ID); owner != nil && !owner.Spec.SameRepo(spec) {
			pi.Conflict = fmt.Sprintf("plugin id %q is already installed from %s", mf.ID, owner.Spec.Canonical())
		}
		p.Plugins = append(p.Plugins, pi)
	}
	m.previews[p.Token] = p
	m.mu.Unlock()
	for i := range p.Plugins {
		m.flag(&p.Plugins[i])
	}
	m.expirePreviews()
	return p, nil
}

func (m *Manager) expirePreviews() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, p := range m.previews {
		if time.Now().After(p.expires) {
			os.RemoveAll(p.dir)
			delete(m.previews, k)
		}
	}
}

// Discard drops a preview without installing it.
func (m *Manager) Discard(tok string) {
	m.mu.Lock()
	p := m.previews[tok]
	delete(m.previews, tok)
	m.mu.Unlock()
	if p != nil {
		os.RemoveAll(p.dir)
	}
}

// InstallOptions for Install.
type InstallOptions struct {
	AutoUpdate bool
	Async      bool // return once the source is registered; finish in the background
	dependency bool
	depth      int
}

// Install installs a previewed source (or updates the existing source for
// the same repo). Setup hooks run before anything is replaced.
func (m *Manager) Install(tok string, o InstallOptions) (Source, error) {
	m.mu.Lock()
	p := m.previews[tok]
	delete(m.previews, tok)
	m.mu.Unlock()
	if p == nil {
		return Source{}, fmt.Errorf("preview expired; add the plugin again")
	}
	for _, pi := range p.Plugins {
		if pi.Conflict != "" {
			os.RemoveAll(p.dir)
			return Source{}, errors.New(pi.Conflict)
		}
	}
	var src *Source
	if p.Existing != "" {
		src = m.get(p.Existing)
	}
	m.mu.Lock()
	if src == nil {
		src = &Source{ID: p.Spec.ID(), Input: p.Input, Spec: p.Spec, Enabled: true, AutoUpdate: o.AutoUpdate,
			Dependency: o.dependency, InstalledAt: time.Now()}
		for _, s := range m.sources {
			if s.ID == src.ID {
				src.ID = src.ID + token()[:4]
			}
		}
		m.sources = append(m.sources, src)
	} else {
		src.Spec, src.Input = p.Spec, p.Input
		if !o.dependency {
			src.AutoUpdate, src.Dependency = o.AutoUpdate, false
		}
	}
	src.Status, src.Error = "installing", ""
	if len(src.Plugins) == 0 {
		src.Plugins = infos(p.manifests)
	}
	m.saveLocked()
	m.mu.Unlock()

	run := func() error {
		m.op.Lock()
		defer m.op.Unlock()
		err := m.apply(src, p.dir, p.Version, p.manifests, o.depth)
		m.finish(src, err)
		return err
	}
	if o.Async {
		go run()
		return m.copyOf(src), nil
	}
	err := run()
	return m.copyOf(src), err
}

func infos(list []*plugin.Manifest) []PluginInfo {
	out := make([]PluginInfo, 0, len(list))
	for _, mf := range list {
		out = append(out, infoFor(mf))
	}
	return out
}

func (m *Manager) copyOf(s *Source) Source {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := *s
	c.Plugins = append([]PluginInfo{}, s.Plugins...)
	return c
}

func (m *Manager) finish(src *Source, err error) {
	m.mu.Lock()
	src.Status = ""
	if err != nil {
		src.Error = err.Error()
		log.Printf("community: %s: %v", src.Spec.Canonical(), err)
	}
	m.saveLocked()
	m.mu.Unlock()
}

// apply runs setup hooks for staged plugins, then swaps them into place
// (keeping the current version in .prev) and reloads the registry.
// Caller holds m.op.
func (m *Manager) apply(src *Source, staged string, v Version, list []*plugin.Manifest, depth int) error {
	defer os.RemoveAll(staged)
	for _, mf := range list {
		if len(mf.Setup) == 0 {
			continue
		}
		if err := m.runSetup(mf); err != nil {
			return fmt.Errorf("setup of %s failed: %w (nothing was changed)", mf.ID, err)
		}
	}
	prevRoot := filepath.Join(m.cfg.Dir, ".prev")
	m.mu.Lock()
	oldIDs := []string{}
	for _, pi := range src.Plugins {
		oldIDs = append(oldIDs, pi.ID)
	}
	hadVersion := src.Commit != ""
	m.mu.Unlock()
	newIDs := map[string]bool{}
	for _, mf := range list {
		newIDs[mf.ID] = true
	}
	// Move the current version aside (also plugins dropped from a pack).
	if hadVersion {
		for _, id := range oldIDs {
			os.RemoveAll(filepath.Join(prevRoot, id))
		}
	}
	for _, id := range oldIDs {
		cur := filepath.Join(m.cfg.Dir, id)
		if _, err := os.Stat(cur); err == nil {
			if hadVersion {
				if err := os.Rename(cur, filepath.Join(prevRoot, id)); err != nil {
					return err
				}
			} else if !newIDs[id] {
				os.RemoveAll(cur)
			}
		}
	}
	for _, mf := range list {
		cur := filepath.Join(m.cfg.Dir, mf.ID)
		os.RemoveAll(cur) // stray folder (e.g. a failed earlier install)
		if err := os.Rename(filepath.Join(staged, mf.ID), cur); err != nil {
			return err
		}
	}
	m.mu.Lock()
	if hadVersion {
		src.Prev = &Prev{Ref: src.Ref, Commit: src.Commit, Plugins: src.Plugins}
	}
	topic := "plugin.installed"
	if hadVersion {
		topic = "plugin.updated"
	}
	src.Ref, src.Commit, src.Plugins = v.Ref, v.Commit, infos(list)
	src.Latest, src.LatestRef = v.Commit, v.Ref
	src.UpdatedAt, src.Error, src.Warning = time.Now(), "", ""
	m.saveLocked()
	m.mu.Unlock()
	m.registry.Reload()
	m.publish(topic, src)
	log.Printf("community: %s %s@%s (%d plugins)", strings.TrimPrefix(topic, "plugin."), src.Spec.Canonical(), short(v.Commit), len(list))
	m.requirements(src, list, depth)
	return nil
}

// runSetup runs a plugin's setup hook as a job against the staged folder and waits.
func (m *Manager) runSetup(mf *plugin.Manifest) error {
	if m.jobs == nil {
		return fmt.Errorf("no job runner")
	}
	j, err := m.jobs.Create(mf)
	if err != nil {
		return err
	}
	if err := m.jobs.StartWith(j, mf, mf.Defaults(nil), jobs.StartOptions{
		Title: "Setup: " + mf.Name, Kind: "setup", Argv: mf.Setup, Dir: mf.Dir,
	}); err != nil {
		m.jobs.Discard(j)
		return err
	}
	snap := j.Wait(context.Background())
	if snap.State != jobs.Done {
		if snap.Error != "" {
			return errors.New(snap.Error)
		}
		return fmt.Errorf("setup %s", snap.State)
	}
	return nil
}

// requirements installs missing community dependencies and warns about
// missing plain ids.
func (m *Manager) requirements(src *Source, list []*plugin.Manifest, depth int) {
	var warn []string
	for _, mf := range list {
		for _, r := range mf.Requires {
			if r.ID != "" {
				if m.registry.Get(r.ID) == nil {
					warn = append(warn, fmt.Sprintf("%s needs plugin %q, which is not installed", mf.ID, r.ID))
				}
				continue
			}
			spec, err := ParseSpec(r.Source)
			if err != nil {
				warn = append(warn, fmt.Sprintf("%s: bad requirement %q: %v", mf.ID, r.Source, err))
				continue
			}
			installed := false
			m.mu.Lock()
			for _, s := range m.sources {
				installed = installed || s.Spec.SameRepo(spec)
			}
			m.mu.Unlock()
			if installed {
				continue
			}
			if depth >= 5 {
				warn = append(warn, fmt.Sprintf("%s: dependency chain too deep at %s", mf.ID, r.Source))
				continue
			}
			p, err := m.Preview(context.Background(), r.Source)
			if err == nil {
				// Called with m.op held; install inline.
				err = m.installLocked(p, InstallOptions{AutoUpdate: src.AutoUpdate, dependency: true, depth: depth + 1})
			}
			if err != nil {
				warn = append(warn, fmt.Sprintf("%s: installing dependency %s failed: %v", mf.ID, r.Source, err))
			}
		}
	}
	if len(warn) > 0 {
		m.mu.Lock()
		src.Warning = strings.Join(warn, "; ")
		m.saveLocked()
		m.mu.Unlock()
	}
}

// installLocked is Install for callers already holding m.op.
func (m *Manager) installLocked(p *Preview, o InstallOptions) error {
	m.mu.Lock()
	delete(m.previews, p.Token)
	src := &Source{ID: p.Spec.ID(), Input: p.Input, Spec: p.Spec, Enabled: true, AutoUpdate: o.AutoUpdate,
		Dependency: o.dependency, InstalledAt: time.Now(), Plugins: infos(p.manifests)}
	for _, pi := range p.Plugins {
		if pi.Conflict != "" {
			m.mu.Unlock()
			os.RemoveAll(p.dir)
			return errors.New(pi.Conflict)
		}
	}
	m.sources = append(m.sources, src)
	m.saveLocked()
	m.mu.Unlock()
	err := m.apply(src, p.dir, p.Version, p.manifests, o.depth)
	m.finish(src, err)
	return err
}

func (m *Manager) publish(topic string, s *Source) {
	m.mu.Lock()
	ids := []string{}
	for _, p := range s.Plugins {
		ids = append(ids, p.ID)
	}
	data := map[string]any{"source": s.ID, "url": s.Spec.Canonical(), "ref": s.Ref, "commit": s.Commit, "plugins": ids}
	m.mu.Unlock()
	m.events.Publish(events.Event{Topic: topic, Data: data})
}

func (m *Manager) setStatus(s *Source, st string) {
	m.mu.Lock()
	s.Status = st
	m.mu.Unlock()
}

// Check looks for a newer version and applies it when auto-update is on
// (or when apply is true). It returns the updated source.
func (m *Manager) Check(ctx context.Context, id string, apply bool) (Source, error) {
	s := m.get(id)
	if s == nil {
		return Source{}, fmt.Errorf("source not found")
	}
	m.op.Lock()
	defer m.op.Unlock()
	m.setStatus(s, "checking")
	m.mu.Lock()
	spec := s.Spec
	m.mu.Unlock()
	v, err := m.Fetcher.Resolve(ctx, spec)
	m.mu.Lock()
	s.CheckedAt, s.Status = time.Now(), ""
	if err != nil {
		s.Error = "update check failed: " + err.Error()
		m.saveLocked()
		m.mu.Unlock()
		return m.copyOf(s), err
	}
	s.Latest, s.LatestRef = v.Commit, v.Ref
	if strings.HasPrefix(s.Error, "update check failed") {
		s.Error = ""
	}
	need := s.Latest != s.Commit && (apply || s.AutoUpdate)
	m.saveLocked()
	m.mu.Unlock()
	if !need {
		return m.copyOf(s), nil
	}
	m.setStatus(s, "updating")
	staged, v, list, err := m.stage(ctx, spec, v)
	if err == nil {
		err = m.checkConflicts(s, list)
		if err != nil {
			os.RemoveAll(staged)
		}
	}
	if err == nil {
		err = m.apply(s, staged, v, list, 0)
	}
	m.finish(s, err)
	return m.copyOf(s), err
}

func (m *Manager) checkConflicts(s *Source, list []*plugin.Manifest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, mf := range list {
		if owner := m.ownerLocked(mf.ID); owner != nil && owner != s {
			return fmt.Errorf("new version adds plugin %q, which is already installed from %s", mf.ID, owner.Spec.Canonical())
		}
	}
	return nil
}

// Rollback swaps the installed version with the one kept in .prev and turns
// auto-update off (otherwise the next check would update again).
func (m *Manager) Rollback(id string) (Source, error) {
	s := m.get(id)
	if s == nil {
		return Source{}, fmt.Errorf("source not found")
	}
	m.op.Lock()
	defer m.op.Unlock()
	m.mu.Lock()
	prev := s.Prev
	cur := Prev{Ref: s.Ref, Commit: s.Commit, Plugins: s.Plugins}
	m.mu.Unlock()
	if prev == nil {
		return m.copyOf(s), fmt.Errorf("no previous version to roll back to")
	}
	prevRoot := filepath.Join(m.cfg.Dir, ".prev")
	swap := filepath.Join(m.cfg.Dir, ".staging", "swap-"+token())
	if err := os.MkdirAll(swap, 0o755); err != nil {
		return m.copyOf(s), err
	}
	defer os.RemoveAll(swap)
	for _, pi := range cur.Plugins {
		if _, err := os.Stat(filepath.Join(m.cfg.Dir, pi.ID)); err == nil {
			if err := os.Rename(filepath.Join(m.cfg.Dir, pi.ID), filepath.Join(swap, pi.ID)); err != nil {
				return m.copyOf(s), err
			}
		}
	}
	for _, pi := range prev.Plugins {
		if err := os.Rename(filepath.Join(prevRoot, pi.ID), filepath.Join(m.cfg.Dir, pi.ID)); err != nil {
			return m.copyOf(s), fmt.Errorf("previous version is incomplete: %w", err)
		}
	}
	for _, pi := range cur.Plugins {
		os.Rename(filepath.Join(swap, pi.ID), filepath.Join(prevRoot, pi.ID))
	}
	m.mu.Lock()
	s.Ref, s.Commit, s.Plugins = prev.Ref, prev.Commit, prev.Plugins
	s.Prev = &cur
	s.AutoUpdate = false
	s.UpdatedAt, s.Error = time.Now(), ""
	m.saveLocked()
	m.mu.Unlock()
	m.registry.Reload()
	m.publish("plugin.updated", s)
	return m.copyOf(s), nil
}

// Update changes a source's settings.
type Update struct {
	Enabled    *bool   `json:"enabled"`
	AutoUpdate *bool   `json:"auto_update"`
	Ref        *string `json:"ref"` // "" = follow latest release / default branch
}

func (m *Manager) Configure(id string, u Update) (Source, error) {
	s := m.get(id)
	if s == nil {
		return Source{}, fmt.Errorf("source not found")
	}
	m.mu.Lock()
	reload := false
	if u.Enabled != nil && *u.Enabled != s.Enabled {
		s.Enabled, reload = *u.Enabled, true
	}
	if u.AutoUpdate != nil {
		s.AutoUpdate = *u.AutoUpdate
	}
	if u.Ref != nil && *u.Ref != s.Spec.Ref {
		if *u.Ref != "" && !validRef(*u.Ref) {
			m.mu.Unlock()
			return Source{}, fmt.Errorf("invalid ref %q", *u.Ref)
		}
		if s.Spec.Kind == Archive {
			m.mu.Unlock()
			return Source{}, fmt.Errorf("archive sources have no refs")
		}
		s.Spec.Ref = *u.Ref
		s.Latest = "" // re-check against the new ref
	}
	m.saveLocked()
	m.mu.Unlock()
	if reload {
		m.registry.Reload()
		topic := "plugin.disabled"
		if s.Enabled {
			topic = "plugin.enabled"
		}
		m.publish(topic, s)
	}
	return m.copyOf(s), nil
}

// Remove deletes a source and its plugin folders. Plugin data under
// $AIO_DATA/plugins/<id> is kept.
func (m *Manager) Remove(id string) error {
	s := m.get(id)
	if s == nil {
		return fmt.Errorf("source not found")
	}
	m.op.Lock()
	defer m.op.Unlock()
	m.mu.Lock()
	for i, x := range m.sources {
		if x == s {
			m.sources = append(m.sources[:i], m.sources[i+1:]...)
			break
		}
	}
	ids := map[string]bool{}
	for _, p := range s.Plugins {
		ids[p.ID] = true
	}
	if s.Prev != nil {
		for _, p := range s.Prev.Plugins {
			ids[p.ID] = true
		}
	}
	m.saveLocked()
	m.mu.Unlock()
	for pid := range ids {
		os.RemoveAll(filepath.Join(m.cfg.Dir, pid))
		os.RemoveAll(filepath.Join(m.cfg.Dir, ".prev", pid))
	}
	m.registry.Reload()
	m.publish("plugin.removed", s)
	return nil
}

// CheckAll checks every enabled source once.
func (m *Manager) CheckAll(ctx context.Context) {
	m.mu.Lock()
	ids := []string{}
	for _, s := range m.sources {
		if s.Enabled && s.Status == "" {
			ids = append(ids, s.ID)
			s.Status = "checking"
		}
	}
	m.mu.Unlock()
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		m.Check(ctx, id, false)
	}
	m.expirePreviews()
}

// Run checks for updates every Interval (with some jitter) until ctx ends.
func (m *Manager) Run(ctx context.Context) {
	if m.cfg.Interval <= 0 {
		return
	}
	first := time.Minute
	for {
		wait := first
		if wait == 0 {
			j, _ := rand.Int(rand.Reader, big.NewInt(int64(m.cfg.Interval/10)+1))
			wait = m.cfg.Interval + time.Duration(j.Int64())
		}
		first = 0
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		m.CheckAll(ctx)
	}
}

// CheckAsync marks the source as checking/updating and runs Check in the
// background, so the UI shows progress immediately.
func (m *Manager) CheckAsync(id string, apply bool) {
	if s := m.get(id); s != nil {
		st := "checking"
		if apply {
			st = "updating"
		}
		m.setStatus(s, st)
	}
	go m.Check(context.Background(), id, apply)
}
