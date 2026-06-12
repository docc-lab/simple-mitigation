// vertical-scaler watches all configured victim targets and patches the
// pods/resize subresource (K8s 1.33+ beta, 1.35+ GA) to grow or shrink CPU
// requests + limits in response to sustained contention. Keeps
// requests == limits so the victim retains Guaranteed QoS.
//
// One Deployment of this binary handles N victim targets via a ConfigMap;
// adding a target is a ConfigMap edit, not a redeploy. Each (target, pod)
// has its own thresholder so cooldowns don't bleed between pods.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coding-workspace/simple-mitigation-1/pkg/podwatch"
	"github.com/coding-workspace/simple-mitigation-1/pkg/scoreclient"
	"github.com/coding-workspace/simple-mitigation-1/pkg/targets"
	"github.com/coding-workspace/simple-mitigation-1/pkg/thresholder"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Defaults documented in the plan (Section 6).
const (
	defaultHI                   = 0.5
	defaultLO                   = 0.2
	defaultMinHoldWin           = 5
	defaultCooldownSec          = 60
	defaultMinCPU               = "200m"
	defaultMaxCPU               = "4"
	defaultScaleUpFactor        = 1.5
	defaultScaleDownFactor      = 0.75
	defaultStaleMs              = 1500
	defaultDownscaleBlock       = 0.5
	defaultInfeasibleBackoffSec = 60
	defaultTargetsPath          = "/etc/mitigation/targets.yaml"
	defaultTickMs               = 100
)

type globalConfig struct {
	hi                float64
	lo                float64
	minHold           int
	cooldown          time.Duration
	minCPU            resource.Quantity
	maxCPU            resource.Quantity
	upFactor          float64
	downFactor        float64
	staleMs           int64
	downscaleBlock    float64
	infeasibleBackoff time.Duration
	targetsPath       string
	tick              time.Duration
}

func loadGlobalConfig() (*globalConfig, error) {
	minCPU, err := resource.ParseQuantity(envStr("MIN_CPU", defaultMinCPU))
	if err != nil {
		return nil, fmt.Errorf("MIN_CPU: %w", err)
	}
	maxCPU, err := resource.ParseQuantity(envStr("MAX_CPU", defaultMaxCPU))
	if err != nil {
		return nil, fmt.Errorf("MAX_CPU: %w", err)
	}
	cfg := &globalConfig{
		hi:                envFloat("HI", defaultHI),
		lo:                envFloat("LO", defaultLO),
		minHold:           envInt("MIN_HOLD_WINDOWS", defaultMinHoldWin),
		cooldown:          time.Duration(envInt("COOLDOWN_SEC", defaultCooldownSec)) * time.Second,
		minCPU:            minCPU,
		maxCPU:            maxCPU,
		upFactor:          envFloat("SCALE_UP_FACTOR", defaultScaleUpFactor),
		downFactor:        envFloat("SCALE_DOWN_FACTOR", defaultScaleDownFactor),
		staleMs:           int64(envInt("STALE_MS", defaultStaleMs)),
		downscaleBlock:    envFloat("DOWNSCALE_BLOCK_TAIL", defaultDownscaleBlock),
		infeasibleBackoff: time.Duration(envInt("INFEASIBLE_BACKOFF_SEC", defaultInfeasibleBackoffSec)) * time.Second,
		targetsPath:       envStr("TARGETS_CONFIG", defaultTargetsPath),
		tick:              time.Duration(envInt("TICK_MS", defaultTickMs)) * time.Millisecond,
	}
	if cfg.upFactor <= 1 {
		return nil, fmt.Errorf("SCALE_UP_FACTOR must be > 1, got %v", cfg.upFactor)
	}
	if cfg.downFactor <= 0 || cfg.downFactor >= 1 {
		return nil, fmt.Errorf("SCALE_DOWN_FACTOR must be in (0,1), got %v", cfg.downFactor)
	}
	if cfg.minCPU.Cmp(cfg.maxCPU) > 0 {
		return nil, fmt.Errorf("MIN_CPU (%s) must be <= MAX_CPU (%s)", cfg.minCPU.String(), cfg.maxCPU.String())
	}
	return cfg, nil
}

func envStr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envFloat(k string, def float64) float64 {
	if v := os.Getenv(k); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	gc, err := loadGlobalConfig()
	if err != nil {
		logger.Error("config", "err", err)
		os.Exit(2)
	}
	tcfg, err := targets.Load(gc.targetsPath)
	if err != nil {
		logger.Error("targets", "err", err)
		os.Exit(2)
	}
	logger.Info("loaded targets", "count", len(tcfg.Targets), "path", gc.targetsPath,
		"hi", gc.hi, "lo", gc.lo, "min_hold_windows", gc.minHold, "cooldown", gc.cooldown,
		"min_cpu", gc.minCPU.String(), "max_cpu", gc.maxCPU.String(),
		"scale_up_factor", gc.upFactor, "scale_down_factor", gc.downFactor,
		"downscale_block_tail", gc.downscaleBlock,
		"infeasible_backoff", gc.infeasibleBackoff)

	kclient, err := newKubeClient()
	if err != nil {
		logger.Error("kube client", "err", err)
		os.Exit(2)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	pool := scoreclient.NewPool(scoreclient.SubReq{MinP50Trend: 0}, logger)
	defer pool.Close()

	var wg sync.WaitGroup
	for i := range tcfg.Targets {
		target := &tcfg.Targets[i]
		if target.ContainerName == "" {
			logger.Error("target missing containerName; skipping", "target", target.Name)
			continue
		}
		watcher, err := podwatch.NewWatcher(kclient, target)
		if err != nil {
			logger.Error("watcher", "target", target.Name, "err", err)
			continue
		}
		c := &controller{
			target:  target,
			gc:      gc,
			kclient: kclient,
			pool:    pool,
			logger:  logger.With("target", target.Name),
			pods:    map[string]*podState{},
		}
		wg.Add(1)
		go func() { defer wg.Done(); c.run(ctx, watcher) }()
	}
	wg.Wait()
}

func newKubeClient() (kubernetes.Interface, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		kc := os.Getenv("KUBECONFIG")
		if kc == "" {
			home, _ := os.UserHomeDir()
			kc = home + "/.kube/config"
		}
		cfg, err = clientcmd.BuildConfigFromFlags("", kc)
		if err != nil {
			return nil, err
		}
	}
	return kubernetes.NewForConfig(cfg)
}

type controller struct {
	target  *targets.Target
	gc      *globalConfig
	kclient kubernetes.Interface
	pool    *scoreclient.Pool
	logger  *slog.Logger

	mu   sync.Mutex
	pods map[string]*podState
}

// podState holds per-pod thresholder + a backoff for "Infeasible" resize
// responses. infeasibleUntilNs is read by the eval loop and written by
// observeResizeStatus, both potentially on different goroutines, so it lives
// in an atomic.
type podState struct {
	name              string
	namespace         string
	ip                string
	th                *thresholder.Thresholder
	infeasibleUntilNs atomic.Int64
}

func (c *controller) run(ctx context.Context, watcher *podwatch.Watcher) {
	go func() {
		if err := watcher.Run(ctx); err != nil {
			c.logger.Error("watcher exited", "err", err)
		}
	}()
	go c.evalLoop(ctx)

	for {
		select {
		case <-ctx.Done():
			watcher.Stop()
			return
		case ev := <-watcher.Events():
			c.handleEvent(ctx, ev)
		}
	}
}

func (c *controller) handleEvent(ctx context.Context, ev podwatch.Event) {
	key := scoreclient.Key{Target: c.target.Name, Pod: ev.PodName}
	switch ev.Type {
	case podwatch.EventDelete:
		c.mu.Lock()
		delete(c.pods, ev.PodName)
		c.mu.Unlock()
		c.pool.Remove(key)
		c.logger.Info("pod removed", "pod", ev.PodName)
	case podwatch.EventAdd, podwatch.EventUpdate:
		if ev.PodIP == "" || !ev.Ready {
			c.mu.Lock()
			if _, ok := c.pods[ev.PodName]; ok {
				delete(c.pods, ev.PodName)
				c.pool.Remove(key)
				c.logger.Info("pod no longer ready", "pod", ev.PodName)
			}
			c.mu.Unlock()
			return
		}
		c.mu.Lock()
		st, ok := c.pods[ev.PodName]
		if !ok {
			th, err := thresholder.New(thresholder.Config{
				HI: c.gc.hi, LO: c.gc.lo,
				MinHoldWindows: c.gc.minHold,
				Cooldown:       c.gc.cooldown,
			})
			if err != nil {
				c.mu.Unlock()
				c.logger.Error("thresholder", "pod", ev.PodName, "err", err)
				return
			}
			st = &podState{name: ev.PodName, namespace: c.target.Namespace, ip: ev.PodIP, th: th}
			c.pods[ev.PodName] = st
		} else {
			st.ip = ev.PodIP
		}
		c.mu.Unlock()
		addr := fmt.Sprintf("%s:%d", ev.PodIP, c.target.ScorePort)
		c.pool.Add(ctx, key, addr)
		c.logger.Info("pod tracked", "pod", ev.PodName, "addr", addr)
	}
}

// evalLoop drives the per-pod thresholder at TICK_MS cadence. Actions are
// dispatched to a goroutine to avoid blocking sibling pods on a slow API call.
func (c *controller) evalLoop(ctx context.Context) {
	t := time.NewTicker(c.gc.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			c.tick(ctx, now)
		}
	}
}

func (c *controller) tick(ctx context.Context, now time.Time) {
	stale := time.Duration(c.gc.staleMs) * time.Millisecond
	snaps := c.pool.LatestForTarget(c.target.Name, stale)
	byPod := make(map[string]scoreclient.Snapshot, len(snaps))
	for _, s := range snaps {
		byPod[s.Pod] = s
	}

	c.mu.Lock()
	type action struct {
		st         *podState
		dir        thresholder.Direction
		p50, tail  float64
	}
	var acts []action
	for _, st := range c.pods {
		snap, ok := byPod[st.name]
		if !ok || !snap.Healthy || snap.Event == nil {
			continue
		}
		if iu := st.infeasibleUntilNs.Load(); iu > 0 && now.UnixNano() < iu {
			continue
		}
		p50 := float64(snap.Event.P50TrendPred)
		tail := float64(snap.Event.TailTrendLabel)
		dir, act := st.th.Observe(p50, now)
		if act {
			acts = append(acts, action{st: st, dir: dir, p50: p50, tail: tail})
		}
	}
	c.mu.Unlock()

	for _, a := range acts {
		go c.actOnPod(ctx, a.st, a.dir, a.p50, a.tail)
	}
}

func (c *controller) actOnPod(ctx context.Context, st *podState, dir thresholder.Direction, p50, tail float64) {
	pod, err := c.kclient.CoreV1().Pods(st.namespace).Get(ctx, st.name, metav1.GetOptions{})
	if err != nil {
		c.logger.Warn("get pod failed", "pod", st.name, "err", err)
		return
	}
	var container *corev1.Container
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == c.target.ContainerName {
			container = &pod.Spec.Containers[i]
			break
		}
	}
	if container == nil {
		c.logger.Error("container not found in pod spec",
			"pod", st.name, "container", c.target.ContainerName)
		return
	}
	currentCPU, ok := container.Resources.Limits[corev1.ResourceCPU]
	if !ok {
		c.logger.Error("container has no CPU limit; cannot compute new value",
			"pod", st.name, "container", c.target.ContainerName)
		return
	}

	var factor float64
	switch dir {
	case thresholder.Up:
		factor = c.gc.upFactor
	case thresholder.Down:
		if tail > c.gc.downscaleBlock {
			c.logger.Info("downscale blocked by tail safety",
				"pod", st.name, "tail", tail, "tail_block_above", c.gc.downscaleBlock)
			return
		}
		factor = c.gc.downFactor
	default:
		return
	}
	newQ := scaleQuantity(currentCPU, factor, c.gc.minCPU, c.gc.maxCPU)
	if newQ.Cmp(currentCPU) == 0 {
		c.logger.Info("resize no-op (clamped at bound)",
			"pod", st.name, "current", currentCPU.String(), "factor", factor)
		return
	}
	if err := c.patchResize(ctx, pod, c.target.ContainerName, newQ); err != nil {
		c.logger.Warn("resize patch failed",
			"pod", st.name, "err", err)
		return
	}
	c.logger.Info("resize patched",
		"pod", st.name, "container", c.target.ContainerName,
		"from", currentCPU.String(), "to", newQ.String(),
		"dir", dir.String(), "p50", p50, "tail", tail, "factor", factor)
	c.observeResizeStatus(ctx, st)
}

// scaleQuantity multiplies cur by factor with rounding, clamped to [min, max].
// Returns a milli-CPU DecimalSI quantity.
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

// patchResize uses a strategic merge patch against the pods/resize subresource.
// The body sets requests == limits to preserve Guaranteed QoS.
func (c *controller) patchResize(ctx context.Context, pod *corev1.Pod, containerName string, cpu resource.Quantity) error {
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
	_, err = c.kclient.CoreV1().Pods(pod.Namespace).Patch(
		ctx,
		pod.Name,
		types.StrategicMergePatchType,
		raw,
		metav1.PatchOptions{},
		"resize",
	)
	return err
}

// observeResizeStatus polls the pod for up to 5s after a resize patch,
// looking for PodResizePending / PodResizeInProgress conditions. On Infeasible
// it sets a per-pod backoff so we don't spam the API. Condition type names are
// referenced as strings to stay forward-compatible across client-go versions
// that may not yet have typed constants for them.
func (c *controller) observeResizeStatus(ctx context.Context, st *podState) {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
		pod, err := c.kclient.CoreV1().Pods(st.namespace).Get(ctx, st.name, metav1.GetOptions{})
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
					until := time.Now().Add(c.gc.infeasibleBackoff)
					st.infeasibleUntilNs.Store(until.UnixNano())
					c.logger.Warn("resize infeasible; backing off",
						"pod", st.name, "until", until,
						"message", cond.Message)
					return
				}
				c.logger.Info("resize pending",
					"pod", st.name, "reason", cond.Reason, "message", cond.Message)
			case "PodResizeInProgress":
				if cond.Reason == "Error" {
					c.logger.Warn("resize error",
						"pod", st.name, "message", cond.Message)
					return
				}
			}
		}
	}
}
