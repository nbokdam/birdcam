# birdcam-server container image. Cross-compiles natively on the build
# platform (no emulation needed for multi-arch builds).
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-s -w -X birdcam/internal/server.Version=$VERSION" \
      -o /out/birdcam-server ./cmd/birdcam-server
# The agent builds the server pushes to the Pis (self-update). They carry the
# same VERSION as the server: the server offers them to every agent that
# reports another version. Raspberry Pi OS 64-bit = arm64, 32-bit = armv7.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath \
      -ldflags "-s -w -X main.version=$VERSION" \
      -o /out/agents/birdcam-agent-linux-arm64 ./cmd/birdcam-agent \
 && CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -trimpath \
      -ldflags "-s -w -X main.version=$VERSION" \
      -o /out/agents/birdcam-agent-linux-armv7 ./cmd/birdcam-agent

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/birdcam-server /usr/local/bin/birdcam-server
COPY --from=build /out/agents /usr/share/birdcam/agents
# Config is expected at /etc/birdcam/server.yaml (mount it, plus any key/secret
# files it references). Generated state (ingest TLS cert) goes to /var/lib/birdcam.
VOLUME /var/lib/birdcam
# HTTP (put a TLS reverse proxy in front), ingest pool, WebRTC media.
EXPOSE 8080/tcp 9001-9010/tcp 8189/udp
ENTRYPOINT ["/usr/local/bin/birdcam-server"]
CMD ["-config", "/etc/birdcam/server.yaml"]
