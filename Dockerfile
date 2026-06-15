# syntax=docker/dockerfile:1

FROM golang:1.25-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go tool bundler -output cmd/ghcp-pool
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/ghcp-pool ./cmd/ghcp-pool

FROM alpine:3.20
WORKDIR /app

COPY --from=build /out/ghcp-pool /app/ghcp-pool
RUN apk add --no-cache ca-certificates \
    && addgroup -S ghcp && adduser -S -G ghcp ghcp \
    && mkdir -p /data /runtime-home \
    && chown -R ghcp:ghcp /data /runtime-home /app

USER ghcp
EXPOSE 8000

ENV GHCP_HOST=0.0.0.0 \
    GHCP_PORT=8000 \
    GHCP_BACKEND=fake \
    GHCP_USAGE_SQLITE_PATH=/data/usage.sqlite \
    HOME=/tmp \
    XDG_CACHE_HOME=/tmp/.cache \
    TMPDIR=/tmp

ENTRYPOINT ["/app/ghcp-pool"]
