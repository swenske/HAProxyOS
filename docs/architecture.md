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
unauthenticated endpoint (`internal/pki`, implemented in Phase 2). Each
node maintains its own self-signed Ed25519 CA (10-year validity), and
issues itself a server certificate for the gRPC listener. On first boot
it also issues an initial admin client certificate and prints it once
(there's no shell to retrieve it later) - the trust anchor a real
deployment would instead hand out through `LifecycleService.Install`'s
side channel (Phase 3, not built yet). `SystemService.
GenerateClientConfiguration` issues further client certificates
(currently 1 year validity, no renewal/rotation flow) once you already
have one; roles are carried in the certificate's `Subject.Organization`
field (the Kubernetes client-cert-auth idiom).

Roles are enforced, not just carried: `internal/api/authz.go`'s
`UnaryAuthInterceptor`/`StreamAuthInterceptor` check every single RPC
(both services are wired via `grpc.UnaryInterceptor`/
`grpc.StreamInterceptor` in `cmd/haproxyosd`) against a static
method -> required-roles table. Two roles exist: `os:admin` (everything)
and `os:reader` (observability/status RPCs only - explicitly *not*
`List`/`Read`/`Copy`/`Dmesg`/`Logs`/`DiskUsage`/`PacketCapture`, which
don't mutate anything but can expose sensitive file contents or traffic,
nor `GenerateClientConfiguration` itself, since issuing credentials is
its own privileged operation - an `os:reader` cannot mint itself an
`os:admin` cert). The table is fail-closed: an RPC with no entry
defaults to admin-only, and a test (`internal/api/authz_test.go`)
registers every service against a real `*grpc.Server` and cross-checks
its actual method list against the table in both directions, so a new
RPC that forgets an entry is caught immediately rather than silently
defaulting open. Verified for real too: an `os:reader` certificate can
call `HAProxyService.ShowInfo` but gets `PermissionDenied` calling
`ApplyConfig` or trying to self-escalate via
`GenerateClientConfiguration`.

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

- **Phase 0** (done): repo structure, gRPC contract, CI/CD skeleton,
  build-system placeholders.
- **Phase 1** (boot proof done): a from-allnoconfig, 1610-line explicit
  kernel config (`kernel/configs/haproxyos_defconfig` - no network, no
  disk/block drivers, no ACPI, initramfs-only) boots under QEMU with
  `rootfs/init` - a plain `CGO_ENABLED=0` Go binary - as PID 1. Verified
  end-to-end via `make qemu-boot-test` (`kernel/Dockerfile`'s `build`/
  `export` stages + `hack/build-initramfs.sh` + `hack/qemu-run.sh`), both
  locally and via the identical Docker build on `haproxyos-runner01`
  (`image-build.yml`).
  Turned out **not to need `pkgs/musl-toolchain` or `pkgs/busybox` at
  all**: a statically-linked Go binary needs no libc, so there's nothing
  for PID 1 to link against - those two `pkgs/` placeholders stay
  `FROM scratch` until something written in C (HAProxy, bird, keepalived)
  actually needs a toolchain, which is Phase 2+.
  Still open for Phase 1: real rootfs assembly beyond a single init binary
  (`rootfs/assemble.sh` still a stub) isn't needed yet either, since the
  initramfs *is* the whole rootfs for this boot-proof milestone.
- **Phase 2** (in progress): HAProxy integration (static musl build,
  supervised by `haproxyosd`), `HAProxyService`'s core RPCs implemented
  and reachable **inside the QEMU-booted kernel itself** - the kernel
  config grew real networking (virtio-net, `CONFIG_UNIX`/`INET`, DHCP via
  kernel-builtin `IP_PNP` - no userspace network tooling needed), and
  `rootfs/init` now supervises `haproxyosd` (which supervises `haproxy`)
  instead of just proving the boot chain. Verified with `make
  qemu-network-test`: a host port forwarded to the guest's HAProxy
  actually answers real HTTP, both locally and via `image-build.yml` on
  `haproxyos-runner01`. mTLS is now mandatory on every connection (see
  "mTLS / PKI" above) - `credentials.NewTLS` on the gRPC server, no
  plaintext fallback, verified with a real mismatched-CA connection
  attempt being rejected (unit test + manual check) and a fresh
  `GenerateClientConfiguration`-issued cert authenticating successfully.
  Role enforcement (`os:admin`/`os:reader`) is implemented and verified
  too - see "mTLS / PKI" above.
  `HAProxyService`'s runtime map/ACL/certificate RPCs are also
  implemented now, against real, empirically-verified HAProxy runtime API
  behavior rather than assumed syntax (probed a live instance's `help`
  output and tested each command directly before writing the Go code -
  see `internal/haproxy/runtime_maps.go`/`runtime_certs.go`). Two things
  worth remembering if you touch this: `MapList`/`ACLUpdate` only see
  *file-backed* maps/ACLs (`map(<path>)`/`acl ... -f <path>` in the
  running config - inline ones aren't addressable via the runtime API at
  all), and HAProxy's `set map` does **not** upsert - it errors on a
  missing key - so `MapUpdate`/`ACLUpdate`'s "set" path is really
  delete-then-add. `CertificateUpload`/`CertificateList`/
  `CertificateDelete` manage HAProxy's in-memory certificate *store*
  (`new`/`set`/`commit`/`del ssl cert`) - a freshly uploaded certificate
  is loaded and inspectable but reports "Unused" until a `bind ... ssl
  crt-list <list>` in the running config actually references it, which
  `ApplyConfig` doesn't have first-class support for wiring up yet.
  Still open: restart-on-crash supervision (`rootfs/init` starts
  `haproxyosd` once and just reaps zombies forever - no restart if it
  crashes), and a dedicated `uid`/`chroot` directive in the bootstrap
  `haproxy.cfg` (HAProxy currently logs its own "started as root without
  chroot" warning - real privilege-drop work belongs in Phase 4's CIS
  hardening pass, but this specific fix is small enough to consider
  sooner).
  HAProxy's own metrics keep using its **built-in** Prometheus exporter
  (`internal/haproxy` just proxies the runtime socket/config, it doesn't
  reimplement metrics export) - `internal/exporter` (HAProxyOS's own,
  system-level, built on top of the gRPC API) is explicitly **deferred
  past Phase 2**, not part of this phase.
- **Phase 3**: real immutability - A/B, dm-verity, UKI, Secure Boot,
  `LifecycleService.Install`/`Upgrade`/`Rollback`.
- **Phase 4**: SELinux policy + full CIS hardening pass.
- **Phase 5**: `NetworkService` - bird (BGP), keepalived (VRRP), nftables.
- **Phase 6**: companion website + dedicated Proxmox-hosted backend
  (separate container from the runner) + remote kernel-menuconfig UI -
  separate repository, separate plan.
