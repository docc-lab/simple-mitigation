// Package harvest is the actuator that lends a victim's idle CPU headroom to
// co-located best-effort (BE) pods by raising their cgroup v2 cpu.max. It is
// the inverse of the isolate actuator: where isolate throttles an aggressor
// down, harvest grants a BE pod up, and reclaims the grant the instant
// contention returns.
//
// It is driven by the harvesting controller (pkg/controllers), whose AIMD law
// produces h(t) = cores to release. Each Apply call writes the BE pod's
// cpu.max to baseline + round(h * period); when h falls (multiplicative
// backoff under contention) the grant shrinks back toward baseline.
//
// The "harvested" cores come from the victim's idle headroom: the node has the
// capacity precisely because the latency-critical service is not using it. The
// actuator does not lower the victim; it only raises BE and reclaims. Whether
// victim and BE share a parent cgroup slice is a deployment/placement concern,
// not the actuator's — it writes the BE pod's own cpu.max either way.
//
// Crash-safety mirrors isolate: the BE pod is annotated with its original
// cpu.max before the first grant; Restore writes that back; Reconcile resets
// every annotated BE pod to baseline at startup (the safe direction, since a
// stranded grant leaves BE over-provisioned).
package harvest

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

// Actuator implements actuators.Actuator for lending CPU to best-effort pods.
type Actuator struct {
	client   kubernetes.Interface
	resolver *cgroup.Resolver
	logger   *slog.Logger
	nodeName string
}

// New constructs a harvest Actuator. resolver may be nil; the default resolver
// (host /sys/fs/cgroup + /var/lib/kubelet/pods) is used in that case.
func New(client kubernetes.Interface, nodeName string, resolver *cgroup.Resolver, logger *slog.Logger) (*Actuator, error) {
	if nodeName == "" {
		return nil, fmt.Errorf("harvest: nodeName required")
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
		logger:   logger.With("actuator", "harvest"),
		nodeName: nodeName,
	}, nil
}

// Name returns "harvest".
func (a *Actuator) Name() string { return "harvest" }

// Apply grants harvested cores to all best-effort pods matched by params on
// the local node. Recognised params:
//
//	be_selector: string    comma-separated key=value labels (REQUIRED)
//	harvest_cores: float   cores to lend on top of baseline (REQUIRED, >= 0)
//	be_namespace: string   optional namespace filter; defaults to target's namespace
//	period_us: int         period for the quota write (default: pod's current period)
//	max_quota_us: int      optional upper clamp on the granted quota
//
// The grant is target_quota = baseline_quota + round(harvest_cores * period).
// BE pods already unbounded ("max") are skipped — there's nothing to grant.
func (a *Actuator) Apply(ctx context.Context, target actuators.Target, params map[string]any) (actuators.ActionResult, error) {
	selStr, _ := params["be_selector"].(string)
	if selStr == "" {
		return actuators.ActionResult{}, fmt.Errorf("harvest: be_selector param required")
	}
	hCores, ok := paramFloatOK(params, "harvest_cores")
	if !ok {
		return actuators.ActionResult{}, fmt.Errorf("harvest: harvest_cores param required")
	}
	if hCores < 0 {
		return actuators.ActionResult{}, fmt.Errorf("harvest: harvest_cores must be >= 0, got %v", hCores)
	}
	periodParam := int64(paramFloat(params, "period_us", 0))
	maxQuota := int64(paramFloat(params, "max_quota_us", 0)) // 0 = no clamp
	ns := target.Spec.Namespace
	if v, ok := params["be_namespace"].(string); ok && v != "" {
		ns = v
	}

	pods, err := a.client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: selStr,
		FieldSelector: "spec.nodeName=" + a.nodeName,
	})
	if err != nil {
		return actuators.ActionResult{}, fmt.Errorf("harvest: list be pods: %w", err)
	}
	if len(pods.Items) == 0 {
		return actuators.ActionResult{
			Applied: false,
			Reason:  fmt.Sprintf("no best-effort pods on node %q matching %q", a.nodeName, selStr),
		}, nil
	}

	var applied int
	touched := make([]string, 0, len(pods.Items))
	var firstErr error
	for i := range pods.Items {
		pod := &pods.Items[i]
		ok, err := a.grantPod(ctx, pod, hCores, periodParam, maxQuota)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			a.logger.Warn("harvest: grant failed; continuing other be pods",
				"pod", pod.Name, "err", err)
			continue
		}
		if ok {
			applied++
			touched = append(touched, pod.Name)
		}
	}
	res := actuators.ActionResult{
		Applied: applied > 0,
		Reason:  fmt.Sprintf("granted %d/%d best-effort pods", applied, len(pods.Items)),
		After:   map[string]any{"changed_pods": touched, "harvest_cores": hCores},
	}
	if firstErr != nil && applied == 0 {
		return res, firstErr
	}
	return res, nil
}

// grantPod sets one BE pod's cpu.max to baseline + harvested cores. Returns
// (true, nil) on a write, (false, nil) on a no-op (unbounded baseline or
// target == current), (false, err) on failure.
func (a *Actuator) grantPod(ctx context.Context, pod *corev1.Pod, hCores float64, periodParam, maxQuota int64) (bool, error) {
	cs := primaryContainerStatus(pod)
	if cs == nil || cs.ContainerID == "" {
		return false, fmt.Errorf("no running container with ID")
	}
	dir, err := a.resolver.PathForPod(string(pod.UID), cs.ContainerID)
	if err != nil {
		return false, fmt.Errorf("resolve cgroup: %w", err)
	}
	current, err := a.resolver.ReadCPUMax(dir)
	if err != nil {
		return false, fmt.Errorf("read cpu.max: %w", err)
	}

	// Baseline is the pod's cpu.max before we first granted: read from the
	// annotation on subsequent calls, else the current value.
	alreadySet := pod.Annotations[actuators.AnnHarvestSetByNode] == a.nodeName
	baseline := current
	if alreadySet {
		b, err := cgroup.ParseCPUMax(pod.Annotations[actuators.AnnHarvestCPUMaxOriginal])
		if err != nil {
			return false, fmt.Errorf("parse harvest baseline: %w", err)
		}
		baseline = b
	}

	// An unbounded BE pod can't be "granted more"; nothing to do.
	if baseline.Quota < 0 {
		return false, nil
	}
	period := periodParam
	if period <= 0 {
		period = baseline.Period
	}
	if period <= 0 {
		period = 100_000
	}

	extra := int64(math.Round(hCores * float64(period)))
	target := cgroup.CPUMax{Quota: baseline.Quota + extra, Period: period}
	if maxQuota > 0 && target.Quota > maxQuota {
		target.Quota = maxQuota
	}
	if target == current {
		return false, nil
	}

	if !alreadySet {
		if err := a.annotatePod(ctx, pod, map[string]string{
			actuators.AnnHarvestCPUMaxOriginal: baseline.String(),
			actuators.AnnHarvestSetByNode:      a.nodeName,
			actuators.AnnHarvestSetAt:          time.Now().UTC().Format(time.RFC3339),
		}); err != nil {
			return false, fmt.Errorf("annotate: %w", err)
		}
	}
	if err := a.resolver.WriteCPUMax(dir, target); err != nil {
		return false, fmt.Errorf("write cpu.max: %w", err)
	}
	a.logger.Info("harvest: granted",
		"pod", pod.Name, "ns", pod.Namespace,
		"baseline", baseline.String(), "to", target.String(),
		"harvest_cores", hCores)
	return true, nil
}

// Restore writes every BE pod we've granted on this node back to its baseline
// and clears the harvest annotations.
func (a *Actuator) Restore(ctx context.Context, target actuators.Target) error {
	pods, err := a.client.CoreV1().Pods(target.Spec.Namespace).List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + a.nodeName,
	})
	if err != nil {
		return fmt.Errorf("harvest.Restore: list pods: %w", err)
	}
	var firstErr error
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Annotations[actuators.AnnHarvestSetByNode] != a.nodeName {
			continue
		}
		if err := a.restorePod(ctx, pod); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			a.logger.Warn("harvest.Restore: pod restore failed", "pod", pod.Name, "err", err)
		}
	}
	return firstErr
}

func (a *Actuator) restorePod(ctx context.Context, pod *corev1.Pod) error {
	originalStr := pod.Annotations[actuators.AnnHarvestCPUMaxOriginal]
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
		actuators.AnnHarvestCPUMaxOriginal: "",
		actuators.AnnHarvestSetByNode:      "",
		actuators.AnnHarvestSetAt:          "",
	}); err != nil {
		a.logger.Warn("harvest.Restore: clear annotations failed", "pod", pod.Name, "err", err)
	}
	a.logger.Info("harvest: restored", "pod", pod.Name, "to", original.String())
	return nil
}

// Reconcile resets every BE pod carrying our annotation back to baseline at
// startup. A stranded grant from a crashed instance leaves BE over-provisioned,
// so the safe recovery is to restore; the controller re-grants on the next tick.
func (a *Actuator) Reconcile(ctx context.Context) error {
	pods, err := a.client.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + a.nodeName,
	})
	if err != nil {
		return fmt.Errorf("harvest.Reconcile: list pods: %w", err)
	}
	var restored int
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Annotations[actuators.AnnHarvestSetByNode] != a.nodeName {
			continue
		}
		if err := a.restorePod(ctx, pod); err != nil {
			a.logger.Warn("harvest.Reconcile: restore failed", "pod", pod.Name, "err", err)
			continue
		}
		restored++
	}
	a.logger.Info("harvest: reconcile complete", "node", a.nodeName, "restored", restored)
	return nil
}

func primaryContainerStatus(pod *corev1.Pod) *corev1.ContainerStatus {
	for i := range pod.Status.ContainerStatuses {
		cs := &pod.Status.ContainerStatuses[i]
		if cs.Ready && cs.ContainerID != "" {
			return cs
		}
	}
	for i := range pod.Status.ContainerStatuses {
		cs := &pod.Status.ContainerStatuses[i]
		if cs.ContainerID != "" {
			return cs
		}
	}
	return nil
}

func (a *Actuator) annotatePod(ctx context.Context, pod *corev1.Pod, ann map[string]string) error {
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

// stringMapToAnyMap maps empty strings to JSON null so the merge patch removes
// the annotation rather than leaving an empty value behind.
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
		var f float64
		if _, err := fmt.Sscanf(x, "%f", &f); err == nil {
			return f, true
		}
	}
	return 0, false
}

// Compile-time interface check.
var _ actuators.Actuator = (*Actuator)(nil)
