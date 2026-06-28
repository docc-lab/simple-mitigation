// Package controllers ports the three minimal control laws prototyped in
// simulation/simulation.py into Go:
//
//   - horizontal : bang-bang +/-1 replicas on p50 with window look-ahead,
//     asymmetric scale-up/scale-down, joint-calm scale-down guard, n_max clamp.
//   - isolating  : P-only saturated linear ramp on p90 -> aggressor core cap.
//   - harvesting : asymmetric AIMD on slack = theta_safe - tail.
//
// The input contract is exactly the argument list of each Python function
// (one runtime signal + a fixed set of tuning parameters); see plan.md S3.
//
// Two surfaces are provided:
//
//   - Batch functions (RunHorizontal / RunIsolating / RunHarvesting) that
//     mirror the Python element-for-element. These are used for offline
//     analysis and as the parity oracle in tests.
//   - Stateful streaming controllers (streaming.go) for the online tick loop.
//
// Both are built on the same single-step helpers (horizontalStep /
// isolatingCap / harvestStep) so the batch and streaming paths cannot drift.
package controllers

import (
	"fmt"
	"math"
)

// HorizontalParams mirrors ctrl_horizontal(p50_svc, theta_on, theta_off,
// h_steps, n_max). HSteps is a look-ahead horizon in ticks (tick = 100 ms).
type HorizontalParams struct {
	ThetaOn  float64 // scale-up threshold on the look-ahead window max
	ThetaOff float64 // scale-down threshold on the current sample
	HSteps   int     // look-ahead window length in ticks (>=0)
	NMax     int     // replica-counter ceiling
}

// Validate rejects nonsensical parameter sets.
func (p HorizontalParams) Validate() error {
	if p.HSteps < 0 {
		return fmt.Errorf("horizontal: h_steps must be >= 0, got %d", p.HSteps)
	}
	if p.NMax < 0 {
		return fmt.Errorf("horizontal: n_max must be >= 0, got %d", p.NMax)
	}
	if p.ThetaOff > p.ThetaOn {
		return fmt.Errorf("horizontal: theta_off (%g) must be <= theta_on (%g)", p.ThetaOff, p.ThetaOn)
	}
	return nil
}

// IsolatingParams mirrors ctrl_isolating(p90_svc, theta_release,
// theta_squeeze, cap_baseline, cap_min, h_steps).
type IsolatingParams struct {
	ThetaRelease float64 // at/below this, cap = CapBaseline (no squeeze)
	ThetaSqueeze float64 // at/above this, cap = CapMin (full squeeze)
	CapBaseline  float64 // cores at rest
	CapMin       float64 // cores under full contention
	HSteps       int     // look-ahead horizon in ticks (>=0; 0 = instant)
}

// Validate enforces the invariants the Python asserts.
func (p IsolatingParams) Validate() error {
	if p.HSteps < 0 {
		return fmt.Errorf("isolating: h_steps must be >= 0, got %d", p.HSteps)
	}
	if p.ThetaSqueeze <= p.ThetaRelease {
		return fmt.Errorf("isolating: theta_squeeze (%g) must be > theta_release (%g)", p.ThetaSqueeze, p.ThetaRelease)
	}
	if p.CapBaseline < p.CapMin {
		return fmt.Errorf("isolating: cap_baseline (%g) must be >= cap_min (%g)", p.CapBaseline, p.CapMin)
	}
	return nil
}

// HarvestingParams mirrors ctrl_harvesting(p90_svc, theta_safe, alpha, beta,
// delta, h_steps).
type HarvestingParams struct {
	ThetaSafe float64 // tail at/above which harvested cores are released
	Alpha     float64 // additive probe per tick (cores)
	Beta      float64 // multiplicative backoff factor in [0,1)
	Delta     float64 // slack deadband; probe only when slack > Delta
	HSteps    int     // look-ahead horizon in ticks (>=0; 0 = instant)
}

// Validate rejects nonsensical parameter sets.
func (p HarvestingParams) Validate() error {
	if p.HSteps < 0 {
		return fmt.Errorf("harvesting: h_steps must be >= 0, got %d", p.HSteps)
	}
	if p.Alpha < 0 {
		return fmt.Errorf("harvesting: alpha must be >= 0, got %g", p.Alpha)
	}
	if p.Beta < 0 || p.Beta >= 1 {
		return fmt.Errorf("harvesting: beta must be in [0,1), got %g", p.Beta)
	}
	if p.Delta < 0 {
		return fmt.Errorf("harvesting: delta must be >= 0, got %g", p.Delta)
	}
	return nil
}

// horizontalStep is one iteration of the bang-bang law. Given the current
// sample pNow and the look-ahead window max pWindow, it returns the command
// (-1, 0, +1) and the new replica counter.
//
//	+1  if pWindow > theta_on                       AND n < n_max   (anticipate)
//	-1  if pNow < theta_off AND pWindow < theta_on  AND n > 0       (confirm idle)
//	 0  otherwise
func horizontalStep(pNow, pWindow float64, nCur int, p HorizontalParams) (cmd, newN int) {
	switch {
	case pWindow > p.ThetaOn && nCur < p.NMax:
		cmd = 1
	case pNow < p.ThetaOff && pWindow < p.ThetaOn && nCur > 0:
		cmd = -1
	}
	return cmd, clampInt(nCur+cmd, 0, p.NMax)
}

// isolatingCap is the memoryless saturated linear ramp: signal -> core cap.
func isolatingCap(signal float64, p IsolatingParams) float64 {
	switch {
	case signal <= p.ThetaRelease:
		return p.CapBaseline
	case signal >= p.ThetaSqueeze:
		return p.CapMin
	default:
		span := p.ThetaSqueeze - p.ThetaRelease
		drop := p.CapBaseline - p.CapMin
		return p.CapBaseline - drop*(signal-p.ThetaRelease)/span
	}
}

// harvestStep is one AIMD iteration. Given the current harvested cores and the
// tail signal it returns the new harvested cores and the slack.
//
//	slack <= 0      -> h <- beta*h        (safety release, multiplicative)
//	slack > delta   -> h <- h + alpha     (probe, additive)
//	else (deadband) -> h unchanged        (hold)
func harvestStep(hCur, scoreTail float64, p HarvestingParams) (newH, slack float64) {
	slack = p.ThetaSafe - scoreTail
	switch {
	case slack <= 0:
		newH = p.Beta * hCur
	case slack > p.Delta:
		newH = hCur + p.Alpha
	default:
		newH = hCur
	}
	return newH, slack
}

// RunHorizontal mirrors ctrl_horizontal over a full p50 trace. It returns the
// per-tick command and the replica counter n(t).
func RunHorizontal(p50 []float64, p HorizontalParams) (cmd, nExtra []int) {
	n := len(p50)
	cmd = make([]int, n)
	nExtra = make([]int, n)
	nCur := 0
	for i := 0; i < n; i++ {
		hi := i + p.HSteps + 1
		if hi > n {
			hi = n
		}
		pWindow := sliceMax(p50[i:hi])
		cmd[i], nCur = horizontalStep(p50[i], pWindow, nCur, p)
		nExtra[i] = nCur
	}
	return cmd, nExtra
}

// RunIsolating mirrors ctrl_isolating over a full p90 trace, returning the
// per-tick core cap.
func RunIsolating(p90 []float64, p IsolatingParams) []float64 {
	n := len(p90)
	out := make([]float64, n)
	for i := 0; i < n; i++ {
		h := i + p.HSteps
		if h > n-1 {
			h = n - 1
		}
		out[i] = isolatingCap(p90[h], p)
	}
	return out
}

// RunHarvesting mirrors ctrl_harvesting over a full p90 trace, returning the
// harvested cores h(t), the (look-ahead) tail signal, and the slack.
func RunHarvesting(p90 []float64, p HarvestingParams) (h, scoreTail, slack []float64) {
	n := len(p90)
	h = make([]float64, n)
	scoreTail = make([]float64, n)
	slack = make([]float64, n)
	hCur := 0.0
	for i := 0; i < n; i++ {
		hp := i + p.HSteps
		if hp > n-1 {
			hp = n - 1
		}
		scoreTail[i] = p90[hp]
		hCur, slack[i] = harvestStep(hCur, scoreTail[i], p)
		h[i] = hCur
	}
	return h, scoreTail, slack
}

// CoresToCPUMax mirrors cores_to_cpu_max: map a core cap to a cgroup v2
// cpu.max string "<quota> <period>". The quota is floored at minQuotaUs so a
// zero cap can't hard-pause the cgroup. periodUs<=0 and minQuotaUs<=0 fall
// back to the upstream defaults (100000 / 1000).
func CoresToCPUMax(capCores float64, periodUs, minQuotaUs int) string {
	if periodUs <= 0 {
		periodUs = 100000
	}
	if minQuotaUs <= 0 {
		minQuotaUs = 1000
	}
	quota := int(math.Round(capCores * float64(periodUs)))
	if quota < minQuotaUs {
		quota = minQuotaUs
	}
	return fmt.Sprintf("%d %d", quota, periodUs)
}

func sliceMax(xs []float64) float64 {
	m := xs[0]
	for _, x := range xs[1:] {
		if x > m {
			m = x
		}
	}
	return m
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
