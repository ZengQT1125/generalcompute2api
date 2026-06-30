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
    CGO_ENABLED=0 GOOS="${GOOS}" GOARCH="${GOARCH}" go build -buildvcs=false -ldflags="-s -w -X generalcompute2api/internal/version.BuildVersion=${BUILD_VERSION_RESOLVED}" -o /out/generalcompute2api ./cmd/generalcompute2api; \
    mkdir -p /out/app/data /out/data

FROM alpine:3.20 AS runtime-base
WORKDIR /app
# Alpine natively includes ca-certificates and busybox. 
EXPOSE 8000
CMD ["/usr/local/bin/generalcompute2api"]

FROM runtime-base AS runtime-from-source
COPY --from=go-builder /out/generalcompute2api /usr/local/bin/generalcompute2api
COPY --from=go-builder /out/app/data /app/data
COPY --from=go-builder /out/data /data

FROM runtime-from-source AS final
