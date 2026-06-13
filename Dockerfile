# syntax=docker/dockerfile:1

FROM golang:1.22-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/ghcp-pool ./cmd/ghcp-pool

FROM alpine:3.20
RUN addgroup -S ghcp && adduser -S -G ghcp ghcp && apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=build /out/ghcp-pool /app/ghcp-pool
RUN mkdir -p /data && chown -R ghcp:ghcp /data /app

USER ghcp
EXPOSE 8000

ENV GHCP_HOST=0.0.0.0 \
    GHCP_PORT=8000 \
    GHCP_BACKEND=fake \
    GHCP_USAGE_SQLITE_PATH=/data/usage.sqlite

ENTRYPOINT ["/app/ghcp-pool"]
