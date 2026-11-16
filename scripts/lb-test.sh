#!/usr/bin/env bash
# Run the load-test client INSIDE the cluster and print the per-pod tally.
#   scripts/lb-test.sh <target> [extra client flags...]
# Uses a bare Pod + wait + logs (more reliable on stage than `kubectl run -i`).
set -euo pipefail
NS=${NS:-grpc-demo}
IMG=${IMG:-greeter:v1}
NAME=lb-test

kubectl -n "$NS" delete pod "$NAME" --ignore-not-found --wait=true >/dev/null
kubectl -n "$NS" run "$NAME" --image="$IMG" --image-pull-policy=IfNotPresent \
  --restart=Never --command -- /client -target "$@" >/dev/null

if ! kubectl -n "$NS" wait pod "$NAME" --for=jsonpath='{.status.phase}'=Succeeded --timeout=60s >/dev/null; then
  kubectl -n "$NS" logs "$NAME" || true
  exit 1
fi
kubectl -n "$NS" logs "$NAME"
kubectl -n "$NS" delete pod "$NAME" --wait=false >/dev/null
