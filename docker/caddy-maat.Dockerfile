# Same trick as reputationdbd.Dockerfile: xcaddy is just a Go build wearing a hat,
# so pin it to the build host and aim it with GOOS/GOARCH instead of emulating the
# whole toolchain under qemu.
FROM --platform=$BUILDPLATFORM caddy:2.11.4-builder-alpine AS builder

ARG TARGETOS
ARG TARGETARCH

COPY . /app/src

# The builder image's WORKDIR is /usr/bin, so `xcaddy build` drops its binary
# straight onto /usr/bin/caddy, which is what the runtime stage below copies.
RUN --mount=type=cache,target=/go/pkg/mod \
  --mount=type=cache,target=/root/.cache/go-build,id=go-build-caddy-$TARGETOS-$TARGETARCH \
  CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH xcaddy build \
  --with github.com/caddyserver/nginx-adapter \
  --with github.com/TecharoHQ/reputationdb/caddy/maat=/app/src \
  --with github.com/inkress/caddy-ja4

FROM caddy:2.11.4-alpine

COPY --from=builder /usr/bin/caddy /usr/bin/caddy
