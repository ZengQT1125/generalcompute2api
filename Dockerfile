FROM --platform=$BUILDPLATFORM golang:1.26 AS go-builder
WORKDIR /app
ARG TARGETOS
ARG TARGETARCH
ARG BUILD_VERSION
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN set -eux; \
    GOOS="${TARGETOS:-$(go env GOOS)}"; \
    GOARCH="${TARGETARCH:-$(go env GOARCH)}"; \
    BUILD_VERSION_RESOLVED="${BUILD_VERSION:-}"; \
    if [ -z "${BUILD_VERSION_RESOLVED}" ] && [ -f VERSION ]; then BUILD_VERSION_RESOLVED="$(cat VERSION | tr -d "[:space:]")"; fi; \
    CGO_ENABLED=0 GOOS="${GOOS}" GOARCH="${GOARCH}" go build -buildvcs=false -ldflags="-s -w -X generalcompute2api/internal/version.BuildVersion=${BUILD_VERSION_RESOLVED}" -o /out/generalcompute2api ./cmd/generalcompute2api

FROM busybox:1.36.1-musl AS busybox-tools

FROM debian:bookworm-slim AS runtime-base
WORKDIR /app
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && groupadd -r generalcompute2api && useradd -r -g generalcompute2api -d /app -s /usr/sbin/nologin generalcompute2api \
    && mkdir -p /app/data /data && chown -R generalcompute2api:generalcompute2api /app /data \
    && rm -rf /var/lib/apt/lists/*
COPY --from=busybox-tools /bin/busybox /usr/local/bin/busybox
EXPOSE 8000
CMD ["/usr/local/bin/generalcompute2api"]

FROM runtime-base AS runtime-from-source
COPY --from=go-builder /out/generalcompute2api /usr/local/bin/generalcompute2api

# USER generalcompute2api # Commented out to run as root and avoid SQLite permission issues on host-mounted volumes

FROM runtime-from-source AS final
