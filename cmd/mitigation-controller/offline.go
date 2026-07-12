// Offline mode: drive the real controller pipeline (scoreclient pool ->
// features window -> CEL policy -> dispatch) from static score-stream
// addresses instead of podwatch discovery, with dry-run actuators instead
// of K8s/cgroup writes. Activated by OFFLINE_ADDRS, e.g.
//
//	OFFLINE_ADDRS="search=127.0.0.1:7900" \
//	TARGETS_CONFIG=sim/offline/targets.yaml \
//	POLICY_CONFIG=sim/offline/policy.yaml \
//	NODE_NAME=offline ./mitigation-controller
//
// Pair with cmd/score-replay serving a captured trace on that address.
// Everything between the gRPC subscribe and the actuator boundary is the
// exact production code path.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/coding-workspace/simple-mitigation-1/pkg/actuators"
	"github.com/coding-workspace/simple-mitigation-1/pkg/features"
	"github.com/coding-workspace/simple-mitigation-1/pkg/policy"
	"github.com/coding-workspace/simple-mitigation-1/pkg/scoreclient"
	"github.com/coding-workspace/simple-mitigation-1/pkg/targets"
)

// dryRunActuator satisfies actuators.Actuator for any kind; Apply/Restore
// only log. Applied=true so dispatch logs look like production ones.
type dryRunActuator struct {
	kind   string
	logger *slog.Logger
}

func (d *dryRunActuator) Name() string { return d.kind }

func (d *dryRunActuator) Apply(_ context.Context, target actuators.Target, params map[string]any) (actuators.ActionResult, error) {
	return actuators.ActionResult{
		Applied: true,
		Reason:  "offline dry-run",
		After:   params,
	}, nil
}

func (d *dryRunActuator) Restore(_ context.Context, target actuators.Target) error {
	d.logger.Info("dry-run restore", "kind", d.kind, "pod", target.PodName)
	return nil
}

func (d *dryRunActuator) Reconcile(context.Context) error { return nil }

// parseOfflineAddrs parses "search=127.0.0.1:7900,profile=127.0.0.1:7901".
func parseOfflineAddrs(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, addr, ok := strings.Cut(part, "=")
		if !ok || name == "" || addr == "" {
			return nil, fmt.Errorf("OFFLINE_ADDRS: bad entry %q (want target=host:port)", part)
		}
		out[name] = addr
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("OFFLINE_ADDRS: no entries")
	}
	return out, nil
}

// runOffline is the OFFLINE_ADDRS counterpart of the per-target goroutines
// in main(). One controller per addressed target; pods are synthetic
// ("<target>-offline") and never change.
func runOffline(ctx context.Context, cfg *runtimeConfig, tcfg *targets.Config,
	engine *policy.Engine, logger *slog.Logger, addrs map[string]string) {

	registry := map[string]actuators.Actuator{}
	for _, kind := range []string{"isolate", "vertical", "horizontal", "harvest"} {
		registry[kind] = &dryRunActuator{kind: kind, logger: logger}
	}

	pool := scoreclient.NewPool(scoreclient.SubReq{MinP50Trend: 0}, logger)
	defer pool.Close()

	var wg sync.WaitGroup
	for i := range tcfg.Targets {
		target := &tcfg.Targets[i]
		addr, ok := addrs[target.Name]
		if !ok {
			logger.Info("offline: target has no OFFLINE_ADDRS entry; skipped",
				"target", target.Name)
			continue
		}
		podName := target.Name + "-offline"
		c := &controller{
			cfg:      cfg,
			target:   target,
			pool:     pool,
			engine:   engine,
			registry: registry,
			logger:   logger.With("target", target.Name, "mode", "offline"),
			windows:  map[string]*features.Window{podName: features.NewWindow(cfg.windowSize)},
			nodeByPod: map[string]string{
				podName: cfg.nodeName,
			},
		}
		pool.Add(ctx, scoreclient.Key{Target: target.Name, Pod: podName}, addr)
		c.logger.Info("offline target wired", "pod", podName, "addr", addr)

		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(cfg.tick)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case now := <-ticker.C:
					c.tick(ctx, now)
				}
			}
		}()
	}
	wg.Wait()
	logger.Info("offline mode shutting down")
}
