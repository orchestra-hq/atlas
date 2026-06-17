# syntax=docker/dockerfile:1

# Atlas container images (ADR-0006). One binary, role chosen by subcommand;
# two final build targets:
#   slim — minimal image that downloads its engine runtime on first run, like
#          the bare binary (llama.cpp, dev, CPU). Multi-arch.
#   cuda — "batteries-included" GPU image with the pinned vLLM venv baked in at
#          the path `atlas up` resolves, so a GPU host serves with no first-run
#          provisioning download. linux/amd64 only.
#
# Build:  docker build --target slim -t atlas:slim .
#         docker build --target cuda -t atlas:cuda .   # needs lots of disk
# Run:    docker run -p 8080:8080 -v atlas-state:/var/lib/atlas atlas:slim

ARG GO_VERSION=1.26
ARG CUDA_IMAGE=nvidia/cuda:12.4.1-runtime-ubuntu22.04

# --- builder: the static Go binary, cross-compiled per target platform -------
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-bookworm AS builder
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=""
ARG DATE=""
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO off => a static binary (ADR-0004); -trimpath for reproducibility.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath \
      -ldflags="-s -w \
        -X github.com/orchestra-hq/atlas/internal/version.Version=${VERSION} \
        -X github.com/orchestra-hq/atlas/internal/version.Commit=${COMMIT} \
        -X github.com/orchestra-hq/atlas/internal/version.Date=${DATE}" \
      -o /out/atlas ./cmd/atlas

# --- common runtime base: shared deps + non-root user + state layout ---------
# llama-server (downloaded at runtime in the slim image) links libgomp and
# libcurl; ca-certificates lets atlas fetch release assets and weights.
FROM debian:bookworm-slim AS runtime-base
RUN apt-get update \
  && apt-get install -y --no-install-recommends ca-certificates libgomp1 libcurl4 \
  && rm -rf /var/lib/apt/lists/*
COPY --from=builder /out/atlas /usr/local/bin/atlas
# State (runtimes, weights, logs) lives here; mount a volume to persist it.
ENV ATLAS_STATE_DIR=/var/lib/atlas
RUN useradd --create-home --home-dir /var/lib/atlas --uid 10001 atlas \
  && chown -R atlas:atlas /var/lib/atlas
USER atlas
WORKDIR /var/lib/atlas
EXPOSE 8080

# --- slim: minimal runtime, downloads engine runtimes on first use ----------
FROM runtime-base AS slim
VOLUME ["/var/lib/atlas"]
ENTRYPOINT ["atlas"]
# Bind to all interfaces so the published port is reachable; clients still need
# the API key atlas prints (or one passed via --api-key / $ATLAS_API_KEY).
CMD ["up", "--addr", "0.0.0.0:8080"]

# --- cuda: GPU image with the vLLM runtime baked in -------------------------
# Mirrors the runtime-base layout (binary, user, state dir) but on a CUDA base
# so the GPU libraries vLLM needs at runtime are present. The vLLM venv is built
# at image-build time via `atlas runtime provision`, landing at the exact path
# `atlas up --engine vllm` resolves — making the later up a no-op (ADR-0006).
FROM ${CUDA_IMAGE} AS cuda
RUN apt-get update \
  && apt-get install -y --no-install-recommends ca-certificates libgomp1 libcurl4 \
  && rm -rf /var/lib/apt/lists/*
COPY --from=builder /out/atlas /usr/local/bin/atlas
ENV ATLAS_STATE_DIR=/var/lib/atlas
RUN useradd --create-home --home-dir /var/lib/atlas --uid 10001 atlas \
  && chown -R atlas:atlas /var/lib/atlas
USER atlas
WORKDIR /var/lib/atlas
RUN atlas runtime provision --engine vllm --state-dir /var/lib/atlas
VOLUME ["/var/lib/atlas"]
EXPOSE 8080
ENTRYPOINT ["atlas"]
CMD ["up", "--engine", "vllm", "--addr", "0.0.0.0:8080"]
