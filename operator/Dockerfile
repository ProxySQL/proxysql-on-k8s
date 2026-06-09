# syntax=docker/dockerfile:1.7
#
# Multi-arch build for the proxysql-operator manager.
#
# The builder stage pins itself to ${BUILDPLATFORM} (the host's arch) and
# cross-compiles to ${TARGETARCH}. That avoids running the Go toolchain
# under qemu emulation when building for arm64 from an amd64 host, which is
# an order of magnitude faster.
#
# Build single-arch (loaded into the local daemon):
#   docker buildx build --load -t proxysql-operator:dev .
#
# Build + push multi-arch:
#   docker buildx build --push --platform=linux/amd64,linux/arm64 \
#     -t ghcr.io/proxysql/proxysql-operator:<tag> .

FROM --platform=${BUILDPLATFORM} golang:1.25 AS builder
ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown

WORKDIR /workspace

# Pre-fetch modules so source changes don't invalidate the dep layer.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download

# Source — .dockerignore filters out tests and tooling.
COPY . .

# Build a static, stripped binary. We don't pass -a (force-rebuild) so the
# build cache layer above stays useful between builds.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build \
      -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /out/manager cmd/main.go

FROM gcr.io/distroless/static:nonroot
ARG VERSION=dev
ARG COMMIT=unknown
LABEL org.opencontainers.image.title="proxysql-operator" \
      org.opencontainers.image.source="https://github.com/ProxySQL/kubernetes" \
      org.opencontainers.image.description="Kubernetes operator for ProxySQL" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}"
WORKDIR /
COPY --from=builder /out/manager /manager
USER 65532:65532
ENTRYPOINT ["/manager"]
