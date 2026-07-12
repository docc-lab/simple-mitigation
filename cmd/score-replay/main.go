// score-replay serves a captured Gordion score trace (the *_sim.json files
// produced by score_replay.py --sim-json) over the ContentionStream gRPC
// interface, so the real mitigation-controller can be driven entirely
// offline: no cluster, no victim pods, no aggressors.
//
//	go run ./cmd/score-replay -data staircase_kgop_sim.json -addr :7900
//	OFFLINE_ADDRS="search=127.0.0.1:7900" ... ./mitigation-controller
//
// Each subscriber replays the trace from the beginning at the cadence
// encoded in offset_ms (divided by -speed). sample_id is monotone per
// subscriber so the controller's window dedup behaves as with a live pod.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"time"

	pb "github.com/coding-workspace/simple-mitigation-1/gen/go/contentionpb"
	"google.golang.org/grpc"
)

type simSample struct {
	OffsetMs float64 `json:"offset_ms"`
	P50      float64 `json:"p50_contention_score"`
	P90      float64 `json:"p90_contention_score"`
}

type simTrace struct {
	ServiceName string      `json:"service_name"`
	Samples     []simSample `json:"samples"`
}

type server struct {
	pb.UnimplementedContentionStreamServer
	trace   simTrace
	speed   float64
	loop    bool
	version string
	logger  *slog.Logger
}

func (s *server) Subscribe(req *pb.ScoreSubscribeReq, stream pb.ContentionStream_SubscribeServer) error {
	s.logger.Info("subscriber connected",
		"min_p50_trend", req.GetMinP50Trend(),
		"min_tail_trend", req.GetMinTailTrend())
	var sampleID int64
	for pass := 1; ; pass++ {
		start := time.Now()
		t0 := s.trace.Samples[0].OffsetMs
		for i := range s.trace.Samples {
			sm := &s.trace.Samples[i]
			due := time.Duration((sm.OffsetMs-t0)/s.speed) * time.Millisecond
			if wait := due - time.Since(start); wait > 0 {
				select {
				case <-stream.Context().Done():
					return stream.Context().Err()
				case <-time.After(wait):
				}
			}
			if float32(sm.P50) < req.GetMinP50Trend() ||
				float32(sm.P90) < req.GetMinTailTrend() {
				continue
			}
			sampleID++
			ev := &pb.ScoreEvent{
				SampleId:       sampleID,
				TimestampNs:    time.Now().UnixNano(),
				Service:        s.trace.ServiceName,
				P50TrendPred:   float32(sm.P50),
				TailTrendLabel: float32(sm.P90),
				ModelVersion:   s.version,
				SourceKind:     "replay",
			}
			if err := stream.Send(ev); err != nil {
				s.logger.Info("subscriber gone", "sent", sampleID, "err", err)
				return err
			}
		}
		s.logger.Info("trace pass complete", "pass", pass, "events", sampleID)
		if !s.loop {
			return nil
		}
	}
}

func main() {
	data := flag.String("data", "", "Gordion sim JSON trace (required)")
	addr := flag.String("addr", "127.0.0.1:7900", "listen address")
	speed := flag.Float64("speed", 1.0, "time acceleration factor")
	loop := flag.Bool("loop", false, "restart the trace when it ends")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if *data == "" {
		logger.Error("-data is required")
		os.Exit(2)
	}
	raw, err := os.ReadFile(*data)
	if err != nil {
		logger.Error("read trace", "err", err)
		os.Exit(2)
	}
	var trace simTrace
	if err := json.Unmarshal(raw, &trace); err != nil {
		logger.Error("parse trace", "err", err)
		os.Exit(2)
	}
	if len(trace.Samples) == 0 {
		logger.Error("trace has no samples")
		os.Exit(2)
	}
	if trace.ServiceName == "" {
		trace.ServiceName = "search"
	}
	span := (trace.Samples[len(trace.Samples)-1].OffsetMs - trace.Samples[0].OffsetMs) / 1000.0

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		logger.Error("listen", "err", err)
		os.Exit(2)
	}
	gs := grpc.NewServer()
	pb.RegisterContentionStreamServer(gs, &server{
		trace:   trace,
		speed:   *speed,
		loop:    *loop,
		version: fmt.Sprintf("replay:%s", *data),
		logger:  logger,
	})
	logger.Info("score-replay serving",
		"addr", *addr, "trace", *data, "service", trace.ServiceName,
		"samples", len(trace.Samples), "span_s", fmt.Sprintf("%.1f", span),
		"speed", *speed, "loop", *loop)
	if err := gs.Serve(lis); err != nil {
		logger.Error("serve", "err", err)
		os.Exit(1)
	}
}
