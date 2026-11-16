# syntax=docker/dockerfile:1
# ---- build ----------------------------------------------------------------
FROM golang:1.26 AS build
WORKDIR /src
COPY . .
# `go mod tidy` writes go.sum on first build; commit it afterwards if you like.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod tidy && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/...

# ---- run ------------------------------------------------------------------
# Static binary + distroless: no shell, no package manager, runs as non-root.
# (Note: no `sleep` or `grpc_health_probe` binary inside, so we rely on the
#  kubelet's native gRPC probe and the preStop `sleep` action instead.)
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/server /out/client /out/dashboard /
# Numeric UID (distroless "nonroot" = 65532): with a named user, the kubelet
# can't verify `runAsNonRoot: true` and refuses to start the container.
USER 65532:65532
EXPOSE 50051
ENTRYPOINT ["/server"]
