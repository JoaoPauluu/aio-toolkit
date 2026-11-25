// Package automation starts plugin jobs on a schedule or when events fire,
// as declared by a manifest's `schedule` and `events` keys.
package automation

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"aio/internal/events"
	"aio/internal/jobs"
	"aio/internal/plugin"
)

type Runner struct {
	Jobs   *jobs.Manager
	Events *events.Bus

	mu      sync.Mutex
	plugins map[string]*plugin.Manifest
	cancel  context.CancelFunc
	ctx     context.Context
}

// Start ties the runner to ctx; call Sync (e.g. from Registry.OnReload) to
// (re)build schedules for the current plugins.
func (r *Runner) Start(ctx context.Context) {
	r.mu.Lock()
	r.ctx = ctx
	r.mu.Unlock()
	if r.Events != nil {
		r.Events.Handle(r.onEvent)
	}
}

// Sync replaces all schedules with the ones declared by plugins.
func (r *Runner) Sync(plugins map[string]*plugin.Manifest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.plugins = plugins
	if r.cancel != nil {
		r.cancel()
	}
	if r.ctx == nil {
		return
	}
	ctx, cancel := context.WithCancel(r.ctx)
	r.cancel = cancel
	for _, p := range plugins {
		for i := range p.Schedule {
			go r.loop(ctx, p, &p.Schedule[i])
		}
	}
}

// Next returns when a schedule fires next after t.
func Next(s *plugin.Schedule, t time.Time) time.Time {
	if s.Spec != nil {
		return s.Spec.Next(t)
	}
	return t.Add(s.Interval)
}

func (r *Runner) loop(ctx context.Context, p *plugin.Manifest, s *plugin.Schedule) {
	for {
		next := Next(s, time.Now())
		if next.IsZero() {
			log.Printf("automation: %s: schedule %q never fires", p.ID, s.Cron)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(next)):
		}
		if err := r.start(p, s.Params, "schedule", "⏰ Scheduled run", nil); err != nil {
			log.Printf("automation: %s: scheduled run: %v", p.ID, err)
		}
	}
}

func (r *Runner) start(p *plugin.Manifest, params map[string]any, kind, title string, ev *events.Event) error {
	j, err := r.Jobs.Create(p)
	if err != nil {
		return err
	}
	if err := r.Jobs.StartWith(j, p, p.Defaults(params), jobs.StartOptions{Title: title, Kind: kind, Event: ev}); err != nil {
		r.Jobs.Discard(j)
		return err
	}
	return nil
}

// Matches reports whether a hook wants an event: topic pattern plus every
// filter field equal (as strings) to the event's data field.
func Matches(h plugin.EventHook, ev events.Event) bool {
	if !events.Match(h.On, ev.Topic) {
		return false
	}
	for k, want := range h.Filter {
		got, ok := ev.Data[k]
		if !ok {
			return false
		}
		if list, isList := want.([]any); isList {
			hit := false
			for _, w := range list {
				hit = hit || fmt.Sprint(w) == fmt.Sprint(got)
			}
			if !hit {
				return false
			}
			continue
		}
		if fmt.Sprint(want) != fmt.Sprint(got) {
			return false
		}
	}
	return true
}

func (r *Runner) onEvent(ev events.Event) {
	if ev.Hops >= events.MaxHops {
		return
	}
	r.mu.Lock()
	plugins := r.plugins
	r.mu.Unlock()
	for _, p := range plugins {
		// Never feed a plugin events caused by its own hook jobs.
		if ev.Source == p.ID {
			continue
		}
		// …nor events about its own hook jobs (job.done of an event run).
		if ev.Data["plugin"] == p.ID && ev.Data["kind"] == "event" {
			continue
		}
		for _, h := range p.Events {
			if !Matches(h, ev) {
				continue
			}
			ev := ev
			if err := r.start(p, h.Params, "event", "⚡ "+ev.Topic, &ev); err != nil {
				log.Printf("automation: %s: event hook: %v", p.ID, err)
			}
			break
		}
	}
}
