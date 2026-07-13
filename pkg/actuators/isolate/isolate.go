// Package isolate is the actuator that throttles co-located aggressor pods
// by writing cpu.max in the unified cgroup v2 hierarchy. It runs only on
// the local node (one DaemonSet instance per node) so the cgroupfs writes
// are race-free without leader election.
//
// Aggressor selection is configured per-rule via a label selector string in
// params.aggressor_selector. The actuator lists pods matching that
// selector, filters to pods on this node, and throttles each one.
//
// Crash-safety: before any cgroup write, the actuator stamps the aggressor
// pod with three annotations (see actuators.AnnAggressor*). Reconcile()
// at startup verifies the on-disk cpu.max matches the expected throttled
// value for every pod that carries our set-by-node annotation; if not, it
// completes the apply.
package isolate

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/coding-workspace/simple-mitigation-1/pkg/actuators"
	"github.com/coding-workspace/simple-mitigation-1/pkg/cgroup"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// Actuator implements actuators.Actuator for cgroup CPU throttling.
type Actuator struct {
	client   kubernetes.Interface
	resolver *cgroup.Resolver
	logger   *slog.Logger
	nodeName string
}

// New constructs an isolate Actuator. resolver may be nil; the default
// resolver (host /sys/fs/cgroup + /var/lib/kubelet/pods) is used in that
// case so production wiring stays a one-liner.
func New(client kubernetes.Interface, nodeName string, resolver *cgroup.Resolver, logger *slog.Logger) (*Actuator, error) {
	if nodeName == "" {
		return nil, fmt.Errorf("isolate: nodeName required")
	}
	if resolver == nil {
		resolver = cgroup.NewDefaultResolver()
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Actuator{
		client:   client,
		resolver: resolver,
		logger:   logger.With("actuator", "isolate"),
		nodeName: nodeName,
	}, nil
}

// Name returns "isolate".
func (a *Actuator) Name() string { return "isolate" }

// Apply throttles all aggressor pods matched by params on the local node.
// Recognised params:
//
//	aggressor_selector: string  comma-separated key=value labels (REQUIRED)
//	aggressor_namespace: string optional namespace filter; defaults to target's namespace
//
// Two target-quota modes (see buildMode):
//
//	throttle_fraction: float    fraction of original quota to keep (default 0.5)
//	cap_cores: float            absolute core cap -> quota = round(cap_cores*period)
//	cpu_max_quota_us: int       absolute quota in microseconds (overrides cap_cores)
//	period_us: int              period for absolute mode (default: pod's current period)
//	min_quota_us: int           floor for absolute quota (default 1000)
//
// Fraction mode (the default) is one-shot per pod and never raises quota.
// Absolute mode (cap_cores / cpu_max_quota_us) is what the isolating
// controller's proportional ramp uses: it re-applies every call and may raise
// quota as contention releases, tracking cap back up toward baseline.
func (a *Actuator) Apply(ctx context.Context, target actuators.Target, params map[string]any) (actuators.ActionResult, error) {
	selStr, _ := params["aggressor_selector"].(string)
	if selStr == "" {
		return actuators.ActionResult{}, fmt.Errorf("isolate: aggressor_selector param required")
	}
	// mode is built after listing so cap_total_cores can be divided across
	// the matched aggressor set (see below).
	ns := target.Spec.Namespace
	if v, ok := params["aggressor_namespace"].(string); ok && v != "" {
		if v == "*" {
			// Aggressors may span testbeds/namespaces (e.g. stage-3's mixed
			// arms); "*" lists cluster-wide, still node-scoped below.
			ns = metav1.NamespaceAll
		} else {
			ns = v
		}
	}

	pods, err := a.client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: selStr,
		FieldSelector: "spec.nodeName=" + a.nodeName,
	})
	if err != nil {
		return actuators.ActionResult{}, fmt.Errorf("isolate: list aggressors: %w", err)
	}
	if len(pods.Items) == 0 {
		return actuators.ActionResult{
			Applied: false,
			Reason:  fmt.Sprintf("no aggressors on node %q matching %q", a.nodeName, selStr),
		}, nil
	}

	// cap_total_cores: the rule's cap is a TOTAL core budget for the whole
	// aggressor set; divide it evenly across the matched pods and fall
	// through to per-pod absolute mode. min_per_pod_cores makes the floor
	// dynamic in the matched-set size: no pod is ever squeezed below it, so
	// the effective aggregate floor is min_per_pod x N.
	if total, ok := paramFloatOK(params, "cap_total_cores"); ok {
		perPod := total / float64(len(pods.Items))
		if minPer, hasMin := paramFloatOK(params, "min_per_pod_cores"); hasMin && perPod < minPer {
			perPod = minPer
		}
		clone := make(map[string]any, len(params)+1)
		for k, v := range params {
			clone[k] = v
		}
		delete(clone, "cap_total_cores")
		delete(clone, "min_per_pod_cores")
		clone["cap_cores"] = perPod
		params = clone
	}
	m, err := buildMode(params)
	if err != nil {
		return actuators.ActionResult{}, err
	}

	var applied int
	touched := make([]string, 0, len(pods.Items))
	var firstErr error
	for i := range pods.Items {
		pod := &pods.Items[i]
		ok, err := a.applyPod(ctx, pod, m)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			a.logger.Warn("isolate: apply failed; continuing other aggressors",
				"pod", pod.Name, "err", err)
			continue
		}
		if ok {
			applied++
			touched = append(touched, pod.Name)
		}
	}
	after := map[string]any{"changed_pods": touched}
	for k, v := range m.after {
		after[k] = v
	}
	res := actuators.ActionResult{
		Applied: applied > 0,
		Reason:  fmt.Sprintf("applied %d/%d aggressors", applied, len(pods.Items)),
		After:   after,
	}
	if firstErr != nil && applied == 0 {
		return res, firstErr
	}
	return res, nil
}

// mode describes how Apply computes and writes the target cpu.max for each
// matched aggressor.
//
//   - fraction (default): target quota = throttle_fraction * original. Never
//     raises quota; applied once per pod (skipped while our annotation holds).
//   - absolute: target quota from cap_cores / cpu_max_quota_us. Used by the
//     isolating controller's proportional ramp, which both lowers and raises
//     the cap as contention changes, so it may raise quota and is re-applied
//     every call.
type mode struct {
	compute    func(current cgroup.CPUMax) cgroup.CPUMax
	allowRaise bool
	reapply    bool
	after      map[string]any
}

// buildMode selects fraction vs absolute mode from the rule params.
func buildMode(params map[string]any) (mode, error) {
	capCores, hasCap := paramFloatOK(params, "cap_cores")
	quotaUs, hasQuota := paramFloatOK(params, "cpu_max_quota_us")
	if hasCap || hasQuota {
		minQuota := int64(paramFloat(params, "min_quota_us", 1000))
		periodParam := int64(paramFloat(params, "period_us", 0))
		after := map[string]any{}
		if hasCap {
			after["cap_cores"] = capCores
		}
		if hasQuota {
			after["cpu_max_quota_us"] = int64(quotaUs)
		}
		return mode{
			allowRaise: true,
			reapply:    true,
			after:      after,
			compute: func(current cgroup.CPUMax) cgroup.CPUMax {
				period := periodParam
				if period <= 0 {
					period = current.Period
				}
				if period <= 0 {
					period = 100_000
				}
				var quota int64
				if hasQuota {
					quota = int64(quotaUs)
				} else {
					quota = int64(math.Round(capCores * float64(period)))
				}
				if quota < minQuota {
					quota = minQuota
				}
				return cgroup.CPUMax{Quota: quota, Period: period}
			},
		}, nil
	}
	fraction := paramFloat(params, "throttle_fraction", 0.5)
	if fraction <= 0 || fraction >= 1 {
		return mode{}, fmt.Errorf("isolate: throttle_fraction must be in (0,1), got %v", fraction)
	}
	return mode{
		allowRaise: false,
		reapply:    false,
		after:      map[string]any{"throttle_fraction": fraction},
		compute: func(current cgroup.CPUMax) cgroup.CPUMax {
			return computeThrottled(current, fraction)
		},
	}, nil
}

// applyPod computes the target cpu.max for one aggressor pod and writes it if
// it differs from the current value. Returns (true, nil) on a successful
// write; (false, nil) on a no-op (already set in fraction mode, would raise
// quota when not allowed, or target == current); (false, err) on failure.
func (a *Actuator) applyPod(ctx context.Context, pod *corev1.Pod, m mode) (bool, error) {
	cs := primaryContainerStatus(pod)
	if cs == nil || cs.ContainerID == "" {
		return false, fmt.Errorf("no running container with ID")
	}
	dir, err := a.resolver.PathForPod(string(pod.UID), cs.ContainerID)
	if err != nil {
		return false, fmt.Errorf("resolve cgroup: %w", err)
	}

	alreadySet := pod.Annotations[actuators.AnnAggressorSetByNode] == a.nodeName
	// Fraction mode is one-shot: once our annotation holds, leave it alone.
	if alreadySet && !m.reapply {
		return false, nil
	}

	current, err := a.resolver.ReadCPUMax(dir)
	if err != nil {
		return false, fmt.Errorf("read cpu.max: %w", err)
	}
	target := m.compute(current)
	if !m.allowRaise && target.Quota >= 0 && current.Quota >= 0 && target.Quota >= current.Quota {
		// Don't increase quota under the "throttle" name.
		return false, nil
	}
	if target == current {
		return false, nil
	}

	// Record the original cpu.max once, the first time this node touches the
	// pod, so Restore/Reconcile have a baseline. Order matters: original-value
	// before set-by-node, so a crash mid-write leaves Reconcile a trail.
	if !alreadySet {
		if err := a.annotatePod(ctx, pod, map[string]string{
			actuators.AnnAggressorCPUMaxOriginal: current.String(),
			actuators.AnnAggressorSetByNode:      a.nodeName,
			actuators.AnnAggressorSetAt:          time.Now().UTC().Format(time.RFC3339),
		}); err != nil {
			return false, fmt.Errorf("annotate: %w", err)
		}
	}
	if err := a.resolver.WriteCPUMax(dir, target); err != nil {
		return false, fmt.Errorf("write cpu.max: %w", err)
	}
	a.logger.Info("isolate: applied",
		"pod", pod.Name, "ns", pod.Namespace,
		"from", current.String(), "to", target.String())
	return true, nil
}

// computeThrottled returns the quota/period pair to write. For a bounded
// input it multiplies quota by fraction; for an unbounded input ("max") it
// picks fraction * period so we still get some throttling rather than a
// no-op write of "max".
func computeThrottled(current cgroup.CPUMax, fraction float64) cgroup.CPUMax {
	period := current.Period
	if period <= 0 {
		period = 100_000 // upstream default
	}
	var quota int64
	if current.Quota < 0 {
		quota = int64(fraction * float64(period))
	} else {
		quota = int64(fraction * float64(current.Quota))
	}
	if quota < 1000 {
		quota = 1000 // floor at 1ms per period
	}
	return cgroup.CPUMax{Quota: quota, Period: period}
}

// Restore reverses Apply for all pods on this node that carry our
// set-by-node annotation. target.Spec.Namespace is used to scope the
// search; pass an empty selector to mean "all pods this controller has
// touched in the target's namespace".
func (a *Actuator) Restore(ctx context.Context, target actuators.Target) error {
	ns := target.Spec.Namespace
	pods, err := a.client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + a.nodeName,
	})
	if err != nil {
		return fmt.Errorf("isolate.Restore: list pods: %w", err)
	}
	var firstErr error
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Annotations[actuators.AnnAggressorSetByNode] != a.nodeName {
			continue
		}
		if err := a.restorePod(ctx, pod); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			a.logger.Warn("isolate.Restore: pod restore failed",
				"pod", pod.Name, "err", err)
		}
	}
	return firstErr
}

func (a *Actuator) restorePod(ctx context.Context, pod *corev1.Pod) error {
	originalStr := pod.Annotations[actuators.AnnAggressorCPUMaxOriginal]
	if originalStr == "" {
		return nil
	}
	original, err := cgroup.ParseCPUMax(originalStr)
	if err != nil {
		return fmt.Errorf("parse original: %w", err)
	}
	cs := primaryContainerStatus(pod)
	if cs == nil || cs.ContainerID == "" {
		return fmt.Errorf("no running container with ID")
	}
	dir, err := a.resolver.PathForPod(string(pod.UID), cs.ContainerID)
	if err != nil {
		return fmt.Errorf("resolve cgroup: %w", err)
	}
	if err := a.resolver.WriteCPUMax(dir, original); err != nil {
		return fmt.Errorf("write cpu.max: %w", err)
	}
	if err := a.annotatePod(ctx, pod, map[string]string{
		actuators.AnnAggressorCPUMaxOriginal: "",
		actuators.AnnAggressorSetByNode:      "",
		actuators.AnnAggressorSetAt:          "",
	}); err != nil {
		a.logger.Warn("isolate.Restore: clear annotations failed",
			"pod", pod.Name, "err", err)
	}
	a.logger.Info("isolate: restored", "pod", pod.Name, "to", original.String())
	return nil
}

// Reconcile: re-walks pods on this node that carry our set-by-node
// annotation and re-applies the throttled cpu.max if it doesn't match
// what's currently on disk. Covers the "annotation written, write
// interrupted" crash window.
func (a *Actuator) Reconcile(ctx context.Context) error {
	pods, err := a.client.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + a.nodeName,
	})
	if err != nil {
		return fmt.Errorf("isolate.Reconcile: list pods: %w", err)
	}
	var checked, repaired int
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Annotations[actuators.AnnAggressorSetByNode] != a.nodeName {
			continue
		}
		checked++
		originalStr := pod.Annotations[actuators.AnnAggressorCPUMaxOriginal]
		if originalStr == "" {
			continue
		}
		original, err := cgroup.ParseCPUMax(originalStr)
		if err != nil {
			a.logger.Warn("isolate.Reconcile: bad original annotation",
				"pod", pod.Name, "value", originalStr, "err", err)
			continue
		}
		cs := primaryContainerStatus(pod)
		if cs == nil || cs.ContainerID == "" {
			continue
		}
		dir, err := a.resolver.PathForPod(string(pod.UID), cs.ContainerID)
		if err != nil {
			a.logger.Warn("isolate.Reconcile: resolve cgroup",
				"pod", pod.Name, "err", err)
			continue
		}
		live, err := a.resolver.ReadCPUMax(dir)
		if err != nil {
			a.logger.Warn("isolate.Reconcile: read cpu.max",
				"pod", pod.Name, "err", err)
			continue
		}
		// If the live quota is already <= original, leave it alone -- some
		// policy already throttled this pod and we can't tell from the
		// annotation alone what the *target* quota should be. We'll re-apply
		// the next time a rule fires.
		if original.Quota > 0 && (live.Quota < 0 || live.Quota >= original.Quota) {
			// Live setting is at original or unbounded; the apply was lost.
			// Re-throttle at the default fraction so the pod isn't roaming
			// unbounded under our annotation. The next rule firing will
			// overwrite to the rule-specified fraction.
			throttled := computeThrottled(original, 0.5)
			if err := a.resolver.WriteCPUMax(dir, throttled); err != nil {
				a.logger.Warn("isolate.Reconcile: rewrite cpu.max",
					"pod", pod.Name, "err", err)
				continue
			}
			repaired++
		}
	}
	a.logger.Info("isolate: reconcile complete",
		"node", a.nodeName, "checked", checked, "repaired", repaired)
	return nil
}

func primaryContainerStatus(pod *corev1.Pod) *corev1.ContainerStatus {
	for i := range pod.Status.ContainerStatuses {
		cs := &pod.Status.ContainerStatuses[i]
		if cs.Ready && cs.ContainerID != "" {
			return cs
		}
	}
	// Fall back to the first one with a container ID, ready or not.
	for i := range pod.Status.ContainerStatuses {
		cs := &pod.Status.ContainerStatuses[i]
		if cs.ContainerID != "" {
			return cs
		}
	}
	return nil
}

func (a *Actuator) annotatePod(ctx context.Context, pod *corev1.Pod, ann map[string]string) error {
	for k, v := range ann {
		if v == "" {
			ann[k] = "" // explicit empty is fine for our reads, no merge-patch null tricks needed
		}
	}
	patch := map[string]any{
		"metadata": map[string]any{"annotations": stringMapToAnyMap(ann)},
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	_, err = a.client.CoreV1().Pods(pod.Namespace).Patch(ctx, pod.Name, types.MergePatchType, body, metav1.PatchOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// stringMapToAnyMap converts a string->string map to string->any, mapping
// empty strings to JSON null so the merge patch removes the annotation
// rather than leaving an empty value behind.
func stringMapToAnyMap(in map[string]string) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		if v == "" {
			out[k] = nil
		} else {
			out[k] = v
		}
	}
	return out
}

func paramFloat(params map[string]any, key string, def float64) float64 {
	if f, ok := paramFloatOK(params, key); ok {
		return f
	}
	return def
}

// paramFloatOK reports whether key is present and coerces it to float64.
func paramFloatOK(params map[string]any, key string) (float64, bool) {
	v, ok := params[key]
	if !ok {
		return 0, false
	}
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case string:
		// Accept quoted numeric literals from YAML rule authors.
		var f float64
		if _, err := fmt.Sscanf(x, "%f", &f); err == nil {
			return f, true
		}
	}
	return 0, false
}

// Compile-time interface check.
var _ actuators.Actuator = (*Actuator)(nil)
