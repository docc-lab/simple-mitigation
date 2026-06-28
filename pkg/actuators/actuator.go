// Package actuators defines the shared interface every concrete mitigation
// actuator (isolate, vertical, horizontal) implements. The controller's
// dispatcher only ever speaks to this interface; rule kind strings
// ("isolate", "vertical", "horizontal", "restore") are mapped to actuator
// instances at controller startup.
package actuators

import (
	"context"

	"github.com/coding-workspace/simple-mitigation-1/pkg/targets"
)

// Target identifies the victim that triggered an action. It carries enough
// context for the actuator to resolve any K8s object it needs:
//
//   - Target.Spec is the loaded target config (namespace, selector, etc.)
//   - PodName / NodeName are the specific local pod that produced the
//     features; isolate uses NodeName as the local-node gate, vertical
//     uses PodName, horizontal uses Target.Spec to find the Deployment.
type Target struct {
	Spec     *targets.Target
	PodName  string
	NodeName string
}

// ActionResult is the actuator's report back to the dispatcher. Fields are
// optional; the dispatcher logs them as-is.
type ActionResult struct {
	// Applied is true iff the actuator made an externally-visible change
	// this call (a write to the K8s API, a write to cgroupfs, etc).
	// Idempotent no-ops (e.g. ensure_min when current >= min) report
	// Applied=false.
	Applied bool
	// Reason is a short human-readable summary used for log lines.
	Reason string
	// Before / After are free-form actuator-specific snapshots used for
	// audit. E.g. vertical reports {"cpu": "500m"} and {"cpu": "750m"}.
	Before map[string]any
	After  map[string]any
}

// Actuator is the contract every kind implements. Implementations must be
// safe to call concurrently across distinct targets; per-target serial
// ordering is the dispatcher's responsibility.
type Actuator interface {
	// Name returns the kind string that selects this actuator from a rule's
	// `fire[].kind`. E.g. "isolate", "vertical", "horizontal", "restore".
	Name() string

	// Apply executes the action for target with the rule-provided params.
	// Returns an ActionResult describing what (if anything) changed.
	Apply(ctx context.Context, target Target, params map[string]any) (ActionResult, error)

	// Restore reverses a prior Apply. Implementations read crash-safe
	// annotations written by Apply (see Section 7 of plan-v2-centralized.md)
	// to determine the baseline. A no-op if there's nothing to restore.
	Restore(ctx context.Context, target Target) error

	// Reconcile is called once at startup before the first tick. It must
	// handle: (a) annotations written by an earlier crashed instance whose
	// underlying write was interrupted, and (b) annotations the actuator
	// otherwise wants to observe (e.g. counting "currently scaled"
	// deployments for metrics). Errors are logged but not fatal.
	Reconcile(ctx context.Context) error
}

// Annotation keys -- single source of truth, kept here so each actuator
// reads/writes the same strings. Values match Section 7 verbatim.
const (
	AnnAggressorCPUMaxOriginal = "mitigation/cpu-max-original"
	AnnAggressorSetByNode      = "mitigation/cpu-max-set-by-node"
	AnnAggressorSetAt          = "mitigation/cpu-max-set-at"

	// Harvest annotations live on the best-effort pod the harvester lends
	// idle cores to. Kept distinct from the aggressor keys so a pod could in
	// principle be both throttled (as an aggressor) and granted (as BE)
	// without the two actuators clobbering each other's baseline.
	AnnHarvestCPUMaxOriginal = "mitigation/harvest-cpu-max-original"
	AnnHarvestSetByNode      = "mitigation/harvest-set-by-node"
	AnnHarvestSetAt          = "mitigation/harvest-set-at"

	AnnVictimCPULimitBaseline = "mitigation/cpu-limit-baseline"

	AnnDeployHorizLastScaledAt = "mitigation/horizontal-last-scaled-at"
	AnnDeployHorizBaseline     = "mitigation/horizontal-baseline-replicas"
)
