# Your first gRPC service on Kubernetes - the checklist

**Package**
- [ ] Static binary in a minimal image (distroless/scratch), with a **numeric** non-root `USER`
- [ ] Register the standard `grpc.health.v1` health service (and reflection for dev, if you want it)

**Health checks**
- [ ] Use native `grpc:` probes, not `httpGet` (HTTP/1.1) and not `tcpSocket` (checks the port, not the app)
- [ ] Probe `port` is a number, not a named port
- [ ] Readiness = "can I serve?" (`service: ""`); liveness = "am I alive?" (a separate name, always SERVING while running)
- [ ] On SIGTERM: health - NOT_SERVING, then `GracefulStop()`; add a `preStop` sleep

**Routing**
- [ ] Use Gateway API **GRPCRoute**, not Ingress annotations (Ingress NGINX was retired in March 2026)
- [ ] Match on gRPC `service` / `method`; set `hostnames`
- [ ] Set `appProtocol: kubernetes.io/h2c` on the Service port
- [ ] Every hop must speak HTTP/2: LB, proxy, probe

**Load balancing**
- [ ] Remember that kube-proxy balances connections, while gRPC multiplexes many RPCs over one connection
- [ ] Pick one of these:
  - client-side: headless Service + `dns:///` + `round_robin`
  - proxy-side: Gateway / service mesh (per-request L7 balancing)
- [ ] Set server `MaxConnectionAge` so clients reconnect and discover new pods (expect up to ~30s with grpc-go's DNS resolver)
- [ ] Verify the distribution: log or return the pod name, and look at per-pod RPS
