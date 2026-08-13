# ── Stage 1: Build Go binary ─────────────────────────────────────
FROM golang:1.25-bookworm AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /purify ./cmd/purify
# The indexer ships alongside the server so an operator can build the local
# search index inside the same container that will serve it.
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /purify-index ./cmd/purify-index

# ── Stage 2: Runtime with Chromium ───────────────────────────────
FROM debian:bookworm-slim

# Install Chromium and minimal dependencies for headless operation.
RUN apt-get update && apt-get install -y --no-install-recommends \
    chromium \
    ca-certificates \
    fonts-noto-cjk \
    dumb-init \
    && rm -rf /var/lib/apt/lists/*

# Non-root user for security.
RUN useradd -m -s /bin/bash purify
RUN mkdir -p /data && chown purify:purify /data
USER purify
WORKDIR /home/purify

COPY --from=builder /purify /usr/local/bin/purify
COPY --from=builder /purify-index /usr/local/bin/purify-index

# Point go-rod to system Chromium; enable no-sandbox for container.
ENV PURIFY_BROWSER_BIN=/usr/bin/chromium
ENV PURIFY_NO_SANDBOX=true
ENV PURIFY_HOST=0.0.0.0
ENV PURIFY_PORT=8080

EXPOSE 8080

# dumb-init handles PID 1 + signal forwarding (graceful Chrome shutdown).
ENTRYPOINT ["dumb-init", "--"]
CMD ["purify"]
