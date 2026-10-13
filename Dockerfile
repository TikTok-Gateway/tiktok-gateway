# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.25.8-bookworm AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
COPY third_party/matrix-tiktok ./third_party/matrix-tiktok
RUN go mod download
COPY internal/greeter ./internal/greeter
COPY internal/gateway ./internal/gateway
COPY cmd/gateway ./cmd/gateway
RUN mkdir -m 0700 /data
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /gateway ./cmd/gateway
FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.source="https://github.com/TimNekk/tiktok-gateway"
LABEL org.opencontainers.image.title="TikTok Gateway"
COPY --from=build /gateway /gateway
COPY --from=build /src/third_party/matrix-tiktok/LICENSE /licenses/matrix-tiktok-MIT.txt
COPY --from=build --chown=65532:65532 /data /data
USER 65532:65532
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["/gateway"]
