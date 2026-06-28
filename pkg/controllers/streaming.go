package controllers

// This file holds the stateful, online versions of the three control laws.
// Each advances one tick per Step call and owns whatever integrator state the
// law carries (n for horizontal, h for harvesting; isolating is memoryless).
// They share the same single-step helpers as the batch functions in
// controllers.go, so a streaming run reproduces the corresponding batch trace
// exactly when fed the same per-tick inputs.
//
// Look-ahead note: the batch functions realize h_steps by indexing the future
// trace, which is only possible offline. Online there is no future, so the
// caller is responsible for supplying the look-ahead value:
//
//   - HorizontalController.Step takes (pNow, pWindow): pWindow is the max over
//     the look-ahead window. With h_steps==0 (or no predictor), pass
//     pWindow == pNow.
//   - IsolatingController.Step / HarvestingController.Step take the already
//     look-ahead-resolved signal (the future sample when a predictor supplies
//     it, otherwise the current sample).

// HorizontalController is the stateful bang-bang replica scaler. Output is the
// replica counter n; emit it to the horizontal actuator as ensure_min: n.
type HorizontalController struct {
	p    HorizontalParams
	nCur int
}

// NewHorizontalController returns a controller seeded at n=0.
func NewHorizontalController(p HorizontalParams) *HorizontalController {
	return &HorizontalController{p: p}
}

// Name identifies the controller.
func (c *HorizontalController) Name() string { return "horizontal" }

// Step advances one tick. pNow is the current p50 signal; pWindow is the
// look-ahead window max (pass pWindow == pNow when there's no look-ahead).
// Returns the command (-1/0/+1) and the new replica counter.
func (c *HorizontalController) Step(pNow, pWindow float64) (cmd, n int) {
	cmd, c.nCur = horizontalStep(pNow, pWindow, c.nCur, c.p)
	return cmd, c.nCur
}

// N returns the current replica counter.
func (c *HorizontalController) N() int { return c.nCur }

// Reset returns the counter to zero (on target/pod teardown).
func (c *HorizontalController) Reset() { c.nCur = 0 }

// IsolatingController is the memoryless saturated-linear-ramp core capper.
// Output is a core cap; map it to a cgroup quota with CoresToCPUMax.
type IsolatingController struct {
	p IsolatingParams
}

// NewIsolatingController returns a controller for the given parameters.
func NewIsolatingController(p IsolatingParams) *IsolatingController {
	return &IsolatingController{p: p}
}

// Name identifies the controller.
func (c *IsolatingController) Name() string { return "isolating" }

// Step returns the core cap for the given (look-ahead-resolved) p90 signal.
func (c *IsolatingController) Step(signal float64) (capCores float64) {
	return isolatingCap(signal, c.p)
}

// Reset is a no-op; the controller carries no state.
func (c *IsolatingController) Reset() {}

// HarvestingController is the stateful AIMD core harvester. Output is the
// harvested-cores integrator h; emit it to the harvest actuator.
type HarvestingController struct {
	p    HarvestingParams
	hCur float64
}

// NewHarvestingController returns a controller seeded at h=0.
func NewHarvestingController(p HarvestingParams) *HarvestingController {
	return &HarvestingController{p: p}
}

// Name identifies the controller.
func (c *HarvestingController) Name() string { return "harvesting" }

// Step advances one AIMD tick with the (look-ahead-resolved) tail signal and
// returns the new harvested cores and the slack.
func (c *HarvestingController) Step(scoreTail float64) (h, slack float64) {
	c.hCur, slack = harvestStep(c.hCur, scoreTail, c.p)
	return c.hCur, slack
}

// H returns the current harvested cores.
func (c *HarvestingController) H() float64 { return c.hCur }

// Reset returns the harvested-cores integrator to zero.
func (c *HarvestingController) Reset() { c.hCur = 0 }
