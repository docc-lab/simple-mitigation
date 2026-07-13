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
// Online realization: the stream carries one predicted value per tick
// (p50_trend_pred). It stands in for BOTH ŷ50 and y50 — the wire contract
// has no separate "current observed" field. ŷ50 is aggregated to service
// level as the mean over healthy replicas (traffic-weighted aggregation
// needs per-replica arrival rates the stream does not carry).
//
// Eq. (2) — isolation saturated proportional (P-only) on the extrinsic
// tail magnitude e_iso(t) = y90(t)·%ext,90(t):
//
//	c(t) = min(c_base, max(c_min, c_base - k_p·(e_iso(t) - θ_ref)))
//
// %ext,90 has no field in this repo's proto yet, so %ext ≡ 1 (e_iso =
// tail_trend_label). c(t) is computed per pod and dispatched to the isolate
// actuator's absolute-cap mode (cap_cores), quantized to CAP_QUANTUM_CORES
// so sensor noise does not turn into a cgroup write per tick.
package main

import (
	"context"
	"fmt"
	"math"
	"os"

	"github.com/coding-workspace/simple-mitigation-1/pkg/policy"
)

// Defaults match simulation/simulation.py's flag defaults (the paper's
// reference values). Per-signal tuning is done via env at run time.
type formulaConfig struct {
	thetaOn  float64 // Eq.1 θ_on
	thetaOff float64 // Eq.1 θ_off
	nMax     int     // Eq.1 n_max
	thetaRef float64 // Eq.2 θ_ref
	kP       float64 // Eq.2 k_p (cores per unit score)
	capBase  float64 // Eq.2 c_base (cores)
	capMin   float64 // Eq.2 c_min (cores)

	// Actuation plumbing (not part of the formulas).
	capQuantum         float64 // dispatch isolate only when the quantized cap changes
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
		capQuantum:         envFloat("CAP_QUANTUM_CORES", 0.25),
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
		"cap_quantum", f.capQuantum,
	}
}

// formulaState is per-target controller state. It lives on the controller
// struct, so all access happens from that target's tick goroutine only.
type formulaState struct {
	n       int                // Eq.1 replica counter n(t), extra replicas above baseline
	lastCap map[string]float64 // pod -> last dispatched quantized cap (cores)
}

// formulaTick runs both laws for one tick. jobs carries one entry per
// healthy pod with a fresh FeatureVector (built by tick()).
func (c *controller) formulaTick(ctx context.Context, jobs []tickJob) {
	if c.fstate == nil {
		c.fstate = &formulaState{lastCap: map[string]float64{}}
	}
	f := c.cfg.formula

	// ── Eq. (1): horizontal, one decision per service per tick ──
	var yhat float64
	for _, j := range jobs {
		yhat += j.fv.P50Now
	}
	yhat /= float64(len(jobs))

	u := 0
	switch {
	case yhat > f.thetaOn && c.fstate.n < f.nMax:
		u = +1
	case yhat < f.thetaOff && yhat < f.thetaOn && c.fstate.n > 0:
		u = -1
	}
	if u != 0 {
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
			"u", u, "n", c.fstate.n, "yhat50", fmt.Sprintf("%.3f", yhat))
	}

	// ── Eq. (2): isolation, one cap per pod per tick ──
	for _, j := range jobs {
		eIso := j.fv.TailNow // %ext ≡ 1 until the proto carries it
		cap := f.capBase - f.kP*(eIso-f.thetaRef)
		cap = math.Min(f.capBase, math.Max(f.capMin, cap))
		// Quantize so noise does not produce a cgroup write every tick.
		q := math.Round(cap/f.capQuantum) * f.capQuantum
		last, seen := c.fstate.lastCap[j.podName]
		if !seen {
			last = f.capBase // unthrottled is the implicit starting state
		}
		if q == last {
			continue
		}
		c.fstate.lastCap[j.podName] = q
		c.dispatch(ctx, policy.ActionRequest{
			RuleName: "formula_isolation",
			Target:   c.target.Name,
			Pod:      j.podName,
			Kind:     "isolate",
			Params: map[string]any{
				"cap_cores":           q,
				"period_us":           f.periodUs,
				"aggressor_selector":  f.aggressorSelector,
				"aggressor_namespace": f.aggressorNamespace,
			},
		}, j.podName, j.nodeName)
		c.logger.Info("formula: isolation cap",
			"pod", j.podName, "e_iso", fmt.Sprintf("%.3f", eIso),
			"cap_cores", q, "prev", last)
	}
}
