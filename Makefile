include versions.mk

MODULE  := github.com/swenske/HAProxyOS
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

BIN_DIR   := bin
BINARIES  := haproxyosd haproxyosctl
BUILD_DIR := build

GEN_DIR := gen

.PHONY: all build test vet lint proto clean kernel-menuconfig \
	kernel-build init initramfs qemu-boot-test

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
