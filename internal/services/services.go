// Package services supervises long-running plugin processes (a manifest's
// `service` key) and reverse-proxies HTTP requests to them at /s/<id>/.
package services

import (
	"bufio"
	"context"
	"fmt"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"aio/internal/events"
	"aio/internal/jobs"
	"aio/internal/plugin"
)

const maxLog = 500

var errRestart = errors.New("restart requested")

// Status is the JSON view of a service.
type Status struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	State     string    `json:"state"` // starting | running | stopped | crashed | backoff
	Port      int       `json:"port,omitempty"`
	PID       int       `json:"pid,omitempty"`
	Restarts  int       `json:"restarts"`
	Started   time.Time `json:"started,omitempty"`
	LastError string    `json:"last_error,omitempty"`
	URL       string    `json:"url"`
}

type service struct {
	m       *plugin.Manifest
	dirInfo os.FileInfo
	cancel  context.CancelFunc
	done   chan struct{}

	mu     sync.Mutex
	st     Status
	logs   []string
	proxy  *httputil.ReverseProxy
	manual chan struct{} // restart request
}

type Supervisor struct {
	Jobs   *jobs.Manager
	Events *events.Bus

	mu   sync.Mutex
	ctx  context.Context
	svcs map[string]*service
}

func (s *Supervisor) Start(ctx context.Context) {
	s.mu.Lock()
	s.ctx = ctx
	if s.svcs == nil {
		s.svcs = map[string]*service{}
	}
	s.mu.Unlock()
	go func() {
		<-ctx.Done()
		s.Sync(nil)
	}()
}

// Sync starts services of new plugins, restarts changed ones and stops the
// ones that went away. Call it from Registry.OnReload.
func (s *Supervisor) Sync(plugins map[string]*plugin.Manifest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.svcs == nil {
		s.svcs = map[string]*service{}
	}
	for id, sv := range s.svcs {
		p := plugins[id]
		if p == nil || p.Service == nil || changed(sv, p) || s.ctx == nil || s.ctx.Err() != nil {
			sv.stop()
			delete(s.svcs, id)
		}
	}
	if s.ctx == nil || s.ctx.Err() != nil {
		return
	}
	for id, p := range plugins {
		if p.Service == nil {
			continue
		}
		if _, ok := s.svcs[id]; ok {
			continue
		}
		sv := &service{m: p, done: make(chan struct{}), manual: make(chan struct{}, 1),
			st: Status{ID: id, Name: p.Name, State: "starting", URL: "/s/" + id + "/"}}
		sv.dirInfo, _ = os.Stat(p.Dir)
		ctx, cancel := context.WithCancel(s.ctx)
		sv.cancel = cancel
		s.svcs[id] = sv
		go s.supervise(ctx, sv)
	}
}

// changed reports whether a reload replaced the plugin's folder (an update
// swaps in a new directory) or its service config.
func changed(sv *service, b *plugin.Manifest) bool {
	a := sv.m
	if a.Dir != b.Dir || strings.Join(a.Service.Run, "\x00") != strings.Join(b.Service.Run, "\x00") ||
		a.Service.Health != b.Service.Health || a.Service.Restart != b.Service.Restart {
		return true
	}
	fi, err := os.Stat(b.Dir)
	return err != nil || sv.dirInfo == nil || !os.SameFile(sv.dirInfo, fi)
}

func (sv *service) stop() {
	sv.cancel()
	select {
	case <-sv.done:
	case <-time.After(15 * time.Second):
	}
}

func (sv *service) logLine(line string) {
	sv.mu.Lock()
	if len(sv.logs) >= maxLog {
		copy(sv.logs, sv.logs[len(sv.logs)-maxLog/2:])
		sv.logs = sv.logs[:maxLog/2]
	}
	sv.logs = append(sv.logs, time.Now().Format("15:04:05 ")+line)
	sv.mu.Unlock()
}

func (sv *service) set(f func(*Status)) {
	sv.mu.Lock()
	f(&sv.st)
	sv.mu.Unlock()
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func (s *Supervisor) publish(topic string, sv *service) {
	sv.mu.Lock()
	data := map[string]any{"plugin": sv.st.ID, "state": sv.st.State, "port": sv.st.Port}
	if sv.st.LastError != "" {
		data["error"] = sv.st.LastError
	}
	sv.mu.Unlock()
	s.Events.Publish(events.Event{Topic: topic, Data: data, Source: sv.st.ID})
}

func (s *Supervisor) supervise(ctx context.Context, sv *service) {
	defer close(sv.done)
	backoff := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := s.runOnce(ctx, sv)
		if ctx.Err() != nil {
			break
		}
		if errors.Is(err, errRestart) {
			backoff = time.Second
			continue
		}
		failed := err != nil
		sv.set(func(st *Status) {
			st.State, st.PID = "crashed", 0
			if err != nil {
				st.LastError = err.Error()
			} else {
				st.State, st.LastError = "stopped", "exited"
			}
		})
		s.publish("service.stopped", sv)
		restart := sv.m.Service.Restart
		if restart == "never" || restart == "on-failure" && !failed {
			select {
			case <-ctx.Done():
			case <-sv.manual:
				continue
			}
			break
		}
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		sv.set(func(st *Status) { st.State = "backoff"; st.Restarts++ })
		sv.logLine(fmt.Sprintf("restarting in %s", backoff))
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		case <-sv.manual:
		}
		if backoff *= 2; backoff > 5*time.Minute {
			backoff = 5 * time.Minute
		}
	}
	sv.set(func(st *Status) { st.State, st.PID = "stopped", 0 })
}

func (s *Supervisor) runOnce(ctx context.Context, sv *service) error {
	p := sv.m
	port, err := freePort()
	if err != nil {
		return err
	}
	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(pctx, p.Service.Run[0], p.Service.Run[1:]...)
	cmd.Dir = p.Dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	cmd.Env = append(s.Jobs.Env(p, ""),
		"AIO_SERVICE_PORT="+strconv.Itoa(port),
		"PORT="+strconv.Itoa(port),
		"AIO_SERVICE_BASE=/s/"+p.ID+"/",
	)
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			sv.logLine(sc.Text())
		}
		io.Copy(io.Discard, pr)
	}()
	target, _ := url.Parse("http://127.0.0.1:" + strconv.Itoa(port))
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1 // stream (SSE, chunked)
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		http.Error(w, "service "+p.ID+" is not reachable: "+err.Error(), http.StatusBadGateway)
	}
	sv.set(func(st *Status) { st.State, st.Port, st.Started, st.LastError = "starting", port, time.Now(), "" })
	sv.logLine("$ " + strings.Join(p.Service.Run, " ") + "  (port " + strconv.Itoa(port) + ")")
	if err := cmd.Start(); err != nil {
		pw.Close()
		return err
	}
	sv.set(func(st *Status) { st.PID = cmd.Process.Pid })
	sv.mu.Lock()
	sv.proxy = proxy
	sv.mu.Unlock()

	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait(); pw.Close() }()

	go func() {
		if s.waitHealthy(pctx, port, p.Service.Health) {
			sv.set(func(st *Status) { st.State = "running" })
			sv.logLine("service is up")
			s.publish("service.started", sv)
		}
	}()
	select {
	case err := <-waitErr:
		return err
	case <-sv.manual:
		cancel()
		<-waitErr
		sv.logLine("restart requested")
		return errRestart
	}
}

// waitHealthy polls the health path (or just the TCP port) until it answers.
func (s *Supervisor) waitHealthy(ctx context.Context, port int, health string) bool {
	addr := "127.0.0.1:" + strconv.Itoa(port)
	client := &http.Client{Timeout: 2 * time.Second}
	for i := 0; i < 600; i++ { // up to ~5 minutes
		if ctx.Err() != nil {
			return false
		}
		if health != "" {
			if res, err := client.Get("http://" + addr + health); err == nil {
				res.Body.Close()
				if res.StatusCode < 500 {
					return true
				}
			}
		} else if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
			c.Close()
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(500 * time.Millisecond):
		}
	}
	return false
}

// List returns the status of all services.
func (s *Supervisor) List() []Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Status, 0, len(s.svcs))
	for _, sv := range s.svcs {
		sv.mu.Lock()
		out = append(out, sv.st)
		sv.mu.Unlock()
	}
	return out
}

func (s *Supervisor) get(id string) *service {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.svcs[id]
}

// Logs returns the recent output of a service.
func (s *Supervisor) Logs(id string) ([]string, bool) {
	sv := s.get(id)
	if sv == nil {
		return nil, false
	}
	sv.mu.Lock()
	defer sv.mu.Unlock()
	return append([]string{}, sv.logs...), true
}

// Restart asks a service to restart now.
func (s *Supervisor) Restart(id string) bool {
	sv := s.get(id)
	if sv == nil {
		return false
	}
	select {
	case sv.manual <- struct{}{}:
	default:
	}
	return true
}

// ServeHTTP proxies /s/<id>/<path> to the service, stripping the prefix.
// The original prefix is passed as X-Forwarded-Prefix.
func (s *Supervisor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sv := s.get(id)
	if sv == nil {
		http.Error(w, "no such service", http.StatusNotFound)
		return
	}
	sv.mu.Lock()
	proxy, state := sv.proxy, sv.st.State
	sv.mu.Unlock()
	if proxy == nil || state == "stopped" || state == "crashed" || state == "backoff" {
		http.Error(w, "service "+id+" is "+state, http.StatusServiceUnavailable)
		return
	}
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/" + r.PathValue("path")
	r2.URL.RawPath = ""
	r2.Header.Set("X-Forwarded-Prefix", "/s/"+id)
	proxy.ServeHTTP(w, r2)
}
