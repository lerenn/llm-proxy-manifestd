# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.23 AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY ui ./ui
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/llm-proxy-manifestd ./cmd/llm-proxy-manifestd

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/llm-proxy-manifestd /llm-proxy-manifestd
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/llm-proxy-manifestd"]
