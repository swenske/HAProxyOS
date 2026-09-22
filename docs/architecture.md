# HAProxyOS — Architecture

HAProxyOS is an ultra-light, immutable, API-driven Linux distribution built
from scratch (LFS-style), inspired by [Talos Linux](https://github.com/siderolabs/talos)
but centered on HAProxy as the primary reverse-proxy/load-balancer, with
optional network features (BGP via [bird](https://bird.network.cz/),
VRRP via [keepalived](https://www.keepalived.org/), firewalling via
nftables).

## Design goals

- **Immutable**: the root filesystem is a read-only squashfs image; nothing
  on a running node is meant to be hand-edited.
- **API-driven, no shell**: there is no SSH daemon, no interactive shell,
  no package manager on the target OS. Every operation - configuration,
  observability, upgrades - goes through the gRPC API served by
  `haproxyosd` (see `docs/api-routes.md`).
- **Minimal attack surface / minimal CVEs**: only what HAProxy (and the
  optional network features) actually need is compiled in. No unused
  kernel drivers, no unused userspace.
- **CIS-hardened, mTLS, SELinux, trusted boot**: see the dedicated sections
  below.

### Build system vs. target OS - an important distinction

The *build system* (this repo's `kernel/`, `pkgs/`, Dockerfiles, and the
self-hosted GitHub Actions runner it runs on) is an ordinary Linux/Docker
toolchain - full shell, full package manager, the works. That's normal and
expected: it's not the thing being hardened.

The *target OS* (what actually boots on a HAProxyOS node) is what has no
shell, no package manager, and no SSH. Nothing in the build system's own
tooling ships into the target rootfs.

## Immutability: A/B partition layout

Modeled directly on Talos's own disk layout:

| Partition  | Purpose                                                        |
|------------|-----------------------------------------------------------------|
| `BOOT-A`   | Kernel + squashfs rootfs, slot A (Unified Kernel Image)          |
| `BOOT-B`   | Kernel + squashfs rootfs, slot B                                 |
| `STATE`    | Node identity, mTLS certs, applied declarative configuration     |
| `EPHEMERAL`| Writable overlay for paths that must persist but aren't part of the image (e.g. `/var/lib/haproxy`) |
| `META`     | Small key/value store for install-time metadata (`MetaWrite`/`MetaDelete`, see `docs/api-routes.md`) |

Only one of `BOOT-A`/`BOOT-B` is active at a time. `LifecycleService.Upgrade`
writes the new image to the *inactive* slot, switches the bootloader
default, reboots, and watches the new slot's health; if it doesn't report
healthy within the configured timeout, the bootloader default is switched
back automatically (`Rollback`) - no manual intervention needed.

## Trusted boot

- Each `BOOT-*` slot is a **Unified Kernel Image** (kernel + initrd +
  cmdline combined into one signed EFI binary).
- The image is signed with the project's own Secure Boot key (`sbsign`);
  UEFI Secure Boot refuses to boot anything not signed by a key enrolled
  on the node.
- The squashfs rootfs is mounted through **dm-verity**, so any tampering
  with the on-disk image (not just the boot chain) is detected at mount
  time, not silently trusted.

## mTLS / PKI

Every gRPC call is authenticated with a client certificate - there is no
unauthenticated endpoint. The project maintains its own CA;
`SystemService.GenerateClientConfiguration` issues short-lived, role-scoped
client certificates (mirroring Talos's own bootstrap flow) rather than
requiring certs to be provisioned out-of-band. `internal/pki` (Phase 2)
owns this.

## SELinux and CIS hardening

- A minimal, project-specific SELinux policy module confines `haproxyosd`,
  HAProxy, and the optional network daemons to exactly the syscalls/files
  they need (Phase 4) - not a stock distro policy.
- Kernel hardening: `lockdown=confidentiality`, no loadable kernel modules
  at runtime in production builds (or a tightly restricted allow-list if
  a specific driver genuinely needs to load late), hardened sysctls baked
  into the image rather than left to runtime configuration.
- No setuid binaries beyond what's strictly required; `haproxyosd` runs as
  the sole privileged process, dropping capabilities it doesn't need.

## The "no shell" API surface

Talos replaces shell access with a fixed set of read-only, scoped gRPC
methods (`List`, `Read`, `Copy`, `Logs`, `Dmesg`, `PacketCapture`) instead
of arbitrary command execution. HAProxyOS follows the same approach - see
`docs/api-routes.md` for the full catalog, derived directly from Talos's
own `machine.proto`/`lifecycle.proto` (verified against
`siderolabs/talos` on GitHub, not reconstructed from memory).

## Optional network features

`bird` (BGP) and `keepalived` (VRRP) are opt-in per node, selected in the
node's declarative configuration. When a feature isn't enabled, its binary
is simply absent from the built rootfs image - not installed-but-disabled,
genuinely not present, which is what keeps a minimal node's attack surface
and image size down. `NetworkService`'s corresponding RPCs report
`MODULE_STATE_NOT_ENABLED` rather than erroring in that case.

## Companion website

A separate, dedicated backend (hosted on the user's own Proxmox, in a
container **distinct from** the self-hosted GitHub Actions runner) will
serve image downloads and, later, a UI for the kernel version/module
selection workflow that `make kernel-menuconfig` currently does locally
(see the Makefile). This is Phase 6, a separate repository and a separate
plan - not implemented yet.

## Roadmap

- **Phase 0** (this scaffolding): repo structure, gRPC contract, CI/CD
  skeleton, build-system placeholders.
- **Phase 1**: minimal kernel + shell-less init that boots under QEMU (no
  HAProxy, no API yet).
- **Phase 2**: HAProxy integration, `HAProxyService` fully implemented,
  HAProxyOS's own Prometheus exporter (distinct from HAProxy's built-in
  one), mTLS/PKI (`internal/pki`).
- **Phase 3**: real immutability - A/B, dm-verity, UKI, Secure Boot,
  `LifecycleService.Install`/`Upgrade`/`Rollback`.
- **Phase 4**: SELinux policy + full CIS hardening pass.
- **Phase 5**: `NetworkService` - bird (BGP), keepalived (VRRP), nftables.
- **Phase 6**: companion website + dedicated Proxmox-hosted backend
  (separate container from the runner) + remote kernel-menuconfig UI -
  separate repository, separate plan.
