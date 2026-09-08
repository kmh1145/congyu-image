# syntax=docker/dockerfile:1.7
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS builder
ARG TARGETOS TARGETARCH
WORKDIR /src
RUN apk add --no-cache git ca-certificates
COPY go.mod ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/congyu-image .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata libwebp-tools && \
    addgroup -S congyu && adduser -S -G congyu -h /app congyu && \
    mkdir -p /data/images && chown -R congyu:congyu /data /app
WORKDIR /app
COPY --from=builder /out/congyu-image /app/congyu-image
USER congyu
EXPOSE 8080
VOLUME ["/data"]
ENV LISTEN_ADDR=:8080 DATA_DIR=/data TZ=Asia/Shanghai
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["/app/congyu-image"]
