// Package vertical is the actuator that patches a victim pod's CPU
// requests/limits via the pods/resize subresource (K8s 1.33 beta / 1.35 GA).
// The bulk of the logic is lifted from the v1 cmd/vertical-scaler binary
// and re-packaged as an Actuator.
//
// Two invariants are preserved from the v1 design:
//
//   - requests == limits, so the victim keeps Guaranteed QoS (a prerequisite
//     for in-place resize on most setups).
//   - Local-only operation: an actuator instance running on node X never
//     mutates a pod whose .spec.nodeName != X. The dispatcher should
//     already enforce this via Target.NodeName, but we double-check here
//     so a misconfigured rule can't reach across nodes.
package vertical

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/coding-workspace/simple-mitigation-1/pkg/actuators"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// Actuator implements actuators.Actuator for in-place CPU resize.
type Actuator struct {
	client   kubernetes.Interface
	logger   *slog.Logger
	nodeName string

	minCPU resource.Quantity
	maxCPU resource.Quantity
}

// Config carries the cluster-wide bounds that any rule's scale factor is
// clamped to. Defaults match the v1 binary (200m / 4).
type Config struct {
	NodeName string
	MinCPU   resource.Quantity
	MaxCPU   resource.Quantity
}

// New constructs a vertical Actuator. Returns an error if MinCPU > MaxCPU.
func New(client kubernetes.Interface, cfg Config, logger *slog.Logger) (*Actuator, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.NodeName == "" {
		return nil, fmt.Errorf("vertical: NodeName is required")
	}
	if cfg.MinCPU.Cmp(cfg.MaxCPU) > 0 {
		return nil, fmt.Errorf("vertical: MinCPU (%s) > MaxCPU (%s)", cfg.MinCPU.String(), cfg.MaxCPU.String())
	}
	return &Actuator{
		client:   client,
		logger:   logger.With("actuator", "vertical"),
		nodeName: cfg.NodeName,
		minCPU:   cfg.MinCPU,
		maxCPU:   cfg.MaxCPU,
	}, nil
}

// Name returns "vertical".
func (a *Actuator) Name() string { return "vertical" }

// Apply resizes the target pod's CPU. Params recognised:
//
//	scale_factor: float  multiplicative; >1 grows, <1 shrinks
//	target_cpu:  string  absolute (e.g. "750m"); overrides scale_factor if both set
func (a *Actuator) Apply(ctx context.Context, target actuators.Target, params map[string]any) (actuators.ActionResult, error) {
	if target.Spec == nil {
		return actuators.ActionResult{}, fmt.Errorf("vertical: nil target spec")
	}
	if target.PodName == "" {
		return actuators.ActionResult{}, fmt.Errorf("vertical: target.PodName required")
	}
	if target.Spec.ContainerName == "" {
		return actuators.ActionResult{}, fmt.Errorf("vertical: target %q has no containerName", target.Spec.Name)
	}

	pod, err := a.client.CoreV1().Pods(target.Spec.Namespace).Get(ctx, target.PodName, metav1.GetOptions{})
	if err != nil {
		return actuators.ActionResult{}, fmt.Errorf("vertical: get pod: %w", err)
	}
	if pod.Spec.NodeName != "" && pod.Spec.NodeName != a.nodeName {
		// Belt-and-suspenders: dispatcher should already gate this.
		return actuators.ActionResult{
			Applied: false,
			Reason:  fmt.Sprintf("pod runs on %q, this instance is on %q", pod.Spec.NodeName, a.nodeName),
		}, nil
	}
	container := findContainer(pod, target.Spec.ContainerName)
	if container == nil {
		return actuators.ActionResult{}, fmt.Errorf("vertical: container %q not found in pod %q", target.Spec.ContainerName, target.PodName)
	}
	current, ok := container.Resources.Limits[corev1.ResourceCPU]
	if !ok {
		return actuators.ActionResult{}, fmt.Errorf("vertical: container %q has no CPU limit", target.Spec.ContainerName)
	}

	// Resolve desired quantity.
	var desired resource.Quantity
	if abs, ok := params["target_cpu"].(string); ok && abs != "" {
		q, err := resource.ParseQuantity(abs)
		if err != nil {
			return actuators.ActionResult{}, fmt.Errorf("vertical: target_cpu %q: %w", abs, err)
		}
		desired = clampQuantity(q, a.minCPU, a.maxCPU)
	} else {
		factor := paramFloat(params, "scale_factor", 1.0)
		if factor <= 0 {
			return actuators.ActionResult{}, fmt.Errorf("vertical: scale_factor must be > 0, got %v", factor)
		}
		desired = scaleQuantity(current, factor, a.minCPU, a.maxCPU)
	}

	if desired.Cmp(current) == 0 {
		return actuators.ActionResult{
			Applied: false,
			Reason:  fmt.Sprintf("no-op (already %s, clamped or unchanged)", current.String()),
			Before:  map[string]any{"cpu": current.String()},
			After:   map[string]any{"cpu": current.String()},
		}, nil
	}

	// First-action baseline annotation (best-effort).
	if _, has := pod.Annotations[actuators.AnnVictimCPULimitBaseline]; !has {
		if err := a.patchPodAnnotations(ctx, pod.Namespace, pod.Name, map[string]string{
			actuators.AnnVictimCPULimitBaseline: current.String(),
		}); err != nil {
			a.logger.Warn("vertical: baseline annotation failed", "pod", pod.Name, "err", err)
		}
	}

	if err := a.patchResize(ctx, pod, target.Spec.ContainerName, desired); err != nil {
		return actuators.ActionResult{}, fmt.Errorf("vertical: patch /resize: %w", err)
	}
	// Observe status non-blockingly; surfaced via logs only.
	go a.observeResizeStatus(context.Background(), pod.Namespace, pod.Name)

	return actuators.ActionResult{
		Applied: true,
		Reason:  "resized",
		Before:  map[string]any{"cpu": current.String()},
		After:   map[string]any{"cpu": desired.String()},
	}, nil
}

// Restore re-patches the pod to its mitigation/cpu-limit-baseline value, if any.
func (a *Actuator) Restore(ctx context.Context, target actuators.Target) error {
	if target.Spec == nil || target.PodName == "" {
		return fmt.Errorf("vertical.Restore: missing target spec or pod name")
	}
	pod, err := a.client.CoreV1().Pods(target.Spec.Namespace).Get(ctx, target.PodName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("vertical.Restore: get pod: %w", err)
	}
	if pod.Spec.NodeName != "" && pod.Spec.NodeName != a.nodeName {
		return nil
	}
	raw := pod.Annotations[actuators.AnnVictimCPULimitBaseline]
	if raw == "" {
		return nil
	}
	q, err := resource.ParseQuantity(raw)
	if err != nil {
		return fmt.Errorf("vertical.Restore: bad baseline %q: %w", raw, err)
	}
	if err := a.patchResize(ctx, pod, target.Spec.ContainerName, q); err != nil {
		return fmt.Errorf("vertical.Restore: patch /resize: %w", err)
	}
	if err := a.patchPodAnnotations(ctx, pod.Namespace, pod.Name, map[string]string{
		actuators.AnnVictimCPULimitBaseline: "",
	}); err != nil {
		a.logger.Warn("vertical.Restore: clear baseline annotation failed",
			"pod", pod.Name, "err", err)
	}
	a.logger.Info("vertical: restored", "pod", pod.Name, "to", q.String())
	return nil
}

// Reconcile is a no-op: pods/resize is a single atomic patch and the
// kubelet drives the actual cgroup write. We could verify that the live
// container's CPU limits match the spec here; deferred.
func (a *Actuator) Reconcile(_ context.Context) error { return nil }

// patchResize uses a strategic merge patch against the pods/resize
// subresource. Body sets requests == limits to preserve Guaranteed QoS.
// Lifted from cmd/vertical-scaler/main.go.
func (a *Actuator) patchResize(ctx context.Context, pod *corev1.Pod, containerName string, cpu resource.Quantity) error {
	type containerPatch struct {
		Name      string `json:"name"`
		Resources struct {
			Requests map[string]string `json:"requests"`
			Limits   map[string]string `json:"limits"`
		} `json:"resources"`
	}
	type podPatch struct {
		Spec struct {
			Containers []containerPatch `json:"containers"`
		} `json:"spec"`
	}
	cp := containerPatch{Name: containerName}
	cp.Resources.Requests = map[string]string{"cpu": cpu.String()}
	cp.Resources.Limits = map[string]string{"cpu": cpu.String()}
	var body podPatch
	body.Spec.Containers = []containerPatch{cp}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	_, err = a.client.CoreV1().Pods(pod.Namespace).Patch(
		ctx, pod.Name, types.StrategicMergePatchType, raw,
		metav1.PatchOptions{}, "resize",
	)
	return err
}

// observeResizeStatus polls the pod for ~5s after a resize patch, watching
// for PodResizePending / PodResizeInProgress conditions and logging
// Infeasible verdicts so operators can spot impossible requests.
//
// Unlike the v1 binary, we do NOT maintain a per-pod infeasible backoff:
// the policy engine's rule cooldown serves the same role. If that proves
// too coarse in practice, add a per-target backoff here.
func (a *Actuator) observeResizeStatus(ctx context.Context, namespace, podName string) {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
		pod, err := a.client.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return
			}
			continue
		}
		for _, cond := range pod.Status.Conditions {
			switch string(cond.Type) {
			case "PodResizePending":
				if cond.Reason == "Infeasible" {
					a.logger.Warn("vertical: resize infeasible",
						"pod", podName, "message", cond.Message)
					return
				}
				a.logger.Info("vertical: resize pending",
					"pod", podName, "reason", cond.Reason, "message", cond.Message)
			case "PodResizeInProgress":
				if cond.Reason == "Error" {
					a.logger.Warn("vertical: resize error",
						"pod", podName, "message", cond.Message)
					return
				}
			}
		}
	}
}

func (a *Actuator) patchPodAnnotations(ctx context.Context, ns, name string, ann map[string]string) error {
	patch := map[string]any{
		"metadata": map[string]any{"annotations": ann},
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	_, err = a.client.CoreV1().Pods(ns).Patch(ctx, name, types.MergePatchType, body, metav1.PatchOptions{})
	return err
}

func findContainer(pod *corev1.Pod, name string) *corev1.Container {
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == name {
			return &pod.Spec.Containers[i]
		}
	}
	return nil
}

// scaleQuantity multiplies cur by factor with rounding, clamped to [min, max].
// Returns a milli-CPU DecimalSI quantity. Lifted from cmd/vertical-scaler/main.go.
func scaleQuantity(cur resource.Quantity, factor float64, min, max resource.Quantity) resource.Quantity {
	curMilli := cur.MilliValue()
	target := int64(math.Round(float64(curMilli) * factor))
	if target < min.MilliValue() {
		target = min.MilliValue()
	}
	if target > max.MilliValue() {
		target = max.MilliValue()
	}
	return *resource.NewMilliQuantity(target, resource.DecimalSI)
}

func clampQuantity(q, min, max resource.Quantity) resource.Quantity {
	v := q.MilliValue()
	if v < min.MilliValue() {
		v = min.MilliValue()
	}
	if v > max.MilliValue() {
		v = max.MilliValue()
	}
	return *resource.NewMilliQuantity(v, resource.DecimalSI)
}

func paramFloat(params map[string]any, key string, def float64) float64 {
	if v, ok := params[key]; ok {
		switch x := v.(type) {
		case float64:
			return x
		case float32:
			return float64(x)
		case int:
			return float64(x)
		case int64:
			return float64(x)
		}
	}
	return def
}
