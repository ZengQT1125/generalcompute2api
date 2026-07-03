# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.26 AS builder
WORKDIR /app
ARG TARGETOS
ARG TARGETARCH
ARG BUILD_VERSION

# 利用 BuildKit 缓存挂载加速 go mod download（增量构建时秒级完成）
RUN --mount=type=cache,target=/go/pkg/mod/ \
    --mount=type=bind,source=go.mod,target=go.mod \
    --mount=type=bind,source=go.sum,target=go.sum \
    go mod download -x

COPY . .

# BuildKit 缓存挂载同时缓存 Go 编译中间产物，大幅加速重复构建
RUN --mount=type=cache,target=/go/pkg/mod/ \
    --mount=type=cache,target=/root/.cache/go-build \
    set -eux; \
    GOOS="${TARGETOS:-linux}"; \
    GOARCH="${TARGETARCH:-arm64}"; \
    BUILD_VERSION_RESOLVED="${BUILD_VERSION:-}"; \
    if [ -z "${BUILD_VERSION_RESOLVED}" ] && [ -f VERSION ]; then \
      BUILD_VERSION_RESOLVED="$(cat VERSION | tr -d "[:space:]")"; \
    fi; \
    CGO_ENABLED=0 GOOS="${GOOS}" GOARCH="${GOARCH}" go build -buildvcs=false \
      -ldflags="-s -w -X generalcompute2api/internal/version.BuildVersion=${BUILD_VERSION_RESOLVED}" \
      -o /out/generalcompute2api ./cmd/generalcompute2api; \
    mkdir -p /out/app/data /out/data

FROM alpine:3.20
WORKDIR /app
EXPOSE 8000
COPY --from=builder /out/generalcompute2api /usr/local/bin/generalcompute2api
COPY --from=builder /out/app/data /app/data
COPY --from=builder /out/data /data
CMD ["/usr/local/bin/generalcompute2api"]
