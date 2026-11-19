// Package server exposes the plugin registry and job manager over HTTP.
package server

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"aio/internal/jobs"
	"aio/internal/plugin"
)

type Server struct {
	Registry  *plugin.Registry
	Jobs      *jobs.Manager
	Static    fs.FS
	MaxUpload int64 // bytes
	Version   string
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "version": s.Version})
	})
	mux.HandleFunc("GET /api/plugins", s.listPlugins)
	mux.HandleFunc("POST /api/plugins/reload", func(w http.ResponseWriter, r *http.Request) {
		s.Registry.Reload()
		s.listPlugins(w, r)
	})
	mux.HandleFunc("POST /api/plugins/{id}/run", s.runPlugin)
	mux.HandleFunc("GET /api/jobs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"jobs": s.Jobs.List()})
	})
	mux.HandleFunc("GET /api/jobs/{id}", s.withJob(func(w http.ResponseWriter, r *http.Request, j *jobs.Job) {
		writeJSON(w, 200, j.Snapshot(true))
	}))
	mux.HandleFunc("GET /api/jobs/{id}/events", s.withJob(s.jobEvents))
	mux.HandleFunc("GET /api/jobs/{id}/files/{name...}", s.withJob(s.jobFile))
	mux.HandleFunc("GET /api/jobs/{id}/zip", s.withJob(s.jobZip))
	mux.HandleFunc("POST /api/jobs/{id}/cancel", s.withJob(func(w http.ResponseWriter, r *http.Request, j *jobs.Job) {
		j.Cancel()
		writeJSON(w, 200, map[string]any{"ok": true})
	}))
	mux.HandleFunc("DELETE /api/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !s.Jobs.Delete(r.PathValue("id")) {
			httpError(w, 404, "job not found")
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	mux.HandleFunc("GET /p/{id}/{path...}", s.pluginAsset)

	static := http.FileServer(http.FS(s.Static))
	mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/vendor/") {
			w.Header().Set("Cache-Control", "public, max-age=604800")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		static.ServeHTTP(w, r)
	}))
	return logRequests(mux)
}

func (s *Server) listPlugins(w http.ResponseWriter, r *http.Request) {
	list, errs := s.Registry.List()
	type view struct {
		*plugin.Manifest
		UIURL string `json:"ui_url,omitempty"`
	}
	out := make([]view, 0, len(list))
	for _, m := range list {
		v := view{Manifest: m}
		if m.UI != "" {
			v.UIURL = "/p/" + m.ID + "/" + m.UI
		}
		out = append(out, v)
	}
	writeJSON(w, 200, map[string]any{"plugins": out, "errors": errs})
}

var unsafeChars = regexp.MustCompile(`[^\p{L}\p{N}._ ()\[\]+-]+`)

func sanitizeFilename(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.TrimSpace(unsafeChars.ReplaceAllString(name, "_"))
	if name == "" || name == "." || name == ".." {
		name = "file"
	}
	if len(name) > 180 {
		ext := filepath.Ext(name)
		if len(ext) > 20 {
			ext = ""
		}
		name = name[:180-len(ext)] + ext
	}
	return name
}

// runPlugin streams a multipart upload to disk, builds params and starts a job.
func (s *Server) runPlugin(w http.ResponseWriter, r *http.Request) {
	p := s.Registry.Get(r.PathValue("id"))
	if p == nil {
		httpError(w, 404, "plugin not found")
		return
	}
	j, err := s.Jobs.Create(p)
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	started := false
	defer func() {
		if !started {
			s.Jobs.Discard(j)
		}
	}()

	values := map[string][]string{}
	files := map[string][]string{}
	var firstFile string
	inDir := filepath.Join(j.Dir(), "in")

	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		r.Body = http.MaxBytesReader(w, r.Body, s.MaxUpload)
		mr, err := r.MultipartReader()
		if err != nil {
			httpError(w, 400, err.Error())
			return
		}
		n := 0
		for {
			part, err := mr.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				httpError(w, 400, "upload failed: "+err.Error())
				return
			}
			field := part.FormName()
			if field == "" {
				continue
			}
			if part.FileName() != "" {
				n++
				orig := sanitizeFilename(part.FileName())
				dst := filepath.Join(inDir, fmt.Sprintf("%03d_%s", n, orig))
				if err := saveFile(dst, part); err != nil {
					httpError(w, 400, "upload failed: "+err.Error())
					return
				}
				files[field] = append(files[field], dst)
				if firstFile == "" {
					firstFile = orig
				}
				continue
			}
			b, err := io.ReadAll(io.LimitReader(part, 4<<20))
			if err != nil {
				httpError(w, 400, err.Error())
				return
			}
			values[field] = append(values[field], string(b))
		}
	} else if err := r.ParseForm(); err == nil {
		values = r.PostForm
	}

	params, err := buildParams(p, values, files)
	if err != nil {
		httpError(w, 400, err.Error())
		return
	}
	title := first(values["__title"])
	if title == "" {
		title = firstFile
	}
	if title == "" {
		for _, in := range p.Inputs {
			if in.Type == "url" || in.Type == "text" || in.Type == "textarea" {
				if v, ok := params[in.Name].(string); ok && v != "" {
					title = strings.SplitN(v, "\n", 2)[0]
					break
				}
			}
		}
	}
	if len(title) > 120 {
		title = title[:120] + "…"
	}
	if err := s.Jobs.Start(j, p, params, title); err != nil {
		httpError(w, 500, err.Error())
		return
	}
	started = true
	writeJSON(w, 201, j.Snapshot(false))
}

func saveFile(dst string, r io.Reader) error {
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func first(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

// buildParams converts raw form values into typed params according to the
// manifest. Unknown fields are passed through as strings; a "__params" field
// holding a JSON object is merged in (used by custom UIs).
func buildParams(p *plugin.Manifest, values map[string][]string, files map[string][]string) (map[string]any, error) {
	params := map[string]any{}
	for k, v := range values {
		if strings.HasPrefix(k, "__") || p.Input(k) != nil {
			continue
		}
		if len(v) == 1 {
			params[k] = v[0]
		} else {
			params[k] = v
		}
	}
	for k, v := range files {
		if p.Input(k) == nil {
			params[k] = v
		}
	}
	if raw := first(values["__params"]); raw != "" {
		var extra map[string]any
		if err := json.Unmarshal([]byte(raw), &extra); err != nil {
			return nil, fmt.Errorf("__params: %w", err)
		}
		for k, v := range extra {
			params[k] = v
		}
	}
	for _, in := range p.Inputs {
		raw, has := values[in.Name]
		val := strings.TrimSpace(first(raw))
		switch in.Type {
		case "file":
			list := files[in.Name]
			if list == nil {
				list = []string{}
			}
			if in.Required && len(list) == 0 {
				return nil, fmt.Errorf("%s is required", in.Label)
			}
			params[in.Name] = list
		case "checkbox":
			// the web UI always sends checkbox fields explicitly as true/false,
			// so a missing field means "use the default" (e.g. curl callers)
			if !has {
				b, _ := in.Default.(bool)
				params[in.Name] = b
				continue
			}
			params[in.Name] = val == "true" || val == "on" || val == "1" || val == "yes"
		case "number":
			if val == "" {
				if in.Required {
					return nil, fmt.Errorf("%s is required", in.Label)
				}
				params[in.Name] = in.Default
				continue
			}
			f, err := strconv.ParseFloat(val, 64)
			if err != nil {
				return nil, fmt.Errorf("%s must be a number", in.Label)
			}
			if in.Min != nil && f < *in.Min || in.Max != nil && f > *in.Max {
				return nil, fmt.Errorf("%s is out of range", in.Label)
			}
			params[in.Name] = f
		default:
			if !has || val == "" {
				if in.Required {
					return nil, fmt.Errorf("%s is required", in.Label)
				}
				if in.Default != nil {
					params[in.Name] = fmt.Sprint(in.Default)
				} else {
					params[in.Name] = ""
				}
				continue
			}
			if in.Type == "select" && len(in.Options) > 0 {
				ok := false
				for _, o := range in.Options {
					ok = ok || o.Value == val
				}
				if !ok {
					return nil, fmt.Errorf("%s: invalid choice %q", in.Label, val)
				}
			}
			if in.Type == "textarea" {
				params[in.Name] = strings.TrimSpace(first(raw))
			} else {
				params[in.Name] = val
			}
		}
	}
	return params, nil
}

func (s *Server) withJob(h func(http.ResponseWriter, *http.Request, *jobs.Job)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		j := s.Jobs.Get(r.PathValue("id"))
		if j == nil {
			httpError(w, 404, "job not found")
			return
		}
		h(w, r, j)
	}
}

func (s *Server) jobEvents(w http.ResponseWriter, r *http.Request, j *jobs.Job) {
	fl, ok := w.(http.Flusher)
	if !ok {
		httpError(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // disable proxy buffering (nginx)

	snap, ch, unsub := j.Subscribe()
	defer unsub()
	send := func(event string, data any) {
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		fl.Flush()
	}
	send("snapshot", snap)
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		case ev := <-ch:
			send(ev.Type, ev.Data)
		}
	}
}

func (s *Server) jobFile(w http.ResponseWriter, r *http.Request, j *jobs.Job) {
	p, err := s.Jobs.FilePath(j, r.PathValue("name"))
	if err != nil {
		httpError(w, 400, err.Error())
		return
	}
	if fi, err := os.Stat(p); err != nil || fi.IsDir() {
		httpError(w, 404, "file not found")
		return
	}
	disp := "attachment"
	if r.URL.Query().Get("inline") == "1" {
		disp = "inline"
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disp, map[string]string{"filename": filepath.Base(p)}))
	http.ServeFile(w, r, p)
}

func (s *Server) jobZip(w http.ResponseWriter, r *http.Request, j *jobs.Job) {
	snap := j.Snapshot(false)
	name := snap.Plugin + "-" + snap.ID + ".zip"
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	zw := zip.NewWriter(w)
	defer zw.Close()
	for _, f := range snap.Files {
		p, err := s.Jobs.FilePath(j, f.Name)
		if err != nil {
			continue
		}
		src, err := os.Open(p)
		if err != nil {
			continue
		}
		// Store, don't deflate: outputs are mostly already-compressed media/PDFs.
		dst, err := zw.CreateHeader(&zip.FileHeader{Name: f.Name, Method: zip.Store, Modified: time.Now()})
		if err == nil {
			io.Copy(dst, src)
		}
		src.Close()
	}
}

// pluginAsset serves files for custom plugin UIs from the folder holding the UI page.
func (s *Server) pluginAsset(w http.ResponseWriter, r *http.Request) {
	p := s.Registry.Get(r.PathValue("id"))
	if p == nil || p.UI == "" {
		http.NotFound(w, r)
		return
	}
	// URL paths are relative to the plugin root, but only the folder that
	// contains the UI page (and its subfolders) is served.
	base := filepath.Join(p.Dir, filepath.Dir(filepath.FromSlash(p.UI)))
	rel := filepath.FromSlash(r.PathValue("path"))
	full := filepath.Join(p.Dir, rel)
	inside, err := filepath.Rel(base, full)
	if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		http.NotFound(w, r)
		return
	}
	for _, seg := range strings.Split(inside, string(filepath.Separator)) {
		if strings.HasPrefix(seg, ".") && seg != "." {
			http.NotFound(w, r)
			return
		}
	}
	if filepath.Base(full) == plugin.ManifestName {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFile(w, r, full)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (s *statusWriter) WriteHeader(c int) { s.code = c; s.ResponseWriter.WriteHeader(c) }
func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/api/health" {
			h.ServeHTTP(w, r)
			return
		}
		sw := &statusWriter{ResponseWriter: w, code: 200}
		start := time.Now()
		h.ServeHTTP(sw, r)
		if !strings.HasSuffix(r.URL.Path, "/events") {
			log.Printf("%s %s %d %s", r.Method, r.URL.Path, sw.code, time.Since(start).Round(time.Millisecond))
		}
	})
}
