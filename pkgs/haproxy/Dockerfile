# syntax=docker/dockerfile:1
#
# Fetches the pinned HAProxy source (see ../../versions.mk), builds it as
# a fully static musl binary using Alpine's own toolchain (gcc/musl-dev via
# build-base - not a from-scratch cross-compiler; see
# ../musl-toolchain/Dockerfile for why that stays a placeholder), and
# exports just the binary via a `FROM scratch` stage.
#
# TODO: signature verification for the downloaded tarball - not wired up
# yet. See ../../docs/architecture.md.

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

FROM alpine:3.22 AS build
ARG HAPROXY_VERSION
RUN apk add --no-cache build-base linux-headers zlib-dev zlib-static \
    openssl-dev openssl-libs-static
COPY --from=fetch /src/haproxy-${HAPROXY_VERSION} /src/haproxy-${HAPROXY_VERSION}
WORKDIR /src/haproxy-${HAPROXY_VERSION}
RUN make -j"$(nproc)" TARGET=linux-musl \
    USE_OPENSSL=1 USE_ZLIB=1 USE_PTHREAD_PSHARED=1 \
    LDFLAGS=-static \
    && strip haproxy \
    && ./haproxy -vv

FROM scratch AS export
ARG HAPROXY_VERSION
COPY --from=build /src/haproxy-${HAPROXY_VERSION}/haproxy /haproxy
