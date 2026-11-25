package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"aio/internal/community"
	"aio/internal/events"
)

// routes for community plugins, services and the event bus.
func (s *Server) extensionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/community", s.listCommunity)
	mux.HandleFunc("POST /api/community/preview", s.communityOp(s.previewCommunity))
	mux.HandleFunc("DELETE /api/community/preview/{token}", s.communityOp(func(w http.ResponseWriter, r *http.Request) {
		s.Community.Discard(r.PathValue("token"))
		writeJSON(w, 200, map[string]any{"ok": true})
	}))
	mux.HandleFunc("POST /api/community/install", s.communityOp(s.installCommunity))
	mux.HandleFunc("POST /api/community/check", s.communityOp(func(w http.ResponseWriter, r *http.Request) {
		go s.Community.CheckAll(context.Background())
		writeJSON(w, 202, map[string]any{"ok": true})
	}))
	mux.HandleFunc("PATCH /api/community/{id}", s.communityOp(s.configureCommunity))
	mux.HandleFunc("POST /api/community/{id}/check", s.communityOp(func(w http.ResponseWriter, r *http.Request) {
		s.communityCheck(w, r, false)
	}))
	mux.HandleFunc("POST /api/community/{id}/update", s.communityOp(func(w http.ResponseWriter, r *http.Request) {
		s.communityCheck(w, r, true)
	}))
	mux.HandleFunc("POST /api/community/{id}/rollback", s.communityOp(func(w http.ResponseWriter, r *http.Request) {
		src, err := s.Community.Rollback(r.PathValue("id"))
		if err != nil {
			httpError(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, sourceView(src))
	}))
	mux.HandleFunc("DELETE /api/community/{id}", s.communityOp(func(w http.ResponseWriter, r *http.Request) {
		if err := s.Community.Remove(r.PathValue("id")); err != nil {
			httpError(w, 404, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	}))

	mux.HandleFunc("GET /api/services", func(w http.ResponseWriter, r *http.Request) {
		if s.Services == nil {
			writeJSON(w, 200, map[string]any{"services": []any{}})
			return
		}
		writeJSON(w, 200, map[string]any{"services": s.Services.List()})
	})
	mux.HandleFunc("GET /api/services/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		if s.Services == nil {
			httpError(w, 404, "no such service")
			return
		}
		logs, ok := s.Services.Logs(r.PathValue("id"))
		if !ok {
			httpError(w, 404, "no such service")
			return
		}
		writeJSON(w, 200, map[string]any{"logs": logs})
	})
	mux.HandleFunc("POST /api/services/{id}/restart", func(w http.ResponseWriter, r *http.Request) {
		if s.Services == nil || !s.Services.Restart(r.PathValue("id")) {
			httpError(w, 404, "no such service")
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	if s.Services != nil {
		// One route per method: a method-less "/s/…" pattern would conflict
		// with "GET /" in net/http's mux.
		for _, m := range []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"} {
			mux.Handle(m+" /s/{id}/{path...}", s.Services)
		}
		mux.HandleFunc("GET /s/{id}", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/s/"+r.PathValue("id")+"/", http.StatusMovedPermanently)
		})
	}

	mux.HandleFunc("GET /api/events", s.eventStream)
	mux.HandleFunc("POST /api/events", s.publishEvent)
}

func (s *Server) communityOp(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Community == nil {
			httpError(w, 404, "community plugins are disabled (AIO_COMMUNITY=off)")
			return
		}
		h(w, r)
	}
}

type sourceJSON struct {
	community.Source
	WebURL          string `json:"web_url"`
	CommitURL       string `json:"commit_url,omitempty"`
	LatestURL       string `json:"latest_url,omitempty"`
	UpdateAvailable bool   `json:"update_available"`
	Canonical       string `json:"canonical"`
}

func sourceView(src community.Source) sourceJSON {
	v := sourceJSON{Source: src, WebURL: src.Spec.WebURL(), UpdateAvailable: src.UpdateAvailable(), Canonical: src.Spec.Canonical()}
	if src.Commit != "" {
		v.CommitURL = src.Spec.CommitURL(src.Commit)
	}
	if v.UpdateAvailable {
		v.LatestURL = src.Spec.CommitURL(src.Latest)
	}
	return v
}

func (s *Server) listCommunity(w http.ResponseWriter, r *http.Request) {
	if s.Community == nil {
		writeJSON(w, 200, map[string]any{"enabled": false, "sources": []any{}})
		return
	}
	list := s.Community.List()
	out := make([]sourceJSON, 0, len(list))
	updates := 0
	for _, src := range list {
		v := sourceView(src)
		if v.UpdateAvailable {
			updates++
		}
		out = append(out, v)
	}
	writeJSON(w, 200, map[string]any{"enabled": true, "sources": out, "updates": updates,
		"check_interval": s.CommunityInterval.String()})
}

func decodeBody(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

func (s *Server) previewCommunity(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL string `json:"url"`
	}
	if err := decodeBody(r, &body); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	p, err := s.Community.Preview(ctx, body.URL)
	if err != nil {
		httpError(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, p)
}

func (s *Server) installCommunity(w http.ResponseWriter, r *http.Request) {
	body := struct {
		Token      string `json:"token"`
		AutoUpdate *bool  `json:"auto_update"`
		Wait       bool   `json:"wait"`
	}{}
	if err := decodeBody(r, &body); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	auto := body.AutoUpdate == nil || *body.AutoUpdate
	src, err := s.Community.Install(body.Token, community.InstallOptions{AutoUpdate: auto, Async: !body.Wait})
	if err != nil {
		code := 400
		if src.ID != "" {
			code = 422 // registered but failed (e.g. setup); details in src.error
		}
		writeJSON(w, code, map[string]any{"error": err.Error(), "source": sourceView(src)})
		return
	}
	code := 201
	if !body.Wait {
		code = 202
	}
	writeJSON(w, code, sourceView(src))
}

func (s *Server) configureCommunity(w http.ResponseWriter, r *http.Request) {
	var u community.Update
	if err := decodeBody(r, &u); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	src, err := s.Community.Configure(r.PathValue("id"), u)
	if err != nil {
		httpError(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, sourceView(src))
}

// communityCheck checks one source; ?wait=1 blocks until done, otherwise the
// check (and any update it applies) runs in the background.
func (s *Server) communityCheck(w http.ResponseWriter, r *http.Request, apply bool) {
	id := r.PathValue("id")
	if _, ok := s.Community.Get(id); !ok {
		httpError(w, 404, "source not found")
		return
	}
	if r.URL.Query().Get("wait") == "" {
		s.Community.CheckAsync(id, apply)
		src, _ := s.Community.Get(id)
		writeJSON(w, 202, sourceView(src))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	src, err := s.Community.Check(ctx, id, apply)
	if err != nil {
		writeJSON(w, 422, map[string]any{"error": err.Error(), "source": sourceView(src)})
		return
	}
	writeJSON(w, 200, sourceView(src))
}

// ---------- events

func (s *Server) eventStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok || s.Events == nil {
		httpError(w, 500, "event stream unavailable")
		return
	}
	patterns := []string{"*"}
	if t := r.URL.Query().Get("topic"); t != "" {
		patterns = strings.Split(t, ",")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	ch, unsub := s.Events.Subscribe("*")
	defer unsub()
	fmt.Fprint(w, ": connected\n\n")
	fl.Flush()
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
			match := false
			for _, p := range patterns {
				match = match || events.Match(strings.TrimSpace(p), ev.Topic)
			}
			if !match {
				continue
			}
			b, _ := json.Marshal(ev)
			fmt.Fprintf(w, "id: %d\ndata: %s\n\n", ev.ID, b)
			fl.Flush()
		}
	}
}

var topicRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*(\.[a-z0-9][a-z0-9._-]*)+$`)

var reservedTopics = []string{"job.", "plugin.", "service.", "share."}

// publishEvent lets plugins (and other callers) publish custom events.
// Topics must look like "<namespace>.<name>"; the built-in namespaces are
// reserved. A job token header attributes the event to the calling plugin.
func (s *Server) publishEvent(w http.ResponseWriter, r *http.Request) {
	if s.Events == nil {
		httpError(w, 500, "events unavailable")
		return
	}
	var body struct {
		Topic string         `json:"topic"`
		Data  map[string]any `json:"data"`
	}
	if err := decodeBody(r, &body); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	if !topicRe.MatchString(body.Topic) {
		httpError(w, 400, "topic must look like <plugin-id>.<event-name> (lowercase)")
		return
	}
	for _, p := range reservedTopics {
		if strings.HasPrefix(body.Topic, p) {
			httpError(w, 400, "topics starting with "+p+" are reserved")
			return
		}
	}
	ev := events.Event{Topic: body.Topic, Data: body.Data}
	if tok := r.Header.Get(jobTokenHeader); tok != "" {
		j := s.Jobs.ByToken(tok)
		if j == nil {
			httpError(w, 400, "unknown or finished job token")
			return
		}
		origin, hops := j.Origin()
		ev.Source, ev.Hops = origin, hops
		if ev.Data == nil {
			ev.Data = map[string]any{}
		}
		ev.Data["from_plugin"] = j.Snapshot(false).Plugin
		ev.Data["from_job"] = j.Snapshot(false).ID
	}
	s.Events.Publish(ev)
	writeJSON(w, 202, map[string]any{"ok": true})
}

const jobTokenHeader = "X-AIO-Job-Token"
