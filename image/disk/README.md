# image/disk

`assemble.sh` builds a single, real GPT-partitioned disk image: two
independently bootable A/B slots (`BOOT-A-DATA`/`BOOT-A-HASH`,
`BOOT-B-DATA`/`BOOT-B-HASH` - each a squashfs data + dm-verity hash tree
pair) plus the persistent `STATE` partition. Both slots get the same
content for now - proven independently bootable by `../../hack/qemu-ab-boot-test.sh`
(`make qemu-ab-boot-test`), not yet a real upgrade workflow (no
`LifecycleService.Upgrade` to install something different into the
inactive slot). `rootfs/init/main.go`'s `mountState` doesn't yet know how
to find `STATE` on this real single-disk layout (no udev, so no
`/dev/disk/by-partlabel/*`) - see `../../docs/architecture.md`.

TODO(Phase 3 cont'd): STATE partition discovery from a single disk, UEFI
boot + Unified Kernel Image (needs `CONFIG_EFI` → `CONFIG_ACPI`, never
enabled so far), Secure Boot signing (`sbsign` with the project's own
key).
