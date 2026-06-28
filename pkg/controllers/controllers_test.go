package controllers

import (
	"math"
	"testing"
)

const tol = 1e-9

// Expected values in this file were cross-checked against the reference
// implementation in simulation/simulation.py (same inputs, same parameters).

func approxEqual(a, b float64) bool { return math.Abs(a-b) <= tol }

func approxSlice(t *testing.T, name string, got, want []float64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: length %d, want %d", name, len(got), len(want))
	}
	for i := range got {
		if !approxEqual(got[i], want[i]) {
			t.Errorf("%s[%d] = %g, want %g", name, i, got[i], want[i])
		}
	}
}

func intSliceEqual(t *testing.T, name string, got, want []int) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: length %d, want %d", name, len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("%s[%d] = %d, want %d", name, i, got[i], want[i])
		}
	}
}

func TestRunHorizontal(t *testing.T) {
	p := HorizontalParams{ThetaOn: 0.3, ThetaOff: 0.1, HSteps: 1, NMax: 2}
	p50 := []float64{0.0, 0.5, 0.0, 0.0, 0.0}
	cmd, n := RunHorizontal(p50, p)
	// Exercises: window look-ahead (+1 at i=0 anticipating the i=1 spike),
	// n_max clamp (n caps at 2), joint-calm scale-down (-1 at i=2,3), and the
	// n>0 guard (no -1 at i=4 once n==0).
	intSliceEqual(t, "cmd", cmd, []int{1, 1, -1, -1, 0})
	intSliceEqual(t, "n", n, []int{1, 2, 1, 0, 0})
}

func TestRunIsolating(t *testing.T) {
	p := IsolatingParams{ThetaRelease: 0.3, ThetaSqueeze: 0.85, CapBaseline: 4.0, CapMin: 0.5, HSteps: 0}
	p90 := []float64{0.2, 0.3, 0.575, 0.85, 0.9}
	cap := RunIsolating(p90, p)
	// Below/at release -> baseline; the midpoint 0.575 -> exact half ramp
	// (2.25); at/above squeeze -> min.
	approxSlice(t, "cap", cap, []float64{4.0, 4.0, 2.25, 0.5, 0.5})
}

func TestRunHarvesting(t *testing.T) {
	p := HarvestingParams{ThetaSafe: 0.7, Alpha: 0.05, Beta: 0.5, Delta: 0.05, HSteps: 0}
	p90 := []float64{0.0, 0.0, 0.8, 0.8, 0.0, 0.0}
	h, _, slack := RunHarvesting(p90, p)
	// Additive probe while slack>delta, multiplicative backoff when slack<=0.
	approxSlice(t, "h", h, []float64{0.05, 0.10, 0.05, 0.025, 0.075, 0.125})
	approxSlice(t, "slack", slack, []float64{0.7, 0.7, -0.1, -0.1, 0.7, 0.7})
}

func TestHarvestingDeadband(t *testing.T) {
	// slack falls inside (0, delta] -> hold, no probe.
	p := HarvestingParams{ThetaSafe: 0.7, Alpha: 0.05, Beta: 0.5, Delta: 0.2, HSteps: 0}
	h, _, slack := RunHarvesting([]float64{0.6}, p)
	approxSlice(t, "slack", slack, []float64{0.1})
	approxSlice(t, "h", h, []float64{0.0})
}

func TestCoresToCPUMax(t *testing.T) {
	cases := []struct {
		cores float64
		want  string
	}{
		{2.25, "225000 100000"},
		{0.5, "50000 100000"},
		{0.0, "1000 100000"},   // floored at min_quota_us
		{0.005, "1000 100000"}, // round(500)=500 < 1000 -> floored
	}
	for _, c := range cases {
		if got := CoresToCPUMax(c.cores, 100000, 1000); got != c.want {
			t.Errorf("CoresToCPUMax(%g) = %q, want %q", c.cores, got, c.want)
		}
	}
}

// TestStreamingMatchesBatch confirms the stateful Step path reproduces the
// batch trace when fed the same per-tick inputs, so the two surfaces can't
// drift.
func TestStreamingMatchesBatch(t *testing.T) {
	t.Run("horizontal", func(t *testing.T) {
		p := HorizontalParams{ThetaOn: 0.3, ThetaOff: 0.1, HSteps: 1, NMax: 2}
		p50 := []float64{0.0, 0.5, 0.0, 0.0, 0.0}
		wantCmd, wantN := RunHorizontal(p50, p)
		c := NewHorizontalController(p)
		for i := range p50 {
			hi := i + p.HSteps + 1
			if hi > len(p50) {
				hi = len(p50)
			}
			cmd, n := c.Step(p50[i], sliceMax(p50[i:hi]))
			if cmd != wantCmd[i] || n != wantN[i] {
				t.Fatalf("tick %d: got (cmd=%d,n=%d), want (cmd=%d,n=%d)", i, cmd, n, wantCmd[i], wantN[i])
			}
		}
	})

	t.Run("isolating", func(t *testing.T) {
		p := IsolatingParams{ThetaRelease: 0.3, ThetaSqueeze: 0.85, CapBaseline: 4.0, CapMin: 0.5, HSteps: 0}
		p90 := []float64{0.2, 0.3, 0.575, 0.85, 0.9}
		want := RunIsolating(p90, p)
		c := NewIsolatingController(p)
		for i := range p90 {
			if got := c.Step(p90[i]); !approxEqual(got, want[i]) {
				t.Fatalf("tick %d: cap=%g, want %g", i, got, want[i])
			}
		}
	})

	t.Run("harvesting", func(t *testing.T) {
		p := HarvestingParams{ThetaSafe: 0.7, Alpha: 0.05, Beta: 0.5, Delta: 0.05, HSteps: 0}
		p90 := []float64{0.0, 0.0, 0.8, 0.8, 0.0, 0.0}
		wantH, _, wantSlack := RunHarvesting(p90, p)
		c := NewHarvestingController(p)
		for i := range p90 {
			h, slack := c.Step(p90[i])
			if !approxEqual(h, wantH[i]) || !approxEqual(slack, wantSlack[i]) {
				t.Fatalf("tick %d: got (h=%g,slack=%g), want (h=%g,slack=%g)", i, h, slack, wantH[i], wantSlack[i])
			}
		}
	})
}

func TestValidate(t *testing.T) {
	if err := (HorizontalParams{ThetaOn: 0.3, ThetaOff: 0.5, NMax: 2}).Validate(); err == nil {
		t.Error("horizontal: expected error when theta_off > theta_on")
	}
	if err := (IsolatingParams{ThetaRelease: 0.5, ThetaSqueeze: 0.3, CapBaseline: 4, CapMin: 0.5}).Validate(); err == nil {
		t.Error("isolating: expected error when theta_squeeze <= theta_release")
	}
	if err := (IsolatingParams{ThetaRelease: 0.3, ThetaSqueeze: 0.85, CapBaseline: 0.5, CapMin: 4}).Validate(); err == nil {
		t.Error("isolating: expected error when cap_baseline < cap_min")
	}
	if err := (HarvestingParams{Beta: 1.0}).Validate(); err == nil {
		t.Error("harvesting: expected error when beta >= 1")
	}
	// A valid set passes.
	if err := (HarvestingParams{ThetaSafe: 0.7, Alpha: 0.05, Beta: 0.5, Delta: 0.05}).Validate(); err != nil {
		t.Errorf("harvesting: unexpected error on valid params: %v", err)
	}
}
