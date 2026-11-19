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
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"aio/internal/plugin"
)

// Config for the job manager.
type Config struct {
	DataDir      string        // jobs are stored under DataDir/jobs
	DownloadsDir string        // exposed to plugins as AIO_DOWNLOADS
	SDKDir       string        // added to PYTHONPATH as AIO_SDK
	MaxJobs      int           // concurrent running jobs
	TTL          time.Duration // finished jobs older than this are deleted
}

type Manager struct {
	cfg  Config
	root string
	sem  chan struct{}

	mu   sync.RWMutex
	jobs map[string]*Job
}

func NewManager(cfg Config) (*Manager, error) {
	if cfg.MaxJobs < 1 {
		cfg.MaxJobs = 1
	}
	m := &Manager{
		cfg:  cfg,
		root: filepath.Join(cfg.DataDir, "jobs"),
		sem:  make(chan struct{}, cfg.MaxJobs),
		jobs: map[string]*Job{},
	}
	if err := os.MkdirAll(m.root, 0o755); err != nil {
		return nil, err
	}
	m.loadExisting()
	go m.cleanupLoop()
	return m, nil
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return time.Now().UTC().Format("060102-150405") + "-" + hex.EncodeToString(b)
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
	}}
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

// Start writes params.json and runs the plugin in the background.
func (m *Manager) Start(j *Job, p *plugin.Manifest, params map[string]any, title string) error {
	b, err := json.MarshalIndent(params, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(j.dir, "params.json"), b, 0o644); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), p.Timeout)
	j.mu.Lock()
	j.cancel = cancel
	j.snap.Title = title
	j.mu.Unlock()

	m.mu.Lock()
	m.jobs[j.snap.ID] = j
	m.mu.Unlock()

	go m.run(ctx, cancel, j, p)
	return nil
}

func (m *Manager) run(ctx context.Context, cancel context.CancelFunc, j *Job, p *plugin.Manifest) {
	defer cancel()
	defer m.persist(j)

	select {
	case m.sem <- struct{}{}:
		defer func() { <-m.sem }()
	case <-ctx.Done():
		j.update(func(s *Snapshot) { s.State, s.Finished = Canceled, time.Now() })
		return
	}

	j.update(func(s *Snapshot) { s.State, s.Started = Running, time.Now() })

	cmd := exec.CommandContext(ctx, p.Run[0], p.Run[1:]...)
	cmd.Dir = p.Dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// kill the whole process group so children (ffmpeg, gs) die too
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	cmd.WaitDelay = 10 * time.Second
	cmd.Env = append(os.Environ(),
		"AIO_JOB_ID="+j.snap.ID,
		"AIO_JOB_DIR="+j.dir,
		"AIO_IN="+filepath.Join(j.dir, "in"),
		"AIO_OUT="+filepath.Join(j.dir, "out"),
		"AIO_PARAMS="+filepath.Join(j.dir, "params.json"),
		"AIO_PLUGIN_DIR="+p.Dir,
		"AIO_DATA="+m.cfg.DataDir,
		"AIO_DOWNLOADS="+m.cfg.DownloadsDir,
		"AIO_SDK="+m.cfg.SDKDir,
		"PYTHONPATH="+joinEnvPath(m.cfg.SDKDir, os.Getenv("PYTHONPATH")),
		"PYTHONUNBUFFERED=1",
	)

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
