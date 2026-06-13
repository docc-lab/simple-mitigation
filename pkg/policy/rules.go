// Package policy is the rule loader + CEL evaluator that turns a per-tick
// FeatureVector into a list of ActionRequests to dispatch to actuators.
//
// Rules live in a YAML file (typically mounted from a ConfigMap with
// subPath for fsnotify live-reload). Each rule has:
//
//   - a CEL `when` expression over a flat FeatureVector vocabulary
//     (see pkg/features.FeatureVector)
//   - one or more `fire` actions, each naming a kind ("isolate" / "vertical"
//     / "horizontal" / "restore") and a free-form params map handed
//     verbatim to the actuator
//   - a `cooldown` applied per-(rule, target, pod) -- two pods of the same
//     deployment cooldown independently
//   - a `priority` (higher first) and `enabled` toggle
package policy

import (
	"fmt"
	"os"
	"sort"
	"time"

	"sigs.k8s.io/yaml"
)

// Action is one entry in a rule's `fire` list. Kind selects the actuator;
// Params is passed through as-is and the actuator validates it.
type Action struct {
	Kind   string         `json:"kind"`
	Params map[string]any `json:"params,omitempty"`
}

// RuleSpec is the on-disk shape of a single rule.
type RuleSpec struct {
	Name     string   `json:"name"`
	When     string   `json:"when"`
	Fire     []Action `json:"fire"`
	Cooldown Duration `json:"cooldown,omitempty"`
	Priority int      `json:"priority,omitempty"`
	Enabled  *bool    `json:"enabled,omitempty"`
}

// Document is the top-level YAML schema: `rules: []`.
type Document struct {
	Rules []RuleSpec `json:"rules"`
}

// Duration mirrors time.Duration but accepts the human-friendly "30s" /
// "5m" strings YAML rule authors expect. sigs.k8s.io/yaml routes through
// JSON, so we implement UnmarshalJSON.
type Duration time.Duration

// UnmarshalJSON parses either a number-of-nanoseconds or a duration string.
func (d *Duration) UnmarshalJSON(data []byte) error {
	s := string(data)
	if len(s) == 0 || s == "null" {
		*d = 0
		return nil
	}
	if s[0] == '"' {
		s = s[1 : len(s)-1]
		v, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("policy: parse duration %q: %w", s, err)
		}
		*d = Duration(v)
		return nil
	}
	// Bare number = nanoseconds.
	v, err := time.ParseDuration(s + "ns")
	if err != nil {
		return fmt.Errorf("policy: parse duration ns %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// MarshalJSON emits the canonical duration string. Round-trips for diffing.
func (d Duration) MarshalJSON() ([]byte, error) {
	return []byte(fmt.Sprintf("%q", time.Duration(d).String())), nil
}

// LoadFile reads, decodes, validates, and returns the rule document at path.
// On any error, returns a wrapped error so callers can log a single concise
// reload-failure message.
func LoadFile(path string) (*Document, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("policy: read %q: %w", path, err)
	}
	return Parse(raw)
}

// Parse decodes raw YAML/JSON and validates it.
func Parse(raw []byte) (*Document, error) {
	var doc Document
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("policy: yaml: %w", err)
	}
	if err := doc.Validate(); err != nil {
		return nil, err
	}
	return &doc, nil
}

// Validate rejects malformed rules. Stops at the first problem.
func (d *Document) Validate() error {
	if len(d.Rules) == 0 {
		return fmt.Errorf("policy: at least one rule required")
	}
	seen := map[string]bool{}
	for i := range d.Rules {
		r := &d.Rules[i]
		if r.Name == "" {
			return fmt.Errorf("policy: rule[%d]: name is required", i)
		}
		if seen[r.Name] {
			return fmt.Errorf("policy: rule[%d]: duplicate name %q", i, r.Name)
		}
		seen[r.Name] = true
		if r.When == "" {
			return fmt.Errorf("policy: rule %q: when expression is required", r.Name)
		}
		if len(r.Fire) == 0 {
			return fmt.Errorf("policy: rule %q: at least one fire action required", r.Name)
		}
		for j, a := range r.Fire {
			if a.Kind == "" {
				return fmt.Errorf("policy: rule %q: fire[%d].kind required", r.Name, j)
			}
		}
		if time.Duration(r.Cooldown) < 0 {
			return fmt.Errorf("policy: rule %q: cooldown must be >= 0", r.Name)
		}
	}
	return nil
}

// SortedRules returns the enabled rules ordered by priority (descending),
// with ties broken by declaration order so deterministic test output.
func (d *Document) SortedRules() []RuleSpec {
	out := make([]RuleSpec, 0, len(d.Rules))
	for _, r := range d.Rules {
		if r.Enabled != nil && !*r.Enabled {
			continue
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Priority > out[j].Priority
	})
	return out
}
