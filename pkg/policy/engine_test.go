package policy

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/coding-workspace/simple-mitigation-1/pkg/features"
)

func mustEngine(t *testing.T, yamlRules string) *Engine {
	t.Helper()
	doc, err := Parse([]byte(yamlRules))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	e, err := NewEngine(doc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	return e
}

func TestEnginePriorityAndCooldown(t *testing.T) {
	rules := `
rules:
  - name: high_priority
    when: "p50_now > 0.5"
    fire: [{kind: vertical}]
    cooldown: "1s"
    priority: 100
  - name: low_priority
    when: "p50_now > 0.5"
    fire: [{kind: horizontal}]
    cooldown: "1s"
    priority: 10
`
	e := mustEngine(t, rules)
	fv := features.FeatureVector{Target: "search", Pod: "p1", P50Now: 0.9}
	now := time.Unix(1700_000_000, 0)
	got := e.Evaluate(fv, now)
	if len(got) != 2 {
		t.Fatalf("want 2 actions, got %d (%+v)", len(got), got)
	}
	if got[0].RuleName != "high_priority" || got[1].RuleName != "low_priority" {
		t.Fatalf("priority ordering broken: %+v", got)
	}

	// Same tick + cooldown active: nothing fires.
	if got := e.Evaluate(fv, now.Add(500*time.Millisecond)); len(got) != 0 {
		t.Fatalf("cooldown should suppress, got %+v", got)
	}

	// After cooldown elapses, both fire again.
	if got := e.Evaluate(fv, now.Add(2*time.Second)); len(got) != 2 {
		t.Fatalf("post-cooldown re-fire failed: %+v", got)
	}
}

func TestEngineWhenFalse(t *testing.T) {
	e := mustEngine(t, `
rules:
  - name: only_when_spatial
    when: "k_spatial > 0.3 && persistence_h >= 2"
    fire: [{kind: isolate}]
`)
	fv := features.FeatureVector{KSpatial: 0.1, PersistenceH: 1}
	if got := e.Evaluate(fv, time.Now()); len(got) != 0 {
		t.Fatalf("rule should not fire, got %+v", got)
	}
}

func TestEngineDisabledRuleSkipped(t *testing.T) {
	e := mustEngine(t, `
rules:
  - name: turned_off
    when: "true"
    fire: [{kind: vertical}]
    enabled: false
`)
	if got := e.Evaluate(features.FeatureVector{}, time.Now()); len(got) != 0 {
		t.Fatalf("disabled rule fired: %+v", got)
	}
}

func TestEngineBandHelper(t *testing.T) {
	e := mustEngine(t, `
rules:
  - name: band_up
    when: 'band(p50_now, 0.2, 0.5) == "up"'
    fire: [{kind: horizontal}]
`)
	if got := e.Evaluate(features.FeatureVector{P50Now: 0.6}, time.Now()); len(got) != 1 {
		t.Fatalf("expected band==up to fire, got %+v", got)
	}
	if got := e.Evaluate(features.FeatureVector{P50Now: 0.3}, time.Now()); len(got) != 0 {
		t.Fatalf("expected stable band not to fire, got %+v", got)
	}
}

func TestParseRejectsEmptyRule(t *testing.T) {
	cases := []string{
		`rules: []`,
		`rules:
  - name: ""
    when: "true"
    fire: [{kind: vertical}]`,
		`rules:
  - name: noWhen
    when: ""
    fire: [{kind: vertical}]`,
		`rules:
  - name: noFire
    when: "true"
    fire: []`,
	}
	for i, c := range cases {
		if _, err := Parse([]byte(c)); err == nil {
			t.Fatalf("case %d: expected error, got nil", i)
		}
	}
}
