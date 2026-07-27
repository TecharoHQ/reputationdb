# Pin the build stage to the host's native architecture and let Go cross-compile
# to $TARGETPLATFORM. Running the Go toolchain itself under qemu is glacially slow.
FROM --platform=$BUILDPLATFORM golang:1.26 AS build

ARG TARGETOS
ARG TARGETARCH

WORKDIR /app
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
  go mod download

COPY . .

RUN --mount=type=cache,target=/go/pkg/mod \
  --mount=type=cache,target=/root/.cache/go-build,id=go-build-$TARGETOS-$TARGETARCH \
  CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" \
  -o /app/bin/reputationdbd ./cmd/reputationdbd

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /app/bin/reputationdbd /app/bin/reputationdbd

EXPOSE 3823
CMD ["/app/bin/reputationdbd"]
