package main

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/coding-workspace/simple-mitigation-1/pkg/actuators"
	"github.com/coding-workspace/simple-mitigation-1/pkg/features"
	"github.com/coding-workspace/simple-mitigation-1/pkg/targets"
)

type fakeHC struct {
	applies, restores int
	applied           bool // what Apply reports
}

func (f *fakeHC) Name() string { return "hc" }
func (f *fakeHC) Apply(context.Context, actuators.Target, map[string]any) (actuators.ActionResult, error) {
	f.applies++
	return actuators.ActionResult{Applied: f.applied}, nil
}
func (f *fakeHC) Restore(context.Context, actuators.Target) error { f.restores++; return nil }
func (f *fakeHC) Reconcile(context.Context) error                 { return nil }

func newHCController(act *fakeHC) (*controller, *formulaConfig) {
	f := &formulaConfig{
		isoActuator: "hc",
		hc: hcFormulaConfig{
			thetaOn: 0.35, thetaOff: 0.15,
			upWindows: 3, downWindows: 2,
			reapply: 1 << 62, // never within a test
			params:  map[string]any{"secondary_cpulist": "5-8"},
		},
	}
	c := &controller{
		cfg:      &runtimeConfig{formula: f},
		target:   &targets.Target{Name: "search", Namespace: "default"},
		registry: map[string]actuators.Actuator{"hc": act},
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		fstate:   &formulaState{lastCap: map[string]float64{}},
	}
	return c, f
}

func eisoJob(e float64) []tickJob {
	return []tickJob{{podName: "search-0", nodeName: "node-4",
		fv: features.FeatureVector{TailNow: e, ExtPct90: 1}}}
}

func TestHCTickPersistenceAndArming(t *testing.T) {
	act := &fakeHC{applied: true}
	c, f := newHCController(act)
	ctx := context.Background()

	// Disarmed: sustained crossing dispatches nothing.
	for i := 0; i < 5; i++ {
		c.isoAltTick(ctx, f, eisoJob(0.5), false)
	}
	if act.applies != 0 {
		t.Fatalf("disarmed: applies=%d, want 0", act.applies)
	}
	// Armed: the run accumulated while disarmed, so the first armed tick applies.
	c.isoAltTick(ctx, f, eisoJob(0.5), true)
	if act.applies != 1 || !c.fstate.hcHeld {
		t.Fatalf("armed: applies=%d held=%v, want 1/true", act.applies, c.fstate.hcHeld)
	}
	// Between the thresholds: neither apply nor restore.
	for i := 0; i < 5; i++ {
		c.isoAltTick(ctx, f, eisoJob(0.25), true)
	}
	if act.applies != 1 || act.restores != 0 {
		t.Fatalf("hold band: applies=%d restores=%d", act.applies, act.restores)
	}
	// Below θ_off for downWindows ticks: restore once.
	c.isoAltTick(ctx, f, eisoJob(0.1), true)
	if act.restores != 0 {
		t.Fatal("restored after one window, want persistence")
	}
	c.isoAltTick(ctx, f, eisoJob(0.1), true)
	if act.restores != 1 || c.fstate.hcHeld {
		t.Fatalf("restore: restores=%d held=%v, want 1/false", act.restores, c.fstate.hcHeld)
	}
}

func TestHCTickNotAppliedStaysReleased(t *testing.T) {
	act := &fakeHC{applied: false} // e.g. no aggressors on the node yet
	c, f := newHCController(act)
	for i := 0; i < 3; i++ {
		c.isoAltTick(context.Background(), f, eisoJob(0.5), true)
	}
	if act.applies != 1 || c.fstate.hcHeld {
		t.Fatalf("applies=%d held=%v, want 1/false", act.applies, c.fstate.hcHeld)
	}
}

func TestFormulaValidate(t *testing.T) {
	cases := []struct {
		name string
		f    formulaConfig
		ok   bool
	}{
		{"isolate", formulaConfig{isoActuator: "isolate"}, true},
		{"none", formulaConfig{isoActuator: "none"}, true},
		{"typo", formulaConfig{isoActuator: "hcc"}, false},
		{"hc no cpus", formulaConfig{isoActuator: "hc", hc: hcFormulaConfig{
			thetaOn: 0.35, thetaOff: 0.15, params: map[string]any{"secondary_cpulist": ""}}}, false},
		{"hc inverted", formulaConfig{isoActuator: "hc", hc: hcFormulaConfig{
			thetaOn: 0.1, thetaOff: 0.2, params: map[string]any{"secondary_cpulist": "5-8"}}}, false},
		{"hc ok", formulaConfig{isoActuator: "hc", hc: hcFormulaConfig{
			thetaOn: 0.35, thetaOff: 0.15, params: map[string]any{"secondary_cpulist": "5-8"}}}, true},
	}
	for _, tc := range cases {
		if err := tc.f.validate(); (err == nil) != tc.ok {
			t.Errorf("%s: err=%v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}
