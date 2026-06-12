// Package scoreclient maintains gRPC subscriptions to one or more pods
// exposing the gordion.contention.ContentionStream API. The Pool stores the
// latest ScoreEvent per (target, pod) key atomically so consumers can read
// snapshots without coordinating with the streaming goroutines.
//
// Reconnect logic: each stream runs in its own goroutine with exponential
// backoff (capped at 10s) plus jitter. The producer is best-effort; a
// disconnected pod simply shows up as Healthy=false until the next Recv.
package scoreclient

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/coding-workspace/simple-mitigation-1/gen/go/contentionpb"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Key identifies one stream within a Pool.
type Key struct {
	Target string
	Pod    string
}

// Snapshot is a per-stream latest-event observation.
type Snapshot struct {
	Key
	Event   *pb.ScoreEvent // may be nil if no event has arrived yet
	AgeMs   int64          // -1 if no event has ever arrived
	Healthy bool           // true iff an event arrived within Pool.Latest's stale window
}

// SubReq mirrors the on-wire ScoreSubscribeReq filter.
type SubReq struct {
	OnlyMethods  []string
	MinP50Trend  float32
	MinTailTrend float32
}

type stream struct {
	key      Key
	addr     string
	sub      SubReq
	logger   *slog.Logger
	cancel   context.CancelFunc
	lastEvt  atomic.Pointer[pb.ScoreEvent]
	lastRecv atomic.Int64 // unix nanos; 0 means never
}

// Pool tracks subscriptions keyed by (target, pod). Concurrent-safe.
type Pool struct {
	sub     SubReq
	logger  *slog.Logger
	mu      sync.RWMutex
	streams map[Key]*stream
}

// NewPool returns an empty Pool. Add/Remove are called by the consumer as
// pods come and go.
func NewPool(sub SubReq, logger *slog.Logger) *Pool {
	if logger == nil {
		logger = slog.Default()
	}
	return &Pool{
		sub:     sub,
		logger:  logger,
		streams: map[Key]*stream{},
	}
}

// Add starts (or restarts) a subscription for key against addr (host:port).
// Idempotent on the (key, addr) pair. If key already exists with a different
// addr, the old stream is cancelled and a new one started.
func (p *Pool) Add(ctx context.Context, key Key, addr string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.streams[key]; ok {
		if s.addr == addr {
			return
		}
		s.cancel()
		delete(p.streams, key)
	}
	sctx, cancel := context.WithCancel(ctx)
	s := &stream{
		key:    key,
		addr:   addr,
		sub:    p.sub,
		logger: p.logger.With("target", key.Target, "pod", key.Pod, "addr", addr),
		cancel: cancel,
	}
	p.streams[key] = s
	go s.run(sctx)
}

// Remove tears down the subscription for key, if any.
func (p *Pool) Remove(key Key) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.streams[key]; ok {
		s.cancel()
		delete(p.streams, key)
	}
}

// Latest returns a snapshot for every active stream. healthy is true iff an
// event arrived within `stale` of now.
func (p *Pool) Latest(stale time.Duration) []Snapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]Snapshot, 0, len(p.streams))
	now := time.Now().UnixNano()
	for k, s := range p.streams {
		e := s.lastEvt.Load()
		rcv := s.lastRecv.Load()
		ageMs := int64(-1)
		healthy := false
		if rcv != 0 {
			age := time.Duration(now - rcv)
			ageMs = age.Milliseconds()
			healthy = age <= stale
		}
		out = append(out, Snapshot{
			Key: k, Event: e, AgeMs: ageMs, Healthy: healthy,
		})
	}
	return out
}

// LatestForTarget filters Latest to a single target name.
func (p *Pool) LatestForTarget(target string, stale time.Duration) []Snapshot {
	all := p.Latest(stale)
	out := all[:0]
	for _, s := range all {
		if s.Target == target {
			out = append(out, s)
		}
	}
	return out
}

// Close tears down every stream.
func (p *Pool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, s := range p.streams {
		s.cancel()
		delete(p.streams, k)
	}
}

func (s *stream) run(ctx context.Context) {
	backoff := 500 * time.Millisecond
	const maxBackoff = 10 * time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		err := s.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		s.logger.Warn("score stream ended; will reconnect", "err", err, "backoff", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(jitter(backoff)):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func (s *stream) runOnce(ctx context.Context) error {
	conn, err := grpc.NewClient(s.addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	cli := pb.NewContentionStreamClient(conn)
	req := &pb.ScoreSubscribeReq{
		OnlyMethods:  s.sub.OnlyMethods,
		MinP50Trend:  s.sub.MinP50Trend,
		MinTailTrend: s.sub.MinTailTrend,
	}
	sub, err := cli.Subscribe(ctx, req)
	if err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	s.logger.Info("score stream subscribed")
	for {
		ev, err := sub.Recv()
		if err != nil {
			return fmt.Errorf("recv: %w", err)
		}
		s.lastEvt.Store(ev)
		s.lastRecv.Store(time.Now().UnixNano())
	}
}

// jitter adds 0-25% random delay on top of d to avoid synchronised reconnect storms.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return d + time.Duration(rand.Int63n(int64(d/4+1)))
}
