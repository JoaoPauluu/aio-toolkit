// Package server exposes the plugin registry and job manager over HTTP.
package server

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"aio/internal/fsutil"
	"aio/internal/jobs"
	"aio/internal/plugin"
	"aio/internal/shares"
)

type Server struct {
	Registry  *plugin.Registry
	Jobs      *jobs.Manager
	Static    fs.FS
	MaxUpload int64 // bytes
	Version   string

	Shares         *shares.Manager
	ShareMaxUpload int64  // bytes
	PublicURL      string // external base URL used to build share links (may be empty)
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
	mux.HandleFunc("GET /api/config", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"public_url": strings.TrimRight(s.PublicURL, "/")})
	})
	if s.Shares != nil {
		// Admin (behind the reverse proxy's auth like the rest of /api).
		mux.HandleFunc("POST /api/shares", s.createShare)
		mux.HandleFunc("GET /api/shares", s.listShares)
		mux.HandleFunc("DELETE /api/shares/{id}", s.deleteShare)
		// Public: the reverse proxy must let /files/* through without auth.
		mux.HandleFunc("GET /files/{id}", s.publicHeaders(s.shareLanding))
		mux.HandleFunc("POST /files/{id}", s.publicHeaders(s.shareUnlock))
		mux.HandleFunc("GET /files/{id}/{name}", s.publicHeaders(s.shareFile))
	}

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

// runPlugin streams a multipart upload to disk, builds params and starts a job.
func (s *Server) runPlugin(w http.ResponseWriter, r *http.Request) {
	p := s.Registry.Get(r.PathValue("id"))
	if p == nil {
		httpError(w, 404, "plugin not found")
		return
	}
	if len(p.Run) == 0 {
		httpError(w, 400, "this plugin has no run command (UI-only)")
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
				orig := fsutil.SanitizeFilename(part.FileName())
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

// ---------- file shares

func (s *Server) createShare(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		httpError(w, 400, "expected multipart/form-data")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.ShareMaxUpload)
	mr, err := r.MultipartReader()
	if err != nil {
		httpError(w, 400, err.Error())
		return
	}
	sh, err := s.Shares.Create(mr)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			httpError(w, 413, fmt.Sprintf("upload exceeds the %d MB share limit", s.ShareMaxUpload>>20))
			return
		}
		httpError(w, 400, err.Error())
		return
	}
	log.Printf("share %s created (%d files, %s)", sh.ID, len(sh.Files), humanSize(sh.TotalSize()))
	writeJSON(w, 201, sh.View())
}

func (s *Server) listShares(w http.ResponseWriter, r *http.Request) {
	list := s.Shares.List()
	out := make([]shares.View, 0, len(list))
	for _, sh := range list {
		out = append(out, sh.View())
	}
	writeJSON(w, 200, map[string]any{"shares": out})
}

func (s *Server) deleteShare(w http.ResponseWriter, r *http.Request) {
	if !s.Shares.Delete(r.PathValue("id")) {
		httpError(w, 404, "share not found")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) publicHeaders(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("X-Robots-Tag", "noindex, nofollow")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("X-Content-Type-Options", "nosniff")
		h(w, r)
	}
}

// shareAuthorized reports whether the request may see the share's files.
func (s *Server) shareAuthorized(r *http.Request, sh *shares.Share) bool {
	if !sh.HasPassword() {
		return true
	}
	c, err := r.Cookie(shares.CookieName(sh.ID))
	return err == nil && s.Shares.CheckToken(sh.ID, c.Value)
}

func (s *Server) shareLanding(w http.ResponseWriter, r *http.Request) {
	sh := s.Shares.Get(r.PathValue("id"))
	if sh == nil {
		renderShareNotFound(w)
		return
	}
	s.renderShare(w, r, sh, 200, "")
}

func (s *Server) shareUnlock(w http.ResponseWriter, r *http.Request) {
	sh := s.Shares.Get(r.PathValue("id"))
	if sh == nil {
		renderShareNotFound(w)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	r.ParseForm()
	if !sh.CheckPassword(r.PostFormValue("password")) {
		time.Sleep(400 * time.Millisecond) // slow down guessing
		s.renderShare(w, r, sh, 401, "Wrong password. Try again.")
		return
	}
	c := &http.Cookie{
		Name:     shares.CookieName(sh.ID),
		Value:    s.Shares.Token(sh.ID),
		Path:     "/files/" + sh.ID,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"),
	}
	if !sh.Expires.IsZero() {
		c.Expires = sh.Expires
	}
	http.SetCookie(w, c)
	http.Redirect(w, r, "/files/"+sh.ID, http.StatusSeeOther)
}

// Types the share page may show inline; everything else is always an attachment.
var inlineTypes = map[string]struct{ mime, kind string }{
	".png": {"image/png", "image"}, ".jpg": {"image/jpeg", "image"}, ".jpeg": {"image/jpeg", "image"},
	".gif": {"image/gif", "image"}, ".webp": {"image/webp", "image"}, ".avif": {"image/avif", "image"},
	".bmp": {"image/bmp", "image"},
	".pdf": {"application/pdf", "pdf"},
	".mp4": {"video/mp4", "video"}, ".m4v": {"video/mp4", "video"}, ".webm": {"video/webm", "video"},
	".mov": {"video/quicktime", "video"}, ".ogv": {"video/ogg", "video"},
	".mp3": {"audio/mpeg", "audio"}, ".m4a": {"audio/mp4", "audio"}, ".aac": {"audio/aac", "audio"},
	".wav": {"audio/wav", "audio"}, ".ogg": {"audio/ogg", "audio"}, ".oga": {"audio/ogg", "audio"},
	".opus": {"audio/ogg", "audio"}, ".flac": {"audio/flac", "audio"},
}

func (s *Server) shareFile(w http.ResponseWriter, r *http.Request) {
	sh := s.Shares.Get(r.PathValue("id"))
	if sh == nil {
		renderShareNotFound(w)
		return
	}
	if !s.shareAuthorized(r, sh) {
		http.Redirect(w, r, "/files/"+sh.ID, http.StatusSeeOther)
		return
	}
	name := r.PathValue("name")
	p := s.Shares.FilePath(sh, name)
	f, err := os.Open(p)
	if p == "" || err != nil {
		renderShareNotFound(w)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		renderShareNotFound(w)
		return
	}
	disp := "attachment"
	if t, ok := inlineTypes[strings.ToLower(filepath.Ext(name))]; ok {
		w.Header().Set("Content-Type", t.mime)
		if r.URL.Query().Get("inline") == "1" {
			disp = "inline"
		}
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disp, map[string]string{"filename": name}))
	w.Header().Set("Cache-Control", "private, no-cache")
	http.ServeContent(w, r, name, fi.ModTime(), f)
}

type shareFileView struct {
	Name, Size, URL, InlineURL string
}

type sharePage struct {
	ID, Title, Error, Expiry, Note, Total string
	Locked, NotFound                      bool
	Files                                 []shareFileView
	PreviewKind, PreviewURL, PreviewName  string
}

func (s *Server) renderShare(w http.ResponseWriter, r *http.Request, sh *shares.Share, code int, errMsg string) {
	pg := sharePage{ID: sh.ID, Title: "Shared files", Error: errMsg, Locked: !s.shareAuthorized(r, sh)}
	switch {
	case pg.Locked:
		pg.Title = "Protected share" // don't reveal file names before the password
	case len(sh.Files) == 1:
		pg.Title = sh.Files[0].Name
	default:
		pg.Title = fmt.Sprintf("%d shared files", len(sh.Files))
	}
	if sh.Expires.IsZero() {
		pg.Expiry = "Never expires"
	} else {
		pg.Expiry = "Expires in " + humanDuration(time.Until(sh.Expires)) + " · " + sh.Expires.UTC().Format("2 Jan 2006 15:04 UTC")
	}
	if !pg.Locked {
		pg.Note = sh.Note
		pg.Total = humanSize(sh.TotalSize())
		for _, f := range sh.Files {
			u := "/files/" + sh.ID + "/" + url.PathEscape(f.Name)
			fv := shareFileView{Name: f.Name, Size: humanSize(f.Size), URL: u}
			if _, ok := inlineTypes[strings.ToLower(filepath.Ext(f.Name))]; ok {
				fv.InlineURL = u + "?inline=1"
			}
			pg.Files = append(pg.Files, fv)
		}
		if len(sh.Files) == 1 {
			if t, ok := inlineTypes[strings.ToLower(filepath.Ext(sh.Files[0].Name))]; ok {
				pg.PreviewKind, pg.PreviewURL, pg.PreviewName = t.kind, pg.Files[0].InlineURL, sh.Files[0].Name
			}
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(code)
	if err := shareTmpl.Execute(w, pg); err != nil {
		log.Printf("share page: %v", err)
	}
}

func renderShareNotFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(404)
	shareTmpl.Execute(w, sharePage{Title: "Link not found", NotFound: true})
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "less than a minute"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute")
	case d < 48*time.Hour:
		return plural(int(d.Hours()), "hour")
	default:
		return plural(int(d.Hours()/24), "day")
	}
}

func plural(n int, w string) string {
	if n == 1 {
		return "1 " + w
	}
	return fmt.Sprintf("%d %ss", n, w)
}

var shareTmpl = template.Must(template.New("share").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex, nofollow">
<meta name="referrer" content="no-referrer">
<title>{{.Title}}</title>
<link rel="stylesheet" href="/style.css">
<style>
  body { min-height: 100vh; display: flex; justify-content: center; padding: 40px 16px; }
  main { width: 100%; max-width: 760px; }
  .head { display: flex; gap: 12px; align-items: center; margin-bottom: 16px; }
  .head .ic { font-size: 30px; line-height: 1; }
  .head h1 { overflow-wrap: anywhere; }
  .box { padding: 18px; display: grid; gap: 14px; }
  .note { white-space: pre-wrap; overflow-wrap: anywhere; background: var(--panel-2); border-radius: 8px; padding: 10px 12px; }
  .files { list-style: none; margin: 0; padding: 0; display: grid; gap: 6px; }
  .files li { display: flex; align-items: center; gap: 10px; background: var(--panel-2); border-radius: 8px; padding: 8px 10px; }
  .files .nm { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .preview { border-radius: 8px; overflow: hidden; background: var(--panel-2); display: flex; justify-content: center; }
  .preview img, .preview video { max-width: 100%; max-height: 70vh; display: block; }
  .preview iframe { width: 100%; height: 75vh; border: 0; background: #fff; }
  .preview audio { width: 100%; margin: 14px; }
  form.pw { display: flex; gap: 8px; flex-wrap: wrap; }
  form.pw input { flex: 1; min-width: 180px; padding: 8px 10px; border: 1px solid var(--border); border-radius: 8px; background: var(--panel); color: var(--text); font: inherit; }
  form.pw input:focus { outline: 2px solid var(--accent-soft); border-color: var(--accent); }
  .err { color: var(--err); }
  footer { margin-top: 14px; text-align: center; }
</style>
</head>
<body>
<main>
{{if .NotFound}}
  <div class="head"><span class="ic">🔗</span><div><h1>Link not found</h1><div class="muted">This share doesn't exist or has expired.</div></div></div>
{{else}}
  <div class="head"><span class="ic">{{if .Locked}}🔒{{else}}📦{{end}}</span>
    <div><h1>{{.Title}}</h1><div class="muted small">{{.Expiry}}{{if .Total}} · {{.Total}}{{end}}</div></div></div>
  <div class="card box">
  {{if .Locked}}
    <div>This share is protected. Enter the password to see the files.</div>
    <form class="pw" method="post" action="/files/{{.ID}}">
      <input type="password" name="password" placeholder="Password" autocomplete="current-password" autofocus required>
      <button class="btn primary" type="submit">Unlock</button>
    </form>
    {{with .Error}}<div class="err">{{.}}</div>{{end}}
  {{else}}
    {{with .Note}}<div class="note">{{.}}</div>{{end}}
    {{if eq .PreviewKind "image"}}<div class="preview"><img src="{{.PreviewURL}}" alt="{{.PreviewName}}"></div>
    {{else if eq .PreviewKind "pdf"}}<div class="preview"><iframe src="{{.PreviewURL}}" title="{{.PreviewName}}"></iframe></div>
    {{else if eq .PreviewKind "video"}}<div class="preview"><video src="{{.PreviewURL}}" controls preload="metadata"></video></div>
    {{else if eq .PreviewKind "audio"}}<div class="preview"><audio src="{{.PreviewURL}}" controls preload="metadata"></audio></div>{{end}}
    <ul class="files">
    {{range .Files}}<li><span class="nm" title="{{.Name}}">{{.Name}}</span><span class="muted small">{{.Size}}</span>
      {{if .InlineURL}}<a class="btn small" href="{{.InlineURL}}" target="_blank" rel="noopener noreferrer">Open</a>{{end}}
      <a class="btn primary small" href="{{.URL}}" download>Download</a></li>
    {{end}}</ul>
  {{end}}
  </div>
{{end}}
  <footer class="muted small">Shared with AIO Toolkit</footer>
</main>
</body>
</html>`))
