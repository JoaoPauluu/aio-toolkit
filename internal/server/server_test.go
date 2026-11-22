package server

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"aio/internal/jobs"
	"aio/internal/plugin"
)

// writePlugin creates a plugin folder with the given manifest and run.sh.
func writePlugin(t *testing.T, root, id, manifest, script string) {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, plugin.ManifestName), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func newTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	root, data := t.TempDir(), t.TempDir()
	writePlugin(t, root, "echo", `
name: Echo
run: [sh, run.sh]
inputs:
  - {name: format, type: select, default: mp4, options: [mp4, mp3]}
  - {name: quality, type: select, default: "1080", options: ["720", "1080"], show_if: {format: [mp4]}}
  - {name: subs, type: checkbox, default: false, show_if: {format: [mp4]}}
  - {name: n, type: number, default: 1, min: 0, max: 10}
  - {name: pages, type: json}
`, `cp "$AIO_PARAMS" "$AIO_OUT/params.json"`+"\n")
	writePlugin(t, root, "sleep", "run: [sh, run.sh]\n", "sleep 5\n")
	writePlugin(t, root, "fail", "run: [sh, run.sh]\n", "echo boom; exit 3\n")
	writePlugin(t, root, "two", "run: [sh, run.sh]\n", `echo a > "$AIO_OUT/a.txt"; echo b > "$AIO_OUT/b.txt"`+"\n")

	mgr, err := jobs.NewManager(jobs.Config{DataDir: data, DownloadsDir: data, MaxJobs: 4})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Registry: plugin.NewRegistry([]string{root}), Jobs: mgr, Static: os.DirFS(t.TempDir()), MaxUpload: 1 << 20}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, data
}

func post(t *testing.T, url, ct string, body io.Reader) (*http.Response, []byte) {
	t.Helper()
	res, err := http.Post(url, ct, body)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, b
}

// runEcho runs the echo plugin synchronously and returns the params it received.
func runEcho(t *testing.T, ts *httptest.Server, ct string, body io.Reader) map[string]any {
	t.Helper()
	res, b := post(t, ts.URL+"/api/plugins/echo/run?wait=10s&output=file", ct, body)
	if res.StatusCode != 200 {
		t.Fatalf("status %d: %s", res.StatusCode, b)
	}
	var params map[string]any
	if err := json.Unmarshal(b, &params); err != nil {
		t.Fatalf("not params.json: %s", b)
	}
	return params
}

func form(fields map[string]string) (string, io.Reader) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		mw.WriteField(k, v)
	}
	mw.Close()
	return mw.FormDataContentType(), &buf
}

func TestJSONBodyMatchesForm(t *testing.T) {
	ts, _ := newTestServer(t)
	ct, body := form(map[string]string{"format": "mp4", "quality": "720", "subs": "true", "n": "3", "pages": `[{"file":0,"page":2}]`})
	fromForm := runEcho(t, ts, ct, body)
	fromJSON := runEcho(t, ts, "application/json", strings.NewReader(
		`{"format":"mp4","quality":720,"subs":true,"n":3,"pages":[{"file":0,"page":2}]}`))
	a, _ := json.Marshal(fromForm)
	b, _ := json.Marshal(fromJSON)
	if !bytes.Equal(a, b) {
		t.Fatalf("form and JSON params differ:\n%s\n%s", a, b)
	}
	if fromJSON["quality"] != "720" || fromJSON["n"] != 3.0 || fromJSON["subs"] != true {
		t.Fatalf("unexpected params: %s", b)
	}
}

func TestShowIfHiddenFieldGetsDefault(t *testing.T) {
	ts, _ := newTestServer(t)
	p := runEcho(t, ts, "application/json", strings.NewReader(`{"format":"mp3","quality":"720","subs":true}`))
	if p["quality"] != "1080" {
		t.Fatalf("hidden select should fall back to default, got %v", p["quality"])
	}
	if p["subs"] != true { // the form always sends checkboxes
		t.Fatalf("checkbox should keep its value, got %v", p["subs"])
	}
}

func TestValidation(t *testing.T) {
	ts, _ := newTestServer(t)
	for _, body := range []string{`{"n":99}`, `{"format":"avi"}`, `{"n":"x"}`, `not json`} {
		res, b := post(t, ts.URL+"/api/plugins/echo/run", "application/json", strings.NewReader(body))
		if res.StatusCode != 400 {
			t.Errorf("%s: want 400, got %d %s", body, res.StatusCode, b)
		}
	}
	// In a form, a json input carries JSON text, which must parse.
	ct, body := form(map[string]string{"pages": "[oops"})
	if res, b := post(t, ts.URL+"/api/plugins/echo/run", ct, body); res.StatusCode != 400 {
		t.Errorf("invalid JSON field: want 400, got %d %s", res.StatusCode, b)
	}
}

func TestWaitModes(t *testing.T) {
	ts, _ := newTestServer(t)

	res, b := post(t, ts.URL+"/api/plugins/sleep/run?wait=200ms", "application/json", strings.NewReader(`{}`))
	if res.StatusCode != 202 || !bytes.Contains(b, []byte(`"state":"running"`)) {
		t.Fatalf("want 202 running, got %d %s", res.StatusCode, b)
	}

	res, b = post(t, ts.URL+"/api/plugins/fail/run?wait=10s&output=file", "application/json", strings.NewReader(`{}`))
	if res.StatusCode != 422 || !bytes.Contains(b, []byte("boom")) {
		t.Fatalf("want 422 with logs, got %d %s", res.StatusCode, b)
	}

	res, b = post(t, ts.URL+"/api/plugins/two/run?wait=10s", "application/json", strings.NewReader(`{}`))
	var job struct {
		ID    string
		State string
		Files []struct{ Name, URL string }
	}
	json.Unmarshal(b, &job)
	if res.StatusCode != 200 || job.State != "done" || len(job.Files) != 2 || job.Files[0].URL != "/api/jobs/"+job.ID+"/files/a.txt" {
		t.Fatalf("unexpected done response %d %s", res.StatusCode, b)
	}

	res, b = post(t, ts.URL+"/api/plugins/two/run?wait=10s&output=file&file=b.txt", "application/json", strings.NewReader(`{}`))
	if res.StatusCode != 200 || string(b) != "b\n" {
		t.Fatalf("want b.txt contents, got %d %q", res.StatusCode, b)
	}
	res, _ = post(t, ts.URL+"/api/plugins/two/run?wait=10s&output=file", "application/json", strings.NewReader(`{}`))
	if res.Header.Get("Content-Type") != "application/zip" {
		t.Fatalf("multiple outputs should be zipped, got %s", res.Header.Get("Content-Type"))
	}

	// GET /api/jobs/{id}?wait= on an already-finished job returns at once.
	start := time.Now()
	gr, err := http.Get(ts.URL + "/api/jobs/" + job.ID + "?wait=10s")
	if err != nil || gr.StatusCode != 200 || time.Since(start) > 2*time.Second {
		t.Fatalf("job wait: %v %v", err, gr)
	}
	gr.Body.Close()
}

func TestPluginSchema(t *testing.T) {
	ts, _ := newTestServer(t)
	res, err := http.Get(ts.URL + "/api/plugins/echo")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		RunURL string `json:"run_url"`
		Inputs []plugin.Input
	}
	json.NewDecoder(res.Body).Decode(&v)
	res.Body.Close()
	if v.RunURL != "/api/plugins/echo/run" || len(v.Inputs) != 5 || v.Inputs[1].ShowIf["format"][0] != "mp4" {
		t.Fatalf("unexpected schema: %+v", v)
	}
	if res, _ := http.Get(ts.URL + "/api/plugins/nope"); res.StatusCode != 404 {
		t.Fatalf("want 404, got %d", res.StatusCode)
	}
}

// TestBuiltinPluginsAPIParity guards "everything the UI can do, the API can
// do": every param a built-in script reads must be declared in its manifest
// (undeclared params can only be set by custom UIs), and each plugin must be
// callable with just its defaults plus required inputs.
func TestBuiltinPluginsAPIParity(t *testing.T) {
	root, _ := filepath.Abs("../../plugins")
	reg := plugin.NewRegistry([]string{root})
	list, errs := reg.List()
	if len(errs) > 0 {
		t.Fatalf("plugin load errors: %v", errs)
	}
	readRe := regexp.MustCompile(`\bp(?:arams)?(?:\.get\(|\[)["']([a-z_][a-z0-9_]*)["']`)
	for _, m := range list {
		if len(m.Run) == 0 {
			continue
		}
		if src, err := os.ReadFile(filepath.Join(m.Dir, "run.py")); err == nil {
			for _, g := range readRe.FindAllStringSubmatch(string(src), -1) {
				if m.Input(g[1]) == nil {
					t.Errorf("%s: run.py reads %q, which plugin.yaml doesn't declare (not settable via the API)", m.ID, g[1])
				}
			}
		}
		values, files := map[string][]string{}, map[string][]string{}
		for _, in := range m.Inputs {
			if !in.Required {
				continue
			}
			switch in.Type {
			case "file":
				files[in.Name] = []string{"/dev/null"}
			case "number":
				values[in.Name] = []string{"1"}
			case "select":
				values[in.Name] = []string{in.Options[0].Value}
			case "json":
				values[in.Name] = []string{"null"}
			default:
				values[in.Name] = []string{"x"}
			}
		}
		if _, err := buildParams(m, values, files); err != nil {
			t.Errorf("%s: can't be called with defaults: %v", m.ID, err)
		}
	}
}
