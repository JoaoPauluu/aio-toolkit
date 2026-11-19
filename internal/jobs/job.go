// Package jobs runs plugin scripts as subprocesses and tracks their state.
package jobs

import (
	"context"
	"sync"
	"time"
)

type State string

const (
	Queued   State = "queued"
	Running  State = "running"
	Done     State = "done"
	Failed   State = "failed"
	Canceled State = "canceled"
)

func (s State) Finished() bool { return s == Done || s == Failed || s == Canceled }

type File struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// Snapshot is the JSON view of a job.
type Snapshot struct {
	ID         string    `json:"id"`
	Plugin     string    `json:"plugin"`
	PluginName string    `json:"plugin_name"`
	Title      string    `json:"title,omitempty"`
	State      State     `json:"state"`
	Progress   float64   `json:"progress"` // 0-100, or -1 when unknown
	Status     string    `json:"status,omitempty"`
	Error      string    `json:"error,omitempty"`
	Files      []File    `json:"files"`
	Created    time.Time `json:"created"`
	Started    time.Time `json:"started,omitempty"`
	Finished   time.Time `json:"finished,omitempty"`
	Logs       []string  `json:"logs,omitempty"`
}

// Event is pushed to subscribers. Type is "update" (Data = Snapshot without
// logs) or "log" (Data = string line).
type Event struct {
	Type string
	Data any
}

const maxLogLines = 2000

type Job struct {
	mu     sync.Mutex
	snap   Snapshot
	logs   []string
	subs   map[chan Event]struct{}
	cancel context.CancelFunc
	dir    string
}

func (j *Job) Snapshot(withLogs bool) Snapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.snapshotLocked(withLogs)
}

func (j *Job) snapshotLocked(withLogs bool) Snapshot {
	s := j.snap
	s.Files = append([]File{}, j.snap.Files...)
	if withLogs {
		s.Logs = append([]string{}, j.logs...)
	}
	return s
}

// update mutates the snapshot under lock and notifies subscribers.
func (j *Job) update(f func(s *Snapshot)) {
	j.mu.Lock()
	f(&j.snap)
	ev := Event{Type: "update", Data: j.snapshotLocked(false)}
	j.broadcastLocked(ev)
	j.mu.Unlock()
}

func (j *Job) log(line string) {
	j.mu.Lock()
	if len(j.logs) >= maxLogLines {
		copy(j.logs, j.logs[len(j.logs)-maxLogLines/2:])
		j.logs = j.logs[:maxLogLines/2]
	}
	j.logs = append(j.logs, line)
	j.broadcastLocked(Event{Type: "log", Data: line})
	j.mu.Unlock()
}

func (j *Job) broadcastLocked(ev Event) {
	for ch := range j.subs {
		select {
		case ch <- ev:
		default: // slow subscriber: drop event rather than block the job
		}
	}
}

// Subscribe returns the current snapshot (with logs) and a channel of
// subsequent events. Call the returned func to unsubscribe.
func (j *Job) Subscribe() (Snapshot, <-chan Event, func()) {
	ch := make(chan Event, 256)
	j.mu.Lock()
	if j.subs == nil {
		j.subs = map[chan Event]struct{}{}
	}
	j.subs[ch] = struct{}{}
	s := j.snapshotLocked(true)
	j.mu.Unlock()
	return s, ch, func() {
		j.mu.Lock()
		delete(j.subs, ch)
		j.mu.Unlock()
	}
}

func (j *Job) Dir() string { return j.dir }

// Cancel stops a queued or running job.
func (j *Job) Cancel() {
	j.mu.Lock()
	c := j.cancel
	j.mu.Unlock()
	if c != nil {
		c()
	}
}
