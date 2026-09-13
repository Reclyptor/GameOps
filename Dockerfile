# syntax=docker/dockerfile:1
#
# ghcr.io/reclyptor/gameops — a distribution-only image holding /opt/gameops:
# one static binary and the adapter shim. Game images do
#   COPY --from=ghcr.io/reclyptor/gameops:<version> /opt/gameops /opt/gameops
# See docs/CONTRACT.md.

FROM debian:trixie-slim AS build
ARG GAMEOPS_VERSION=dev
SHELL ["/bin/bash", "-eo", "pipefail", "-c"]
RUN apt-get update \
 && apt-get install -y --no-install-recommends golang-go ca-certificates \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY go.mod ./
COPY cmd/ cmd/
COPY internal/ internal/
ENV CGO_ENABLED=0 GOFLAGS=-mod=mod GOTOOLCHAIN=local
RUN go vet ./... \
 && go test ./... \
 && go build -trimpath -ldflags="-s -w -X main.Version=${GAMEOPS_VERSION}" -o /opt/gameops/bin/gameops ./cmd/gameops
COPY shim/ /opt/gameops/shim/
COPY docs/CONTRACT.md /opt/gameops/docs/CONTRACT.md
RUN chmod 0755 /opt/gameops/bin/gameops /opt/gameops/shim/adapter-exec \
 && chmod 0644 /opt/gameops/shim/adapter.sh /opt/gameops/docs/CONTRACT.md \
 && bash -n /opt/gameops/shim/adapter.sh /opt/gameops/shim/adapter-exec \
 && /opt/gameops/bin/gameops version

FROM scratch
COPY --from=build /opt/gameops /opt/gameops
LABEL org.opencontainers.image.title="gameops" \
      org.opencontainers.image.description="The operations layer shared by Reclyptor game-server images: supervised lifecycle, backups, in-place updates, Discord notifications, player events, console pipe, health check" \
      org.opencontainers.image.source="https://github.com/Reclyptor/GameOps" \
      org.opencontainers.image.licenses="MIT"
