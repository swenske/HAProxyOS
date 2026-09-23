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
	qemu-verity-boot-test

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
