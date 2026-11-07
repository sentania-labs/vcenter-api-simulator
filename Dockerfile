# syntax=docker/dockerfile:1.7
# Multi-stage build of the simulator: a static Go binary on a distroless
# non-root runtime. Reproducible for a given source commit: pinned base
# image digests, -trimpath, no build id, and the version/commit stamped from
# build arguments rather than read from the working tree.

ARG GO_IMAGE=golang:1.26.5-bookworm@sha256:53eeac89074db483fdf0ab3be1df32bf6e47562263d2d0d6baa7f26acb4957dd
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

FROM ${GO_IMAGE} AS build
ARG VERSION=dev
ARG COMMIT=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOFLAGS=-mod=readonly go build -trimpath \
      -ldflags "-s -w -buildid= -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /out/vcsimulator ./cmd/vcsimulator
# The state directory, owned by the runtime user (distroless nonroot, uid
# 65532) so a fresh named volume mounted there is writable without a chown.
RUN mkdir -p /out/data/tls && chown -R 65532:65532 /out/data

FROM ${RUNTIME_IMAGE}
ARG VERSION=dev
ARG COMMIT=unknown
ARG SOURCE=https://github.com/sentania-labs/vcenter-api-simulator
ARG CREATED=1970-01-01T00:00:00Z
LABEL org.opencontainers.image.title="vcenter-api-simulator" \
      org.opencontainers.image.description="Simulated vCenter /sdk and vSAN ESA Performance Service /vsanHealth endpoint for management-pack compatibility and scale testing" \
      org.opencontainers.image.source="${SOURCE}" \
      org.opencontainers.image.url="${SOURCE}" \
      org.opencontainers.image.documentation="${SOURCE}#readme" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.created="${CREATED}" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.vendor="sentania-labs"
COPY --from=build /out/vcsimulator /vcsimulator
COPY --from=build --chown=65532:65532 /out/data /data
# State: the identity registry and the persisted certificate (tls/cert.pem,
# tls/key.pem). Mount a volume here so the identities and the certificate the
# management pack pinned survive a restart.
VOLUME ["/data"]
WORKDIR /data
# vcsim keeps per-datastore scratch under TMPDIR; give it a tmpfs or emptyDir
# when the root filesystem is read-only.
ENV TMPDIR=/tmp
USER nonroot:nonroot
EXPOSE 8443
ENTRYPOINT ["/vcsimulator"]
CMD ["-listen", "0.0.0.0:8443", "-data", "/data", "-seed", "phase1"]
