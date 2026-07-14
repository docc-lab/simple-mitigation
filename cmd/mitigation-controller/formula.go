// Formula mode: the finalized paper control laws, replacing CEL dispatch
// for the two mitigation paths when FORMULA_MODE=1.
//
// Eq. (1) — horizontal bang-bang on the predicted median score ŷ50 with a
// replica counter n(t) ∈ {0..n_max} guarding scale-down:
//
//	          ⎧ +1  if ŷ50(t) > θ_on  ∧  n(t) < n_max
//	u_horz =  ⎨ -1  if y50(t) < θ_off ∧ ŷ50(t) < θ_on ∧ n(t) > 0
//	          ⎩  0  otherwise
//	n(t+1) = min(n_max, max(0, n(t) + u_horz(t)))
//
// Online realization: ŷ50 = p50_trend_pred (the model's prediction when
// prediction is ON; the formula's current score otherwise) and y50 =
// y50_current (dsb wire field 8; falls back to the p50 channel on older
// producers). Both are aggregated to service level as the mean over healthy
// replicas (traffic-weighted aggregation needs per-replica arrival rates
// the stream does not carry).
//
// Eq. (2) — isolation saturated proportional (P-only) on the extrinsic
// tail magnitude e_iso(t) = y90(t)·%ext,90(t):
//
//	c(t) = min(c_base, max(c_min, c_base - k_p·(e_iso(t) - θ_ref)))
//
// e_iso = tail_trend_label × ext_pct_90 (dsb wire field 10; ≡1 on older
// producers). c(t) is the TOTAL core budget for the aggressor set
// on the node: c_base = shareable cores (node CPUs minus the victim's
// share), c_min = the liveness floor (e.g. 1 core per aggressor). The
// isolate actuator divides c(t) evenly across the matched aggressor pods
// (param cap_total_cores). The cap is quantized to CAP_QUANTUM_CORES with
// hysteresis so sensor noise does not turn into a cgroup write per tick.
package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/coding-workspace/simple-mitigation-1/pkg/policy"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Defaults match simulation/simulation.py's flag defaults (the paper's
// reference values). Per-signal tuning is done via env at run time.
type formulaConfig struct {
	thetaOn  float64 // Eq.1 θ_on
	thetaOff float64 // Eq.1 θ_off
	nMax     int     // Eq.1 n_max
	thetaRef float64 // Eq.2 θ_ref
	kP       float64 // Eq.2 k_p (cores per unit score)
	capBase  float64 // Eq.2 c_base (total cores)
	capMin   float64 // Eq.2 c_min (total cores; static lower clamp)
	// minPerPod > 0 makes the floor dynamic in the aggressor count: the
	// actuator never squeezes a pod below this, so the effective aggregate
	// floor is minPerPod x N(matched pods).
	minPerPod float64
	// yhatFromCurrent substitutes y50_current for ŷ50 in Eq.(1)'s scale-up
	// term. Use while the producer's prediction channel is unreliable
	// (equivalent to prediction-off semantics enforced controller-side).
	yhatFromCurrent bool
	// armFile, when set (FORMULA_ARM_FILE), gates ACTUATION on the file's
	// existence: the controller senses, computes, and traces from startup,
	// but dispatches nothing until the file appears. Gives phased
	// experiments a clean unmitigated->mitigated boundary with continuous
	// sensor state. Empty = always armed.
	armFile string
	// Per-pod guarantees for the shared-pool isolation model: every
	// aggressor keeps guaranteeDefault cores (guaranteeHR for pods in
	// guaranteeHRNs); Eq.(2)'s c(t) governs only the discretionary pool on
	// top, so c_min=0 no longer starves anyone.
	guaranteeDefault float64
	guaranteeHR      float64
	guaranteeHRNs    string

	// Actuation plumbing (not part of the formulas).
	capQuantum         float64 // cap resolution: dispatch in steps of this many cores
	capHysteresis      float64 // leave the current step only when the raw cap moved this fraction of a quantum away
	aggressorSelector  string
	aggressorNamespace string
	periodUs           int
}

func loadFormulaConfig() *formulaConfig {
	v := os.Getenv("FORMULA_MODE")
	if v != "1" && v != "true" {
		return nil
	}
	return &formulaConfig{
		thetaOn:            envFloat("THETA_ON", 0.3),
		thetaOff:           envFloat("THETA_OFF", 0.1),
		nMax:               envInt("N_MAX", 10),
		thetaRef:           envFloat("THETA_REF", 0.3),
		kP:                 envFloat("K_P", 6.4),
		capBase:            envFloat("CAP_BASE_CORES", 4.0),
		capMin:             envFloat("CAP_MIN_CORES", 0.5),
		minPerPod:          envFloat("CAP_MIN_PER_POD_CORES", 0),
		yhatFromCurrent:    os.Getenv("YHAT_FROM_CURRENT") == "1",
		armFile:            os.Getenv("FORMULA_ARM_FILE"),
		guaranteeDefault:   envFloat("AGGRESSOR_GUARANTEE_CORES", 0),
		guaranteeHR:        envFloat("AGGRESSOR_GUARANTEE_HR_CORES", 0),
		guaranteeHRNs:      envStr("AGGRESSOR_GUARANTEE_HR_NS", "default"),
		capQuantum:         envFloat("CAP_QUANTUM_CORES", 0.25),
		capHysteresis:      envFloat("CAP_HYSTERESIS_FRACTION", 0.75),
		aggressorSelector:  envStr("AGGRESSOR_SELECTOR", "tier=batch"),
		aggressorNamespace: os.Getenv("AGGRESSOR_NAMESPACE"),
		periodUs:           envInt("ISOLATE_PERIOD_US", 100000),
	}
}

func (f *formulaConfig) logAttrs() []any {
	return []any{
		"theta_on", f.thetaOn, "theta_off", f.thetaOff, "n_max", f.nMax,
		"theta_ref", f.thetaRef, "k_p", f.kP,
		"cap_base", f.capBase, "cap_min", f.capMin,
		"min_per_pod", f.minPerPod,
		"cap_quantum", f.capQuantum,
		"yhat_from_current", f.yhatFromCurrent,
	}
}

// formulaState is per-target controller state. It lives on the controller
// struct, so all access happens from that target's tick goroutine only.
type formulaState struct {
	n       int                // Eq.1 replica counter n(t), extra replicas above baseline
	lastCap map[string]float64 // pod -> last dispatched quantized cap (cores)
	// lastNSync rate-limits the n(t) <-> Deployment reconciliation below.
	lastNSync time.Time
	// armed latches true once the arm file appears (or immediately when no
	// arm file is configured).
	armed bool
}

// checkArmed reports whether actuation is enabled, latching on first sight
// of the arm file so a later file deletion doesn't silently disarm mid-run.
func (c *controller) checkArmed(f *formulaConfig) bool {
	if c.fstate.armed {
		return true
	}
	if f.armFile == "" {
		c.fstate.armed = true
		return true
	}
	if _, err := os.Stat(f.armFile); err == nil {
		c.fstate.armed = true
		c.logger.Info("formula: actuation ARMED", "arm_file", f.armFile)
		return true
	}
	return false
}

// syncN reconciles the commanded counter n(t) with the Deployment's actual
// replica count. Needed because n is open-loop state: an external reset
// (e.g. the experiment harness re-applying the victim manifest between
// runs) changes real replicas without the controller stepping n, after
// which Eq.(1) regulates against a phantom count. Skipped offline (no kube
// client).
func (c *controller) syncN(ctx context.Context, f *formulaConfig) {
	if c.kclient == nil {
		return
	}
	now := time.Now()
	if now.Sub(c.fstate.lastNSync) < 5*time.Second {
		return
	}
	c.fstate.lastNSync = now
	scale, err := c.kclient.AppsV1().Deployments(c.target.Namespace).
		GetScale(ctx, c.target.Name, metav1.GetOptions{})
	if err != nil {
		c.logger.Warn("formula: n resync failed", "err", err)
		return
	}
	baseline := envInt("BASELINE_REPLICAS", 1)
	real := int(scale.Spec.Replicas) - baseline
	if real < 0 {
		real = 0
	}
	if real > f.nMax {
		real = f.nMax
	}
	if real != c.fstate.n {
		c.logger.Info("formula: n resynced to actual replicas",
			"n_was", c.fstate.n, "n_now", real,
			"deployment_replicas", scale.Spec.Replicas)
		c.fstate.n = real
	}
}

// formulaTick runs both laws for one tick. jobs carries one entry per
// healthy pod with a fresh FeatureVector (built by tick()).
func (c *controller) formulaTick(ctx context.Context, jobs []tickJob) {
	if c.fstate == nil {
		c.fstate = &formulaState{lastCap: map[string]float64{}}
	}
	f := c.cfg.formula
	c.syncN(ctx, f)
	armed := c.checkArmed(f)

	// ── Eq. (1): horizontal, one decision per service per tick ──
	var yhat, ynow float64
	for _, j := range jobs {
		yhat += j.fv.P50Now     // ŷ50: prediction channel
		ynow += j.fv.Y50Current // y50: current observed (falls back to ŷ50)
	}
	yhat /= float64(len(jobs))
	ynow /= float64(len(jobs))
	if f.yhatFromCurrent {
		yhat = ynow
	}

	u := 0
	switch {
	case yhat > f.thetaOn && c.fstate.n < f.nMax:
		u = +1
	case ynow < f.thetaOff && yhat < f.thetaOn && c.fstate.n > 0:
		u = -1
	}
	if u != 0 && armed {
		c.fstate.n += u
		c.dispatch(ctx, policy.ActionRequest{
			RuleName: "formula_horizontal",
			Target:   c.target.Name,
			Pod:      jobs[0].podName,
			Kind:     "horizontal",
			Params: map[string]any{
				"delta":        u,
				"min_replicas": 1,
			},
		}, jobs[0].podName, jobs[0].nodeName)
		c.logger.Info("formula: horizontal step",
			"u", u, "n", c.fstate.n,
			"yhat50", fmt.Sprintf("%.3f", yhat),
			"y50", fmt.Sprintf("%.3f", ynow))
	}

	// ── Eq. (2): isolation, one cap per pod per tick ──
	for _, j := range jobs {
		eIso := j.fv.TailNow * j.fv.ExtPct90 // e_iso = y90 · %ext,90
		cap := f.capBase - f.kP*(eIso-f.thetaRef)
		cap = math.Min(f.capBase, math.Max(f.capMin, cap))
		last, seen := c.fstate.lastCap[j.podName]
		if !seen {
			last = f.capBase // unthrottled is the implicit starting state
		}
		// Score trace: one CSV row per replica per tick, so mitigated runs
		// keep the full per-replica score curves alongside controller state.
		if tf := c.cfg.scoreTrace; tf != nil {
			fmt.Fprintf(tf, "%d,%s,%s,%.4f,%.4f,%.4f,%.4f,%.4f,%d,%.2f\n",
				time.Now().UnixMilli(), c.target.Name, j.podName,
				j.fv.P50Now, j.fv.Y50Current, j.fv.TailNow, j.fv.ExtPct90,
				eIso, c.fstate.n, last)
		}
		// Quantize + hysteresis so score noise straddling a quantum boundary
		// does not flap between adjacent caps. This bounds steady-state write
		// chatter only — real level shifts still dispatch on consecutive
		// ticks; there is no minimum interval between writes. (cpu.max is
		// enforced per CFS period anyway, so sub-period writes carry no
		// additional mitigation.)
		if math.Abs(cap-last) < f.capHysteresis*f.capQuantum {
			continue
		}
		q := math.Round(cap/f.capQuantum) * f.capQuantum
		if q == last {
			continue
		}
		if !armed {
			// Disarmed: the trace above records what the law WOULD do; no
			// dispatch, no latch (so arming applies the current desired cap
			// immediately rather than replaying stale deltas).
			continue
		}
		c.fstate.lastCap[j.podName] = q
		c.dispatch(ctx, policy.ActionRequest{
			RuleName: "formula_isolation",
			Target:   c.target.Name,
			Pod:      j.podName,
			Kind:     "isolate",
			Params: map[string]any{
				"cap_total_cores":       q,
				"min_per_pod_cores":     f.minPerPod,
				"guarantee_cores":       f.guaranteeDefault,
				"guarantee_hr_cores":    f.guaranteeHR,
				"guarantee_hr_ns":       f.guaranteeHRNs,
				"period_us":             f.periodUs,
				"aggressor_selector":    f.aggressorSelector,
				"aggressor_namespace":   f.aggressorNamespace,
			},
		}, j.podName, j.nodeName)
		c.logger.Info("formula: isolation cap",
			"pod", j.podName, "e_iso", fmt.Sprintf("%.3f", eIso),
			"cap_total_cores", q, "prev", last)
	}
}
