// Package horizontal is the actuator that scales the victim Deployment's
// replica count via the apps/v1.Deployment/scale subresource. It replaces
// the entire custom-pod-autoscaler + sidecar story from the v1 design.
//
// Two semantics are supported, selected by rule params:
//
//   - delta: N    -> reads current via /scale, patches to current+N, retries
//                    once on a 409 ResourceVersion conflict.
//   - ensure_min: N  -> only patches if current < N. Fully idempotent;
//                       multiple node-instances firing the same rule at
//                       the same tick converge to N.
//
// Every successful Apply also stamps the Deployment with
// `mitigation/horizontal-last-scaled-at` (RFC3339) and
// `mitigation/horizontal-baseline-replicas` (on first action only) so
// Restore knows where to scale back to.
package horizontal

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/coding-workspace/simple-mitigation-1/pkg/actuators"

	autoscalingv1 "k8s.io/api/autoscaling/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// Actuator implements actuators.Actuator for horizontal scaling.
type Actuator struct {
	client   kubernetes.Interface
	logger   *slog.Logger
	nodeName string

	// DefaultCooldown bounds how often the same Deployment can be scaled
	// regardless of how many node-instances fire concurrently. Per-rule
	// cooldown in the policy engine is per-node; this is the cross-node
	// bound. Set via NewActuator; zero disables the gate.
	defaultCooldown time.Duration
}

// New constructs a horizontal Actuator.
func New(client kubernetes.Interface, nodeName string, defaultCooldown time.Duration, logger *slog.Logger) *Actuator {
	if logger == nil {
		logger = slog.Default()
	}
	return &Actuator{
		client:          client,
		logger:          logger.With("actuator", "horizontal"),
		nodeName:        nodeName,
		defaultCooldown: defaultCooldown,
	}
}

// Name returns "horizontal".
func (a *Actuator) Name() string { return "horizontal" }

// Apply scales target's Deployment per the params. Params recognised:
//
//	delta: int           additive change (positive or negative)
//	ensure_min: int      lower bound; no-op when current >= ensure_min
//	max_replicas: int    upper clamp for delta (defaults to math.MaxInt32)
//	min_replicas: int    lower clamp for delta (defaults to 1)
//
// Exactly one of delta or ensure_min must be set.
func (a *Actuator) Apply(ctx context.Context, target actuators.Target, params map[string]any) (actuators.ActionResult, error) {
	if target.Spec == nil {
		return actuators.ActionResult{}, fmt.Errorf("horizontal: nil target spec")
	}
	// Explicit override first: the overflow-deployment pattern scales a
	// deployment that deliberately does NOT match the target selector
	// (the victim deployment stays pinned at its baseline replicas).
	deployName, _ := params["deployment"].(string)
	if deployName == "" {
		var err error
		deployName, err = a.resolveDeployment(ctx, target)
		if err != nil {
			return actuators.ActionResult{}, err
		}
	}

	deltaRaw, hasDelta := params["delta"]
	minRaw, hasMin := params["ensure_min"]
	if hasDelta == hasMin {
		return actuators.ActionResult{}, fmt.Errorf("horizontal: rule must set exactly one of delta or ensure_min")
	}

	minReplicas := paramInt32(params, "min_replicas", 1)
	maxReplicas := paramInt32(params, "max_replicas", 1<<30)

	scale, err := a.client.AppsV1().Deployments(target.Spec.Namespace).GetScale(ctx, deployName, metav1.GetOptions{})
	if err != nil {
		return actuators.ActionResult{}, fmt.Errorf("horizontal: get /scale: %w", err)
	}
	current := scale.Spec.Replicas

	// Cross-node cooldown gate: read the annotation on the Deployment proper.
	if a.defaultCooldown > 0 {
		if remaining, gated, err := a.checkCooldown(ctx, target.Spec.Namespace, deployName); err != nil {
			a.logger.Warn("horizontal: cooldown check failed; allowing action",
				"deployment", deployName, "err", err)
		} else if gated {
			return actuators.ActionResult{
				Applied: false,
				Reason:  fmt.Sprintf("cross-node cooldown active, %s remaining", remaining),
				Before:  map[string]any{"replicas": current},
				After:   map[string]any{"replicas": current},
			}, nil
		}
	}

	var desired int32
	switch {
	case hasDelta:
		delta, err := toInt32(deltaRaw)
		if err != nil {
			return actuators.ActionResult{}, fmt.Errorf("horizontal: delta: %w", err)
		}
		desired = current + delta
	case hasMin:
		floor, err := toInt32(minRaw)
		if err != nil {
			return actuators.ActionResult{}, fmt.Errorf("horizontal: ensure_min: %w", err)
		}
		if current >= floor {
			return actuators.ActionResult{
				Applied: false,
				Reason:  fmt.Sprintf("ensure_min satisfied (current=%d >= %d)", current, floor),
				Before:  map[string]any{"replicas": current},
				After:   map[string]any{"replicas": current},
			}, nil
		}
		desired = floor
	}
	if desired < minReplicas {
		desired = minReplicas
	}
	if desired > maxReplicas {
		desired = maxReplicas
	}
	if desired == current {
		return actuators.ActionResult{
			Applied: false,
			Reason:  fmt.Sprintf("desired==current (%d), clamped or no-op", current),
			Before:  map[string]any{"replicas": current},
			After:   map[string]any{"replicas": current},
		}, nil
	}

	// Annotate baseline only once -- subsequent Restores read this.
	if err := a.ensureBaseline(ctx, target.Spec.Namespace, deployName, current); err != nil {
		a.logger.Warn("horizontal: baseline annotation failed; continuing",
			"deployment", deployName, "err", err)
	}

	// Patch /scale with optimistic concurrency. On 409, re-read and retry once.
	if err := a.patchScale(ctx, target.Spec.Namespace, deployName, scale.ResourceVersion, desired); err != nil {
		if apierrors.IsConflict(err) {
			rv2, current2, perr := a.refetchScale(ctx, target.Spec.Namespace, deployName)
			if perr != nil {
				return actuators.ActionResult{}, fmt.Errorf("horizontal: re-read /scale after conflict: %w", perr)
			}
			// If another writer already took us to or past desired, that's a
			// win; treat as no-op rather than re-patching backwards.
			if current2 >= desired && hasMin {
				return actuators.ActionResult{
					Applied: false,
					Reason:  fmt.Sprintf("concurrent writer satisfied ensure_min (now=%d)", current2),
					Before:  map[string]any{"replicas": current},
					After:   map[string]any{"replicas": current2},
				}, nil
			}
			if err := a.patchScale(ctx, target.Spec.Namespace, deployName, rv2, desired); err != nil {
				return actuators.ActionResult{}, fmt.Errorf("horizontal: retry patch /scale: %w", err)
			}
		} else {
			return actuators.ActionResult{}, fmt.Errorf("horizontal: patch /scale: %w", err)
		}
	}

	// Stamp the cooldown annotation; ignored on failure (the patch already won).
	if err := a.stampLastScaledAt(ctx, target.Spec.Namespace, deployName, time.Now()); err != nil {
		a.logger.Warn("horizontal: last-scaled-at annotation failed",
			"deployment", deployName, "err", err)
	}

	return actuators.ActionResult{
		Applied: true,
		Reason:  "scaled",
		Before:  map[string]any{"replicas": current},
		After:   map[string]any{"replicas": desired},
	}, nil
}

// Restore scales target's Deployment back to its mitigation/horizontal-baseline-replicas
// annotation, if any. Removes the baseline annotation on success.
func (a *Actuator) Restore(ctx context.Context, target actuators.Target) error {
	if target.Spec == nil {
		return fmt.Errorf("horizontal.Restore: nil target spec")
	}
	deployName, err := a.resolveDeployment(ctx, target)
	if err != nil {
		return err
	}
	dep, err := a.client.AppsV1().Deployments(target.Spec.Namespace).Get(ctx, deployName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("horizontal.Restore: get deployment: %w", err)
	}
	baseRaw := dep.Annotations[actuators.AnnDeployHorizBaseline]
	if baseRaw == "" {
		return nil
	}
	base64, err := strconv.ParseInt(baseRaw, 10, 32)
	if err != nil {
		return fmt.Errorf("horizontal.Restore: bad baseline %q: %w", baseRaw, err)
	}
	desired := int32(base64)

	scale, err := a.client.AppsV1().Deployments(target.Spec.Namespace).GetScale(ctx, deployName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("horizontal.Restore: get /scale: %w", err)
	}
	if scale.Spec.Replicas != desired {
		if err := a.patchScale(ctx, target.Spec.Namespace, deployName, scale.ResourceVersion, desired); err != nil {
			return fmt.Errorf("horizontal.Restore: patch /scale: %w", err)
		}
	}
	// Drop both annotations so a fresh apply re-stamps the baseline.
	if err := a.clearAnnotations(ctx, target.Spec.Namespace, deployName,
		actuators.AnnDeployHorizBaseline, actuators.AnnDeployHorizLastScaledAt); err != nil {
		a.logger.Warn("horizontal.Restore: clear annotations failed",
			"deployment", deployName, "err", err)
	}
	a.logger.Info("horizontal: restored", "deployment", deployName, "to", desired)
	return nil
}

// Reconcile is a no-op for horizontal: the /scale patch is single-write
// atomic, so there's nothing partial to fix up after a crash. We could
// surface "currently scaled" deployments to metrics here; deferred.
func (a *Actuator) Reconcile(_ context.Context) error { return nil }

// resolveDeployment returns the Deployment name matching the target's
// label selector in target.Spec.Namespace. Errors if zero or many match
// so misconfiguration is loud, not silently scaling the wrong workload.
func (a *Actuator) resolveDeployment(ctx context.Context, target actuators.Target) (string, error) {
	sel := labels.SelectorFromSet(labels.Set(target.Spec.Selector)).String()
	deps, err := a.client.AppsV1().Deployments(target.Spec.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: sel,
	})
	if err != nil {
		return "", fmt.Errorf("horizontal: list deployments: %w", err)
	}
	switch len(deps.Items) {
	case 0:
		return "", fmt.Errorf("horizontal: no deployment matches selector %q in ns %q", sel, target.Spec.Namespace)
	case 1:
		return deps.Items[0].Name, nil
	default:
		// Common when a single label is shared across multiple deployments.
		// Caller should tighten target.Selector.
		names := make([]string, 0, len(deps.Items))
		for _, d := range deps.Items {
			names = append(names, d.Name)
		}
		return "", fmt.Errorf("horizontal: selector %q matches %d deployments (%v); narrow it", sel, len(deps.Items), names)
	}
}

func (a *Actuator) patchScale(ctx context.Context, ns, name, resourceVersion string, replicas int32) error {
	patch := autoscalingv1.Scale{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       ns,
			ResourceVersion: resourceVersion,
		},
		Spec: autoscalingv1.ScaleSpec{Replicas: replicas},
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	_, err = a.client.AppsV1().Deployments(ns).Patch(ctx, name, types.MergePatchType, body, metav1.PatchOptions{}, "scale")
	return err
}

func (a *Actuator) refetchScale(ctx context.Context, ns, name string) (string, int32, error) {
	s, err := a.client.AppsV1().Deployments(ns).GetScale(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", 0, err
	}
	return s.ResourceVersion, s.Spec.Replicas, nil
}

func (a *Actuator) ensureBaseline(ctx context.Context, ns, name string, current int32) error {
	dep, err := a.client.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if _, ok := dep.Annotations[actuators.AnnDeployHorizBaseline]; ok {
		return nil
	}
	return a.patchAnnotations(ctx, ns, name, map[string]string{
		actuators.AnnDeployHorizBaseline: strconv.FormatInt(int64(current), 10),
	})
}

func (a *Actuator) stampLastScaledAt(ctx context.Context, ns, name string, when time.Time) error {
	return a.patchAnnotations(ctx, ns, name, map[string]string{
		actuators.AnnDeployHorizLastScaledAt: when.UTC().Format(time.RFC3339),
	})
}

func (a *Actuator) checkCooldown(ctx context.Context, ns, name string) (time.Duration, bool, error) {
	dep, err := a.client.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return 0, false, err
	}
	raw := dep.Annotations[actuators.AnnDeployHorizLastScaledAt]
	if raw == "" {
		return 0, false, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return 0, false, fmt.Errorf("parse last-scaled-at %q: %w", raw, err)
	}
	elapsed := time.Since(t)
	if elapsed >= a.defaultCooldown {
		return 0, false, nil
	}
	return a.defaultCooldown - elapsed, true, nil
}

func (a *Actuator) patchAnnotations(ctx context.Context, ns, name string, ann map[string]string) error {
	patch := map[string]any{
		"metadata": map[string]any{"annotations": ann},
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	_, err = a.client.AppsV1().Deployments(ns).Patch(ctx, name, types.MergePatchType, body, metav1.PatchOptions{})
	return err
}

func (a *Actuator) clearAnnotations(ctx context.Context, ns, name string, keys ...string) error {
	ann := make(map[string]any, len(keys))
	for _, k := range keys {
		ann[k] = nil // JSON null deletes the key under merge-patch semantics
	}
	patch := map[string]any{
		"metadata": map[string]any{"annotations": ann},
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	_, err = a.client.AppsV1().Deployments(ns).Patch(ctx, name, types.MergePatchType, body, metav1.PatchOptions{})
	return err
}

// toInt32 unwraps the int / int64 / float64 forms that YAML/CEL may produce.
func toInt32(v any) (int32, error) {
	switch x := v.(type) {
	case int:
		return int32(x), nil
	case int32:
		return x, nil
	case int64:
		return int32(x), nil
	case float64:
		return int32(x), nil
	case float32:
		return int32(x), nil
	default:
		return 0, fmt.Errorf("not an integer (%T)", v)
	}
}

func paramInt32(params map[string]any, key string, def int32) int32 {
	if v, ok := params[key]; ok {
		if n, err := toInt32(v); err == nil {
			return n
		}
	}
	return def
}
