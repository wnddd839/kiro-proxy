# syntax=docker/dockerfile:1
#
# Multi-stage build for kiro.cache.
#
#   docker build -t kiro-cache .
#   docker run --rm -p 127.0.0.1:8787:8787 \
#     -v kiro-data:/data kiro-cache
#
# The runtime image is distroless: no shell, no package manager, no curl.
# Configuration and credentials live under /data, which should be a volume.

ARG GO_VERSION=1.26.4

FROM golang:${GO_VERSION} AS build
WORKDIR /src

# Cache module downloads separately from source changes.
COPY go.mod ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 go build \
      -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/kiro-proxy \
      ./cmd/kiro-proxy

# Smoke test: the binary must start and print a version without touching data.
RUN /out/kiro-proxy -version
# A marker file makes the empty directory reproducible and gives the runtime
# volume the right ownership when Docker initializes it.
RUN mkdir -p /out/data && \
    printf '%s\n' \
      'This volume holds kiro.cache state: accounts.json, keys.json,' \
      'usage.jsonl, promptcache.json and prices.json.' \
      'Back it up like a password; accounts.json contains credentials.' \
      > /out/data/README.txt

FROM gcr.io/distroless/static-debian12:nonroot

# /data holds accounts.json, keys.json, usage.jsonl and prices.json.
# The process runs as uid 65532 (nonroot), so the directory and the binary
# are copied with that ownership. A named volume inherits it on first use.
COPY --from=build --chown=65532:65532 /out/data/ /data/
COPY --from=build --chown=65532:65532 /out/kiro-proxy /usr/local/bin/kiro-proxy
WORKDIR /data
# The container listens on all interfaces so published ports work. The
# startup guard still refuses to run without a downstream key unless the
# operator explicitly passes -allow-insecure-listen.
EXPOSE 8787
VOLUME ["/data"]

# distroless has no shell or curl; use the binary itself for health checks.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/usr/local/bin/kiro-proxy", "-healthcheck", "-listen", "127.0.0.1:8787"]

ENTRYPOINT ["/usr/local/bin/kiro-proxy"]
CMD ["-config", "/data/kiro-proxy.json", "-listen", "0.0.0.0:8787"]
