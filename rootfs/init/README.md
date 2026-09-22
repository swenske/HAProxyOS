# rootfs/init

TODO(Phase 1): custom PID 1 (Go, not systemd) - mounts the squashfs
rootfs + ephemeral overlay, starts `haproxyosd`, supervises the managed
services it controls (`haproxy`, and optionally `bird`/`keepalived`).
Empty placeholder for now - see `../../docs/architecture.md`.
