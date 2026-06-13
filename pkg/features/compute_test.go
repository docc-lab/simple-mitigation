package features

import (
	"math"
	"testing"
	"time"

	pb "github.com/coding-workspace/simple-mitigation-1/gen/go/contentionpb"
)

func TestBuildEmptyInputs(t *testing.T) {
	fv := Build(nil, nil, BuildConfig{HiThreshold: 0.5})
	if fv.WindowSize != 0 || fv.HasSpatial {
		t.Fatalf("expected empty FV, got %+v", fv)
	}
	if fv.KSpatial != 0 || fv.KTemporal != 0 || fv.PersistenceH != 0 {
		t.Fatalf("expected all-zero feature derivations, got %+v", fv)
	}
}

func TestBuildSingleHorizonFallback(t *testing.T) {
	// Old-style predictor: no p50_horizons populated. Spatial features
	// must degrade to zero without panicking.
	ev := &pb.ScoreEvent{P50TrendPred: 0.8, TailTrendLabel: 0.6}
	fv := Build(ev, nil, BuildConfig{HiThreshold: 0.5})
	if fv.HasSpatial {
		t.Fatalf("HasSpatial should be false for empty p50_horizons")
	}
	if fv.KSpatial != 0 || fv.PersistenceH != 0 || fv.P50MaxHorizonMs != 0 {
		t.Fatalf("spatial features should be zero, got %+v", fv)
	}
	if math.Abs(fv.P50Now-0.8) > 1e-6 || math.Abs(fv.TailNow-0.6) > 1e-6 {
		t.Fatalf("temporal-now fields wrong, got %+v", fv)
	}
}

func TestBuildSpatialMonotoneRising(t *testing.T) {
	ev := &pb.ScoreEvent{
		P50TrendPred:  0.3,
		P50Horizons:   []float32{0.3, 0.5, 0.7, 0.9},
		HorizonMs:     []int32{100, 500, 1000, 5000},
	}
	fv := Build(ev, nil, BuildConfig{HiThreshold: 0.6})
	if !fv.HasSpatial {
		t.Fatal("HasSpatial should be true")
	}
	if fv.KSpatial <= 0 {
		t.Fatalf("expected positive spatial slope, got %v", fv.KSpatial)
	}
	if fv.PersistenceH != 2 { // 0.7 and 0.9
		t.Fatalf("expected PersistenceH=2 at hi=0.6, got %d", fv.PersistenceH)
	}
	if fv.P50MaxHorizonMs != 5000 {
		t.Fatalf("expected argmax horizon 5000, got %d", fv.P50MaxHorizonMs)
	}
}

func TestBuildTemporalRisingSlope(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	window := make([]Sample, 0, 5)
	for i := 0; i < 5; i++ {
		ev := &pb.ScoreEvent{
			TimestampNs:  t0.Add(time.Duration(i*100) * time.Millisecond).UnixNano(),
			P50TrendPred: float32(0.1 * float64(i+1)),
		}
		window = append(window, Sample{Event: ev, Received: t0.Add(time.Duration(i*100) * time.Millisecond)})
	}
	fv := Build(window[len(window)-1].Event, window, BuildConfig{HiThreshold: 0.4})
	if fv.KTemporal <= 0 {
		t.Fatalf("expected positive temporal slope, got %v", fv.KTemporal)
	}
	// Variance should be > 0 for a monotone sequence.
	if fv.Variance <= 0 {
		t.Fatalf("expected positive variance, got %v", fv.Variance)
	}
	// Most-recent run above 0.4 = samples 4 and 5 (values 0.4 and 0.5),
	// spanning a single 100ms tick.
	if fv.DurationAboveHiMs != 100 {
		t.Fatalf("expected duration_above_hi_ms=100, got %d", fv.DurationAboveHiMs)
	}
}

func TestBuildTemporalFlatSlope(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	window := make([]Sample, 0, 5)
	for i := 0; i < 5; i++ {
		ev := &pb.ScoreEvent{
			TimestampNs:  t0.Add(time.Duration(i*100) * time.Millisecond).UnixNano(),
			P50TrendPred: 0.2,
		}
		window = append(window, Sample{Event: ev, Received: t0.Add(time.Duration(i*100) * time.Millisecond)})
	}
	fv := Build(window[len(window)-1].Event, window, BuildConfig{HiThreshold: 0.5})
	if math.Abs(fv.KTemporal) > 1e-6 {
		t.Fatalf("expected ~0 temporal slope on flat input, got %v", fv.KTemporal)
	}
	if fv.Variance > 1e-12 {
		t.Fatalf("expected ~0 variance on flat input, got %v", fv.Variance)
	}
	if fv.DurationAboveHiMs != 0 {
		t.Fatalf("expected duration_above_hi_ms=0 (latest below threshold), got %d", fv.DurationAboveHiMs)
	}
}

func TestWindowRingBuffer(t *testing.T) {
	w := NewWindow(3)
	t0 := time.Unix(1_700_000_000, 0)
	for i := 1; i <= 5; i++ {
		w.Push(&pb.ScoreEvent{SampleId: int64(i), P50TrendPred: float32(i)}, t0)
	}
	snap := w.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("expected 3 samples after wrap, got %d", len(snap))
	}
	wantIDs := []int64{3, 4, 5}
	for i, s := range snap {
		if s.Event.SampleId != wantIDs[i] {
			t.Fatalf("snapshot[%d]: want sample_id=%d got %d", i, wantIDs[i], s.Event.SampleId)
		}
	}
}

func TestWindowDeduplicates(t *testing.T) {
	w := NewWindow(5)
	t0 := time.Unix(1_700_000_000, 0)
	w.Push(&pb.ScoreEvent{SampleId: 7}, t0)
	w.Push(&pb.ScoreEvent{SampleId: 7}, t0)
	if got := w.Len(); got != 1 {
		t.Fatalf("expected 1 after dedup, got %d", got)
	}
}
