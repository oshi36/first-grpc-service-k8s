# Demo driver for "Your First gRPC Service on Kubernetes: A Beginner's Roadmap"
# Run targets in order. Each act is idempotent, so you can re-run any of them.
SHELL      := /bin/bash
CLUSTER    ?= grpc-demo
NS         ?= grpc-demo
IMG        ?= greeter:v1
# Envoy Gateway chart version: check https://gateway.envoyproxy.io for the latest
EG_VERSION ?= v1.6.1
HOST       ?= greeter.demo.local
GW_PORT    ?= 8080

ENVOY_SVC = $$(kubectl -n envoy-gateway-system get svc \
  -l gateway.envoyproxy.io/owning-gateway-name=grpc-gw,gateway.envoyproxy.io/owning-gateway-namespace=$(NS) \
  -o jsonpath='{.items[0].metadata.name}')

.PHONY: help prereqs setup build act1 act2-wrong act2-fix act3 gw-forward act3-call \
        act4-problem act4-fix-client act4-fix-gateway act4-watch act4-scale act4-conn-age \
        dashboard ui stage reset clean generate

help:
	@grep -E '^[a-z0-9-]+:.*?## ' $(MAKEFILE_LIST) | awk -F':.*?## ' '{printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

prereqs: ## Check local tools
	@for t in docker kind kubectl helm grpcurl; do command -v $$t >/dev/null && echo "✓ $$t" || echo "✗ $$t MISSING"; done

# ── Before the talk ──────────────────────────────────────────────────────────
setup: ## Create kind cluster + install Envoy Gateway (do this BEFORE going on stage)
	kind create cluster --config kind-config.yaml
	helm install eg oci://docker.io/envoyproxy/gateway-helm --version $(EG_VERSION) \
	  -n envoy-gateway-system --create-namespace --wait
	kubectl apply -f k8s/00-namespace.yaml

# ── Act 1: package it ────────────────────────────────────────────────────────
build: ## Act 1: build the image and load it into kind
	docker build -t $(IMG) .
	kind load docker-image $(IMG) --name $(CLUSTER)

dashboard: ## Deploy the live dashboard (needs `make build` first)
	kubectl apply -f k8s/00-namespace.yaml -f k8s/06-dashboard.yaml
	kubectl -n $(NS) rollout restart deploy/dashboard >/dev/null 2>&1 || true
	kubectl -n $(NS) rollout status deploy/dashboard --timeout=90s

ui: ## Open the dashboard at http://localhost:8081 (leave running)
	@echo "▶ open http://localhost:8081   (keys: 0-4 switch act, r resets counters)"
	kubectl -n $(NS) port-forward svc/dashboard 8081:80

stage: build dashboard ## Everything to run right before going on stage (then: make ui)
	@echo "✓ ready. Run 'make ui' in its own terminal and open http://localhost:8081"

act1: build ## Act 1: build + show the image is tiny and has no shell
	@docker image ls $(IMG)
	@echo; echo "▶ No shell in distroless:"; docker run --rm --entrypoint sh $(IMG) -c 'echo hi' 2>&1 | tail -1 || true

# ── Act 2: health checks ─────────────────────────────────────────────────────
act2-wrong: ## Act 2a: deploy with HTTP probes -> never Ready, restart loop
	kubectl apply -f k8s/01-deployment-WRONG-probes.yaml -f k8s/03-services.yaml
	@echo "▶ watch:    kubectl -n $(NS) get pods -w"
	@echo "▶ then:     kubectl -n $(NS) describe pod -l app=greeter | grep -A2 'probe failed' | head"

act2-fix: ## Act 2b: native gRPC probes -> Ready after warm-up
	kubectl apply -f k8s/02-deployment.yaml -f k8s/03-services.yaml
	@echo "▶ watch READY flip 0/1 -> 1/1 after ~8s warm-up:  kubectl -n $(NS) get pods -w"
	kubectl -n $(NS) rollout status deploy/greeter --timeout=120s

# ── Act 3: route traffic with GRPCRoute ──────────────────────────────────────
act3: ## Act 3: Gateway + GRPCRoute (dashboard's Gateway lane lights up)
	kubectl apply -f k8s/03-services.yaml -f k8s/04-gateway.yaml -f k8s/05-grpcroute.yaml
	@kubectl -n $(NS) wait gateway/grpc-gw --for=condition=Programmed --timeout=120s || { \
	  echo "✗ Gateway not Programmed yet. Its status says why:"; \
	  kubectl -n $(NS) get gateway grpc-gw -o jsonpath='{range .status.conditions[*]}  {.type}={.status} ({.reason}) {.message}{"\n"}{end}'; \
	  kubectl -n envoy-gateway-system get svc,pods; exit 1; }
	kubectl -n $(NS) get gateway,grpcroute

gw-forward: ## Act 3: port-forward the Envoy proxy to localhost:$(GW_PORT) (leave running in a 2nd terminal)
	kubectl -n envoy-gateway-system port-forward svc/$(ENVOY_SVC) $(GW_PORT):80

act3-call: ## Act 3: call through the gateway with grpcurl (needs gw-forward running)
	grpcurl -plaintext -authority $(HOST) localhost:$(GW_PORT) list
	grpcurl -plaintext -authority $(HOST) -d '{"name":"KCD"}' localhost:$(GW_PORT) greeter.v1.Greeter/SayHello
	@echo; echo "▶ Wrong hostname -> no route matches:"
	-grpcurl -plaintext -authority nope.demo.local -d '{}' localhost:$(GW_PORT) greeter.v1.Greeter/SayHello

# ── Act 4: the load-balancing surprise ───────────────────────────────────────
act4-problem: ## Act 4a: 300 RPCs via ClusterIP -> ALL land on one pod
	NS=$(NS) IMG=$(IMG) scripts/lb-test.sh greeter.$(NS).svc.cluster.local:50051 -n 300

act4-fix-client: ## Act 4b: headless Service + dns:/// + round_robin -> even spread
	NS=$(NS) IMG=$(IMG) scripts/lb-test.sh dns:///greeter-headless.$(NS).svc.cluster.local:50051 -lb round_robin -n 300

act4-fix-gateway: ## Act 4c: through the Gateway (L7, per-request) -> even spread, zero client changes
	NS=$(NS) IMG=$(IMG) scripts/lb-test.sh $(ENVOY_SVC).envoy-gateway-system.svc.cluster.local:80 -authority $(HOST) -n 300

act4-watch: ## Act 4d: long-running round_robin client (2nd terminal), tally every 10s
	kubectl -n $(NS) delete pod lb-watch --ignore-not-found
	kubectl -n $(NS) run lb-watch --image=$(IMG) --image-pull-policy=IfNotPresent --restart=Never \
	  --command -- /client -target dns:///greeter-headless.$(NS).svc.cluster.local:50051 -lb round_robin -n 0 -rate 20 -report 10s
	kubectl -n $(NS) wait pod lb-watch --for=condition=Ready --timeout=30s
	kubectl -n $(NS) logs -f lb-watch

act4-scale: ## Act 4d: scale 3 -> 4. New pod gets NOTHING (client never re-resolves DNS)
	kubectl -n $(NS) scale deploy/greeter --replicas=4

act4-conn-age: ## Act 4e: set MaxConnectionAge=15s, then scale to 5 -> new pod picked up
	kubectl -n $(NS) set env deploy/greeter MAX_CONN_AGE=15s
	kubectl -n $(NS) rollout status deploy/greeter --timeout=120s
	@echo "▶ restart the watcher (make act4-watch), wait one tally, then:"
	@echo "    kubectl -n $(NS) scale deploy/greeter --replicas=5"
	@echo "  The new pod shows up in the tally ~15-30s later (MaxConnectionAge + grpc-go's 30s DNS re-resolution floor)."

# ── Housekeeping ─────────────────────────────────────────────────────────────
reset: ## Reset app state between rehearsals (keeps cluster, Envoy Gateway, dashboard)
	-kubectl -n $(NS) delete pod lb-test lb-watch --ignore-not-found
	-kubectl -n $(NS) delete events --all >/dev/null 2>&1
	-kubectl delete -f k8s/05-grpcroute.yaml -f k8s/04-gateway.yaml -f k8s/03-services.yaml -f k8s/02-deployment.yaml --ignore-not-found

clean: ## Delete the kind cluster
	kind delete cluster --name $(CLUSTER)

generate: ## Regenerate Go code from proto (needs protoc, protoc-gen-go, protoc-gen-go-grpc)
	protoc -Iproto --go_out=gen --go_opt=module=grpc-k8s-demo/gen \
	  --go-grpc_out=gen --go-grpc_opt=module=grpc-k8s-demo/gen proto/greeter/v1/greeter.proto