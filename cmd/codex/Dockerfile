# syntax=docker/dockerfile:1
# Build context: the repository root (docker build -f cmd/codex/Dockerfile .).

FROM --platform=$BUILDPLATFORM golang:1.23 AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/codex ./cmd/codex
COPY internal/adapter ./internal/adapter
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w" -o /out/adapter-codex ./cmd/codex

# Official Codex CLI release asset (a single static musl executable: no Node.js
# needed). Pinned by version; the sha256 values below are the digests GitHub
# reports for the release assets (and match the downloaded files).
FROM --platform=$BUILDPLATFORM debian:bookworm-slim AS codex
ARG TARGETARCH
ARG CODEX_VERSION=0.169.0
ARG CODEX_SHA256_AMD64=9af4d751fce183faedea629437d5252dbf458eeb04130cf4f97be844621ea985
ARG CODEX_SHA256_ARM64=97b97d87eea075c5cb7709c7f6156a451622a7667953ecda40e0276637196f3f
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*
RUN set -eu; \
    case "$TARGETARCH" in \
      amd64) t=x86_64-unknown-linux-musl; sum="$CODEX_SHA256_AMD64";; \
      arm64) t=aarch64-unknown-linux-musl; sum="$CODEX_SHA256_ARM64";; \
      *) echo "unsupported arch $TARGETARCH" >&2; exit 1;; \
    esac; \
    curl -fsSL -o /codex.tgz "https://github.com/openai/codex/releases/download/rust-v${CODEX_VERSION}/codex-${t}.tar.gz"; \
    echo "$sum  /codex.tgz" | sha256sum -c -; \
    tar -xzf /codex.tgz -C /tmp; \
    install -m 0755 "/tmp/codex-${t}" /codex

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --uid 10001 --no-create-home --shell /usr/sbin/nologin adapter
COPY --from=codex /codex /usr/local/bin/codex
COPY --from=build /out/adapter-codex /usr/local/bin/adapter-codex
ENV CODEX_BIN=/usr/local/bin/codex \
    CODEX_HOME=/data/codex \
    ADAPTER_LISTEN=127.0.0.1:9100 \
    HOME=/tmp
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/adapter-codex"]
