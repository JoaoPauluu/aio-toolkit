package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCronNext(t *testing.T) {
	loc := time.UTC
	from := time.Date(2026, 1, 1, 10, 7, 30, 0, loc) // a Thursday
	cases := map[string]time.Time{
		"*/15 * * * *": time.Date(2026, 1, 1, 10, 15, 0, 0, loc),
		"0 3 * * *":    time.Date(2026, 1, 2, 3, 0, 0, 0, loc),
		"30 9 * * 1-5": time.Date(2026, 1, 2, 9, 30, 0, 0, loc),
		"0 0 1 * *":    time.Date(2026, 2, 1, 0, 0, 0, 0, loc),
		"@hourly":      time.Date(2026, 1, 1, 11, 0, 0, 0, loc),
		"0 12 * * 7":   time.Date(2026, 1, 4, 12, 0, 0, 0, loc), // Sunday
		"0 0 29 2 *":   time.Date(2028, 2, 29, 0, 0, 0, 0, loc),
	}
	for expr, want := range cases {
		s, err := ParseCron(expr)
		if err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		if got := s.Next(from); !got.Equal(want) {
			t.Errorf("%s: next = %v, want %v", expr, got, want)
		}
	}
	for _, bad := range []string{"* * * *", "60 * * * *", "*/0 * * * *", "a * * * *", "5-1 * * * *"} {
		if _, err := ParseCron(bad); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
	if s, _ := ParseCron("0 0 31 2 *"); !s.Next(from).IsZero() {
		t.Error("Feb 31 should never fire")
	}
}

func writeManifest(t *testing.T, body string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "p")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, ManifestName), []byte(body), 0o644)
	return dir
}

func TestManifestExtensions(t *testing.T) {
	m, err := Load(writeManifest(t, `
run: [sh, run.sh]
provides: [latex.compile]
requires: [{id: pdf-merge}, {source: github.com/o/r}]
service: {run: [python3, srv.py], health: health}
schedule: [{every: 1h, params: {mode: full}}, {cron: "0 3 * * *"}]
events: [{on: job.done, filter: {plugin: ytdlp}}]
inputs: [{name: mode, type: select, default: quick, options: [quick, full]}, {name: n, type: number, default: 2}, {name: f, type: file}]
`))
	if err != nil {
		t.Fatal(err)
	}
	if m.Service.Restart != "always" || m.Service.Health != "/health" || m.Schedule[0].Interval != time.Hour || m.Schedule[1].Spec == nil || !m.HasCapability("latex.compile") {
		t.Fatalf("unexpected: %+v", m)
	}
	d := m.Defaults(map[string]any{"mode": "full"})
	if d["mode"] != "full" || d["n"] != 2 || len(d["f"].([]string)) != 0 {
		t.Fatalf("defaults: %v", d)
	}
	bad := map[string]string{
		"api":            "run: [x]\naio_api: 99\n",
		"service-norun":  "service: {}\n",
		"restart":        "service: {run: [x], restart: sometimes}\n",
		"schedule-both":  "run: [x]\nschedule: [{every: 1h, cron: '* * * * *'}]\n",
		"schedule-fast":  "run: [x]\nschedule: [{every: 1s}]\n",
		"schedule-norun": "ui: ui/index.html\nschedule: [{every: 1h}]\n",
		"event-noon":     "run: [x]\nevents: [{filter: {a: b}}]\n",
		"requires":       "run: [x]\nrequires: [{id: a, source: b}]\n",
	}
	for name, body := range bad {
		if _, err := Load(writeManifest(t, body)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if _, err := Load(writeManifest(t, "service: {run: [x]}\n")); err != nil {
		t.Errorf("service-only plugin should load: %v", err)
	}
}

func TestRegistryFilterAndHooks(t *testing.T) {
	root := t.TempDir()
	for _, id := range []string{"a", "b"} {
		os.MkdirAll(filepath.Join(root, id), 0o755)
		os.WriteFile(filepath.Join(root, id, ManifestName), []byte("run: [x]\n"), 0o644)
	}
	r := NewRegistry([]string{root})
	r.SetCommunityRoot(root)
	var seen []string
	r.OnReload(func(m map[string]*Manifest) {
		ids := []string{}
		for id := range m {
			ids = append(ids, id)
		}
		seen = append(seen, strings.Join(sortStrings(ids), ","))
	})
	r.SetFilter(func(m *Manifest) bool { return m.ID != "b" })
	r.Reload()
	if r.Get("b") != nil || r.Get("a") == nil || !r.Get("a").Community {
		t.Fatal("filter / community flag not applied")
	}
	if len(seen) != 2 || seen[1] != "a" {
		t.Fatalf("hooks: %v", seen)
	}
}

func sortStrings(s []string) []string {
	for i := range s {
		for j := i + 1; j < len(s); j++ {
			if s[j] < s[i] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
	return s
}
