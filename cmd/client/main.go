// Load-test client that makes the gRPC load-balancing surprise visible.
//
// It opens ONE grpc.ClientConn (like a real app would), fires RPCs and
// prints how many replies came from each pod.
//
//	client -target greeter.grpc-demo.svc.cluster.local:50051 -n 300
//	client -target dns:///greeter-headless.grpc-demo.svc.cluster.local:50051 -lb round_robin -n 300
//	client -target <envoy-svc>:80 -authority greeter.demo.local -n 300
//	client -target ... -n 0 -rate 20      # run forever, tally every 5s
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	greeterv1 "grpc-k8s-demo/gen/greeter/v1"
)

type tally struct {
	mu     sync.Mutex
	counts map[string]int
	errs   int
}

func (t *tally) add(pod string, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err != nil {
		t.errs++
		return
	}
	t.counts[pod]++
}

func (t *tally) print(title string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	total := t.errs
	pods := make([]string, 0, len(t.counts))
	for p, c := range t.counts {
		pods = append(pods, p)
		total += c
	}
	sort.Strings(pods)
	fmt.Printf("\n── %s ── %d requests, %d pod(s) hit\n", title, total, len(pods))
	for _, p := range pods {
		c := t.counts[p]
		bar := strings.Repeat("█", c*40/max(total, 1))
		fmt.Printf("  %-36s %5d  %s\n", p, c, bar)
	}
	if t.errs > 0 {
		fmt.Printf("  %-36s %5d\n", "ERRORS", t.errs)
	}
}

func main() {
	target := flag.String("target", "localhost:50051", "gRPC target, e.g. host:port or dns:///host:port")
	n := flag.Int("n", 300, "number of RPCs (0 = run until Ctrl-C)")
	rate := flag.Int("rate", 0, "RPCs per second (0 = as fast as possible)")
	lb := flag.String("lb", "pick_first", "client load-balancing policy: pick_first | round_robin")
	authority := flag.String("authority", "", "override :authority (needed when routing by hostname through a Gateway)")
	report := flag.Duration("report", 5*time.Second, "print a running tally this often when -n 0")
	flag.Parse()

	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultServiceConfig(fmt.Sprintf(`{"loadBalancingConfig":[{"%s":{}}]}`, *lb)),
	}
	if *authority != "" {
		opts = append(opts, grpc.WithAuthority(*authority))
	}

	conn, err := grpc.NewClient(*target, opts...)
	if err != nil {
		log.Fatalf("new client: %v", err)
	}
	defer conn.Close()
	c := greeterv1.NewGreeterClient(conn)

	fmt.Printf("target=%s  lb=%s  authority=%q  (one ClientConn, reused for every RPC)\n", *target, *lb, *authority)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	t := &tally{counts: map[string]int{}}
	var tick <-chan time.Time
	if *rate > 0 {
		tk := time.NewTicker(time.Second / time.Duration(*rate))
		defer tk.Stop()
		tick = tk.C
	}
	var rep <-chan time.Time
	if *n == 0 {
		rt := time.NewTicker(*report)
		defer rt.Stop()
		rep = rt.C
	}

	for i := 0; *n == 0 || i < *n; i++ {
		if tick != nil {
			select {
			case <-tick:
			case <-ctx.Done():
				t.print("final")
				return
			}
		}
		select {
		case <-ctx.Done():
			t.print("final")
			return
		case <-rep:
			t.print(time.Now().Format("15:04:05"))
		default:
		}
		rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		resp, err := c.SayHello(rctx, &greeterv1.HelloRequest{Name: "KCD"})
		cancel()
		if err != nil && ctx.Err() == nil && t.errs < 3 {
			log.Printf("rpc error: %v", err)
		}
		t.add(resp.GetPod(), err)
	}
	t.print("result")
}
