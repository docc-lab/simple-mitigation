// horizontal-cpa-sidecar serves the HTTP /metric + /evaluate endpoints that
// the custom-pod-autoscaler (CPA) binary calls every interval. It runs as a
// second container alongside the unmodified CPA container in the same Pod
// (sharing localhost). One CPA Pod is deployed per victim Deployment; the
// sidecar is parametric via env vars and the same image is reused.
//
// At a high level each tick is:
//
//	(victim pods' :scorePort gRPC stream) --100ms--> in-memory snapshot
//	                                                 \
//	                                                  -> aggregated p50_agg
//	                                                     \
//	                                                      -> Thresholder
//	                                                         \
//	                                                          -> targetReplicas
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coding-workspace/simple-mitigation-1/pkg/aggregator"
	"github.com/coding-workspace/simple-mitigation-1/pkg/podwatch"
	"github.com/coding-workspace/simple-mitigation-1/pkg/scoreclient"
	"github.com/coding-workspace/simple-mitigation-1/pkg/targets"
	"github.com/coding-workspace/simple-mitigation-1/pkg/thresholder"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Defaults documented in the plan (Section 5).
const (
	defaultListen       = ":8080"
	defaultHI           = 0.5
	defaultLO           = 0.2
	defaultMinHoldWin   = 3
	defaultCooldownSec  = 30
	defaultMinReplicas  = 1
	defaultMaxReplicas  = 10
	defaultStaleMs      = 1500
	defaultDownscaleBlk = 0.5
	defaultAgg          = "max"
)

type config struct {
	namespace      string
	selector       map[string]string
	scorePort      int
	minReplicas    int32
	maxReplicas    int32
	hi             float64
	lo             float64
	minHold        int
	cooldown       time.Duration
	staleMs        int64
	downscaleBlock float64
	agg            aggregator.Aggregator
	listen         string
}

func loadConfig() (*config, error) {
	c := &config{
		listen:         envStr("LISTEN_ADDR", defaultListen),
		scorePort:      envInt("SCORE_PORT", targets.DefaultScorePort),
		minReplicas:    int32(envInt("MIN_REPLICAS", defaultMinReplicas)),
		maxReplicas:    int32(envInt("MAX_REPLICAS", defaultMaxReplicas)),
		hi:             envFloat("HI", defaultHI),
		lo:             envFloat("LO", defaultLO),
		minHold:        envInt("MIN_HOLD_WINDOWS", defaultMinHoldWin),
		cooldown:       time.Duration(envInt("COOLDOWN_SEC", defaultCooldownSec)) * time.Second,
		staleMs:        int64(envInt("STALE_MS", defaultStaleMs)),
		downscaleBlock: envFloat("DOWNSCALE_BLOCK_TAIL", defaultDownscaleBlk),
	}
	c.namespace = envStr("TARGET_NAMESPACE", "")
	if c.namespace == "" {
		return nil, fmt.Errorf("TARGET_NAMESPACE is required")
	}
	selRaw := envStr("TARGET_SELECTOR", "")
	if selRaw == "" {
		return nil, fmt.Errorf("TARGET_SELECTOR is required (comma-separated key=value list)")
	}
	sel, err := parseSelector(selRaw)
	if err != nil {
		return nil, fmt.Errorf("TARGET_SELECTOR: %w", err)
	}
	c.selector = sel

	agg, err := aggregator.New(envStr("AGG", defaultAgg))
	if err != nil {
		return nil, fmt.Errorf("AGG: %w", err)
	}
	c.agg = agg

	if c.minReplicas < 1 {
		return nil, fmt.Errorf("MIN_REPLICAS must be >= 1, got %d", c.minReplicas)
	}
	if c.maxReplicas < c.minReplicas {
		return nil, fmt.Errorf("MAX_REPLICAS (%d) must be >= MIN_REPLICAS (%d)", c.maxReplicas, c.minReplicas)
	}
	return c, nil
}

func parseSelector(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		kv := strings.SplitN(p, "=", 2)
		if len(kv) != 2 || strings.TrimSpace(kv[0]) == "" {
			return nil, fmt.Errorf("bad pair %q (want key=value)", p)
		}
		out[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no key=value pairs found")
	}
	return out, nil
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

	cfg, err := loadConfig()
	if err != nil {
		logger.Error("config error", "err", err)
		os.Exit(2)
	}
	logger.Info("starting horizontal-cpa-sidecar",
		"namespace", cfg.namespace, "selector", cfg.selector,
		"scorePort", cfg.scorePort,
		"min_replicas", cfg.minReplicas, "max_replicas", cfg.maxReplicas,
		"hi", cfg.hi, "lo", cfg.lo,
		"min_hold_windows", cfg.minHold, "cooldown", cfg.cooldown,
		"agg", cfg.agg.Name(),
		"downscale_block_tail", cfg.downscaleBlock)

	kclient, err := newKubeClient()
	if err != nil {
		logger.Error("kube client", "err", err)
		os.Exit(2)
	}

	target := &targets.Target{
		Name:      "horizontal-target",
		Namespace: cfg.namespace,
		Selector:  cfg.selector,
		ScorePort: cfg.scorePort,
	}
	watcher, err := podwatch.NewWatcher(kclient, target)
	if err != nil {
		logger.Error("watcher", "err", err)
		os.Exit(2)
	}

	pool := scoreclient.NewPool(scoreclient.SubReq{
		// Keep the producer-side filter loose; our LO is the controlling threshold.
		MinP50Trend: 0,
	}, logger)

	th, err := thresholder.New(thresholder.Config{
		HI: cfg.hi, LO: cfg.lo,
		MinHoldWindows: cfg.minHold,
		Cooldown:       cfg.cooldown,
	})
	if err != nil {
		logger.Error("thresholder", "err", err)
		os.Exit(2)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	go func() {
		if err := watcher.Run(ctx); err != nil {
			logger.Error("watcher exited", "err", err)
			cancel()
		}
	}()
	go runDiscoveryLoop(ctx, watcher, pool, target, cfg.scorePort, logger)

	srv := newServer(cfg, pool, th, logger)
	httpSrv := &http.Server{
		Addr:         cfg.listen,
		Handler:      srv,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}
	go func() {
		logger.Info("listening", "addr", cfg.listen)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http server", "err", err)
			cancel()
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")
	shutdownCtx, sc := context.WithTimeout(context.Background(), 5*time.Second)
	defer sc()
	_ = httpSrv.Shutdown(shutdownCtx)
	watcher.Stop()
	pool.Close()
}

// runDiscoveryLoop consumes podwatch events and keeps the scoreclient pool
// in sync with the live set of Ready pod IPs.
func runDiscoveryLoop(ctx context.Context, w *podwatch.Watcher, pool *scoreclient.Pool, target *targets.Target, scorePort int, logger *slog.Logger) {
	known := map[string]string{} // pod name -> pod ip
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-w.Events():
			key := scoreclient.Key{Target: target.Name, Pod: ev.PodName}
			switch ev.Type {
			case podwatch.EventDelete:
				delete(known, ev.PodName)
				pool.Remove(key)
				logger.Info("pod removed", "pod", ev.PodName)
			case podwatch.EventAdd, podwatch.EventUpdate:
				if ev.PodIP == "" || !ev.Ready {
					if known[ev.PodName] != "" {
						delete(known, ev.PodName)
						pool.Remove(key)
						logger.Info("pod no longer ready", "pod", ev.PodName)
					}
					continue
				}
				if known[ev.PodName] == ev.PodIP {
					continue
				}
				known[ev.PodName] = ev.PodIP
				addr := fmt.Sprintf("%s:%d", ev.PodIP, scorePort)
				pool.Add(ctx, key, addr)
				logger.Info("pod tracked", "pod", ev.PodName, "addr", addr)
			}
		}
	}
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

// server implements the CPA HTTP contract.
//
//	GET  /metric    -> JSON metric body (CPA stuffs the response into metrics[0].value as a string)
//	POST /evaluate  -> reads CPA's metric envelope, returns {"targetReplicas": N}
//	GET  /healthz   -> 200 ok for liveness/readiness probes
type server struct {
	cfg    *config
	pool   *scoreclient.Pool
	th     *thresholder.Thresholder
	thMu   sync.Mutex
	logger *slog.Logger
}

func newServer(c *config, pool *scoreclient.Pool, th *thresholder.Thresholder, logger *slog.Logger) http.Handler {
	s := &server{cfg: c, pool: pool, th: th, logger: logger}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metric", s.metric)
	mux.HandleFunc("POST /evaluate", s.evaluate)
	mux.HandleFunc("GET /healthz", s.health)
	return mux
}

// metricPayload is the body returned to CPA's metric stage. CPA will wrap
// the entire response body verbatim into metrics[0].value as a string before
// passing it to /evaluate.
type metricPayload struct {
	P50Agg    float64            `json:"p50_agg"`
	P50PerPod map[string]float64 `json:"p50_per_pod"`
	TailMax   float64            `json:"tail_max"`
	Pods      int                `json:"pods"`
	StaleMs   int64              `json:"stale_ms"`
	AggPolicy string             `json:"agg_policy"`
}

func (s *server) metric(w http.ResponseWriter, _ *http.Request) {
	snaps := s.pool.Latest(time.Duration(s.cfg.staleMs) * time.Millisecond)
	payload := metricPayload{
		P50PerPod: map[string]float64{},
		AggPolicy: s.cfg.agg.Name(),
	}
	p50s := make([]float64, 0, len(snaps))
	var maxAge int64
	for _, sn := range snaps {
		if !sn.Healthy || sn.Event == nil {
			continue
		}
		p50 := float64(sn.Event.P50TrendPred)
		tail := float64(sn.Event.TailTrendLabel)
		p50s = append(p50s, p50)
		payload.P50PerPod[sn.Pod] = p50
		if tail > payload.TailMax {
			payload.TailMax = tail
		}
		if sn.AgeMs > maxAge {
			maxAge = sn.AgeMs
		}
	}
	payload.Pods = len(p50s)
	payload.StaleMs = maxAge
	if len(p50s) > 0 {
		payload.P50Agg = s.cfg.agg.Apply(p50s)
	}
	writeJSON(w, payload)
}

// evaluateEnvelope is the JSON CPA POSTs to /evaluate. Only the fields the
// sidecar needs are decoded; CPA's full schema includes more.
type evaluateEnvelope struct {
	Metrics []struct {
		Resource string `json:"resource"`
		Value    string `json:"value"` // metric stage's body, as a string
	} `json:"metrics"`
	Resource struct {
		Spec struct {
			Replicas int32 `json:"replicas"`
		} `json:"spec"`
	} `json:"resource"`
	RunType string `json:"runType"`
}

type evaluateResp struct {
	TargetReplicas int32 `json:"targetReplicas"`
}

func (s *server) evaluate(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var env evaluateEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		s.logger.Warn("evaluate: bad envelope", "err", err, "body", string(body))
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var payload metricPayload
	if len(env.Metrics) > 0 {
		if err := json.Unmarshal([]byte(env.Metrics[0].Value), &payload); err != nil {
			s.logger.Warn("evaluate: bad metric value", "err", err, "val", env.Metrics[0].Value)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	current := env.Resource.Spec.Replicas
	if current < 1 {
		current = s.cfg.minReplicas
	}

	// Safety: no healthy pods reporting -> no signal -> don't touch replicas.
	// Otherwise the sidecar would observe p50_agg=0 every tick, eventually
	// trip the Down threshold, and scale silently to MIN_REPLICAS on a
	// network blip or a slow pod rollout.
	if payload.Pods == 0 {
		s.logger.Info("evaluate: no healthy pods reporting; holding current replicas",
			"current", current, "stale_ms", payload.StaleMs)
		writeJSON(w, evaluateResp{TargetReplicas: current})
		return
	}

	s.thMu.Lock()
	dir, act := s.th.Observe(payload.P50Agg, time.Now())
	s.thMu.Unlock()

	target := current
	blockedByTail := false
	if act {
		switch dir {
		case thresholder.Up:
			target = current + 1
		case thresholder.Down:
			if payload.TailMax > s.cfg.downscaleBlock {
				blockedByTail = true
				target = current
			} else {
				target = current - 1
			}
		}
	}
	if target < s.cfg.minReplicas {
		target = s.cfg.minReplicas
	}
	if target > s.cfg.maxReplicas {
		target = s.cfg.maxReplicas
	}

	s.logger.Info("evaluate",
		"p50_agg", payload.P50Agg, "tail_max", payload.TailMax,
		"pods_reporting", payload.Pods, "stale_ms", payload.StaleMs,
		"dir", dir.String(), "act", act, "tail_blocked", blockedByTail,
		"current", current, "target", target,
		"agg", payload.AggPolicy)

	writeJSON(w, evaluateResp{TargetReplicas: target})
}

func (s *server) health(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Default().Warn("write json", "err", err)
	}
}
