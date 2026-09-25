# rootfs/init

`main.go` is the Phase 1 boot-proof PID 1: mounts proc/sysfs/devtmpfs,
prints `JANUS_INIT_BOOT_OK`, powers off. Built and tested via
`make qemu-boot-test` (see `../../Makefile` and `../../hack/qemu-run.sh`).

TODO(Phase 2): the real init - mounts the squashfs rootfs + ephemeral
overlay, starts `janusd`, supervises the managed services it controls
(`haproxy`, and optionally `bird`/`keepalived`). See
`../../docs/architecture.md`.
