// mitigation-controller is the single-binary DaemonSet that replaces the
// v1 horizontal-cpa-sidecar + vertical-scaler + CPA stack. One instance
// runs per node; each instance:
//
//   - subscribes only to victim pods on its own node (NODE_NAME field
//     selector via pkg/podwatch.NewLocalNodeWatcher)
//   - maintains a rolling window of ScoreEvents per (target, pod) and
//     builds a FeatureVector each TICK_MS
//   - evaluates a CEL policy (loaded from a hot-reloaded ConfigMap) against
//     each FeatureVector and dispatches the matching actions to one of
//     three actuators: isolate (cgroup), vertical (pods/resize), or
//     horizontal (deployments/scale)
//
// Coordination between node-instances for horizontal scale is the
// Deployment's `mitigation/horizontal-last-scaled-at` annotation; the
// actuator skips a patch if another instance fired recently.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/coding-workspace/simple-mitigation-1/pkg/actuators"
	"github.com/coding-workspace/simple-mitigation-1/pkg/actuators/horizontal"
	"github.com/coding-workspace/simple-mitigation-1/pkg/actuators/isolate"
	"github.com/coding-workspace/simple-mitigation-1/pkg/actuators/vertical"
	"github.com/coding-workspace/simple-mitigation-1/pkg/features"
	"github.com/coding-workspace/simple-mitigation-1/pkg/podwatch"
	"github.com/coding-workspace/simple-mitigation-1/pkg/policy"
	"github.com/coding-workspace/simple-mitigation-1/pkg/scoreclient"
	"github.com/coding-workspace/simple-mitigation-1/pkg/targets"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Defaults per plan-v2-centralized.md Sections 4 / 5 / 11.
const (
	defaultTargetsPath        = "/etc/mitigation/targets.yaml"
	defaultPolicyPath         = "/etc/mitigation/policy.yaml"
	defaultTickMs             = 100
	defaultStaleMs            = 1500
	defaultWindowSize         = 20
	defaultHiThreshold        = 0.5
	defaultMinCPU             = "200m"
	defaultMaxCPU             = "4"
	defaultHorizontalCooldown = 30 * time.Second
)

type runtimeConfig struct {
	nodeName              string
	targetsPath           string
	policyPath            string
	tick                  time.Duration
	stale                 time.Duration
	windowSize            int
	hiThreshold           float64
	minCPU                resource.Quantity
	maxCPU                resource.Quantity
	horizontalCooldown    time.Duration
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := loadConfig()
	if err != nil {
		logger.Error("config", "err", err)
		os.Exit(2)
	}
	logger.Info("starting mitigation-controller",
		"node", cfg.nodeName,
		"targets_path", cfg.targetsPath,
		"policy_path", cfg.policyPath,
		"tick", cfg.tick, "stale", cfg.stale,
		"window_size", cfg.windowSize,
		"hi_threshold", cfg.hiThreshold,
		"min_cpu", cfg.minCPU.String(), "max_cpu", cfg.maxCPU.String(),
		"horizontal_cooldown", cfg.horizontalCooldown,
	)

	tcfg, err := targets.Load(cfg.targetsPath)
	if err != nil {
		logger.Error("targets", "err", err)
		os.Exit(2)
	}
	doc, err := policy.LoadFile(cfg.policyPath)
	if err != nil {
		logger.Error("policy", "err", err)
		os.Exit(2)
	}
	engine, err := policy.NewEngine(doc, logger)
	if err != nil {
		logger.Error("policy engine", "err", err)
		os.Exit(2)
	}

	kclient, err := newKubeClient()
	if err != nil {
		logger.Error("kube client", "err", err)
		os.Exit(2)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Build actuators.
	verticalAct, err := vertical.New(kclient, vertical.Config{
		NodeName: cfg.nodeName,
		MinCPU:   cfg.minCPU,
		MaxCPU:   cfg.maxCPU,
	}, logger)
	if err != nil {
		logger.Error("vertical actuator", "err", err)
		os.Exit(2)
	}
	horizontalAct := horizontal.New(kclient, cfg.nodeName, cfg.horizontalCooldown, logger)
	isolateAct, err := isolate.New(kclient, cfg.nodeName, nil, logger)
	if err != nil {
		logger.Error("isolate actuator", "err", err)
		os.Exit(2)
	}
	registry := map[string]actuators.Actuator{
		verticalAct.Name():   verticalAct,
		horizontalAct.Name(): horizontalAct,
		isolateAct.Name():    isolateAct,
	}
	// Run startup reconcile before the first tick.
	for name, act := range registry {
		if err := act.Reconcile(ctx); err != nil {
			logger.Warn("reconcile failed; continuing", "actuator", name, "err", err)
		}
	}

	// Hot-reload policy on file change.
	go func() {
		if err := policy.Watch(ctx, cfg.policyPath, logger, func(doc *policy.Document) {
			if err := engine.Reload(doc); err != nil {
				logger.Warn("policy: reload rejected", "err", err)
			}
		}); err != nil {
			logger.Warn("policy watcher exited", "err", err)
		}
	}()

	// Shared score pool across all targets.
	pool := scoreclient.NewPool(scoreclient.SubReq{MinP50Trend: 0}, logger)
	defer pool.Close()

	var wg sync.WaitGroup
	for i := range tcfg.Targets {
		target := &tcfg.Targets[i]
		c := &controller{
			cfg:      cfg,
			target:   target,
			kclient:  kclient,
			pool:     pool,
			engine:   engine,
			registry: registry,
			logger:   logger.With("target", target.Name),
			windows:  map[string]*features.Window{},
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.run(ctx)
		}()
	}

	wg.Wait()
	logger.Info("mitigation-controller shutting down")
}

func loadConfig() (*runtimeConfig, error) {
	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		return nil, fmt.Errorf("NODE_NAME env is required (set via downward API)")
	}
	minQ, err := resource.ParseQuantity(envStr("MIN_CPU", defaultMinCPU))
	if err != nil {
		return nil, fmt.Errorf("MIN_CPU: %w", err)
	}
	maxQ, err := resource.ParseQuantity(envStr("MAX_CPU", defaultMaxCPU))
	if err != nil {
		return nil, fmt.Errorf("MAX_CPU: %w", err)
	}
	cooldownSec := envInt("HORIZONTAL_COOLDOWN_SEC", int(defaultHorizontalCooldown/time.Second))
	return &runtimeConfig{
		nodeName:           nodeName,
		targetsPath:        envStr("TARGETS_CONFIG", defaultTargetsPath),
		policyPath:         envStr("POLICY_CONFIG", defaultPolicyPath),
		tick:               time.Duration(envInt("TICK_MS", defaultTickMs)) * time.Millisecond,
		stale:              time.Duration(envInt("STALE_MS", defaultStaleMs)) * time.Millisecond,
		windowSize:         envInt("WINDOW_SIZE", defaultWindowSize),
		hiThreshold:        envFloat("HI_THRESHOLD", defaultHiThreshold),
		minCPU:             minQ,
		maxCPU:             maxQ,
		horizontalCooldown: time.Duration(cooldownSec) * time.Second,
	}, nil
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

// controller runs the per-target loop: maintain pod-watch subscriptions,
// push events into per-pod windows, tick the policy engine, dispatch.
type controller struct {
	cfg      *runtimeConfig
	target   *targets.Target
	kclient  kubernetes.Interface
	pool     *scoreclient.Pool
	engine   *policy.Engine
	registry map[string]actuators.Actuator
	logger   *slog.Logger

	mu      sync.Mutex
	windows map[string]*features.Window // pod name -> window
	// nodeByPod remembers the local node for each pod so dispatch can stamp
	// Target.NodeName without re-listing. All entries are the controller's
	// own NODE_NAME (informer is field-selected) but keeping a map keeps the
	// data flow symmetric with v1.
	nodeByPod map[string]string
}

func (c *controller) run(ctx context.Context) {
	watcher, err := podwatch.NewLocalNodeWatcher(c.kclient, c.target, c.cfg.nodeName)
	if err != nil {
		c.logger.Error("watcher", "err", err)
		return
	}
	go func() {
		if err := watcher.Run(ctx); err != nil {
			c.logger.Error("watcher exited", "err", err)
		}
	}()

	// Discovery loop: keep pool + windows in sync with podwatch.
	go c.discoveryLoop(ctx, watcher)

	// Tick loop: build features and evaluate per-pod.
	ticker := time.NewTicker(c.cfg.tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			watcher.Stop()
			return
		case now := <-ticker.C:
			c.tick(ctx, now)
		}
	}
}

func (c *controller) discoveryLoop(ctx context.Context, w *podwatch.Watcher) {
	if c.nodeByPod == nil {
		c.mu.Lock()
		c.nodeByPod = map[string]string{}
		c.mu.Unlock()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.Events():
			if !ok {
				return
			}
			key := scoreclient.Key{Target: c.target.Name, Pod: ev.PodName}
			switch ev.Type {
			case podwatch.EventDelete:
				c.pool.Remove(key)
				c.mu.Lock()
				if win, ok := c.windows[ev.PodName]; ok {
					win.Reset()
					delete(c.windows, ev.PodName)
				}
				delete(c.nodeByPod, ev.PodName)
				c.mu.Unlock()
				c.logger.Info("pod removed", "pod", ev.PodName)
			case podwatch.EventAdd, podwatch.EventUpdate:
				if ev.PodIP == "" || !ev.Ready {
					c.pool.Remove(key)
					c.mu.Lock()
					if win, ok := c.windows[ev.PodName]; ok {
						win.Reset()
					}
					c.mu.Unlock()
					continue
				}
				addr := fmt.Sprintf("%s:%d", ev.PodIP, c.target.ScorePort)
				c.pool.Add(ctx, key, addr)
				c.mu.Lock()
				if _, ok := c.windows[ev.PodName]; !ok {
					c.windows[ev.PodName] = features.NewWindow(c.cfg.windowSize)
				}
				c.nodeByPod[ev.PodName] = ev.NodeName
				c.mu.Unlock()
				c.logger.Info("pod tracked", "pod", ev.PodName, "addr", addr)
			}
		}
	}
}

func (c *controller) tick(ctx context.Context, now time.Time) {
	snaps := c.pool.LatestForTarget(c.target.Name, c.cfg.stale)
	c.mu.Lock()
	type job struct {
		podName  string
		nodeName string
		fv       features.FeatureVector
	}
	var jobs []job
	for _, snap := range snaps {
		if !snap.Healthy || snap.Event == nil {
			continue
		}
		win, ok := c.windows[snap.Pod]
		if !ok {
			win = features.NewWindow(c.cfg.windowSize)
			c.windows[snap.Pod] = win
		}
		win.Push(snap.Event, now)
		fv := features.Build(snap.Event, win.Snapshot(), features.BuildConfig{
			HiThreshold: c.cfg.hiThreshold,
		})
		fv.Target = c.target.Name
		fv.Pod = snap.Pod
		jobs = append(jobs, job{
			podName:  snap.Pod,
			nodeName: c.nodeByPod[snap.Pod],
			fv:       fv,
		})
	}
	c.mu.Unlock()

	for _, j := range jobs {
		actions := c.engine.Evaluate(j.fv, now)
		if len(actions) == 0 {
			continue
		}
		for _, req := range actions {
			c.dispatch(ctx, req, j.podName, j.nodeName)
		}
	}
}

// dispatch routes one ActionRequest. "restore" is a meta-kind that fans
// out to every actuator's Restore for this target/pod.
func (c *controller) dispatch(ctx context.Context, req policy.ActionRequest, podName, nodeName string) {
	tgt := actuators.Target{
		Spec:     c.target,
		PodName:  podName,
		NodeName: nodeName,
	}
	if req.Kind == "restore" {
		c.restoreAll(ctx, req, tgt)
		return
	}
	act, ok := c.registry[req.Kind]
	if !ok {
		c.logger.Warn("dispatch: unknown action kind",
			"rule", req.RuleName, "kind", req.Kind)
		return
	}
	res, err := act.Apply(ctx, tgt, req.Params)
	attrs := []any{
		"rule", req.RuleName, "kind", req.Kind,
		"pod", podName, "node", nodeName,
		"applied", res.Applied, "reason", res.Reason,
	}
	if res.Before != nil {
		attrs = append(attrs, "before", res.Before)
	}
	if res.After != nil {
		attrs = append(attrs, "after", res.After)
	}
	if err != nil {
		attrs = append(attrs, "err", err)
		c.logger.Warn("action failed", attrs...)
		return
	}
	c.logger.Info("action", attrs...)
}

func (c *controller) restoreAll(ctx context.Context, req policy.ActionRequest, tgt actuators.Target) {
	for name, act := range c.registry {
		if err := act.Restore(ctx, tgt); err != nil {
			c.logger.Warn("restore failed",
				"rule", req.RuleName, "actuator", name,
				"pod", tgt.PodName, "err", err)
			continue
		}
		c.logger.Info("restore",
			"rule", req.RuleName, "actuator", name,
			"pod", tgt.PodName)
	}
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
