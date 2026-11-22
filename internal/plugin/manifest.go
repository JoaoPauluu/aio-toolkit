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

	Dir     string        `yaml:"-" json:"-"`
	Timeout time.Duration `yaml:"-" json:"-"`
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
	if len(m.Run) == 0 && m.UI == "" {
		return nil, fmt.Errorf("%s: 'run' is required (or set 'ui' for a UI-only plugin)", m.ID)
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

// Input returns the input definition with the given name.
func (m *Manifest) Input(name string) *Input {
	for i := range m.Inputs {
		if m.Inputs[i].Name == name {
			return &m.Inputs[i]
		}
	}
	return nil
}
