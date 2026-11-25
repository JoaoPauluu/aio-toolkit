package community

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"aio/internal/events"
	"aio/internal/jobs"
	"aio/internal/plugin"
)

func TestParseSpec(t *testing.T) {
	cases := []struct {
		in   string
		want Spec
		err  bool
	}{
		{in: "https://github.com/Owner/Repo", want: Spec{Kind: GitHub, Host: "github.com", Repo: "Owner/Repo"}},
		{in: "github.com/o/r.git", want: Spec{Kind: GitHub, Host: "github.com", Repo: "o/r"}},
		{in: "o/r", want: Spec{Kind: GitHub, Host: "github.com", Repo: "o/r"}},
		{in: "o/r@v1.2.0", want: Spec{Kind: GitHub, Host: "github.com", Repo: "o/r", Ref: "v1.2.0"}},
		{in: "o/r@main/plugins/foo", want: Spec{Kind: GitHub, Host: "github.com", Repo: "o/r", Ref: "main", Subdir: "plugins/foo"}},
		{in: "https://github.com/o/r/tree/dev/examples/x", want: Spec{Kind: GitHub, Host: "github.com", Repo: "o/r", Ref: "dev", Subdir: "examples/x"}},
		{in: "https://github.com/o/r#sub", want: Spec{Kind: GitHub, Host: "github.com", Repo: "o/r", Subdir: "sub"}},
		{in: "https://gitlab.com/g/sub/r", want: Spec{Kind: GitLab, Host: "gitlab.com", Repo: "g/sub/r"}},
		{in: "https://gitlab.com/g/r/-/tree/main/p", want: Spec{Kind: GitLab, Host: "gitlab.com", Repo: "g/r", Ref: "main", Subdir: "p"}},
		{in: "https://codeberg.org/o/r", want: Spec{Kind: Gitea, Host: "codeberg.org", Repo: "o/r"}},
		{in: "https://codeberg.org/o/r/src/branch/main/x", want: Spec{Kind: Gitea, Host: "codeberg.org", Repo: "o/r", Ref: "main", Subdir: "x"}},
		{in: "http://git.lan:3000/o/r", want: Spec{Kind: Gitea, Scheme: "http", Host: "git.lan:3000", Repo: "o/r"}},
		{in: "https://example.com/dl/plugin.tar.gz?x=1", want: Spec{Kind: Archive, URL: "https://example.com/dl/plugin.tar.gz?x=1"}},
		{in: "", err: true},
		{in: "https://github.com/o", err: true},
		{in: "https://github.com/o/r/issues/1", err: true},
		{in: "o/r@--upload-pack", err: true},
		{in: "ftp://x/y/z", err: true},
		{in: "https://github.com/o/r/tree/main/../..", err: true},
	}
	for _, c := range cases {
		got, err := ParseSpec(c.in)
		if c.err {
			if err == nil {
				t.Errorf("%q: expected error, got %+v", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("%q:\n got  %+v\n want %+v", c.in, got, c.want)
		}
	}
}

type entry struct {
	name, body, link string
	typ             byte
	mode            int64
}

func tarGz(t *testing.T, entries []entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		h := &tar.Header{Name: e.name, Typeflag: typ, Mode: mode, Size: int64(len(e.body)), Linkname: e.link}
		if typ != tar.TypeReg {
			h.Size = 0
		}
		if typ == tar.TypeXGlobalHeader {
			h = &tar.Header{Typeflag: typ, PAXRecords: map[string]string{"comment": "abc"}}
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func writeTemp(t *testing.T, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "a")
	os.WriteFile(p, b, 0o644)
	return p
}

func TestExtractSafety(t *testing.T) {
	bad := map[string][]entry{
		"dotdot":       {{name: "top/../../evil", body: "x"}},
		"absolute":     {{name: "/etc/evil", body: "x"}},
		"link-out":     {{name: "top/l", typ: tar.TypeSymlink, link: "../../etc"}},
		"link-abs":     {{name: "top/l", typ: tar.TypeSymlink, link: "/etc/passwd"}},
		"hardlink":     {{name: "top/h", typ: tar.TypeLink, link: "top/x"}},
		"device":       {{name: "top/d", typ: tar.TypeChar}},
		"link-in-link": {{name: "top/a", typ: tar.TypeSymlink, link: "."}, {name: "top/a/b", typ: tar.TypeSymlink, link: "../x"}},
		"too-big":      {{name: "top/big", body: strings.Repeat("x", 2000)}},
	}
	for name, ents := range bad {
		dest := filepath.Join(t.TempDir(), "out")
		if err := Extract(writeTemp(t, tarGz(t, ents)), dest, 1000); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	// A good archive: top folder stripped, exec bit kept, internal link allowed.
	dest := filepath.Join(t.TempDir(), "out")
	err := Extract(writeTemp(t, tarGz(t, []entry{
		{name: "pax_global_header", typ: tar.TypeXGlobalHeader},
		{name: "repo-abc/", typ: tar.TypeDir},
		{name: "repo-abc/run.sh", body: "echo hi", mode: 0o755},
		{name: "repo-abc/sub/file.txt", body: "x"},
		{name: "repo-abc/link", typ: tar.TypeSymlink, link: "sub/file.txt"},
	})), dest, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dest, "run.sh"))
	if err != nil || fi.Mode()&0o100 == 0 {
		t.Fatalf("run.sh missing or not executable: %v %v", fi, err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "link")); err != nil || string(b) != "x" {
		t.Fatalf("link: %q %v", b, err)
	}

	// zip works too
	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	w, _ := zw.Create("p/plugin.yaml")
	w.Write([]byte("run: [sh, run.sh]\n"))
	zw.Close()
	dest = filepath.Join(t.TempDir(), "out")
	if err := Extract(writeTemp(t, zb.Bytes()), dest, 1<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "plugin.yaml")); err != nil {
		t.Fatal(err)
	}
}

// fakeGitHub serves the few API endpoints the fetcher uses for repo o/r.
type fakeGitHub struct {
	mu       sync.Mutex
	commit   string
	release  string
	files    map[string]map[string]string // commit -> path -> content
	requests []string
}

func (f *fakeGitHub) set(commit string, files map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commit = commit
	if f.files == nil {
		f.files = map[string]map[string]string{}
	}
	f.files[commit] = files
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.URL.Path)
	switch {
	case r.URL.Path == "/repos/o/r/releases/latest":
		if f.release == "" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"tag_name": f.release})
	case r.URL.Path == "/repos/o/r":
		json.NewEncoder(w).Encode(map[string]string{"default_branch": "main"})
	case strings.HasPrefix(r.URL.Path, "/repos/o/r/commits/"):
		if r.Header.Get("Accept") != "application/vnd.github.sha" {
			http.Error(w, "bad accept", 400)
			return
		}
		fmt.Fprint(w, f.commit)
	case strings.HasPrefix(r.URL.Path, "/repos/o/r/tarball/"):
		sha := strings.TrimPrefix(r.URL.Path, "/repos/o/r/tarball/")
		files := f.files[sha]
		if files == nil {
			http.NotFound(w, r)
			return
		}
		var ents []entry
		for p, body := range files {
			mode := int64(0o644)
			if strings.HasSuffix(p, ".sh") {
				mode = 0o755
			}
			ents = append(ents, entry{name: "o-r-" + sha[:7] + "/" + p, body: body, mode: mode})
		}
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		for _, e := range ents {
			tw.WriteHeader(&tar.Header{Name: e.name, Mode: e.mode, Size: int64(len(e.body)), Typeflag: tar.TypeReg})
			tw.Write([]byte(e.body))
		}
		tw.Close()
		gz.Close()
		w.Write(buf.Bytes())
	default:
		http.NotFound(w, r)
	}
}

type env struct {
	m        *Manager
	reg      *plugin.Registry
	gh       *fakeGitHub
	dir      string
	builtins string
	bus      *events.Bus
}

func newEnv(t *testing.T) *env {
	t.Helper()
	base := t.TempDir()
	builtins, dir, local, data := filepath.Join(base, "builtin"), filepath.Join(base, "community"), filepath.Join(base, "local"), filepath.Join(base, "data")
	for _, d := range []string{builtins, local, data} {
		os.MkdirAll(d, 0o755)
	}
	os.MkdirAll(filepath.Join(builtins, "pdf-merge"), 0o755)
	os.WriteFile(filepath.Join(builtins, "pdf-merge", "plugin.yaml"), []byte("run: [sh, run.sh]\n"), 0o644)
	reg := plugin.NewRegistry([]string{builtins, dir, local})
	bus := events.New()
	jm, err := jobs.NewManager(jobs.Config{DataDir: data, DownloadsDir: data, MaxJobs: 2, Events: bus})
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(Config{Dir: dir, MaxBytes: 10 << 20}, reg, jm, bus)
	if err != nil {
		t.Fatal(err)
	}
	gh := &fakeGitHub{}
	ts := httptest.NewServer(gh)
	t.Cleanup(ts.Close)
	m.Fetcher.GitHubAPI = ts.URL
	return &env{m: m, reg: reg, gh: gh, dir: dir, builtins: builtins, bus: bus}
}

func manifest(id, version string, extra string) string {
	return fmt.Sprintf("id: %s\nname: %s\nversion: %s\nrun: [sh, run.sh]\n%s", id, strings.ToUpper(id), version, extra)
}

func (e *env) install(t *testing.T, input string, auto bool) Source {
	t.Helper()
	p, err := e.m.Preview(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	src, err := e.m.Install(p.Token, InstallOptions{AutoUpdate: auto})
	if err != nil {
		t.Fatalf("install: %v (%+v)", err, src)
	}
	return src
}

func TestInstallUpdateRollbackRemove(t *testing.T) {
	e := newEnv(t)
	sha1, sha2 := strings.Repeat("a", 40), strings.Repeat("b", 40)
	e.gh.set(sha1, map[string]string{
		"plugin.yaml": manifest("hello", "1", "setup: [sh, setup.sh]\n"),
		"run.sh":      "echo v1",
		"setup.sh":    `echo ready > "$AIO_PLUGIN_DATA/setup-ran"`,
	})
	sub, unsub := e.bus.Subscribe("plugin.*")
	defer unsub()

	p, err := e.m.Preview(context.Background(), "https://github.com/o/r")
	if err != nil {
		t.Fatal(err)
	}
	if p.Version.Ref != "main" || p.Version.Commit != sha1 || len(p.Plugins) != 1 || p.Plugins[0].ID != "hello" {
		t.Fatalf("preview: %+v", p)
	}
	if e.reg.Get("hello") != nil {
		t.Fatal("previewing must not install")
	}
	src, err := e.m.Install(p.Token, InstallOptions{AutoUpdate: false})
	if err != nil {
		t.Fatal(err)
	}
	got := e.reg.Get("hello")
	if got == nil || !got.Community || got.Version != "1" {
		t.Fatalf("not installed: %+v", got)
	}
	if ev := <-sub; ev.Topic != "plugin.installed" {
		t.Fatalf("event %v", ev.Topic)
	}

	// New commit; auto-update off → only recorded.
	e.gh.set(sha2, map[string]string{"plugin.yaml": manifest("hello", "2", ""), "run.sh": "echo v2"})
	s, err := e.m.Check(context.Background(), src.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if !s.UpdateAvailable() || s.Commit != sha1 || e.reg.Get("hello").Version != "1" {
		t.Fatalf("should only record the update: %+v", s)
	}
	// Turn auto-update on → the next check applies it.
	on := true
	e.m.Configure(src.ID, Update{AutoUpdate: &on})
	if s, err = e.m.Check(context.Background(), src.ID, false); err != nil {
		t.Fatal(err)
	}
	if s.Commit != sha2 || s.UpdateAvailable() || e.reg.Get("hello").Version != "2" || s.Prev == nil || s.Prev.Commit != sha1 {
		t.Fatalf("auto-update not applied: %+v", s)
	}
	// Roll back.
	if s, err = e.m.Rollback(src.ID); err != nil {
		t.Fatal(err)
	}
	if s.Commit != sha1 || s.AutoUpdate || e.reg.Get("hello").Version != "1" {
		t.Fatalf("rollback: %+v", s)
	}
	// Disable / enable.
	off := false
	e.m.Configure(src.ID, Update{Enabled: &off})
	if e.reg.Get("hello") != nil {
		t.Fatal("disabled plugin still active")
	}
	e.m.Configure(src.ID, Update{Enabled: &on})
	if e.reg.Get("hello") == nil {
		t.Fatal("re-enabled plugin missing")
	}
	// State survives a restart.
	m2, err := NewManager(Config{Dir: e.dir, MaxBytes: 10 << 20}, e.reg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if l := m2.List(); len(l) != 1 || l[0].Commit != sha1 || !l[0].Enabled {
		t.Fatalf("state after restart: %+v", l)
	}
	// Remove.
	if err := e.m.Remove(src.ID); err != nil {
		t.Fatal(err)
	}
	if e.reg.Get("hello") != nil || len(e.m.List()) != 0 {
		t.Fatal("not removed")
	}
	if _, err := os.Stat(filepath.Join(e.dir, "hello")); !os.IsNotExist(err) {
		t.Fatal("folder left behind")
	}
}

func TestSetupFailureKeepsCurrentVersion(t *testing.T) {
	e := newEnv(t)
	sha1, sha2 := strings.Repeat("1", 40), strings.Repeat("2", 40)
	e.gh.set(sha1, map[string]string{"plugin.yaml": manifest("s", "1", ""), "run.sh": "true"})
	src := e.install(t, "o/r", true)
	e.gh.set(sha2, map[string]string{"plugin.yaml": manifest("s", "2", "setup: [sh, setup.sh]\n"), "run.sh": "true", "setup.sh": "echo nope; exit 1"})
	s, err := e.m.Check(context.Background(), src.ID, false)
	if err == nil || !strings.Contains(s.Error, "setup") {
		t.Fatalf("expected setup error, got %v / %q", err, s.Error)
	}
	if e.reg.Get("s").Version != "1" || s.Commit != sha1 {
		t.Fatal("failed update replaced the plugin")
	}
}

func TestPackSubdirReleaseAndOverride(t *testing.T) {
	e := newEnv(t)
	sha := strings.Repeat("c", 40)
	e.gh.release = "v3.0"
	e.gh.set(sha, map[string]string{
		"README.md":                   "x",
		"plugins/one/plugin.yaml":     manifest("one", "1", ""),
		"plugins/one/run.sh":          "true",
		"plugins/pdf-merge/plugin.yaml": manifest("pdf-merge", "9", ""),
		"plugins/pdf-merge/run.sh":    "true",
	})
	p, err := e.m.Preview(context.Background(), "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if p.Version.Ref != "v3.0" || len(p.Plugins) != 2 {
		t.Fatalf("preview: %+v", p)
	}
	var override bool
	for _, pi := range p.Plugins {
		override = override || pi.ID == "pdf-merge" && pi.OverridesBuiltin
	}
	if !override {
		t.Fatalf("pdf-merge should be flagged as overriding a built-in: %+v", p.Plugins)
	}
	if _, err := e.m.Install(p.Token, InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	if e.reg.Get("pdf-merge").Version != "9" || e.reg.Get("one") == nil {
		t.Fatal("pack not installed / not overriding")
	}

	// A subfolder pointing at one plugin, from another "repo" spec, conflicts.
	if p, err = e.m.Preview(context.Background(), "https://github.com/o/r/tree/main/plugins/one"); err != nil {
		t.Fatal(err)
	}
	if p.Plugins[0].Conflict == "" {
		t.Fatal("expected conflict with the installed pack")
	}
	if _, err := e.m.Install(p.Token, InstallOptions{}); err == nil {
		t.Fatal("conflicting install should fail")
	}
}

func TestRootManifestWithoutIDUsesRepoName(t *testing.T) {
	e := newEnv(t)
	sha := strings.Repeat("d", 40)
	e.gh.set(sha, map[string]string{"plugin.yaml": "name: R\nrun: [sh, run.sh]\n", "run.sh": "true"})
	e.install(t, "o/r", false)
	if e.reg.Get("r") == nil {
		t.Fatalf("expected plugin id r")
	}
}

func TestRejectsNewerAPI(t *testing.T) {
	e := newEnv(t)
	sha := strings.Repeat("e", 40)
	e.gh.set(sha, map[string]string{"plugin.yaml": manifest("x", "1", "aio_api: 99\n"), "run.sh": "true"})
	if _, err := e.m.Preview(context.Background(), "o/r"); err == nil || !strings.Contains(err.Error(), "API") {
		t.Fatalf("expected API version error, got %v", err)
	}
}
