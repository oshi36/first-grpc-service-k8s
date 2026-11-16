// Live dashboard for "Your First gRPC Service on Kubernetes".
//
// Runs INSIDE the cluster. It is both:
//   - the gRPC CLIENT: three lanes send traffic to the greeter in three ways
//     (ClusterIP / headless+round_robin / Gateway), and
//   - an observer: it polls the Kubernetes API for pods, probe config and
//     Warning events, and streams everything to a browser via SSE.
//
// Env:
//
//	NAMESPACE           (default grpc-demo)
//	DEPLOYMENT          (default greeter)
//	SELECTOR            (default app=greeter)
//	TARGET_CLUSTERIP    (default greeter.<ns>.svc.cluster.local:50051)
//	TARGET_HEADLESS     (default dns:///greeter-headless.<ns>.svc.cluster.local:50051)
//	TARGET_GATEWAY      (optional; otherwise auto-discovered Envoy Service)
//	GATEWAY_NAME        (default grpc-gw)
//	GATEWAY_AUTHORITY   (default greeter.demo.local)
//	RATE                RPCs/sec per lane (default 20)
//	LISTEN              (default :8080)
package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed ui
var uiFS embed.FS

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

type Pod struct {
	Name     string `json:"name"`
	IP       string `json:"ip"`
	Node     string `json:"node"`
	Status   string `json:"status"` // Ready | Starting | CrashLoopBackOff | Terminating | Pending | ...
	Ready    bool   `json:"ready"`
	Restarts int    `json:"restarts"`
	AgeSec   int    `json:"ageSec"`
}

type Event struct {
	Time   time.Time `json:"time"`
	Source string    `json:"source"` // k8s | app
	Object string    `json:"object"`
	Reason string    `json:"reason"`
	Msg    string    `json:"msg"`
	Count  int       `json:"count"`
}

type Deploy struct {
	Replicas   int    `json:"replicas"`
	Ready      int    `json:"ready"`
	Readiness  string `json:"readiness"`
	Liveness   string `json:"liveness"`
	MaxConnAge string `json:"maxConnAge"`
	Found      bool   `json:"found"`
}

type State struct {
	Time      time.Time   `json:"time"`
	Namespace string      `json:"namespace"`
	Deploy    Deploy      `json:"deploy"`
	Pods      []Pod       `json:"pods"`
	Lanes     []LaneState `json:"lanes"`
	Events    []Event     `json:"events"`
	KubeErr   string      `json:"kubeErr"`
}

type App struct {
	kube             *Kube
	ns, deploy, sel  string
	gwName, gwTarget string
	gwFixed          bool
	lanes            []*Lane
	mu               sync.Mutex
	pods             []Pod
	dep              Deploy
	k8sEvents        []Event
	appEvents        []Event
	kubeErr          string
	prevPods         map[string]Pod
}

func (a *App) appEvent(msg string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.appEvents = append(a.appEvents, Event{Time: time.Now(), Source: "app", Msg: msg})
	if len(a.appEvents) > 60 {
		a.appEvents = a.appEvents[len(a.appEvents)-60:]
	}
	log.Print(msg)
}

func (a *App) gatewayTarget() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.gwTarget
}

func podStatus(p struct {
	Ready        bool `json:"ready"`
	RestartCount int  `json:"restartCount"`
	State        struct {
		Waiting    *struct{ Reason string } `json:"waiting"`
		Running    *struct{}                `json:"running"`
		Terminated *struct{ Reason string } `json:"terminated"`
	} `json:"state"`
}) string {
	switch {
	case p.State.Waiting != nil && p.State.Waiting.Reason != "":
		return p.State.Waiting.Reason
	case p.State.Terminated != nil:
		return "Terminated"
	case p.Ready:
		return "Ready"
	case p.State.Running != nil:
		return "NotReady"
	}
	return "Pending"
}

func (a *App) pollPods() {
	pl, err := a.kube.Pods(a.ns, a.sel)
	if err != nil {
		a.mu.Lock()
		a.kubeErr = err.Error()
		a.mu.Unlock()
		return
	}
	var pods []Pod
	for _, it := range pl.Items {
		p := Pod{Name: it.Metadata.Name, IP: it.Status.PodIP, Node: it.Spec.NodeName, Status: it.Status.Phase}
		if t, err := time.Parse(time.RFC3339, it.Metadata.CreationTimestamp); err == nil {
			p.AgeSec = int(time.Since(t).Seconds())
		}
		if len(it.Status.ContainerStatuses) > 0 {
			cs := it.Status.ContainerStatuses[0]
			p.Restarts = cs.RestartCount
			p.Ready = cs.Ready
			p.Status = podStatus(cs)
		}
		if it.Metadata.DeletionTimestamp != nil {
			p.Status, p.Ready = "Terminating", false
		}
		pods = append(pods, p)
	}
	sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })

	// Diff against the previous poll to narrate what changed.
	cur := map[string]Pod{}
	for _, p := range pods {
		cur[p.Name] = p
		old, existed := a.prevPods[p.Name]
		switch {
		case !existed && a.prevPods != nil:
			a.appEvent(fmt.Sprintf("pod %s created", p.Name))
		case existed && !old.Ready && p.Ready:
			a.appEvent(fmt.Sprintf("pod %s is Ready (health check SERVING) → added to Service endpoints", p.Name))
		case existed && old.Ready && !p.Ready && p.Status != "Terminating":
			a.appEvent(fmt.Sprintf("pod %s became NotReady → removed from Service endpoints", p.Name))
		case existed && old.Status != "Terminating" && p.Status == "Terminating":
			a.appEvent(fmt.Sprintf("pod %s terminating (preStop sleep → SIGTERM → health NOT_SERVING → GracefulStop)", p.Name))
		}
		if existed && p.Restarts > old.Restarts {
			a.appEvent(fmt.Sprintf("pod %s RESTARTED by kubelet (restart #%d): liveness probe failed", p.Name, p.Restarts))
		}
	}
	for n := range a.prevPods {
		if _, ok := cur[n]; !ok {
			a.appEvent(fmt.Sprintf("pod %s deleted", n))
		}
	}
	a.prevPods = cur

	a.mu.Lock()
	a.pods, a.kubeErr = pods, ""
	a.mu.Unlock()
}

func (a *App) pollDeploy() {
	d, err := a.kube.Deployment(a.ns, a.deploy)
	dep := Deploy{}
	if err == nil {
		dep.Found = true
		dep.Replicas, dep.Ready = d.Spec.Replicas, d.Status.ReadyReplicas
		if cs := d.Spec.Template.Spec.Containers; len(cs) > 0 {
			dep.Readiness = cs[0].ReadinessProbe.kind()
			dep.Liveness = cs[0].LivenessProbe.kind()
			for _, e := range cs[0].Env {
				if e.Name == "MAX_CONN_AGE" {
					dep.MaxConnAge = e.Value
				}
			}
		}
	}
	a.mu.Lock()
	a.dep = dep
	a.mu.Unlock()
}

func (a *App) pollEvents() {
	el, err := a.kube.WarningEvents(a.ns)
	if err != nil {
		return
	}
	var evs []Event
	for _, it := range el.Items {
		ts := it.LastTimestamp
		if it.Series != nil && it.Series.LastObservedTime != "" {
			ts = it.Series.LastObservedTime
		}
		if ts == "" {
			ts = it.EventTime
		}
		t, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			t, err = time.Parse(time.RFC3339Nano, ts)
		}
		if err != nil || time.Since(t) > 10*time.Minute {
			continue
		}
		evs = append(evs, Event{Time: t, Source: "k8s", Object: it.InvolvedObject.Name, Reason: it.Reason, Msg: it.Message, Count: it.Count})
	}
	a.mu.Lock()
	a.k8sEvents = evs
	a.mu.Unlock()
}

func (a *App) pollGateway() {
	if a.gwFixed {
		return
	}
	sel := fmt.Sprintf("gateway.envoyproxy.io/owning-gateway-name=%s,gateway.envoyproxy.io/owning-gateway-namespace=%s", a.gwName, a.ns)
	sl, err := a.kube.Services("envoy-gateway-system", sel)
	t := ""
	if err == nil && len(sl.Items) > 0 {
		t = sl.Items[0].Metadata.Name + ".envoy-gateway-system.svc.cluster.local:80"
	}
	a.mu.Lock()
	changed := t != a.gwTarget
	a.gwTarget = t
	a.mu.Unlock()
	if changed && t != "" {
		a.appEvent("Gateway is up: Envoy Service discovered → Gateway lane starts sending")
	}
}

func (a *App) loop(ctx context.Context) {
	fast := time.NewTicker(1 * time.Second)
	slow := time.NewTicker(3 * time.Second)
	a.pollPods()
	a.pollDeploy()
	a.pollEvents()
	a.pollGateway()
	for {
		select {
		case <-ctx.Done():
			return
		case <-fast.C:
			a.pollPods()
			a.pollDeploy()
		case <-slow.C:
			a.pollEvents()
			a.pollGateway()
		}
	}
}

func (a *App) state() State {
	a.mu.Lock()
	st := State{Time: time.Now(), Namespace: a.ns, Deploy: a.dep, Pods: append([]Pod{}, a.pods...), KubeErr: a.kubeErr, Lanes: []LaneState{}}
	events := append(append([]Event{}, a.k8sEvents...), a.appEvents...)
	a.mu.Unlock()

	var known []string
	for _, p := range st.Pods {
		if p.Status != "Terminating" {
			known = append(known, p.Name)
		}
	}
	for _, l := range a.lanes {
		st.Lanes = append(st.Lanes, l.State(known))
	}
	sort.Slice(events, func(i, j int) bool { return events[i].Time.After(events[j].Time) })
	if len(events) > 40 {
		events = events[:40]
	}
	st.Events = events
	return st
}

func (a *App) lane(id string) *Lane {
	for _, l := range a.lanes {
		if l.ID == id {
			return l
		}
	}
	return nil
}

func writeErr(w http.ResponseWriter, err error) {
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func main() {
	ns := env("NAMESPACE", "grpc-demo")
	rate, _ := strconv.Atoi(env("RATE", "20"))
	a := &App{kube: NewKube(), ns: ns, deploy: env("DEPLOYMENT", "greeter"), sel: env("SELECTOR", "app=greeter"),
		gwName: env("GATEWAY_NAME", "grpc-gw")}
	if t := os.Getenv("TARGET_GATEWAY"); t != "" {
		a.gwTarget, a.gwFixed = t, true
	}
	cip := env("TARGET_CLUSTERIP", "greeter."+ns+".svc.cluster.local:50051")
	hl := env("TARGET_HEADLESS", "dns:///greeter-headless."+ns+".svc.cluster.local:50051")
	auth := env("GATEWAY_AUTHORITY", "greeter.demo.local")

	a.lanes = []*Lane{
		NewLane("clusterip", "ClusterIP Service", "default client · pick_first",
			[]string{"client", "1 TCP connection", "ClusterIP VIP (kube-proxy)", "one pod"},
			"pick_first", "", func() string { return cip }, rate, a.appEvent),
		NewLane("headless", "Headless + round_robin", "client-side load balancing",
			[]string{"client", "DNS → every pod IP", "1 connection per pod", "round_robin per RPC"},
			"round_robin", "", func() string { return hl }, rate, a.appEvent),
		NewLane("gateway", "Gateway API · GRPCRoute", "L7 proxy (Envoy) balances per request",
			[]string{"client", "1 connection", "Envoy (GRPCRoute)", "per-RPC to every pod"},
			"pick_first", auth, a.gatewayTarget, rate, a.appEvent),
	}

	ctx := context.Background()
	go a.loop(ctx)
	for _, l := range a.lanes {
		go l.Run(ctx)
	}

	mux := http.NewServeMux()
	sub, _ := fs.Sub(uiFS, "ui")
	mux.Handle("/", http.FileServer(http.FS(sub)))

	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(a.state())
	})
	// Server-Sent Events: push state twice a second.
	mux.HandleFunc("/api/stream", func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no streaming", 500)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		tk := time.NewTicker(500 * time.Millisecond)
		defer tk.Stop()
		for {
			b, _ := json.Marshal(a.state())
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
			select {
			case <-r.Context().Done():
				return
			case <-tk.C:
			}
		}
	})
	mux.HandleFunc("POST /api/scale", func(w http.ResponseWriter, r *http.Request) {
		n, err := strconv.Atoi(r.URL.Query().Get("replicas"))
		if err != nil || n < 1 || n > 8 {
			http.Error(w, "replicas must be 1..8", 400)
			return
		}
		a.appEvent(fmt.Sprintf("UI: scale deployment/%s → %d replicas", a.deploy, n))
		writeErr(w, a.kube.Scale(a.ns, a.deploy, n))
	})
	mux.HandleFunc("POST /api/kill", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Query().Get("pod")
		if !strings.HasPrefix(p, a.deploy) {
			http.Error(w, "not a greeter pod", 400)
			return
		}
		a.appEvent(fmt.Sprintf("UI: delete pod %s (watch for errors: there should be none)", p))
		writeErr(w, a.kube.DeletePod(a.ns, p))
	})
	mux.HandleFunc("POST /api/connage", func(w http.ResponseWriter, r *http.Request) {
		v := r.URL.Query().Get("value")
		if v == "off" {
			v = ""
		} else if _, err := time.ParseDuration(v); err != nil {
			http.Error(w, "bad duration", 400)
			return
		}
		a.appEvent(fmt.Sprintf("UI: set MAX_CONN_AGE=%q on deployment/%s (rolling update)", v, a.deploy))
		writeErr(w, a.kube.SetEnv(a.ns, a.deploy, "greeter", "MAX_CONN_AGE", v))
	})
	mux.HandleFunc("POST /api/lane", func(w http.ResponseWriter, r *http.Request) {
		l := a.lane(r.URL.Query().Get("id"))
		if l == nil {
			http.Error(w, "no such lane", 404)
			return
		}
		switch r.URL.Query().Get("action") {
		case "pause":
			l.enabled.Store(false)
		case "resume":
			l.enabled.Store(true)
		case "reconnect":
			l.Reconnect()
		case "reset":
			l.Reset()
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/reset", func(w http.ResponseWriter, r *http.Request) {
		for _, l := range a.lanes {
			l.Reset()
		}
		a.mu.Lock()
		a.appEvents = nil
		a.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})

	addr := env("LISTEN", ":8080")
	log.Printf("dashboard on %s (namespace %s, %d rps per lane)", addr, ns, rate)
	log.Fatal(http.ListenAndServe(addr, mux))
}
