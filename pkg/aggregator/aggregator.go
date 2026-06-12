// Package aggregator defines the pluggable policy used by the
// horizontal-cpa-sidecar to combine per-pod scores into a single value for
// the scaling decision.
package aggregator

import (
	"fmt"
	"math"
	"sort"
)

// Aggregator combines a slice of per-pod scores into one scalar.
// Implementations must be safe to call concurrently.
type Aggregator interface {
	Name() string
	Apply(values []float64) float64
}

type maxAgg struct{}

func (maxAgg) Name() string { return "max" }
func (maxAgg) Apply(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	m := v[0]
	for _, x := range v[1:] {
		if x > m {
			m = x
		}
	}
	return m
}

type meanAgg struct{}

func (meanAgg) Name() string { return "mean" }
func (meanAgg) Apply(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	var s float64
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}

type p90Agg struct{}

func (p90Agg) Name() string { return "p90" }
func (p90Agg) Apply(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	// Nearest-rank p90: index = ceil(0.90 * n) - 1, clamped to [0, n-1].
	idx := int(math.Ceil(0.9*float64(len(s)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(s) {
		idx = len(s) - 1
	}
	return s[idx]
}

// Built-in instances. Treated as immutable.
var (
	Max  Aggregator = maxAgg{}
	Mean Aggregator = meanAgg{}
	P90  Aggregator = p90Agg{}
)

// New returns the aggregator registered under name. Empty name selects Max.
func New(name string) (Aggregator, error) {
	switch name {
	case "", "max":
		return Max, nil
	case "mean", "avg":
		return Mean, nil
	case "p90":
		return P90, nil
	default:
		return nil, fmt.Errorf("aggregator %q: unknown (have: max, mean, p90)", name)
	}
}
