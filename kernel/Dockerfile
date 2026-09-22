# syntax=docker/dockerfile:1
#
# Fetches and verifies the pinned kernel source (see ../versions.mk), and
# provides a "config" stage used interactively by `make kernel-menuconfig`
# (see ../Makefile) to run `make menuconfig` and produce
# configs/haproxyos_defconfig.
#
# TODO(Phase 1): a "build" stage that actually compiles the kernel from
# configs/haproxyos_defconfig and exports bzImage/modules - not implemented
# yet, this Dockerfile is Phase 0 scaffolding (source fetch + menuconfig
# only). See docs/architecture.md.

ARG KERNEL_VERSION

FROM debian:trixie-slim AS fetch
ARG KERNEL_VERSION
RUN apt-get update && apt-get install -y --no-install-recommends \
    curl ca-certificates gnupg \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
RUN curl -fsSLO https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-${KERNEL_VERSION}.tar.xz \
    && curl -fsSLO https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-${KERNEL_VERSION}.tar.sign
# TODO(Phase 1): verify the detached PGP signature against kernel.org's
# published release-signing keys before extracting anything below - not
# wired up yet, so this stage is not yet safe to feed into a real build.
RUN tar xf linux-${KERNEL_VERSION}.tar.xz

FROM fetch AS config
ARG KERNEL_VERSION
RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential libncurses-dev flex bison bc \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src/linux-${KERNEL_VERSION}
COPY configs/haproxyos_defconfig .config
CMD ["make", "menuconfig"]
