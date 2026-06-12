// Package targets parses the multi-victim configuration used by the
// vertical-scaler. The horizontal-cpa-sidecar uses a single target derived
// from env vars rather than this config file (one CPA Pod per victim
// Deployment is a CPA framework constraint).
package targets

import (
	"fmt"
	"os"

	"sigs.k8s.io/yaml"
)

// Target identifies one victim service that exposes a ContentionStream.
type Target struct {
	Name          string            `json:"name"`
	Namespace     string            `json:"namespace"`
	Selector      map[string]string `json:"selector"`
	ScorePort     int               `json:"scorePort,omitempty"`
	ContainerName string            `json:"containerName,omitempty"`
	// Agg names the aggregator policy used when summarising across pods of
	// this target. Empty means use the caller's default (typically "max").
	Agg string `json:"agg,omitempty"`
}

// Config is the on-disk shape of the targets ConfigMap.
type Config struct {
	Targets []Target `json:"targets"`
}

// DefaultScorePort is applied when a target omits scorePort.
const DefaultScorePort = 7900

// Load reads and validates a targets YAML file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read targets file %q: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse targets file %q: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Validate fills defaults and rejects malformed entries. Mutates the receiver
// in place to apply defaults.
func (c *Config) Validate() error {
	if len(c.Targets) == 0 {
		return fmt.Errorf("targets: must specify at least one target")
	}
	seen := map[string]bool{}
	for i := range c.Targets {
		t := &c.Targets[i]
		if t.Name == "" {
			return fmt.Errorf("targets[%d]: name is required", i)
		}
		if seen[t.Name] {
			return fmt.Errorf("targets[%d]: duplicate name %q", i, t.Name)
		}
		seen[t.Name] = true
		if t.Namespace == "" {
			return fmt.Errorf("targets[%s]: namespace is required", t.Name)
		}
		if len(t.Selector) == 0 {
			return fmt.Errorf("targets[%s]: selector is required", t.Name)
		}
		if t.ScorePort == 0 {
			t.ScorePort = DefaultScorePort
		}
		if t.ScorePort < 1 || t.ScorePort > 65535 {
			return fmt.Errorf("targets[%s]: scorePort %d out of range", t.Name, t.ScorePort)
		}
	}
	return nil
}
