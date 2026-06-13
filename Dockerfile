# syntax=docker/dockerfile:1

FROM golang:1.22-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/ghcp-pool ./cmd/ghcp-pool

FROM python:3.11-slim
WORKDIR /app

COPY requirements-copilot.txt ./
RUN pip install --no-cache-dir -r requirements-copilot.txt

COPY --from=build /out/ghcp-pool /app/ghcp-pool
COPY internal/gateway/copilot_worker.py /app/copilot_worker.py
RUN addgroup --system ghcp && adduser --system --ingroup ghcp ghcp \
    && mkdir -p /data /runtime-home \
    && chown -R ghcp:ghcp /data /runtime-home /app

USER ghcp
EXPOSE 8000

ENV GHCP_HOST=0.0.0.0 \
    GHCP_PORT=8000 \
    GHCP_BACKEND=fake \
    GHCP_USAGE_SQLITE_PATH=/data/usage.sqlite \
    GHCP_COPILOT_WORKER=/app/copilot_worker.py

ENTRYPOINT ["/app/ghcp-pool"]
