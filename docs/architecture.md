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
- **Phase 2** (done): HAProxy
  integration (static musl build,
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
  `CertificateDelete` manage HAProxy's certificate *store*
  (`new`/`set`/`commit`/`del ssl cert`) and - now, `CertificateUpload`'s
  optional `crt_list`/`sni` fields - can also **bind** an uploaded cert
  into a `crt-list` a running `bind ... ssl crt-list <path>` already
  references (`add ssl crt-list`, with SNI filters), making it actually
  reachable by TLS clients instead of just sitting in the store reporting
  "Unused". `CertificateDelete`'s matching `crt_list` field unbinds
  first - HAProxy refuses `del ssl cert` on anything still bound
  ("in use, can't be deleted!"). One real constraint worth remembering:
  HAProxy refuses to even **start** a `bind ... ssl crt-list <path>`
  whose crt-list file is empty ("no SSL certificate specified") - a
  crt-list-backed listener needs at least one seed certificate already
  in the file at boot, uploaded certs are *additional* entries, not the
  first one. Verified with real TLS handshakes, SNI included: uploaded +
  bound a second certificate under a distinct SNI name, confirmed
  `openssl s_client -servername <name>` gets that certificate while a
  plain connection with no SNI still gets the original seed certificate,
  then unbound + deleted it. This was the one Phase 2 gap left open
  before - Phase 2 is now fully done.

  The other two gaps this phase had are closed:

  **Restart-on-crash.** `rootfs/init` no longer just reaps zombies
  forever - `rootfs/init/supervisor.go`'s `Supervisor` restarts
  `haproxyosd` every time it exits, with a backoff that grows (capped at
  30s) on fast repeated crashes and resets to the 1s minimum once an
  instance has stayed up 60s (so one old crash loop doesn't leave a
  later, unrelated crash waiting the full backoff to recover). There's
  deliberately **no give-up threshold** - `haproxyosd` is the only way to
  reach a node at all (see "no shell" above), so stopping restarts after
  N failures - systemd's default - would leave the node permanently
  unmanageable with no fallback the way SSH would be for a normal box.
  All child-reaping happens through a single shared `wait4(-1, ...)`
  loop, matching the pattern (never call `exec.Cmd.Wait()` when a
  supervisor loop also reaps children directly) `internal/haproxy.
  Manager` already relied on for `haproxy`'s own `-sf` reload; a second,
  independent reap loop would race the first to collect the same pid.
  Verified with real subprocesses, not mocks
  (`rootfs/init/supervisor_test.go`): three consecutive crashes produce
  three distinct new pids, the backoff sequence is exactly right for a
  fast crash loop and resets correctly after a stable run, an unrelated
  decoy child exiting is never mistaken for the supervised process, and
  a process that fails to even start doesn't hang the loop.

  **HAProxy privilege drop.** The bootstrap `haproxy.cfg` now sets
  `chroot /var/empty` + numeric `uid 1000`/`gid 1000` (no `/etc/passwd`
  on this rootfs to resolve named `user`/`group` against, and HAProxy
  doesn't need one for numeric ids). `haproxyosd` creates `/var/empty`
  itself (`-haproxy-chroot-dir`, mode `0000` - genuinely empty and
  inaccessible, since nothing is ever opened from inside it: every file
  HAProxy touches - config, maps, ACLs, certs, the stats socket bind - is
  opened before it chroots and drops privileges). HAProxy's own "started
  as root without chroot" warning is gone. Verified end-to-end with
  privilege dropping actually active, not just config-parses-cleanly:
  confirmed the live `haproxy` process's real UID/GID (`ps`), and reran
  the full ApplyConfig/map/ACL/cert test sequence against it to confirm
  none of that broke under chroot + dropped privileges - it doesn't,
  since all the file access those need happens pre-chroot.
  HAProxy's own metrics keep using its **built-in** Prometheus exporter
  (`internal/haproxy` just proxies the runtime socket/config, it doesn't
  reimplement metrics export) - `internal/exporter` (HAProxyOS's own,
  system-level, built on top of the gRPC API) is explicitly **deferred
  past Phase 2**, not part of this phase.
- **Phase 3** (build side started): real immutability - A/B, dm-verity,
  UKI, Secure Boot, `LifecycleService.Install`/`Upgrade`/`Rollback`.
  `rootfs/assemble.sh` builds a real squashfs image of the rootfs (same
  content as Phase 2's initramfs - init, haproxyosd, static haproxy,
  bootstrap config - `-all-root` since there's no `/etc/passwd` to
  resolve any other owner against) and computes its dm-verity hash tree
  via `veritysetup format`, requiring neither step to run as root. The
  kernel config grew `SQUASHFS`/`DM_VERITY` support (both nested behind
  gating menus - `MISC_FILESYSTEMS` and `MD` respectively - that
  `merge_config.sh` doesn't warn about if you forget them, it just
  silently drops the symbol; caught by grepping the merged `.config`
  afterward, not by trusting a clean merge). Verified two ways `veritysetup
  verify` actually enforces integrity, not just that the happy path
  works: accepts the real image against its own root hash, and rejects a
  deliberately single-byte-tampered copy of it, reporting the exact
  corrupted block position.
  A real build-time bug caught along the way: `mksquashfs`, run as a
  non-root build user, silently *drops* any directory it can't `open()`
  to traverse - including one this project itself `chmod 000`'d on
  purpose (the HAProxy chroot jail, `/var/empty`) - with only a
  one-line, easy-to-miss "Could not open ... skipping" warning. Fixed by
  defining that entry as an `mksquashfs` pseudo-file (`-p "var/empty D 0
  0000 0 0"`) instead of a real chmod'd directory in the build tree, so
  it never needs to be traversed at all. `mktemp -d`'s default `0700`
  leaking into the squashfs root directory's own mode was a second,
  related "build user's own environment quietly changes the image"
  bug, fixed with an explicit `-root-mode 0755`.
  The kernel now boots root **directly from that dm-verity-protected
  squashfs image** - no initramfs, no userspace verity setup at all -
  via the `dm-mod.create=` cmdline parameter (`CONFIG_DM_INIT`, built
  for exactly this: "allow mounting rootfs without requiring an
  initramfs"). Two virtio-blk drives (squashfs data + verity hash tree),
  the kernel assembles and verifies `/dev/dm-0` itself before mounting
  it read-only and running `/sbin/init` straight out of the verified
  image (see `hack/qemu-verity-boot-test.sh`). Getting there needed
  three more kernel config additions, each found by a real boot failing
  first, not by reading docs in advance: `CONFIG_VIRTIO_BLK` (itself
  gated behind `drivers/block`'s own `menuconfig BLK_DEV`, the same
  silently-dropped-symbol trap as `SQUASHFS`/`DM_VERITY` above -
  `CONFIG_BLK_DEV=y` first); `CONFIG_DM_INIT` for the cmdline parameter
  itself; and `CONFIG_CRYPTO_SHA256` - `DM_VERITY`'s own `select
  CRYPTO_HASH` doesn't pull in an actual sha256 implementation reachable
  by name through the crypto API, only the separate `CRYPTO_LIB_SHA256`
  helper other kernel code already needed - the first boot attempt got
  as far as constructing the dm-verity target and failed with "Cannot
  initialize hash function (-2)". The exact dm-verity table string
  (field order, and in particular `hash_start_block=1` - the hash tree
  always starts one hash-block after `veritysetup`'s own superblock)
  was derived and confirmed with a real `dmsetup create --readonly`
  against loop devices - including deliberately corrupting the
  underlying data device in place and confirming a live, mounted
  dm-verity device throws a real I/O error on the next read - before
  ever putting it in a kernel cmdline. `/dev/vda`/`/dev/vdb` **path**
  references work in `dm-mod.create=` (`CONFIG_DEVTMPFS_MOUNT=y` gets
  `/dev` populated in time for it), so there was no need to fall back to
  the kernel doc's major:minor form. The boot test also reboots against
  a single-byte-corrupted copy of the image and confirms the *kernel*
  refuses to mount it - corrupting the squashfs superblock specifically
  (offset 0), since a random deeper offset (the one the userspace
  `veritysetup verify` tamper test above uses) can land in a file
  `/sbin/init` only reads *after* printing its own boot marker, letting
  a real corruption slip past a naive "did the marker print" check.
  `rootfs/init/main.go`'s `mountEphemeral` now gives the verified root a
  writable layer, entirely tmpfs-backed: `/run` and `/tmp` mounted
  empty (nothing pre-existing there needs to survive - haproxyosd's
  `/run/haproxyos`, HAProxy's stats socket/pid file, `Manager.Validate`'s
  tmpfile); `/etc` needs its bootstrap `haproxy.cfg` bytes read *before*
  the tmpfs overmount and rewritten after, since that file (unlike
  `/run`/`/tmp`) isn't empty on a freshly-booted node - this is what
  makes both PKI bootstrap (`/etc/haproxyos/pki`) and a live
  `ApplyConfig` RPC (same path) actually work. `/var` is deliberately
  left alone, still squashfs-backed: the only thing under it is
  `/var/empty`, HAProxy's chroot jail, which must keep the exact
  immutable mode-0000 baked into the image, not a fresh writable one.
  `hack/qemu-verity-boot-test.sh`'s "good" boot now adds virtio-net +
  DHCP like Phase 2's own test and asserts real HTTP 200 from HAProxy,
  not just the boot marker - proving the whole chain (PKI bootstrap,
  HAProxy startup, config read) genuinely works from a dm-verity-booted,
  read-only node, not just that the kernel got as far as running
  `/sbin/init`.
  A real persistent STATE partition now backs both `/etc/haproxyos/pki`
  **and** applied HAProxy config: `rootfs/state-image.sh` pre-formats a
  small, blank ext4 image at **build time** (`mkfs.ext4` - no mkfs
  binary ships on the target, matching the "no package manager on the
  node" rule; needed `CONFIG_EXT4_FS` in the kernel, pulled in cleanly
  via `select` with no gating-menu surprise this time), and
  `rootfs/init/main.go`'s new `mountState` mounts it once, at
  `/etc/.state` (has to live inside the tmpfs `mountEphemeral` already
  put at `/etc` - a fresh path like `/mnt/state` doesn't exist on the
  read-only squashfs root and can't be created there; caught by a real
  boot silently regenerating a new CA every time despite `mountState`
  running, since every step past the failed `MkdirAll` just logged and
  moved on rather than aborting the boot), then bind-mounts its `pki/`
  and `haproxy/` subdirectories over `/etc/haproxyos/pki` and
  `/etc/haproxy` respectively. `/etc/haproxy` needs first-boot seeding
  the same way `mountEphemeral` already seeds `/etc` itself: the
  bootstrap `haproxy.cfg` bytes are copied into the persistent
  `haproxy/` subdirectory *only if it's still empty*, so a later boot
  after a real `ApplyConfig` never gets overwritten back to the
  bootstrap default. Both `cmd/haproxyosd`'s PKI bootstrap and
  `internal/haproxy.Manager.Apply` now call `syscall.Sync()` right
  after writing, so durability doesn't depend on QEMU's own
  shutdown-time cache flush.
  Proven with a real three-boot test (`hack/qemu-state-persist-test.sh`,
  not just "the mount didn't error"), against the *same* `state.img`
  each time: boot 1 must log haproxyosd's "first boot - generated a new
  CA" line (fresh bootstrap) and serve on the bootstrap default's
  `:8080`; boot 2 must not log that line again (loaded, not
  regenerated); between boot 2 and boot 3 the script directly injects a
  new `haproxy.cfg` into `state.img` via `debugfs -w` - no mount, no
  loop device, no root - bound to `:8081` instead, standing in for a
  real `ApplyConfig` RPC (a real `mount -o loop` was the first thing
  tried here, and failed outright on `haproxyos-runner01` - an
  unprivileged LXC container - with "failed to setup loop device",
  despite working fine locally; already covered elsewhere by
  `image-build.yml`'s own mTLS integration test; what's under test here
  is specifically whether `Manager.Apply`'s write target actually lives
  on persistent storage) - and boot 3 must answer on `:8081` and
  specifically **not** on `:8080`, proving HAProxy started from the
  persisted config, not the squashfs's read-only bootstrap default.
  A real GPT A/B partition layout now exists too:
  `image/disk/assemble.sh` builds a single disk image with two
  independently bootable slots - `BOOT-A-DATA`/`BOOT-A-HASH` and
  `BOOT-B-DATA`/`BOOT-B-HASH` (each a squashfs + dm-verity hash tree
  pair, fixed-size and over-provisioned like a real A/B system, not
  sized to exactly fit today's content) plus `STATE` - all written
  directly at computed byte offsets (`sgdisk -i` for the exact sector,
  `dd seek=`), no mount, no loop device, matching the lesson from the
  STATE-persistence test's own `debugfs` fix above. `CONFIG_EFI_PARTITION`
  (GPT table *parsing*) turned out to already be on by default - it's
  independent of `CONFIG_EFI` (the UEFI *runtime services* feature,
  still not enabled - see below), so no kernel change was needed for
  the kernel to recognize the partitions at all.
  `hack/qemu-ab-boot-test.sh` proves both slots are actually,
  independently bootable, not just that the partition table looks
  right: boots the *same* `disk.img` twice, `dm-mod.create=` pointed at
  `/dev/vda1`+`/dev/vda2` (slot A) then `/dev/vda3`+`/dev/vda4` (slot B),
  both must serve real HTTP. Both slots hold identical content for now
  - there's no `LifecycleService.Upgrade` yet to install something
  different into the inactive slot, so this proves the layout/dm-verity-
  via-partition-device mechanics, not a real upgrade workflow.
  Still open: `rootfs/init/main.go`'s `mountState` doesn't know how to
  find the `STATE` partition (or which slot it booted from) on this
  real single-disk layout yet - it still expects STATE as a separate
  virtio-blk drive, which `hack/qemu-verity-boot-test.sh`/
  `qemu-state-persist-test.sh` still provide (deliberately kept as-is -
  they cover dm-verity tamper detection and STATE persistence, which
  `qemu-ab-boot-test.sh` doesn't re-test). There's no udev on this
  system, so no `/dev/disk/by-partlabel/*` to just read - finding STATE
  robustly needs either a kernel cmdline convention the bootloader/UKI
  sets, or a small amount of Go parsing the GPT table directly, given
  init already knows (from its own cmdline) which disk root came from.
  UEFI boot + a Unified Kernel Image is a bigger jump than it first
  looked: `CONFIG_EFI` (needed for `CONFIG_EFI_STUB`) `depends on
  CONFIG_ACPI`, and this kernel has never enabled ACPI at all - "UEFI
  boot" now also means bringing up a whole new subsystem, not just
  adding stub support, and is being tracked as its own separate slice
  rather than folded into A/B partitioning. Secure Boot signing and the
  `LifecycleService` RPCs to drive an actual install/upgrade/rollback
  are also still separate, unbuilt pieces.
- **Phase 4**: SELinux policy + full CIS hardening pass.
- **Phase 5**: `NetworkService` - bird (BGP), keepalived (VRRP), nftables.
- **Phase 6**: companion website + dedicated Proxmox-hosted backend
  (separate container from the runner) + remote kernel-menuconfig UI -
  separate repository, separate plan.
