# syntax=docker/dockerfile:1
#
# Compiles classes.conf + policy.conf (see both files' own headers) into
# the single binary policy rootfs/init loads at boot by writing it
# straight to /sys/fs/selinux/load - no libselinux/semodule/policy
# store on the target, just one file `checkpolicy` produced offline.
#
# `-U allow` (not the checkpolicy default of "reject") matches
# scripts/install_policy.sh's own convention from the kernel source
# tree: any object class or permission this exact kernel's classes.conf
# doesn't know about is allowed rather than rejected at load time -
# forward-compatible if a future kernel bump adds new classes this
# policy hasn't been taught about yet, rather than refusing to boot.
FROM debian:trixie-slim AS build
RUN apt-get update && apt-get install -y --no-install-recommends \
    checkpolicy \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY classes.conf policy.conf ./
RUN cat classes.conf policy.conf > combined.conf \
    && checkpolicy -U allow -o janus.policy combined.conf

FROM scratch AS export
COPY --from=build /src/janus.policy /janus.policy
