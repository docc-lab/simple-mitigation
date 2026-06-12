// Package thresholder turns a noisy per-tick score in [0, 1] into a clean
// (direction, should-act) signal for a controller. Hysteresis comes from the
// HI/LO band plus a sustained-window count; rate limiting comes from a
// post-action cooldown.
//
// The state machine is intentionally minimal so it can be reused by both the
// horizontal sidecar (1 instance per CPA Pod) and the vertical scaler
// (1 instance per victim pod).
package thresholder

import (
	"fmt"
	"time"
)

// Direction is the controller's recommended motion based on the most recent
// observation. Stable means "neither band sustained".
type Direction int

const (
	Stable Direction = iota
	Up
	Down
)

func (d Direction) String() string {
	switch d {
	case Up:
		return "up"
	case Down:
		return "down"
	case Stable:
		return "stable"
	}
	return "?"
}

// Config controls the band shape and cadence. LO must be strictly below HI.
type Config struct {
	HI             float64       // fire-up threshold; score >= HI counts as Up
	LO             float64       // fire-down threshold; score <= LO counts as Down
	MinHoldWindows int           // required consecutive same-direction samples before acting
	Cooldown       time.Duration // minimum spacing between actions
}

// Validate returns nil if cfg is internally consistent.
func (c Config) Validate() error {
	if c.LO < 0 || c.LO > 1 {
		return fmt.Errorf("LO out of [0,1]: %v", c.LO)
	}
	if c.HI < 0 || c.HI > 1 {
		return fmt.Errorf("HI out of [0,1]: %v", c.HI)
	}
	if c.LO >= c.HI {
		return fmt.Errorf("LO must be strictly below HI: %v >= %v", c.LO, c.HI)
	}
	if c.MinHoldWindows < 1 {
		return fmt.Errorf("MinHoldWindows must be >= 1, got %d", c.MinHoldWindows)
	}
	if c.Cooldown < 0 {
		return fmt.Errorf("Cooldown must be >= 0, got %v", c.Cooldown)
	}
	return nil
}

// Thresholder is not safe for concurrent use. Wrap with a mutex if multiple
// goroutines call Observe.
type Thresholder struct {
	cfg          Config
	aboveCount   int
	belowCount   int
	lastActionAt time.Time
}

// New constructs a Thresholder. cfg is validated.
func New(cfg Config) (*Thresholder, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Thresholder{cfg: cfg}, nil
}

// Config returns the active configuration.
func (t *Thresholder) Config() Config { return t.cfg }

// Observe updates internal state with the latest sample and returns:
//   - dir: classification of this sample (Up/Down/Stable). Even when act=false,
//     dir is useful for logging/exposing "we'd act if cooldown hadn't gated us".
//   - act: true iff the controller should take an action this tick.
//     act is only ever true when dir != Stable.
//
// act becomes true once both gates pass:
//  1. score has stayed in the same firing band for >= MinHoldWindows samples;
//  2. >= Cooldown time has elapsed since the previous time act was true.
func (t *Thresholder) Observe(score float64, now time.Time) (dir Direction, act bool) {
	switch {
	case score >= t.cfg.HI:
		t.aboveCount++
		t.belowCount = 0
		dir = Up
	case score <= t.cfg.LO:
		t.belowCount++
		t.aboveCount = 0
		dir = Down
	default:
		t.aboveCount = 0
		t.belowCount = 0
		return Stable, false
	}

	var sustained int
	if dir == Up {
		sustained = t.aboveCount
	} else {
		sustained = t.belowCount
	}
	if sustained < t.cfg.MinHoldWindows {
		return dir, false
	}
	if !t.lastActionAt.IsZero() && now.Sub(t.lastActionAt) < t.cfg.Cooldown {
		return dir, false
	}
	t.lastActionAt = now
	return dir, true
}

// Reset clears all internal state, as if New had just been called.
func (t *Thresholder) Reset() {
	t.aboveCount = 0
	t.belowCount = 0
	t.lastActionAt = time.Time{}
}
