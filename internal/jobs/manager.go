package jobs

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"aio/internal/events"
	"aio/internal/plugin"
)

// Config for the job manager.
type Config struct {
	DataDir      string        // jobs are stored under DataDir/jobs
	DownloadsDir string        // exposed to plugins as AIO_DOWNLOADS
	SDKDir       string        // added to PYTHONPATH as AIO_SDK
	MaxJobs      int           // concurrent running jobs
	TTL          time.Duration // finished jobs older than this are deleted
	APIURL       string        // base URL plugins use to call the server (AIO_API)
	Events       *events.Bus   // optional: receives job.* events
}

// MaxDepth limits how deeply jobs may call other plugins through the API.
const MaxDepth = 4

type Manager struct {
	cfg  Config
	root string
	sem  chan struct{}

	mu     sync.RWMutex
	jobs   map[string]*Job
	tokens map[string]*Job // AIO_JOB_TOKEN -> active job
}

func NewManager(cfg Config) (*Manager, error) {
	if cfg.MaxJobs < 1 {
		cfg.MaxJobs = 1
	}
	m := &Manager{
		cfg:  cfg,
		root: filepath.Join(cfg.DataDir, "jobs"),
		sem:  make(chan struct{}, cfg.MaxJobs),
		jobs:   map[string]*Job{},
		tokens: map[string]*Job{},
	}
	if err := os.MkdirAll(m.root, 0o755); err != nil {
		return nil, err
	}
	m.loadExisting()
	go m.cleanupLoop()
	return m, nil
}

func newID() string {
	return time.Now().UTC().Format("060102-150405") + "-" + randHex(6)
}

// Create allocates a job and its in/out directories. The job is not started.
func (m *Manager) Create(p *plugin.Manifest) (*Job, error) {
	id := newID()
	dir := filepath.Join(m.root, id)
	for _, sub := range []string{"in", "out"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, err
		}
	}
	j := &Job{dir: dir, snap: Snapshot{
		ID: id, Plugin: p.ID, PluginName: p.Name, State: Queued,
		Progress: -1, Files: []File{}, Created: time.Now(),
	}, done: make(chan struct{})}
	return j, nil
}

// Discard removes a job that was created but never started (e.g. upload failed).
func (m *Manager) Discard(j *Job) { os.RemoveAll(j.dir) }

func (m *Manager) Get(id string) *Job {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.jobs[id]
}

// List returns jobs newest first.
func (m *Manager) List() []Snapshot {
	m.mu.RLock()
	out := make([]Snapshot, 0, len(m.jobs))
	for _, j := range m.jobs {
		out = append(out, j.Snapshot(false))
	}
	m.mu.RUnlock()
	sort.Slice(out, func(i, k int) bool { return out[i].Created.After(out[k].Created) })
	return out
}

// Delete cancels the job if needed and removes its files.
func (m *Manager) Delete(id string) bool {
	m.mu.Lock()
	j := m.jobs[id]
	delete(m.jobs, id)
	m.mu.Unlock()
	if j == nil {
		return false
	}
	j.Cancel()
	go func() {
		// give the process a moment to exit before removing its files
		for i := 0; i < 50 && !j.Snapshot(false).State.Finished(); i++ {
			time.Sleep(200 * time.Millisecond)
		}
		os.RemoveAll(j.dir)
	}()
	return true
}

// StartOptions controls how a job is started.
type StartOptions struct {
	Title  string
	Kind   string   // "" | setup | schedule | event (shown in the UI)
	Argv   []string // command to run instead of the manifest's run (setup hooks)
	Dir    string   // working directory / AIO_PLUGIN_DIR override (staged plugins)
	Parent *Job     // calling job: the child skips the queue and dies with it
	Event  *events.Event
}

// Start writes params.json and runs the plugin in the background.
func (m *Manager) Start(j *Job, p *plugin.Manifest, params map[string]any, title string) error {
	return m.StartWith(j, p, params, StartOptions{Title: title})
}

// StartWith is Start with extra options.
func (m *Manager) StartWith(j *Job, p *plugin.Manifest, params map[string]any, o StartOptions) error {
	if o.Event != nil {
		params["event"] = o.Event
	}
	b, err := json.MarshalIndent(params, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(j.dir, "params.json"), b, 0o644); err != nil {
		return err
	}
	if o.Event != nil {
		eb, _ := json.MarshalIndent(o.Event, "", "  ")
		os.WriteFile(filepath.Join(j.dir, "event.json"), eb, 0o644)
	}
	base := context.Background()
	depth := 0
	if o.Parent != nil {
		o.Parent.mu.Lock()
		pctx, pdepth := o.Parent.ctx, o.Parent.depth
		o.Parent.mu.Unlock()
		if pctx == nil {
			return fmt.Errorf("calling job is no longer running")
		}
		if pdepth+1 > MaxDepth {
			return fmt.Errorf("plugin call chain is too deep (max %d)", MaxDepth)
		}
		base, depth = pctx, pdepth+1
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Hour
	}
	ctx, cancel := context.WithTimeout(base, timeout)
	tok := randHex(16)
	j.mu.Lock()
	j.cancel, j.ctx, j.depth, j.token = cancel, ctx, depth, tok
	j.snap.Title = o.Title
	j.snap.Kind = o.Kind
	if o.Parent != nil {
		j.snap.Parent = o.Parent.snap.ID
		j.origin, j.hops = o.Parent.origin, o.Parent.hops
	}
	if o.Event != nil {
		j.origin, j.hops = p.ID, o.Event.Hops+1
	}
	j.mu.Unlock()

	m.mu.Lock()
	m.jobs[j.snap.ID] = j
	m.tokens[tok] = j
	m.mu.Unlock()

	m.publish(j, "job.queued")
	go m.run(ctx, cancel, j, p, o)
	return nil
}

// ByToken returns the queued or running job holding the given AIO_JOB_TOKEN.
func (m *Manager) ByToken(tok string) *Job {
	if tok == "" {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.tokens[tok]
}

// Origin reports which plugin's event hook (directly or through calls)
// started this job, and how many hook hops led to it.
func (j *Job) Origin() (string, int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.origin, j.hops
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Env returns the environment shared by a plugin's jobs and its service:
// paths, the API address and PATH with the plugin's and shared tool folders.
func (m *Manager) Env(p *plugin.Manifest, dir string) []string {
	if dir == "" {
		dir = p.Dir
	}
	pdata := filepath.Join(m.cfg.DataDir, "plugins", p.ID)
	tools := filepath.Join(m.cfg.DataDir, "tools", "bin")
	os.MkdirAll(pdata, 0o755)
	env := make([]string, 0, len(os.Environ())+16)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "PATH=") && !strings.HasPrefix(kv, "PYTHONPATH=") {
			env = append(env, kv)
		}
	}
	return append(env,
		"AIO_PLUGIN_ID="+p.ID,
		"AIO_PLUGIN_DIR="+dir,
		"AIO_PLUGIN_DATA="+pdata,
		"AIO_TOOLS="+tools,
		"AIO_DATA="+m.cfg.DataDir,
		"AIO_DOWNLOADS="+m.cfg.DownloadsDir,
		"AIO_SDK="+m.cfg.SDKDir,
		"AIO_API="+m.cfg.APIURL,
		"AIO_ARCH="+Arch(),
		"PATH="+strings.Join([]string{filepath.Join(pdata, "bin"), tools, os.Getenv("PATH")}, string(os.PathListSeparator)),
		"PYTHONPATH="+joinEnvPath(m.cfg.SDKDir, os.Getenv("PYTHONPATH")),
		"PYTHONUNBUFFERED=1",
	)
}

func (m *Manager) publish(j *Job, topic string) {
	if m.cfg.Events == nil {
		return
	}
	j.mu.Lock()
	s := j.snapshotLocked(false)
	origin, hops := j.origin, j.hops
	j.mu.Unlock()
	files := make([]map[string]any, 0, len(s.Files))
	for _, f := range s.Files {
		files = append(files, map[string]any{"name": f.Name, "size": f.Size, "url": FileURL(s.ID, f.Name)})
	}
	data := map[string]any{"job": s.ID, "plugin": s.Plugin, "title": s.Title, "state": string(s.State), "kind": s.Kind, "files": files}
	if s.Error != "" {
		data["error"] = s.Error
	}
	if s.Parent != "" {
		data["parent"] = s.Parent
	}
	m.cfg.Events.Publish(events.Event{Topic: topic, Data: data, Source: origin, Hops: hops})
}

// FileURL is the API path of a job's output file.
func FileURL(id, name string) string {
	parts := strings.Split(name, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return "/api/jobs/" + id + "/files/" + strings.Join(parts, "/")
}

func (m *Manager) run(ctx context.Context, cancel context.CancelFunc, j *Job, p *plugin.Manifest, o StartOptions) {
	defer cancel()
	defer m.persist(j)
	defer func() {
		j.mu.Lock()
		tok := j.token
		j.ctx = nil
		state := j.snap.State
		j.mu.Unlock()
		m.mu.Lock()
		delete(m.tokens, tok)
		m.mu.Unlock()
		m.publish(j, "job."+string(state))
	}()

	// Child jobs run at once: their parent holds a slot and is waiting on
	// them, so queueing them could deadlock when every slot is a parent.
	if o.Parent == nil {
		select {
		case m.sem <- struct{}{}:
			defer func() { <-m.sem }()
		case <-ctx.Done():
			j.update(func(s *Snapshot) { s.State, s.Finished = Canceled, time.Now() })
			return
		}
	}

	j.update(func(s *Snapshot) { s.State, s.Started = Running, time.Now() })
	m.publish(j, "job.started")

	argv, dir := p.Run, p.Dir
	if len(o.Argv) > 0 {
		argv = o.Argv
	}
	if o.Dir != "" {
		dir = o.Dir
	}
	if len(argv) == 0 {
		j.update(func(s *Snapshot) { s.State, s.Error, s.Finished = Failed, "nothing to run", time.Now() })
		return
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// kill the whole process group so children (ffmpeg, gs) die too
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	cmd.WaitDelay = 10 * time.Second
	j.mu.Lock()
	tok := j.token
	j.mu.Unlock()
	cmd.Env = append(m.Env(p, dir),
		"AIO_JOB_ID="+j.snap.ID,
		"AIO_JOB_TOKEN="+tok,
		"AIO_JOB_DIR="+j.dir,
		"AIO_IN="+filepath.Join(j.dir, "in"),
		"AIO_OUT="+filepath.Join(j.dir, "out"),
		"AIO_PARAMS="+filepath.Join(j.dir, "params.json"),
	)
	if o.Event != nil {
		cmd.Env = append(cmd.Env, "AIO_EVENT="+filepath.Join(j.dir, "event.json"))
	}

	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	var userErr string
	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		sc.Split(scanLinesCR)
		for sc.Scan() {
			if e := m.handleLine(j, sc.Text()); e != "" {
				userErr = e
			}
		}
		io.Copy(io.Discard, pr)
	}()

	err := cmd.Start()
	if err == nil {
		err = cmd.Wait()
	}
	pw.Close()
	<-scanDone

	files := listFiles(filepath.Join(j.dir, "out"))
	j.update(func(s *Snapshot) {
		s.Finished = time.Now()
		s.Files = files
		switch {
		case err == nil:
			s.State, s.Progress = Done, 100
		case errors.Is(ctx.Err(), context.Canceled):
			s.State, s.Error = Canceled, "canceled"
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			s.State, s.Error = Failed, "timed out after "+p.Timeout.String()
		default:
			s.State = Failed
			s.Error = userErr
			if s.Error == "" {
				s.Error = err.Error()
			}
		}
	})
}

// handleLine interprets the stdout protocol. Returns a user error message if
// the line was "::error ...".
func (m *Manager) handleLine(j *Job, line string) string {
	line = strings.TrimRight(line, " \t")
	if line == "" {
		return ""
	}
	if !strings.HasPrefix(line, "::") {
		j.log(line)
		return ""
	}
	cmd, arg, _ := strings.Cut(line[2:], " ")
	arg = strings.TrimSpace(arg)
	switch cmd {
	case "progress":
		v, err := strconv.ParseFloat(strings.TrimSuffix(arg, "%"), 64)
		if err != nil {
			v = -1
		}
		if v > 100 {
			v = 100
		}
		j.update(func(s *Snapshot) { s.Progress = v })
	case "status":
		// not copied to the log: progress-style statuses can arrive many times per second
		j.update(func(s *Snapshot) { s.Status = arg })
	case "title":
		j.update(func(s *Snapshot) { s.Title = arg })
	case "error":
		j.log("ERROR: " + arg)
		return arg
	default:
		j.log(line)
	}
	return ""
}

// scanLinesCR splits on \n, \r\n or a lone \r (progress bars use \r).
func scanLinesCR(data []byte, atEOF bool) (int, []byte, error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		adv := i + 1
		if data[i] == '\r' && i+1 < len(data) && data[i+1] == '\n' {
			adv++
		}
		return adv, data[:i], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func listFiles(dir string) []File {
	files := []File{}
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		files = append(files, File{Name: filepath.ToSlash(rel), Size: info.Size()})
		return nil
	})
	sort.Slice(files, func(i, k int) bool { return files[i].Name < files[k].Name })
	return files
}

func joinEnvPath(a, b string) string {
	if b == "" {
		return a
	}
	return a + string(os.PathListSeparator) + b
}

// persist writes job.json so history survives restarts.
func (m *Manager) persist(j *Job) {
	s := j.Snapshot(true)
	if len(s.Logs) > 500 {
		s.Logs = s.Logs[len(s.Logs)-500:]
	}
	b, _ := json.Marshal(s)
	if err := os.WriteFile(filepath.Join(j.dir, "job.json"), b, 0o644); err != nil && !os.IsNotExist(err) {
		log.Printf("jobs: persist %s: %v", s.ID, err)
	}
}

func (m *Manager) loadExisting() {
	entries, _ := os.ReadDir(m.root)
	for _, e := range entries {
		dir := filepath.Join(m.root, e.Name())
		b, err := os.ReadFile(filepath.Join(dir, "job.json"))
		if err != nil {
			// never finished (server crashed/restarted mid-run) or never started
			os.RemoveAll(dir)
			continue
		}
		var s Snapshot
		if json.Unmarshal(b, &s) != nil || s.ID == "" {
			os.RemoveAll(dir)
			continue
		}
		if !s.State.Finished() {
			s.State, s.Error = Failed, "interrupted by server restart"
		}
		logs := s.Logs
		s.Logs = nil
		m.jobs[s.ID] = &Job{dir: dir, snap: s, logs: logs}
	}
	if len(m.jobs) > 0 {
		log.Printf("jobs: restored %d from disk", len(m.jobs))
	}
}

func (m *Manager) cleanupLoop() {
	if m.cfg.TTL <= 0 {
		return
	}
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		m.cleanup()
		<-t.C
	}
}

func (m *Manager) cleanup() {
	cutoff := time.Now().Add(-m.cfg.TTL)
	var old []string
	m.mu.RLock()
	for id, j := range m.jobs {
		s := j.Snapshot(false)
		if s.State.Finished() && s.Finished.Before(cutoff) {
			old = append(old, id)
		}
	}
	m.mu.RUnlock()
	for _, id := range old {
		m.Delete(id)
	}
	if len(old) > 0 {
		log.Printf("jobs: cleaned up %d expired", len(old))
	}
}

// FilePath resolves a file inside a job's out directory, refusing escapes.
func (m *Manager) FilePath(j *Job, name string) (string, error) {
	out := filepath.Join(j.dir, "out")
	p := filepath.Join(out, filepath.FromSlash(name))
	if rel, err := filepath.Rel(out, p); err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("invalid path")
	}
	return p, nil
}
