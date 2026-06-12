package thresholder_test

import (
	"testing"
	"time"

	"github.com/coding-workspace/simple-mitigation-1/pkg/thresholder"
)

func mustNew(t *testing.T, c thresholder.Config) *thresholder.Thresholder {
	t.Helper()
	th, err := thresholder.New(c)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return th
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		cfg  thresholder.Config
		ok   bool
	}{
		{"happy", thresholder.Config{HI: 0.5, LO: 0.2, MinHoldWindows: 3, Cooldown: time.Second}, true},
		{"lo>=hi", thresholder.Config{HI: 0.3, LO: 0.3, MinHoldWindows: 1, Cooldown: 0}, false},
		{"lo<0", thresholder.Config{HI: 0.5, LO: -0.1, MinHoldWindows: 1, Cooldown: 0}, false},
		{"hi>1", thresholder.Config{HI: 1.1, LO: 0.2, MinHoldWindows: 1, Cooldown: 0}, false},
		{"hold0", thresholder.Config{HI: 0.5, LO: 0.2, MinHoldWindows: 0, Cooldown: 0}, false},
		{"cooldown<0", thresholder.Config{HI: 0.5, LO: 0.2, MinHoldWindows: 1, Cooldown: -time.Second}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if (err == nil) != tc.ok {
				t.Fatalf("Validate: ok=%v err=%v", tc.ok, err)
			}
		})
	}
}

func TestObserveFiresAfterHoldWindow(t *testing.T) {
	th := mustNew(t, thresholder.Config{
		HI: 0.5, LO: 0.2,
		MinHoldWindows: 3,
		Cooldown:       time.Second,
	})
	t0 := time.Unix(0, 0)

	// In dead band: Stable, no act.
	if d, a := th.Observe(0.4, t0); d != thresholder.Stable || a {
		t.Fatalf("dead band: got %v/%v", d, a)
	}
	// First above-HI: Up, not yet acting.
	if d, a := th.Observe(0.6, t0.Add(100*time.Millisecond)); d != thresholder.Up || a {
		t.Fatalf("1st above: got %v/%v", d, a)
	}
	// Second: still not.
	if d, a := th.Observe(0.6, t0.Add(200*time.Millisecond)); d != thresholder.Up || a {
		t.Fatalf("2nd above: got %v/%v", d, a)
	}
	// Third: act!
	if d, a := th.Observe(0.7, t0.Add(300*time.Millisecond)); d != thresholder.Up || !a {
		t.Fatalf("3rd above: got %v/%v", d, a)
	}
}

func TestCooldownGatesRepeatedActions(t *testing.T) {
	th := mustNew(t, thresholder.Config{
		HI: 0.5, LO: 0.2,
		MinHoldWindows: 1,
		Cooldown:       2 * time.Second,
	})
	t0 := time.Unix(0, 0)

	// Fire immediately (hold=1).
	if _, a := th.Observe(0.9, t0); !a {
		t.Fatal("first fire failed")
	}
	// Next sample inside cooldown: no act.
	if _, a := th.Observe(0.9, t0.Add(500*time.Millisecond)); a {
		t.Fatal("acted during cooldown")
	}
	// After cooldown elapses: act.
	if _, a := th.Observe(0.9, t0.Add(3*time.Second)); !a {
		t.Fatal("did not act after cooldown")
	}
}

func TestDeadBandResetsHoldCount(t *testing.T) {
	th := mustNew(t, thresholder.Config{
		HI: 0.5, LO: 0.2,
		MinHoldWindows: 3,
		Cooldown:       time.Second,
	})
	t0 := time.Unix(0, 0)

	th.Observe(0.6, t0)
	th.Observe(0.6, t0.Add(100*time.Millisecond))
	// Drop into dead band -> aboveCount reset.
	if d, _ := th.Observe(0.3, t0.Add(200*time.Millisecond)); d != thresholder.Stable {
		t.Fatalf("expected Stable after dead-band, got %v", d)
	}
	// Back above; must collect 3 fresh samples.
	if _, a := th.Observe(0.6, t0.Add(300*time.Millisecond)); a {
		t.Fatal("acted after only 1 fresh above")
	}
	if _, a := th.Observe(0.6, t0.Add(400*time.Millisecond)); a {
		t.Fatal("acted after only 2 fresh above")
	}
	if _, a := th.Observe(0.6, t0.Add(500*time.Millisecond)); !a {
		t.Fatal("did not act after 3 fresh above")
	}
}

func TestDownDirection(t *testing.T) {
	th := mustNew(t, thresholder.Config{
		HI: 0.5, LO: 0.2,
		MinHoldWindows: 2,
		Cooldown:       0,
	})
	t0 := time.Unix(0, 0)

	if d, a := th.Observe(0.1, t0); d != thresholder.Down || a {
		t.Fatalf("1st below: got %v/%v", d, a)
	}
	if d, a := th.Observe(0.05, t0.Add(100*time.Millisecond)); d != thresholder.Down || !a {
		t.Fatalf("2nd below: got %v/%v", d, a)
	}
}
