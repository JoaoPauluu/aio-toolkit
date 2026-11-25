// Package plugin discovers and describes tools that live in plugin folders.
//
// A plugin is a directory containing a plugin.yaml manifest and a script.
// The manifest declares the inputs; the UI renders a form from them and the
// job runner executes the script with those inputs.
package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const ManifestName = "plugin.yaml"

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// Option is one choice of a select input. In YAML it may be written as a
// plain scalar ("ebook") or as a mapping ({value: ebook, label: "E-book"}).
type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

func (o *Option) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		o.Value, o.Label = n.Value, n.Value
		return nil
	}
	type raw Option
	var r raw
	if err := n.Decode(&r); err != nil {
		return err
	}
	*o = Option(r)
	if o.Label == "" {
		o.Label = o.Value
	}
	return nil
}

// Input is one form field.
type Input struct {
	Name        string   `yaml:"name" json:"name"`
	Type        string   `yaml:"type" json:"type"` // file text textarea number select checkbox url hidden json
	Label       string   `yaml:"label" json:"label"`
	Help        string   `yaml:"help" json:"help,omitempty"`
	Placeholder string   `yaml:"placeholder" json:"placeholder,omitempty"`
	Accept      string   `yaml:"accept" json:"accept,omitempty"`
	Multiple    bool     `yaml:"multiple" json:"multiple,omitempty"`
	Required    bool     `yaml:"required" json:"required,omitempty"`
	Default     any      `yaml:"default" json:"default,omitempty"`
	Options     []Option `yaml:"options" json:"options,omitempty"`
	Min         *float64 `yaml:"min" json:"min,omitempty"`
	Max         *float64 `yaml:"max" json:"max,omitempty"`
	Step        *float64 `yaml:"step" json:"step,omitempty"`
	// ShowIf hides the field unless another field has one of the given values,
	// e.g. {mode: [ranges]}.
	ShowIf map[string][]string `yaml:"show_if" json:"show_if,omitempty"`
}

var validTypes = map[string]bool{
	"file": true, "text": true, "textarea": true, "number": true,
	"select": true, "checkbox": true, "url": true, "hidden": true,
	"json": true, // any JSON value; not rendered by the generic form
}

// APIVersion is the plugin API this build implements. A manifest declaring
// a higher aio_api is rejected so old hosts don't run plugins they can't serve.
const APIVersion = 1

// Requirement names another plugin this one needs: either an id that must
// already exist (e.g. a built-in) or a community source to install.
type Requirement struct {
	ID     string `yaml:"id" json:"id,omitempty"`
	Source string `yaml:"source" json:"source,omitempty"`
}

// Service is a long-running process supervised by the server and reachable
// through the reverse proxy at /s/<id>/.
type Service struct {
	Run     []string `yaml:"run" json:"run"`
	Health  string   `yaml:"health" json:"health,omitempty"`   // HTTP path polled to tell it is up
	Restart string   `yaml:"restart" json:"restart,omitempty"` // always (default) | on-failure | never
}

// Schedule starts a job periodically: either every <duration> or on a
// five-field cron expression (minute hour day-of-month month day-of-week).
type Schedule struct {
	Every  string         `yaml:"every" json:"every,omitempty"`
	Cron   string         `yaml:"cron" json:"cron,omitempty"`
	Params map[string]any `yaml:"params" json:"params,omitempty"`

	Interval time.Duration `yaml:"-" json:"-"`
	Spec     *CronSpec     `yaml:"-" json:"-"`
}

// EventHook starts a job when a matching event is published.
type EventHook struct {
	On     string         `yaml:"on" json:"on"`                   // topic, "job.done" or a prefix pattern "job.*"
	Filter map[string]any `yaml:"filter" json:"filter,omitempty"` // event data fields that must match
	Params map[string]any `yaml:"params" json:"params,omitempty"`
}

// Manifest is the parsed plugin.yaml.
type Manifest struct {
	ID          string   `yaml:"id" json:"id"`
	Name        string   `yaml:"name" json:"name"`
	Category    string   `yaml:"category" json:"category"`
	Icon        string   `yaml:"icon" json:"icon"`
	Description string   `yaml:"description" json:"description"`
	Version     string   `yaml:"version" json:"version,omitempty"`
	Run         []string `yaml:"run" json:"-"`
	Inputs      []Input  `yaml:"inputs" json:"inputs"`
	UI          string   `yaml:"ui" json:"ui,omitempty"`
	SubmitLabel string   `yaml:"submit_label" json:"submit_label,omitempty"`
	TimeoutStr  string   `yaml:"timeout" json:"-"`

	// Community / extension fields (all optional).
	AIOAPI   int           `yaml:"aio_api" json:"aio_api,omitempty"`
	Author   string        `yaml:"author" json:"author,omitempty"`
	Homepage string        `yaml:"homepage" json:"homepage,omitempty"`
	License  string        `yaml:"license" json:"license,omitempty"`
	Provides []string      `yaml:"provides" json:"provides,omitempty"`
	Requires []Requirement `yaml:"requires" json:"requires,omitempty"`
	Setup    []string      `yaml:"setup" json:"setup,omitempty"`
	Service  *Service      `yaml:"service" json:"service,omitempty"`
	Schedule []Schedule    `yaml:"schedule" json:"schedule,omitempty"`
	Events   []EventHook   `yaml:"events" json:"events,omitempty"`

	Dir     string        `yaml:"-" json:"-"`
	Timeout time.Duration `yaml:"-" json:"-"`
	// Community is set by the registry for plugins installed from a source.
	Community bool `yaml:"-" json:"community,omitempty"`
}

// Load parses and validates dir/plugin.yaml.
func Load(dir string) (*Manifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, ManifestName))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", ManifestName, err)
	}
	m.Dir = dir
	if m.ID == "" {
		m.ID = filepath.Base(dir)
	}
	if !idRe.MatchString(m.ID) {
		return nil, fmt.Errorf("invalid id %q (use lowercase letters, digits, . _ -)", m.ID)
	}
	if m.Name == "" {
		m.Name = m.ID
	}
	if m.Category == "" {
		m.Category = "Other"
	}
	if len(m.Run) == 0 && m.UI == "" && m.Service == nil {
		return nil, fmt.Errorf("%s: 'run' is required (or set 'ui' or 'service')", m.ID)
	}
	if m.AIOAPI > APIVersion {
		return nil, fmt.Errorf("%s: needs plugin API %d, this AIO Toolkit supports %d (update the toolkit)", m.ID, m.AIOAPI, APIVersion)
	}
	for i, r := range m.Requires {
		if (r.ID == "") == (r.Source == "") {
			return nil, fmt.Errorf("%s: requires #%d needs exactly one of id or source", m.ID, i+1)
		}
	}
	if m.Service != nil {
		if len(m.Service.Run) == 0 {
			return nil, fmt.Errorf("%s: service.run is required", m.ID)
		}
		switch m.Service.Restart {
		case "":
			m.Service.Restart = "always"
		case "always", "on-failure", "never":
		default:
			return nil, fmt.Errorf("%s: service.restart must be always, on-failure or never", m.ID)
		}
		if m.Service.Health != "" && !strings.HasPrefix(m.Service.Health, "/") {
			m.Service.Health = "/" + m.Service.Health
		}
	}
	if (len(m.Schedule) > 0 || len(m.Events) > 0) && len(m.Run) == 0 {
		return nil, fmt.Errorf("%s: schedule and events start jobs, so 'run' is required", m.ID)
	}
	for i := range m.Schedule {
		sc := &m.Schedule[i]
		switch {
		case sc.Every != "" && sc.Cron != "":
			return nil, fmt.Errorf("%s: schedule #%d: set every or cron, not both", m.ID, i+1)
		case sc.Every != "":
			d, err := time.ParseDuration(sc.Every)
			if err != nil || d < time.Minute {
				return nil, fmt.Errorf("%s: schedule #%d: every must be a duration of at least 1m", m.ID, i+1)
			}
			sc.Interval = d
		case sc.Cron != "":
			spec, err := ParseCron(sc.Cron)
			if err != nil {
				return nil, fmt.Errorf("%s: schedule #%d: %w", m.ID, i+1, err)
			}
			sc.Spec = spec
		default:
			return nil, fmt.Errorf("%s: schedule #%d needs every or cron", m.ID, i+1)
		}
	}
	for i, e := range m.Events {
		if e.On == "" {
			return nil, fmt.Errorf("%s: events #%d needs 'on'", m.ID, i+1)
		}
	}
	m.Timeout = 2 * time.Hour
	if m.TimeoutStr != "" {
		d, err := time.ParseDuration(m.TimeoutStr)
		if err != nil {
			return nil, fmt.Errorf("%s: bad timeout: %w", m.ID, err)
		}
		m.Timeout = d
	}
	if m.UI != "" {
		clean := filepath.Clean(m.UI)
		if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
			return nil, fmt.Errorf("%s: ui must be a relative path inside the plugin", m.ID)
		}
		m.UI = filepath.ToSlash(clean)
	}
	seen := map[string]bool{}
	for i := range m.Inputs {
		in := &m.Inputs[i]
		if in.Name == "" || strings.HasPrefix(in.Name, "__") {
			return nil, fmt.Errorf("%s: input #%d needs a valid name", m.ID, i+1)
		}
		if seen[in.Name] {
			return nil, fmt.Errorf("%s: duplicate input %q", m.ID, in.Name)
		}
		seen[in.Name] = true
		if in.Type == "" {
			in.Type = "text"
		}
		if !validTypes[in.Type] {
			return nil, fmt.Errorf("%s: input %q has unknown type %q", m.ID, in.Name, in.Type)
		}
		if in.Label == "" {
			in.Label = in.Name
		}
	}
	return &m, nil
}

// Defaults returns params built from each input's default, with overrides
// applied on top. Used for jobs not started from a form (schedules, events).
func (m *Manifest) Defaults(overrides map[string]any) map[string]any {
	out := map[string]any{}
	for _, in := range m.Inputs {
		switch in.Type {
		case "file":
			out[in.Name] = []string{}
		case "checkbox":
			b, _ := in.Default.(bool)
			out[in.Name] = b
		case "number", "json":
			out[in.Name] = in.Default
		default:
			if in.Default != nil {
				out[in.Name] = fmt.Sprint(in.Default)
			} else {
				out[in.Name] = ""
			}
		}
	}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}

// HasCapability reports whether the plugin lists cap in provides.
func (m *Manifest) HasCapability(cap string) bool {
	for _, p := range m.Provides {
		if p == cap {
			return true
		}
	}
	return false
}

// Input returns the input definition with the given name.
func (m *Manifest) Input(name string) *Input {
	for i := range m.Inputs {
		if m.Inputs[i].Name == name {
			return &m.Inputs[i]
		}
	}
	return nil
}
