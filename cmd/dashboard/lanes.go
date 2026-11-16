package main

// A Lane is one gRPC client (ONE ClientConn, like a real app) that sends a
// steady stream of RPCs and records which pod answered each one.

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	greeterv1 "grpc-k8s-demo/gen/greeter/v1"
)

const window = 5 // seconds used for the live RPS bars

type bucket struct {
	sec  int64
	pods map[string]int
	errs int
}

type Lane struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Subtitle  string   `json:"subtitle"`
	Path      []string `json:"path"`
	LB        string   `json:"lb"`
	Authority string   `json:"authority"`

	target  func() string
	enabled atomic.Bool
	rate    int
	onEvent func(string)

	mu        sync.Mutex
	buckets   [window]bucket
	total     map[string]int
	totalErr  int
	lastErr   string
	conn      *grpc.ClientConn
	curTarget string
	seen      map[string]bool
	reconnect chan struct{}
}

type LanePod struct {
	Name  string  `json:"name"`
	RPS   float64 `json:"rps"`
	Total int     `json:"total"`
}

type LaneState struct {
	ID       string    `json:"id"`
	Title    string    `json:"title"`
	Subtitle string    `json:"subtitle"`
	Path     []string  `json:"path"`
	LB       string    `json:"lb"`
	Enabled  bool      `json:"enabled"`
	Target   string    `json:"target"`
	Status   string    `json:"status"`
	Pods     []LanePod `json:"pods"`
	RPS      float64   `json:"rps"`
	ErrRPS   float64   `json:"errRps"`
	TotalErr int       `json:"totalErr"`
	LastErr  string    `json:"lastErr"`
}

func NewLane(id, title, subtitle string, path []string, lb, authority string, target func() string, rate int, onEvent func(string)) *Lane {
	l := &Lane{ID: id, Title: title, Subtitle: subtitle, Path: path, LB: lb, Authority: authority,
		target: target, rate: rate, onEvent: onEvent,
		total: map[string]int{}, seen: map[string]bool{}, reconnect: make(chan struct{}, 1)}
	l.enabled.Store(true)
	return l
}

func (l *Lane) dial(target string) (*grpc.ClientConn, error) {
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultServiceConfig(fmt.Sprintf(`{"loadBalancingConfig":[{"%s":{}}]}`, l.LB)),
	}
	if l.Authority != "" {
		opts = append(opts, grpc.WithAuthority(l.Authority))
	}
	return grpc.NewClient(target, opts...)
}

func (l *Lane) Run(ctx context.Context) {
	tk := time.NewTicker(time.Second / time.Duration(l.rate))
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-l.reconnect:
			l.mu.Lock()
			if l.conn != nil {
				l.conn.Close()
				l.conn = nil
			}
			l.mu.Unlock()
			l.onEvent(fmt.Sprintf("%s: closed connection, dialing fresh", l.Title))
		case <-tk.C:
		}
		if !l.enabled.Load() {
			continue
		}
		t := l.target()
		l.mu.Lock()
		if t != l.curTarget && l.conn != nil {
			l.conn.Close()
			l.conn = nil
		}
		if l.conn == nil && t != "" {
			c, err := l.dial(t)
			if err != nil {
				l.lastErr = err.Error()
			} else {
				l.conn, l.curTarget = c, t
			}
		}
		conn := l.conn
		l.mu.Unlock()
		if conn == nil {
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, 800*time.Millisecond)
		resp, err := greeterv1.NewGreeterClient(conn).SayHello(rctx, &greeterv1.HelloRequest{Name: "gRPConf"})
		cancel()
		l.record(resp.GetPod(), err)
	}
}

func (l *Lane) cur(now int64) *bucket {
	b := &l.buckets[now%window]
	if b.sec != now {
		*b = bucket{sec: now, pods: map[string]int{}}
	}
	return b
}

func (l *Lane) record(pod string, err error) {
	now := time.Now().Unix()
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.cur(now)
	if err != nil {
		b.errs++
		l.totalErr++
		if s, ok := status.FromError(err); ok {
			l.lastErr = fmt.Sprintf("%s: %s", s.Code(), s.Message())
		} else {
			l.lastErr = err.Error()
		}
		return
	}
	b.pods[pod]++
	l.total[pod]++
	if !l.seen[pod] {
		l.seen[pod] = true
		go l.onEvent(fmt.Sprintf("%s: first RPC served by %s", l.Title, pod))
	}
}

func (l *Lane) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buckets = [window]bucket{}
	l.total = map[string]int{}
	l.seen = map[string]bool{}
	l.totalErr, l.lastErr = 0, ""
}

func (l *Lane) Reconnect() {
	select {
	case l.reconnect <- struct{}{}:
	default:
	}
}

// State returns live numbers. knownPods (current pods in the cluster) are
// always listed, even at 0 RPS: an idle pod is the whole point of Act 4.
func (l *Lane) State(knownPods []string) LaneState {
	now := time.Now().Unix()
	l.mu.Lock()
	defer l.mu.Unlock()
	rps := map[string]float64{}
	errs := 0
	// Use completed seconds only (exclude the current partial one).
	for i := int64(1); i <= window; i++ {
		b := l.buckets[(now-i)%window]
		if b.sec != now-i {
			continue
		}
		for p, c := range b.pods {
			rps[p] += float64(c) / window
		}
		errs += b.errs
	}
	names := map[string]bool{}
	for _, p := range knownPods {
		names[p] = true
	}
	for p := range rps {
		names[p] = true
	}
	st := LaneState{ID: l.ID, Title: l.Title, Subtitle: l.Subtitle, Path: l.Path, LB: l.LB,
		Enabled: l.enabled.Load(), Target: l.curTarget, TotalErr: l.totalErr, LastErr: l.lastErr,
		ErrRPS: float64(errs) / window, Pods: []LanePod{}}
	for n := range names {
		st.Pods = append(st.Pods, LanePod{Name: n, RPS: rps[n], Total: l.total[n]})
		st.RPS += rps[n]
	}
	sort.Slice(st.Pods, func(i, j int) bool { return st.Pods[i].Name < st.Pods[j].Name })
	switch {
	case !st.Enabled:
		st.Status = "paused"
	case l.target() == "":
		st.Status = "waiting"
	case st.RPS == 0 && errs > 0:
		st.Status = "failing"
	default:
		st.Status = "running"
	}
	return st
}
