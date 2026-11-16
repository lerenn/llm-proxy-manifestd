# syntax=docker/dockerfile:1
# Build context: the repository root (docker build -f adapters/claude/Dockerfile .).

FROM --platform=$BUILDPLATFORM golang:1.23 AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY adapters ./adapters
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w" -o /out/adapter-claude ./adapters/claude

# Official Claude Code native binary (a self-contained executable: no Node.js
# needed). Pinned by version and verified against the sha256 in the release
# manifest published next to the binary.
FROM --platform=$BUILDPLATFORM debian:bookworm-slim AS claude
ARG TARGETARCH
ARG CLAUDE_VERSION=2.1.325
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl jq \
    && rm -rf /var/lib/apt/lists/*
RUN set -eu; \
    case "$TARGETARCH" in amd64) p=linux-x64;; arm64) p=linux-arm64;; *) echo "unsupported arch $TARGETARCH" >&2; exit 1;; esac; \
    base=https://downloads.claude.ai/claude-code-releases/${CLAUDE_VERSION}; \
    sum="$(curl -fsSL "$base/manifest.json" | jq -er --arg p "$p" '.platforms[$p].checksum')"; \
    curl -fsSL -o /claude "$base/$p/claude"; \
    echo "$sum  /claude" | sha256sum -c -; \
    chmod 0755 /claude

# glibc-based runtime (the linux-x64/linux-arm64 builds are glibc builds).
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --uid 10001 --no-create-home --shell /usr/sbin/nologin adapter
COPY --from=claude /claude /usr/local/bin/claude
COPY --from=build /out/adapter-claude /usr/local/bin/adapter-claude
ENV CLAUDE_BIN=/usr/local/bin/claude \
    CLAUDE_CONFIG_DIR=/data/claude \
    ADAPTER_LISTEN=127.0.0.1:9100 \
    HOME=/tmp \
    DISABLE_AUTOUPDATER=1
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/adapter-claude"]
