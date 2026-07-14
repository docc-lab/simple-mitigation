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
	// horzFromExt drives Eq.(1) from the INTRINSIC-weighted signal
	// e_horz = y50·(1−ext50) instead of raw y50/ŷ50. Horizontal scaling
	// adds capacity, so it can only mitigate load-induced (intrinsic)
	// pressure; raw y50 cannot attribute the excess and false-positively
	// scales out under extrinsic interference (isolation's job) while
	// never earning scale-down credit once intrinsic pressure is relieved.
	// e_horz is the mirror image of Eq.(2)'s e_iso = y90·ext90.
	// Thresholds move to horzThetaOn/Off (HORZ_THETA_ON/HORZ_THETA_OFF,
	// default THETA_ON/THETA_OFF): the product signal lives on a smaller
	// scale than raw y50.
	horzFromExt  bool
	horzThetaOn  float64
	horzThetaOff float64
	// horizontalDeployment redirects Eq.(1)'s scale target (and syncN) to a
	// separate deployment — the "overflow" pattern: the victim deployment
	// stays hard-pinned to the target node at replicas=1, while n(t) scales
	// an unpinned spread-scheduled twin (HORIZONTAL_DEPLOYMENT, e.g.
	// "search-overflow") between 0 and n_max. One deployment cannot pin one
	// replica and spread the rest; two can. Empty = scale the target's own
	// deployment (min 1), the original behavior.
	horizontalDeployment string
	// horzDownDwell requires e_horz to stay below θ_off continuously for
	// this long before u=-1 fires (HORZ_DOWN_DWELL_SEC, default 8s).
	// Scale-up stays immediate: bursty load makes the intrinsic channel
	// spiky, and an up->down flap within seconds churns pods for nothing.
	horzDownDwell time.Duration
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
	thetaOn := envFloat("THETA_ON", 0.3)
	thetaOff := envFloat("THETA_OFF", 0.1)
	return &formulaConfig{
		thetaOn:            thetaOn,
		thetaOff:           thetaOff,
		horzFromExt:        os.Getenv("HORZ_FROM_EXT") == "1",
		horzThetaOn:        envFloat("HORZ_THETA_ON", thetaOn),
		horzThetaOff:       envFloat("HORZ_THETA_OFF", thetaOff),
		horizontalDeployment: os.Getenv("HORIZONTAL_DEPLOYMENT"),
		horzDownDwell:        time.Duration(envFloat("HORZ_DOWN_DWELL_SEC", 8) * float64(time.Second)),
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
		"horz_from_ext", f.horzFromExt,
		"horz_theta_on", f.horzThetaOn, "horz_theta_off", f.horzThetaOff,
		"horizontal_deployment", f.horizontalDeployment,
		"horz_down_dwell", f.horzDownDwell,
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
	// belowSince marks when the scale-down signal first dropped below
	// θ_off; zero while the signal sits above it. u=-1 needs the signal
	// below θ_off for horzDownDwell continuously.
	belowSince time.Time
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
	// With an overflow deployment, n(t) IS its replica count (baseline 0);
	// classic mode counts extras above the victim deployment's baseline.
	deployName := c.target.Name
	baselineDefault := 1
	if f.horizontalDeployment != "" {
		deployName = f.horizontalDeployment
		baselineDefault = 0
	}
	scale, err := c.kclient.AppsV1().Deployments(c.target.Namespace).
		GetScale(ctx, deployName, metav1.GetOptions{})
	if err != nil {
		c.logger.Warn("formula: n resync failed", "deployment", deployName, "err", err)
		return
	}
	baseline := envInt("BASELINE_REPLICAS", baselineDefault)
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
	var yhat, ynow, ext50 float64
	for _, j := range jobs {
		yhat += j.fv.P50Now     // ŷ50: prediction channel
		ynow += j.fv.Y50Current // y50: current observed (falls back to ŷ50)
		ext50 += j.fv.ExtPct50  // extrinsic share of the p50 displacement
	}
	yhat /= float64(len(jobs))
	ynow /= float64(len(jobs))
	ext50 /= float64(len(jobs))
	if f.yhatFromCurrent {
		yhat = ynow
	}

	// Intrinsic weighting: horizontal scaling only fixes load-induced
	// pressure, so gate it on the intrinsic share of the displacement.
	// upSig anticipates (prediction channel), downSig confirms (current).
	upSig, downSig := yhat, ynow
	hOn, hOff := f.thetaOn, f.thetaOff
	if f.horzFromExt {
		upSig = yhat * (1 - ext50)
		downSig = ynow * (1 - ext50)
		hOn, hOff = f.horzThetaOn, f.horzThetaOff
	}

	// Scale-down dwell: the intrinsic channel is spiky under bursty load;
	// require the signal to HOLD below θ_off before shedding a replica.
	// Scale-up stays immediate.
	nowT := time.Now()
	if downSig < hOff && upSig < hOn {
		if c.fstate.belowSince.IsZero() {
			c.fstate.belowSince = nowT
		}
	} else {
		c.fstate.belowSince = time.Time{}
	}
	dwellMet := !c.fstate.belowSince.IsZero() &&
		nowT.Sub(c.fstate.belowSince) >= f.horzDownDwell

	u := 0
	switch {
	case upSig > hOn && c.fstate.n < f.nMax:
		u = +1
	case dwellMet && c.fstate.n > 0:
		u = -1
	}
	if u != 0 && armed {
		c.fstate.n += u
		minReplicas := 1
		horzParams := map[string]any{
			"delta":        u,
			"min_replicas": minReplicas,
		}
		if f.horizontalDeployment != "" {
			// Overflow deployment: scaling target is the unpinned twin,
			// which legitimately goes to zero.
			horzParams["deployment"] = f.horizontalDeployment
			horzParams["min_replicas"] = 0
		}
		c.dispatch(ctx, policy.ActionRequest{
			RuleName: "formula_horizontal",
			Target:   c.target.Name,
			Pod:      jobs[0].podName,
			Kind:     "horizontal",
			Params:   horzParams,
		}, jobs[0].podName, jobs[0].nodeName)
		c.logger.Info("formula: horizontal step",
			"u", u, "n", c.fstate.n,
			"yhat50", fmt.Sprintf("%.3f", yhat),
			"y50", fmt.Sprintf("%.3f", ynow),
			"ext50", fmt.Sprintf("%.3f", ext50),
			"e_horz", fmt.Sprintf("%.3f", downSig))
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
			eHorz := j.fv.Y50Current * (1 - j.fv.ExtPct50)
			fmt.Fprintf(tf, "%d,%s,%s,%.4f,%.4f,%.4f,%.4f,%.4f,%d,%.2f,%.4f,%.4f\n",
				time.Now().UnixMilli(), c.target.Name, j.podName,
				j.fv.P50Now, j.fv.Y50Current, j.fv.TailNow, j.fv.ExtPct90,
				eIso, c.fstate.n, last, j.fv.ExtPct50, eHorz)
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
