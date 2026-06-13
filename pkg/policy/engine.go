package policy

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/coding-workspace/simple-mitigation-1/pkg/features"

	"github.com/google/cel-go/cel"
)

// ActionRequest is one action to dispatch. The engine emits these in
// priority order; the dispatcher routes by Kind to an Actuator.
type ActionRequest struct {
	RuleName string
	Target   string
	Pod      string
	Kind     string
	Params   map[string]any
}

// compiledRule pairs a RuleSpec with its compiled program so Evaluate
// doesn't re-compile every tick.
type compiledRule struct {
	Spec RuleSpec
	Prog cel.Program
}

// cooldownKey scopes the rule's `cooldown` field to (rule, target, pod) so
// two pods in the same Deployment don't cooldown each other.
type cooldownKey struct {
	Rule, Target, Pod string
}

// Engine evaluates the active rule set against a FeatureVector. Safe for
// concurrent Evaluate from multiple goroutines; Reload swaps the rule
// snapshot atomically.
type Engine struct {
	env    *cel.Env
	logger *slog.Logger

	mu       sync.RWMutex
	rules    []compiledRule
	cooldown map[cooldownKey]time.Time
}

// NewEngine builds an Engine seeded with the rules in doc. Returns the
// first compilation error wrapped with the offending rule's name.
func NewEngine(doc *Document, logger *slog.Logger) (*Engine, error) {
	if logger == nil {
		logger = slog.Default()
	}
	env, err := celEnv()
	if err != nil {
		return nil, fmt.Errorf("policy: build CEL env: %w", err)
	}
	e := &Engine{
		env:      env,
		logger:   logger,
		cooldown: map[cooldownKey]time.Time{},
	}
	if err := e.Reload(doc); err != nil {
		return nil, err
	}
	return e, nil
}

// Reload replaces the active rule set. Failure leaves the previous rules in
// place so a typo in a hot ConfigMap can't take the controller offline.
func (e *Engine) Reload(doc *Document) error {
	specs := doc.SortedRules()
	compiled := make([]compiledRule, 0, len(specs))
	for _, r := range specs {
		prog, err := compileRule(e.env, r)
		if err != nil {
			return err
		}
		compiled = append(compiled, compiledRule{Spec: r, Prog: prog})
	}
	e.mu.Lock()
	e.rules = compiled
	e.mu.Unlock()
	e.logger.Info("policy reloaded", "rules", len(compiled))
	return nil
}

// RuleCount returns the active rule count. Useful for health endpoints /
// startup checks.
func (e *Engine) RuleCount() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return len(e.rules)
}

// Evaluate walks rules in priority order, runs each `when` against fv, and
// emits ActionRequests for matches whose per-(rule, target, pod) cooldown
// has elapsed. Cooldowns are recorded on a successful match before the
// dispatcher actually runs, which means a failing actuator does NOT
// re-fire next tick. That's the same model the existing thresholder uses
// (lastActionAt is stamped on observe, not on apply).
func (e *Engine) Evaluate(fv features.FeatureVector, now time.Time) []ActionRequest {
	activation := vectorToActivation(fv)
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []ActionRequest
	for _, r := range e.rules {
		val, _, err := r.Prog.Eval(activation)
		if err != nil {
			e.logger.Warn("policy: rule eval error",
				"rule", r.Spec.Name, "target", fv.Target, "pod", fv.Pod, "err", err)
			continue
		}
		fired, ok := val.Value().(bool)
		if !ok || !fired {
			continue
		}
		key := cooldownKey{Rule: r.Spec.Name, Target: fv.Target, Pod: fv.Pod}
		if cd := time.Duration(r.Spec.Cooldown); cd > 0 {
			if last, ok := e.cooldown[key]; ok && now.Sub(last) < cd {
				e.logger.Debug("policy: rule in cooldown",
					"rule", r.Spec.Name, "target", fv.Target, "pod", fv.Pod,
					"remaining", cd-now.Sub(last))
				continue
			}
		}
		e.cooldown[key] = now
		for _, a := range r.Spec.Fire {
			out = append(out, ActionRequest{
				RuleName: r.Spec.Name,
				Target:   fv.Target,
				Pod:      fv.Pod,
				Kind:     a.Kind,
				Params:   a.Params,
			})
		}
	}
	return out
}

// ResetCooldown clears the cooldown bookkeeping. Used by tests and by
// Reload paths where the operator wants a clean slate (not the default).
func (e *Engine) ResetCooldown() {
	e.mu.Lock()
	e.cooldown = map[cooldownKey]time.Time{}
	e.mu.Unlock()
}
