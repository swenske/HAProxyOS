# syntax=docker/dockerfile:1
#
# Fetches the pinned HAProxy source (see ../../versions.mk).
#
# TODO(Phase 2): a "build" stage that actually compiles HAProxy statically
# against musl (see ../musl-toolchain) and a signature-verification step
# for the downloaded tarball - not implemented yet. This is Phase 0
# scaffolding: source fetch only. See ../../docs/architecture.md.

ARG HAPROXY_VERSION

FROM debian:trixie-slim AS fetch
ARG HAPROXY_VERSION
RUN apt-get update && apt-get install -y --no-install-recommends \
    curl ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
RUN branch="$(echo "${HAPROXY_VERSION}" | cut -d. -f1,2)" \
    && curl -fsSLO "https://www.haproxy.org/download/${branch}/src/haproxy-${HAPROXY_VERSION}.tar.gz"
RUN tar xf haproxy-${HAPROXY_VERSION}.tar.gz
