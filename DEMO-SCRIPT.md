# Presenter script - gRPC Conf Bengaluru

**Talk:** Your First gRPC Service on Kubernetes: A Beginner's Roadmap
**Demo length:** about 20 minutes across 4 acts (plus 2 minutes of buffer)

For each step, this script tells you what to **type**, where to **point** on the dashboard, and what to **say**.

---

## 0. Screen layout

```
┌──────────────────────────────────────────┬─────────────────────┐
│                                          │  TERMINAL A         │
│        BROWSER: http://localhost:8081    │  (font size 20+)    │
│  (zoom: 1080p → 100–110% · 720p → 90%)   │  you type here      │
│                                          ├─────────────────────┤
│                                          │  TERMINAL B         │
│                                          │  port-forwards      │
└──────────────────────────────────────────┴─────────────────────┘
```

The dashboard has five areas. These names are used throughout the script:

| Area | Where | What it shows |
|---|---|---|
| **Caption bar** | top, blue strip | the one-line takeaway for the current act (press `0`-`4` to switch) |
| **Deployment panel** | top-left | Ready replicas, readiness/liveness probe type, MaxConnectionAge, the - / + replica buttons |
| **Pod cards** | top-right | one card per pod, with a stable color, status, ↻ restart count, IP and node. ✕ deletes the pod |
| **Lanes** | middle, 3 columns | three clients sending the same traffic three ways. The big number is how many pods are actually serving. The bars show RPC/s per pod. The pill gives a verdict |
| **"What just happened" feed** | bottom | Kubernetes Warning events (K8S) and plain-English narration (APP) |

**Keyboard:** `0` Overview · `1` Package · `2` Health · `3` Route · `4` Balance · `r` reset counters.
Switching acts highlights the relevant panels and dims the rest, so the audience always knows where to look.

---

## 1. Before you go on stage (T-30 min)

```bash
make setup     # only if the cluster doesn't exist yet (slow: downloads images)
make reset     # clean slate: deletes greeter, gateway and route; keeps the dashboard
make stage     # builds the image, loads it into kind, (re)deploys the dashboard
```
Terminal B:
```bash
make ui        # leave running → open http://localhost:8081
```
Check before you start:
- [ ] The dashboard dot (top-right) says **live**.
- [ ] Pod cards say *"No greeter pods yet"*.
- [ ] All three lanes say *no healthy endpoints*, or the Gateway lane says *waiting for Gateway*. That's expected at this point.
- [ ] Laptop is on power, notifications are off, display sleep is off.

---

## 2. Act 1: Package it (≈3 min) · press `1`

| Type | Point at | Say |
|---|---|---|
| `make act1` | terminal: the image size | "One static Go binary in a distroless image. No shell, no package manager." |
| (it prints *exec: "sh": not found*) | terminal | "There's no shell. That's good for security, but it means I can't exec a `grpc_health_probe` binary for health checks. Remember that for Act 2." |
| open `Dockerfile`, last lines | `USER 65532:65532` | "Use a **numeric** UID. With `USER nonroot`, a pod with `runAsNonRoot: true` refuses to start, because the kubelet can't verify a name. That's beginner mistake #1." |
| (optional) | Caption bar | "The same image runs the server, the CLI client, and the dashboard you're looking at." |

---

## 3. Act 2: Health checks (≈5 min) · press `2`

### 2a: the copy-paste mistake

| Type | Point at | Say |
|---|---|---|
| show `k8s/01-deployment-WRONG-probes.yaml` | `httpGet: /healthz` | "This is what most of us copy from our REST services." |
| `make act2-wrong` | **Pod cards** appear | "Three pods are created…" |
| wait ~15s | **Deployment panel → Readiness probe: `httpGet ✗`** | "…and they never become Ready." |
| | **Pod cards → `↻` counter turning red** and *CrashLoop* | "The liveness probe fails, so the kubelet keeps restarting a server that is perfectly healthy." |
| | **Feed → yellow `K8S Unhealthy` line** with `malformed HTTP response "\x00\x00\x06\x04…"` | "Here's the kubelet's own error. Those bytes are an HTTP/2 SETTINGS frame. The probe asked in HTTP/1.1, and the server answered in HTTP/2." |
| | **Lanes → `0 / 0 pods serving`, `no healthy endpoints`** | "Not-Ready pods are removed from the Service, so every client gets errors. One wrong probe and the service is down." |
| | | "This is the same root cause as the classic Ingress breakage: **anything in the path that only speaks HTTP/1.1 breaks gRPC.**" |

### 2b: the fix

| Type | Point at | Say |
|---|---|---|
| show `k8s/02-deployment.yaml` | `grpc: { port: 50051 }` | "Kubernetes has a native gRPC probe, GA since 1.27. It calls the standard `grpc.health.v1` service. The port must be a **number**; named ports aren't supported here." |
| | `service: liveness` vs default | "Two different questions. **Readiness:** can I take traffic? **Liveness:** is the process alive? My server says NOT_SERVING for 8 seconds while it warms up, but liveness stays SERVING, so a slow start never causes a restart." |
| `make act2-fix` | **Deployment panel → `grpc ✓` `grpc ✓`** | "Probe type is now gRPC." |
| | **Pod cards → new pods show *Not ready* for ~8s, then turn green *Ready*** | "Watch: Not ready… Not ready… Ready. That's the warm-up." |
| | **Feed → `pod … is Ready (health check SERVING) → added to Service endpoints`** | "Only now does the pod start getting traffic." |
| | **Lanes → bars come alive** | "And traffic flows." |

---

## 4. Act 3: Route it with GRPCRoute (≈4 min) · press `3`

| Type | Point at | Say |
|---|---|---|
| | **Gateway lane: `waiting for Gateway`** | "This third client goes through a Gateway, which doesn't exist yet." |
| show `k8s/05-grpcroute.yaml` | `method: { service: greeter.v1.Greeter }` | "GRPCRoute matches on gRPC concepts: service and method, plus hostname. No URL-path tricks and no vendor annotations." |
| | | "Ingress NGINX, the controller most tutorials still use with a `backend-protocol: GRPC` annotation, was retired in March 2026. Gateway API is the way forward." |
| `make act3` | terminal: `Programmed` | "Platform team owns the GatewayClass, the operator owns the Gateway, and the app team owns this route." |
| | **Feed → `Gateway is up: Envoy Service discovered`** | |
| | **Gateway lane → bars fill, `balanced ✓`** | "Traffic now goes client → Envoy → pods, over HTTP/2 end to end." |
| (Terminal B) `make gw-forward`, then `make act3-call` | terminal: grpcurl output with `"pod": "…"` | "Same route, from my laptop, with grpcurl." |
| | terminal: *404* for `nope.demo.local` | "Wrong hostname means no route matches. The routing is real." |

Stop `gw-forward` (Ctrl-C) after this. You don't need it again.

---

## 5. Act 4: The load-balancing surprise (≈7 min) · press `4`

This is the main moment of the talk. Slow down.

### 4a: the problem (already visible)

| Point at | Say |
|---|---|
| **ClusterIP lane → big red `1` / 3 pods serving, pill `1 pod does all the work`**, one full bar and two `0.0/s` | "I have three replicas, and one does 100% of the work. Nothing is broken. kube-proxy balances **connections**, and a gRPC client opens **one** HTTP/2 connection and sends every RPC over it." |
| path chips: `client → 1 TCP connection → ClusterIP VIP → one pod` | "One connection means one pod." |
| click **Reconnect** on the ClusterIP lane | "If I force a new connection, I land on a different pod… but still only one." |

### 4b: fix #1, client-side load balancing

| Point at | Say |
|---|---|
| **Headless lane → green `3`, even bars, `balanced ✓`** | "A headless Service (`clusterIP: None`) makes DNS return every pod IP. With the `dns:///` scheme and the `round_robin` policy, the client opens a connection to each pod and rotates per RPC." |
| | "You need all three parts. A headless Service with the default pick_first still gives you one pod." |

### 4c: fix #2, an L7 proxy

| Point at | Say |
|---|---|
| **Gateway lane → balanced ✓** | "Or let a proxy do it. Envoy terminates HTTP/2 and balances **every request**, with no client changes. A service mesh works the same way." |

### 4d: the sequel, scaling up

| Do | Point at | Say |
|---|---|---|
| click **+** (3 → 4) | **new pod card pops in, becomes Ready** | "Traffic is growing, so let's scale." |
| wait ~10s | **Headless lane → new pod bar red `0.0/s`, pill `1 pod idle`** · **ClusterIP lane → still 1** · **Gateway lane → 4 pods ✓** | "The new pod is Ready, and the client-side lane ignores it. The client resolved DNS once, its connections are healthy, so it never looks again. The Gateway picked it up immediately because Envoy watches endpoints." |
| click **Set MaxConnectionAge 15s** | **Deployment panel → MaxConnectionAge `15s`**, pods roll one by one | "The server-side fix: tell clients to reconnect every 15 seconds. That's `MaxConnectionAge` in grpc-go. The server sends a GOAWAY, the client reconnects and re-resolves DNS." |
| | **Lanes: errors stay at 0 during the rollout** | "Look at the error counters during the rolling update: zero. That's the graceful drain from Act 2." |
| click **+** (→ 5) once the rollout settles | **Headless lane: new pod's bar starts at 0…** | "Scale once more…" |
| wait 15–30s (keep talking) | **…then fills. Feed: `Headless + round_robin: first RPC served by <new pod>`** | "…and there it is. Expect up to ~30 seconds, because grpc-go re-resolves DNS at most every 30s." |

### 4e: optional, graceful shutdown

| Do | Point at | Say |
|---|---|---|
| click **✕** on the pod the ClusterIP lane is using | **pod card goes grey *Terminating***; **feed: `preStop sleep → SIGTERM → health NOT_SERVING → GracefulStop`** | "Delete the pod that's taking all the ClusterIP traffic." |
| | **ClusterIP lane moves to another pod, errors stay 0** | "The client moves over without a single failed RPC." |

---

## 6. Close (≈1 min) · press `0`

Show `CHECKLIST.md` (or your slide version of it). Say:
> "Package it small and non-root. Use gRPC probes, not HTTP probes. Route with GRPCRoute, not annotations. And remember that Kubernetes balances connections, not requests: use client-side round_robin plus MaxConnectionAge, or an L7 proxy."

---

## 7. If something goes wrong on stage

| Symptom | Fix (say "live demos!" and smile) |
|---|---|
| Dashboard dot shows *reconnecting…* | Terminal B: Ctrl-C, then `make ui` again. Refresh the browser. |
| Dashboard shows `Kubernetes API: … forbidden` | RBAC wasn't applied: `kubectl apply -f k8s/06-dashboard.yaml`. |
| Gateway lane stuck on *waiting for Gateway* | `kubectl -n grpc-demo get gateway` should say Programmed. If it's slow, skip it and go on; Act 4a and 4b tell the story without it. |
| Pods stuck `ErrImageNeverPull` / `ImagePullBackOff` | `make build`, then `kubectl -n grpc-demo rollout restart deploy/greeter`. |
| Need to restart an act | `make reset`, then rerun `make act2-fix && make act3`. Takes about 30s. |
| Wi-Fi is down | Not a problem: after `make stage`, everything runs locally. |
| Laptop dies | Play the backup recording (see below). |

**Record a backup.** Do one full rehearsal while screen-recording (QuickTime / OBS) and keep it on the desktop.

---

## 8. Rehearsal checklist

- [ ] Do a full run-through twice, timed. Aim for 20 minutes of demo.
- [ ] Check the venue projector resolution. At **1280×720**, set browser zoom to **90%** so the event feed stays on screen. At 1080p, 100–110% looks best from the back row.
- [ ] Practice the 15–30s wait in 4d with talking points. Don't stand silently.
- [ ] `make reset && make stage` works from a cold start.
