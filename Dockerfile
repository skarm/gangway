FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build

ARG TARGETOS
ARG TARGETARCH

ENV CGO_ENABLED=0 \
    GOTOOLCHAIN=local

WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod verify
COPY main.go ./
COPY internal ./internal

RUN GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -buildvcs=false -ldflags='-s -w' -o /out/gangway .

# The runtime user cannot create this itself on a read-only root filesystem.
RUN mkdir -p /out/rootfs/run/gangway \
    && chown 1000:1000 /out/rootfs/run/gangway \
    && chmod 0750 /out/rootfs/run/gangway

FROM scratch

ARG VERSION=""
ARG REVISION=""

LABEL org.opencontainers.image.title="gangway" \
      org.opencontainers.image.description="Minimal read-only proxy for the Docker Engine API subset used by SPIRE's Docker workload attestor" \
      org.opencontainers.image.source="https://github.com/skarm/gangway" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.base.name="scratch" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}"

COPY --from=build /out/rootfs/ /
COPY --from=build /out/gangway /gangway

USER 1000:1000
STOPSIGNAL SIGTERM

HEALTHCHECK --interval=30s --timeout=7s --start-period=5s --retries=3 \
    CMD ["/gangway", "--healthcheck"]
ENTRYPOINT ["/gangway"]
