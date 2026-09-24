include versions.mk

MODULE  := github.com/swenske/HAProxyOS
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

BIN_DIR   := bin
BINARIES  := haproxyosd haproxyosctl
BUILD_DIR := build

GEN_DIR := gen

.PHONY: all build test vet lint proto clean kernel-menuconfig \
	kernel-build init initramfs qemu-boot-test haproxy-build \
	daemon-static initramfs-full qemu-network-test rootfs-build \
	qemu-verity-boot-test state-image qemu-state-persist-test \
	disk-image qemu-ab-boot-test uki-image qemu-uefi-boot-test \
	qemu-uefi-ab-boot-test qemu-lifecycle-rollback-test qemu-secureboot-test \
	qemu-lifecycle-upgrade-test qemu-lifecycle-upgrade-health-test

all: build

build:
	mkdir -p $(BIN_DIR)
	for b in $(BINARIES); do \
		go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$$b ./cmd/$$b ; \
	done

test:
	go test ./...

vet:
	go vet ./...

lint:
	golangci-lint run ./...

# Regenerates gen/haproxyos/v1alpha1 from api/proto/**.proto. Requires buf
# and the protoc-gen-go/protoc-gen-go-grpc plugins on PATH (`go install
# google.golang.org/protobuf/cmd/protoc-gen-go@latest` and
# `google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest`, plus
# `github.com/bufbuild/buf/cmd/buf@latest`). CI's lint job checks the
# generated tree isn't stale (`make proto && git diff --exit-code -- gen`),
# same pattern as gotochanger's `make guide` drift check.
proto:
	buf lint
	buf generate

clean:
	rm -rf $(BIN_DIR)

# Opens an interactive `make menuconfig` inside a throwaway container built
# from kernel/Dockerfile's "config" stage, seeded from the currently
# committed kernel/configs/haproxyos_defconfig, and writes the resulting
# defconfig back out so it can be reviewed with `git diff` and committed.
# This is the whole "module selection" workflow for Phase 0/1 - a Proxmox-
# hosted UI wrapping the same container is Phase 6, not required to get
# started.
kernel-menuconfig:
	docker build --target config -t haproxyos-kernel-config \
		--build-arg KERNEL_VERSION=$(KERNEL_VERSION) kernel
	docker run --rm -it \
		-v "$(CURDIR)/kernel/configs:/out" \
		haproxyos-kernel-config \
		sh -c 'make menuconfig && cp .config /out/haproxyos_defconfig'
	@echo "Updated kernel/configs/haproxyos_defconfig - review with 'git diff' and commit."

# Builds bzImage from kernel/configs/haproxyos_defconfig via kernel/
# Dockerfile's "export" stage (needs Docker Buildx - `docker buildx
# version` to check) and pulls it out to build/bzImage.
kernel-build:
	mkdir -p $(BUILD_DIR)
	docker build --target export --build-arg KERNEL_VERSION=$(KERNEL_VERSION) \
		-o $(BUILD_DIR) kernel

# Builds the Phase 1 PID 1 (rootfs/init) as a static binary - CGO must stay
# disabled since the target has no libc.
init:
	mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" \
		-o $(BUILD_DIR)/init ./rootfs/init

# Packages build/init into build/initramfs.cpio.gz (see hack/build-initramfs.sh).
initramfs: init
	./hack/build-initramfs.sh $(BUILD_DIR)/init $(BUILD_DIR)/initramfs.cpio.gz

# The Phase 1 boot-proof: builds the kernel + initramfs and boots them
# under QEMU, checking for rootfs/init's success marker on the console
# (see hack/qemu-run.sh). Requires qemu-system-x86_64 on PATH.
qemu-boot-test: kernel-build initramfs
	./hack/qemu-run.sh $(BUILD_DIR)/bzImage $(BUILD_DIR)/initramfs.cpio.gz

# Builds a fully static (musl, via Alpine's own toolchain - see pkgs/
# haproxy/Dockerfile) haproxy binary with OpenSSL and pulls it out to
# build/haproxy. No PCRE2 (Alpine ships no static pcre2-posix lib;
# HAProxy's built-in regex engine covers Phase 2's needs).
haproxy-build:
	mkdir -p $(BUILD_DIR)
	docker build --target export --build-arg HAPROXY_VERSION=$(HAPROXY_VERSION) \
		-o $(BUILD_DIR) pkgs/haproxy

# Builds haproxyosd as a static binary (CGO_ENABLED=0, same reasoning as
# `init`) for packaging into the initramfs - separate from `build`'s
# bin/haproxyosd, which doesn't force CGO off since it only needs to run
# on the build host there, not on the target kernel.
daemon-static:
	mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" \
		-o $(BUILD_DIR)/haproxyosd ./cmd/haproxyosd

# The Phase 2 network-integration rootfs: init + haproxyosd + the real
# static haproxy binary + the bootstrap config (see rootfs/base/etc/
# haproxy/haproxy.cfg). rootfs/init detects haproxyosd's presence and
# supervises it instead of powering off - see rootfs/init/main.go.
initramfs-full: init daemon-static haproxy-build
	./hack/build-initramfs.sh $(BUILD_DIR)/init $(BUILD_DIR)/initramfs-full.cpio.gz \
		$(BUILD_DIR)/haproxyosd:sbin/haproxyosd \
		$(BUILD_DIR)/haproxy:usr/local/sbin/haproxy \
		rootfs/base/etc/haproxy/haproxy.cfg:etc/haproxy/haproxy.cfg

# The Phase 2 network-integration boot test: boots the kernel + full
# initramfs under QEMU with virtio-net + DHCP, and polls a forwarded host
# port until HAProxy - running *inside* the VM - answers over real TCP/IP
# (see hack/qemu-network-test.sh). Requires qemu-system-x86_64 on PATH.
qemu-network-test: kernel-build initramfs-full
	./hack/qemu-network-test.sh $(BUILD_DIR)/bzImage $(BUILD_DIR)/initramfs-full.cpio.gz

# Phase 3: builds a squashfs image of the real rootfs (init + haproxyosd +
# haproxy + bootstrap config, same content as initramfs-full but as a
# proper filesystem image instead of a cpio archive) and its dm-verity
# hash tree (see rootfs/assemble.sh). Requires mksquashfs (squashfs-tools)
# and veritysetup (cryptsetup-bin) on PATH - neither needs root. This is
# the build-side half of Phase 3's immutability story, verified by
# mounting + `veritysetup verify` - see qemu-verity-boot-test for the
# kernel actually booting from it.
rootfs-build: init daemon-static haproxy-build
	mkdir -p $(BUILD_DIR)/rootfs
	./rootfs/assemble.sh $(BUILD_DIR)/rootfs $(BUILD_DIR)/init $(BUILD_DIR)/haproxyosd \
		$(BUILD_DIR)/haproxy rootfs/base/etc/haproxy/haproxy.cfg

# Phase 3 cont'd: boots the kernel directly from rootfs-build's
# squashfs+dm-verity image via the "dm-mod.create=" cmdline parameter
# (CONFIG_DM_INIT) - no initramfs, no userspace verity setup at all. Two
# virtio-blk drives (squashfs data + verity hash tree), the kernel
# assembles and verifies /dev/dm-0 itself before mounting it as root and
# running /sbin/init straight out of the verified image. Also re-runs
# the boot against a corrupted copy of the image and checks the kernel
# refuses to mount it - see hack/qemu-verity-boot-test.sh for exactly
# why (superblock corruption, not a random offset) and how the dm-verity
# table's fields are derived from rootfs.verity.info.
qemu-verity-boot-test: kernel-build rootfs-build
	./hack/qemu-verity-boot-test.sh $(BUILD_DIR)/bzImage $(BUILD_DIR)/rootfs

# Phase 3 cont'd: builds a blank, pre-formatted ext4 image for the
# persistent STATE partition (/etc/haproxyos/pki, /etc/haproxy, and -
# until real OCI/HTTPS image distribution exists - a staging area for
# LifecycleService.Upgrade's release bundles too, see image/disk/
# assemble.sh's own STATE_MB comment for why) - formatted here, at
# build time, not on the target (see rootfs/state-image.sh for why).
# Requires mkfs.ext4 (e2fsprogs) - doesn't need root. Must match
# image/disk/assemble.sh's own STATE_MB, or <state-image> won't
# actually fill the partition it gets dd'd into.
STATE_IMAGE_MB := 128
state-image:
	mkdir -p $(BUILD_DIR)/rootfs
	./rootfs/state-image.sh $(BUILD_DIR)/rootfs/state.img $(STATE_IMAGE_MB)

# Phase 3 cont'd: proves the STATE partition actually persists across a
# reboot, not just that it can be mounted - boots the same dm-verity
# image twice against the *same* state.img (a third, writable virtio-blk
# drive, unlike the two read-only root drives), and checks haproxyosd's
# own "first boot" log line appears on the first boot and does NOT
# reappear on the second - see hack/qemu-state-persist-test.sh.
qemu-state-persist-test: kernel-build rootfs-build state-image
	./hack/qemu-state-persist-test.sh $(BUILD_DIR)/bzImage $(BUILD_DIR)/rootfs $(BUILD_DIR)/rootfs/state.img

# Phase 3 cont'd: assembles a single, real GPT-partitioned disk image -
# the actual, complete shape a deployed node would have: an ESP (with
# the active slot's Unified Kernel Image at \EFI\BOOT\BOOTX64.EFI),
# two independently bootable A/B slots (BOOT-A-DATA/HASH,
# BOOT-B-DATA/HASH - both slots get the same rootfs content for now,
# there's no LifecycleService.Upgrade yet to install something
# different into the inactive one), and STATE. As opposed to
# qemu-verity-boot-test/qemu-state-persist-test's separate-virtio-blk-
# drives harness (which keeps working, and still covers what it always
# covered). Requires sgdisk (gdisk), ukify (systemd-ukify),
# mtools/dosfstools - doesn't need root. See image/disk/assemble.sh and
# image/disk/activate-slot.sh (switches which slot's UKI is on the ESP,
# in place, without touching STATE or either slot's content - the
# groundwork for a real LifecycleService.Upgrade/Rollback).
disk-image: kernel-build rootfs-build state-image
	./image/disk/assemble.sh $(BUILD_DIR)/rootfs/disk.img $(BUILD_DIR)/bzImage \
		$(BUILD_DIR)/rootfs $(BUILD_DIR)/rootfs/state.img A

# Phase 3 cont'd: proves both A/B slots of disk-image's single GPT disk
# are actually, independently bootable via QEMU's own -kernel/-append
# (not through the ESP/UKI - see qemu-uefi-ab-boot-test for that) - not
# just that the partition table looks right. Boots the SAME disk image
# twice, once with dm-mod.create= pointed at BOOT-A-DATA/BOOT-A-HASH
# (partitions 2/3), once at BOOT-B-DATA/BOOT-B-HASH (partitions 4/5);
# both must serve real HTTP. See hack/qemu-ab-boot-test.sh.
qemu-ab-boot-test: kernel-build disk-image
	./hack/qemu-ab-boot-test.sh $(BUILD_DIR)/bzImage $(BUILD_DIR)/rootfs

# Phase 3 cont'd: proves the *whole* real, single-disk, UEFI-bootable
# shape end to end - real OVMF firmware, one virtio-blk drive, no
# -kernel/-append at all - and that image/disk/activate-slot.sh's
# in-place ESP swap actually works: boot slot A (fresh CA onto STATE),
# switch the ESP to slot B in place, boot again, and confirm slot B is
# now what's live *and* that STATE (the CA from the slot A boot)
# survived the switch untouched. See hack/qemu-uefi-ab-boot-test.sh.
qemu-uefi-ab-boot-test: disk-image
	./hack/qemu-uefi-ab-boot-test.sh $(BUILD_DIR)/rootfs/disk.img $(BUILD_DIR)/bzImage $(BUILD_DIR)/rootfs

# Phase 3 cont'd: proves LifecycleService.Rollback (internal/api/
# lifecycle.go) works over a real gRPC call against a running node -
# boots slot A, calls `haproxyosctl lifecycle rollback` over real mTLS
# (credentials extracted straight from disk.img's STATE partition via
# debugfs, not the console - see hack/qemu-lifecycle-rollback-test.sh
# for why), and watches the guest genuinely reboot itself (no
# -no-reboot this time) into slot B with STATE intact. Requires
# haproxyosctl built (see `build`).
qemu-lifecycle-rollback-test: build disk-image
	./hack/qemu-lifecycle-rollback-test.sh $(BUILD_DIR)/rootfs/disk.img $(BIN_DIR)/haproxyosctl

# Phase 3 cont'd: proves Secure Boot signing/enforcement actually works,
# both directions - a UKI signed with a throwaway test key (image/
# secureboot/gen-test-key.sh, generated fresh every run, never
# committed) boots under real, Secure-Boot-enabled UEFI firmware with
# that key enrolled (image/secureboot/enroll-vars.sh); an unsigned UKI
# on the exact same enrolled vars is refused by firmware itself
# ("Access Denied"), never reaching the kernel. Requires sbsigntool
# (ukify shells out to sbsign) and python3-virt-firmware (virt-fw-vars).
# See hack/qemu-secureboot-test.sh for why this needs
# OVMF_CODE_4M.secboot.fd and -machine q35,smm=on specifically, unlike
# every other UEFI boot test here.
qemu-secureboot-test: kernel-build rootfs-build
	./hack/qemu-secureboot-test.sh $(BUILD_DIR)/bzImage $(BUILD_DIR)/rootfs

# Phase 3 cont'd: proves LifecycleService.Upgrade actually installs a
# *genuinely new* rootfs onto the inactive slot over a real gRPC call -
# builds a second, genuinely different rootfs (different squashfs,
# different root hash) and release bundle (image/release/assemble.sh)
# on the fly, boots the existing disk.img, calls `haproxyosctl
# lifecycle upgrade` with the new bundle, and watches the guest
# genuinely reboot itself into it - HTTP healthy again, STATE intact,
# and the kernel's own cmdline confirming the new slot's partitions and
# root hash (not which HTTP port answers - STATE's persisted config is
# shared across both slots by design, see hack/
# qemu-lifecycle-upgrade-test.sh's own header comment for the real bug
# that assumption caught) - then checks Rollback afterward still
# correctly brings back the untouched original slot. Requires
# haproxyosctl built (see `build`). See
# hack/qemu-lifecycle-upgrade-test.sh.
qemu-lifecycle-upgrade-test: build disk-image
	./hack/qemu-lifecycle-upgrade-test.sh $(BUILD_DIR)/rootfs/disk.img $(BUILD_DIR)/bzImage $(BUILD_DIR) $(BIN_DIR)/haproxyosctl

# Phase 3 cont'd: proves LifecycleService.Upgrade's wait_for_health -
# a healthy new slot confirms (Supervisor.OnStable -> internal/
# bootcommit's marker cleared) and stays; an unhealthy one (haproxyosd
# built dynamically-linked into a rootfs with no libc/dynamic linker at
# all, so it can never even exec - see hack/
# qemu-lifecycle-upgrade-health-test.sh's own comment) reverts and
# reboots back automatically (Supervisor.GiveUpAfter/OnGiveUp), with no
# RPC call driving the revert itself. Requires haproxyosctl built (see
# `build`).
qemu-lifecycle-upgrade-health-test: build disk-image
	./hack/qemu-lifecycle-upgrade-health-test.sh $(BUILD_DIR)/rootfs/disk.img $(BUILD_DIR)/bzImage $(BUILD_DIR) $(BIN_DIR)/haproxyosctl

# Phase 3 cont'd: assembles a real Unified Kernel Image (UKI) - kernel +
# exact boot cmdline, one PE/COFF executable - via `ukify`
# (systemd-ukify), and a FAT32 ESP image with it installed at the
# UEFI-spec removable-media fallback path (image/uki/esp-image.sh, no
# mount/loop device, mtools only). root's data/hash devices are baked
# in as /dev/vdb+/dev/vdc, not /dev/vda+/dev/vdb - the ESP itself takes
# the vda slot once it's attached (see hack/qemu-uefi-boot-test.sh's own
# comment for how that was actually caught). Requires ukify
# (systemd-ukify) and mtools/dosfstools.
uki-image: kernel-build rootfs-build
	./image/uki/assemble.sh $(BUILD_DIR)/rootfs/haproxyos.efi $(BUILD_DIR)/bzImage \
		$(BUILD_DIR)/rootfs /dev/vdb /dev/vdc
	./image/uki/esp-image.sh $(BUILD_DIR)/rootfs/esp.img $(BUILD_DIR)/rootfs/haproxyos.efi 64

# Phase 3 cont'd: proves the UKI actually boots under *real* UEFI
# firmware (OVMF) - no QEMU -kernel/-append shortcut at all, unlike
# every other boot test here. See hack/qemu-uefi-boot-test.sh. Requires
# OVMF (package: ovmf).
qemu-uefi-boot-test: uki-image
	./hack/qemu-uefi-boot-test.sh $(BUILD_DIR)/rootfs $(BUILD_DIR)/rootfs/esp.img
