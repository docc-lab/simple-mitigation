package features

import (
	"math"

	pb "github.com/coding-workspace/simple-mitigation-1/gen/go/contentionpb"
)

// FeatureVector is the flat feature row consumed by the policy engine. Each
// field is exposed to CEL as a top-level identifier; see pkg/policy/cel.go.
//
// Field naming mirrors Section 4 of plan-v2-centralized.md so rule authors
// reading the doc can copy expressions verbatim. Slope units are
// "score-units per millisecond" for spatial and "score-units per second"
// for temporal (chosen so default policy thresholds stay in [0, 1]-ish
// ranges humans can reason about).
type FeatureVector struct {
	// Target / pod identity (set by the caller after Build, kept here so the
	// policy engine can include them in cooldown keys without an extra wrap).
	Target string
	Pod    string

	// Spatial features (multi-horizon, from the latest event).
	P50H             []float64 // p50_horizons coerced to float64
	TailH            []float64 // tail_horizons coerced to float64
	HorizonMs        []int64   // horizon_ms coerced to int64
	KSpatial         float64   // least-squares slope of P50H vs HorizonMs (score/ms)
	AccelSpatial     float64   // mean second-difference of P50H
	P50MaxHorizonMs  int64     // argmax horizon_ms[i] over P50H
	PersistenceH     int       // count of P50H entries >= HiThreshold

	// Temporal features (rolling window over p50_trend_pred).
	P50Now             float64 // latest p50_trend_pred (= ŷ50 when prediction is on)
	TailNow            float64 // latest tail_trend_label (= formula y90)
	Y50Current         float64 // latest y50_current (formula y50 at "now"); falls back to P50Now when absent
	ExtPct50           float64 // extrinsic share of p50 displacement; 1 when absent
	ExtPct90           float64 // extrinsic share of p90 displacement; 1 when absent
	KTemporal          float64 // least-squares slope over window (score/s)
	AccelTemporal      float64 // mean second-difference over window
	Variance           float64 // sample variance over window
	DurationAboveHiMs  int64   // length of the most-recent contiguous run above HiThreshold

	// Diagnostics.
	WindowSize int    // number of samples in the rolling window when built
	HasSpatial bool   // true iff len(p50_horizons) > 0 on the latest event
	ModelVer   string // pass-through of the latest event's model_version
	SourceKind string // pass-through of the latest event's source_kind
}

// BuildConfig parameterises Build. HiThreshold is used by both spatial
// (PersistenceH) and temporal (DurationAboveHiMs) computations so a single
// "what counts as elevated?" knob applies consistently.
type BuildConfig struct {
	HiThreshold float64
}

// Build produces a FeatureVector from the latest spatial event plus a
// rolling window of temporal samples. The window slice MUST be in
// chronological order (oldest first), as returned by Window.Snapshot.
// Either argument may be nil/empty; the corresponding feature group then
// reads as zero.
func Build(latest *pb.ScoreEvent, window []Sample, cfg BuildConfig) FeatureVector {
	fv := FeatureVector{
		WindowSize: len(window),
	}
	if latest != nil {
		fv.P50Now = float64(latest.P50TrendPred)
		fv.TailNow = float64(latest.TailTrendLabel)
		fv.ModelVer = latest.ModelVersion
		fv.SourceKind = latest.SourceKind
		// dsb fields 8-10; zero means "not emitted by this producer build":
		// y50_current falls back to the p50 channel, ext factors to 1.
		fv.Y50Current = float64(latest.Y50Current)
		if fv.Y50Current == 0 {
			fv.Y50Current = fv.P50Now
		}
		fv.ExtPct50 = float64(latest.ExtPct50)
		if fv.ExtPct50 == 0 {
			fv.ExtPct50 = 1
		}
		fv.ExtPct90 = float64(latest.ExtPct90)
		if fv.ExtPct90 == 0 {
			fv.ExtPct90 = 1
		}
		fv.HasSpatial = len(latest.P50Horizons) > 0
		fv.P50H = toFloat64s(latest.P50Horizons)
		fv.TailH = toFloat64s(latest.TailHorizons)
		fv.HorizonMs = toInt64s(latest.HorizonMs)
	}
	if fv.HasSpatial {
		fv.KSpatial, fv.AccelSpatial = spatialDerivatives(fv.P50H, fv.HorizonMs)
		fv.P50MaxHorizonMs = argmaxHorizon(fv.P50H, fv.HorizonMs)
		fv.PersistenceH = countAtLeast(fv.P50H, cfg.HiThreshold)
	}
	if len(window) >= 2 {
		fv.KTemporal, fv.AccelTemporal, fv.Variance = temporalDerivatives(window)
	}
	fv.DurationAboveHiMs = durationAboveHi(window, cfg.HiThreshold)
	return fv
}

// spatialDerivatives returns (slope, mean second-difference) of p50 versus
// horizon_ms. Slope is least-squares; acceleration averages the bare
// (y[i+1] - 2*y[i] + y[i-1]) terms (no normalisation by spacing because
// horizons are typically log-spaced and a unit-correct curvature wouldn't
// add value over the simple sign).
func spatialDerivatives(p50 []float64, horizons []int64) (slope, accel float64) {
	n := minInt(len(p50), len(horizons))
	if n < 2 {
		return 0, 0
	}
	xs := make([]float64, n)
	for i := 0; i < n; i++ {
		xs[i] = float64(horizons[i])
	}
	slope = leastSquaresSlope(xs, p50[:n])
	if n >= 3 {
		var sum float64
		var count int
		for i := 1; i < n-1; i++ {
			sum += p50[i+1] - 2*p50[i] + p50[i-1]
			count++
		}
		if count > 0 {
			accel = sum / float64(count)
		}
	}
	return slope, accel
}

// argmaxHorizon returns the horizon_ms entry corresponding to the largest
// p50 prediction. Ties go to the longer horizon (more pessimistic).
func argmaxHorizon(p50 []float64, horizons []int64) int64 {
	n := minInt(len(p50), len(horizons))
	if n == 0 {
		return 0
	}
	best := p50[0]
	idx := 0
	for i := 1; i < n; i++ {
		if p50[i] >= best {
			best = p50[i]
			idx = i
		}
	}
	return horizons[idx]
}

func countAtLeast(xs []float64, threshold float64) int {
	var c int
	for _, x := range xs {
		if x >= threshold {
			c++
		}
	}
	return c
}

// temporalDerivatives computes (slope, accel, variance) of p50_trend_pred
// across the window. Slope is least-squares in seconds (so thresholds like
// "k_temporal > 0.3" read as "rising 0.3 per second"). Acceleration is the
// mean second-difference of the sequence in score-units per tick^2 (kept
// raw rather than normalised by dt^2 because ticks are usually uniform and
// the sign is what rules care about).
func temporalDerivatives(window []Sample) (slope, accel, variance float64) {
	n := len(window)
	xs := make([]float64, n)
	ys := make([]float64, n)
	base := timestampNs(window[0])
	for i, s := range window {
		xs[i] = float64(timestampNs(s)-base) / 1e9 // seconds since start
		ys[i] = float64(s.Event.P50TrendPred)
	}
	slope = leastSquaresSlope(xs, ys)
	if n >= 3 {
		var sum float64
		var count int
		for i := 1; i < n-1; i++ {
			sum += ys[i+1] - 2*ys[i] + ys[i-1]
			count++
		}
		if count > 0 {
			accel = sum / float64(count)
		}
	}
	variance = sampleVariance(ys)
	return slope, accel, variance
}

// durationAboveHi returns the length, in milliseconds, of the most recent
// contiguous run of samples with p50_trend_pred >= threshold. Returns 0 if
// the latest sample is below threshold or the window is empty.
func durationAboveHi(window []Sample, threshold float64) int64 {
	if len(window) == 0 {
		return 0
	}
	last := len(window) - 1
	if float64(window[last].Event.P50TrendPred) < threshold {
		return 0
	}
	runStart := last
	for i := last - 1; i >= 0; i-- {
		if float64(window[i].Event.P50TrendPred) < threshold {
			break
		}
		runStart = i
	}
	deltaNs := timestampNs(window[last]) - timestampNs(window[runStart])
	if deltaNs < 0 {
		return 0
	}
	return deltaNs / int64(1_000_000)
}

func timestampNs(s Sample) int64 {
	if s.Event != nil && s.Event.TimestampNs != 0 {
		return s.Event.TimestampNs
	}
	return s.Received.UnixNano()
}

// leastSquaresSlope returns the slope of the OLS line y = a + b*x. Returns
// 0 if the x values are all identical (degenerate fit).
func leastSquaresSlope(xs, ys []float64) float64 {
	n := len(xs)
	if n < 2 || n != len(ys) {
		return 0
	}
	var sx, sy, sxx, sxy float64
	for i := 0; i < n; i++ {
		sx += xs[i]
		sy += ys[i]
		sxx += xs[i] * xs[i]
		sxy += xs[i] * ys[i]
	}
	fn := float64(n)
	denom := fn*sxx - sx*sx
	if math.Abs(denom) < 1e-12 {
		return 0
	}
	return (fn*sxy - sx*sy) / denom
}

// sampleVariance returns the unbiased sample variance of ys. Returns 0 for
// len(ys) < 2.
func sampleVariance(ys []float64) float64 {
	n := len(ys)
	if n < 2 {
		return 0
	}
	var mean float64
	for _, y := range ys {
		mean += y
	}
	mean /= float64(n)
	var s float64
	for _, y := range ys {
		d := y - mean
		s += d * d
	}
	return s / float64(n-1)
}

func toFloat64s(in []float32) []float64 {
	if len(in) == 0 {
		return nil
	}
	out := make([]float64, len(in))
	for i, v := range in {
		out[i] = float64(v)
	}
	return out
}

func toInt64s(in []int32) []int64 {
	if len(in) == 0 {
		return nil
	}
	out := make([]int64, len(in))
	for i, v := range in {
		out[i] = int64(v)
	}
	return out
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
