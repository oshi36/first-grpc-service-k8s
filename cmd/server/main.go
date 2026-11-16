// Greeter server for "Your First gRPC Service on Kubernetes".
//
// Demo-relevant behaviour, all driven by env vars so the same image is used
// in every act:
//
//	PORT              listen port (default 50051)
//	POD_NAME          injected via the Downward API, returned in every reply
//	WARMUP_SECONDS    report NOT_SERVING on the gRPC health service for N
//	                  seconds after start (shows readiness gating)  (default 0)
//	MAX_CONN_AGE      e.g. "30s". Server sends GOAWAY after this age so
//	                  clients reconnect and re-resolve DNS (shows how new
//	                  replicas start receiving traffic)              (default off)
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"

	greeterv1 "grpc-k8s-demo/gen/greeter/v1"
)

type greeter struct {
	greeterv1.UnimplementedGreeterServer
	pod string
}

func (g *greeter) SayHello(_ context.Context, req *greeterv1.HelloRequest) (*greeterv1.HelloReply, error) {
	name := req.GetName()
	if name == "" {
		name = "KubeCon"
	}
	return &greeterv1.HelloReply{
		Message: fmt.Sprintf("Hello, %s!", name),
		Pod:     g.pod,
	}, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	port := env("PORT", "50051")
	pod := env("POD_NAME", "unknown")
	if pod == "unknown" {
		if h, err := os.Hostname(); err == nil {
			pod = h
		}
	}
	warmup, _ := strconv.Atoi(env("WARMUP_SECONDS", "0"))

	var opts []grpc.ServerOption
	if v := os.Getenv("MAX_CONN_AGE"); v != "" {
		age, err := time.ParseDuration(v)
		if err != nil {
			log.Fatalf("bad MAX_CONN_AGE %q: %v", v, err)
		}
		opts = append(opts, grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionAge:      age,
			MaxConnectionAgeGrace: 10 * time.Second, // let in-flight RPCs finish
		}))
		log.Printf("MaxConnectionAge=%s (clients will be asked to reconnect)", age)
	}

	srv := grpc.NewServer(opts...)
	greeterv1.RegisterGreeterServer(srv, &greeter{pod: pod})

	// Standard gRPC health service (grpc.health.v1.Health).
	// This is what the kubelet's native `grpc:` probe calls.
	hs := health.NewServer()
	healthpb.RegisterHealthServer(srv, hs)
	const svcName = "greeter.v1.Greeter"

	setAll := func(s healthpb.HealthCheckResponse_ServingStatus) {
		hs.SetServingStatus("", s) // "" = overall server health (probe default)
		hs.SetServingStatus(svcName, s)
	}

	if warmup > 0 {
		setAll(healthpb.HealthCheckResponse_NOT_SERVING)
		log.Printf("warming up for %ds, health=NOT_SERVING", warmup)
		go func() {
			time.Sleep(time.Duration(warmup) * time.Second)
			setAll(healthpb.HealthCheckResponse_SERVING)
			log.Printf("warm-up done, health=SERVING")
		}()
	} else {
		setAll(healthpb.HealthCheckResponse_SERVING)
	}
	// Separate name for the liveness probe: "is the process alive?" stays
	// SERVING during warm-up, so a slow start never triggers a restart.
	// Readiness asks the "" (overall) status: "can I take traffic yet?"
	hs.SetServingStatus("liveness", healthpb.HealthCheckResponse_SERVING)

	// Reflection lets grpcurl discover services without the .proto file.
	reflection.Register(srv)

	lis, err := net.Listen("tcp", ":"+port)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}

	// Graceful shutdown: on SIGTERM flip health to NOT_SERVING first so the
	// pod is pulled from endpoints, then drain in-flight RPCs.
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		log.Printf("shutting down: health=NOT_SERVING, draining")
		hs.Shutdown()
		done := make(chan struct{})
		go func() { srv.GracefulStop(); close(done) }()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			srv.Stop()
		}
	}()

	log.Printf("greeter listening on :%s as pod %s", port, pod)
	if err := srv.Serve(lis); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
