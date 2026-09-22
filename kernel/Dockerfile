# syntax=docker/dockerfile:1
#
# Fetches and verifies the pinned kernel source (see ../versions.mk),
# provides a "config" stage used interactively by `make kernel-menuconfig`
# (see ../Makefile) to run `make menuconfig`, a "build" stage that compiles
# bzImage from configs/haproxyos_defconfig, and an "export" stage
# (`FROM scratch`) that holds just the resulting bzImage so
# `docker build --target export --output type=local,dest=<dir>` can pull it
# out without a container filesystem attached.
#
# Proven end-to-end locally (bzImage boots under QEMU with rootfs/init as
# PID 1, see ../hack/qemu-run.sh) before being encoded here - see
# ../docs/architecture.md's Phase 1.

ARG KERNEL_VERSION

FROM debian:trixie-slim AS fetch
ARG KERNEL_VERSION
RUN apt-get update && apt-get install -y --no-install-recommends \
    curl ca-certificates gnupg xz-utils \
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
    build-essential libncurses-dev flex bison bc libelf-dev \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src/linux-${KERNEL_VERSION}
COPY configs/haproxyos_defconfig .config
CMD ["make", "menuconfig"]

FROM config AS build
ARG KERNEL_VERSION
WORKDIR /src/linux-${KERNEL_VERSION}
RUN make ARCH=x86_64 olddefconfig \
    && make ARCH=x86_64 -j"$(nproc)" bzImage

FROM scratch AS export
ARG KERNEL_VERSION
COPY --from=build /src/linux-${KERNEL_VERSION}/arch/x86/boot/bzImage /bzImage
