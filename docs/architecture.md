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
  `rootfs/init/main.go`'s `mountState` now finds `STATE` on this real
  single-disk layout too: `resolveStateDevice` (`internal/bootslot` -
  moved there from a `rootfs/init`-local file once `LifecycleService.
  Rollback` became a second consumer, see later in this roadmap)
  parses init's own `/proc/cmdline` for the `dm-mod.create=` parameter
  it already booted with (confirmed, by actually booting a debug init
  and reading it back, that `/proc/cmdline` preserves the quotes
  verbatim - not assumed from the kernel's own reformatted dmesg
  "Command line:" line) and pulls out the verity target's data device.
  If that device is itself a partition (e.g. `/dev/vda1` - ends in a
  digit), `STATE` is derived by the fixed convention
  `image/disk/assemble.sh`'s own layout uses: always partition 5 on
  that same disk - no udev, no `/dev/disk/by-partlabel/*` needed, since
  this project controls both ends (image assembly and init) and can fix
  the convention rather than discover it generically. If the data
  device is a bare whole-disk path instead (e.g. `/dev/vda`, no
  partition number at all), that's `hack/qemu-verity-boot-test.sh`'s/
  `qemu-state-persist-test.sh`'s older separate-virtio-blk-drives
  harness - `resolveStateDevice` falls back to the original fixed
  `/dev/vdc`, so those two tests keep working completely unchanged
  (they're deliberately kept - they cover dm-verity tamper detection
  and STATE persistence in isolation, which `qemu-ab-boot-test.sh`
  doesn't re-test). `qemu-ab-boot-test.sh` now reboots slot A a second
  time on the same disk and confirms haproxyosd's "first boot" line
  does *not* reappear - proving `resolveStateDevice` actually works in
  practice, not just via `cmdline_test.go`'s unit tests (which cover
  the parsing itself, including the real cmdline string a boot produced
  and several malformed/foreign ones, without needing a VM for each
  case). The disk is no longer attached read-only in that test - only
  dm-verity's own `ro` flag in `dm-mod.create=` needs to protect the
  verity-mapped root, which it does regardless of the backing device's
  own writability, so `STATE` (an ordinary, unprotected partition) can
  be written to without weakening that.
  UEFI boot + a Unified Kernel Image turned out bigger than it first
  looked, but is now real: `CONFIG_EFI` (needed for `CONFIG_EFI_STUB`)
  `depends on CONFIG_ACPI`, so getting there meant bringing up ACPI in
  this kernel for the first time - resolved cleanly (`CONFIG_ACPI=y`
  alone was enough; its own dependency, `ARCH_SUPPORTS_ACPI`, is
  unconditionally selected by `X86_64` already), with `CPU_IDLE`/
  `POWER_SUPPLY`/`THERMAL` coming along as ACPI's own dependents,
  nothing silently dropped (checked the same way as every kernel config
  change in this project: grep the real, `olddefconfig`-resolved
  output, not just trust a clean build).
  `image/uki/assemble.sh` builds a genuine Unified Kernel Image with
  `ukify` (systemd-ukify) rather than hand-rolling one with `objcopy`:
  reading the actual kernel EFI stub source
  (`drivers/firmware/efi/libstub/efi-stub-helper.c`'s
  `efi_convert_cmdline`) showed it only ever reads its command line
  from the EFI `LoadOptions` the firmware passes when an image is
  launched interactively (matching
  `Documentation/admin-guide/efi-stub.rst`'s own documented "EFI
  shell" usage) - it does **not** look for a `.cmdline` PE section on
  its own, so a plain `objcopy`-assembled UKI booted with no NVRAM
  entry (the UEFI spec's removable-media fallback path, which is what
  this needs - no boot menu) would get no cmdline at all. `ukify`'s own
  stub (systemd-stub) does read `.cmdline`, then chain-loads into the
  *kernel's own* embedded EFI stub via the EFI handover protocol
  (`CONFIG_EFI_HANDOVER_PROTOCOL`, on by default once `EFI_STUB` is) -
  which is exactly why `CONFIG_EFI_STUB` still has to be real in the
  kernel too, not just present in systemd's stub binary: the code
  receiving that handover call lives in the kernel.
  `image/uki/esp-image.sh` builds the FAT32 ESP with `mtools`
  (`mformat`/`mmd`/`mcopy`) directly against the image file - no mount,
  no loop device, same reasoning as `rootfs/state-image.sh` and
  `image/disk/assemble.sh`.
  `hack/qemu-uefi-boot-test.sh` proves the whole chain works under
  *real* OVMF UEFI firmware - no QEMU `-kernel`/`-append` shortcut at
  all, unlike every other boot test in this project. First real attempt
  caught a genuine bug in the test itself, not the mechanism: the ESP
  becomes the *first* virtio-blk drive once attached, shifting
  squashfs/verity from `/dev/vda`+`/dev/vdb` to `/dev/vdb`+`/dev/vdc` -
  OVMF and the kernel both booted fine, dm-verity even assembled
  `/dev/dm-0` successfully, but against the ESP's own FAT metadata
  instead of the real squashfs ("metadata block 1 is corrupted"), since
  the UKI's baked-in cmdline still referenced the old device order.
  Since the exact dm-verity table computation was now duplicated across
  four places (this test plus `hack/qemu-verity-boot-test.sh`/
  `qemu-ab-boot-test.sh`/`qemu-state-persist-test.sh`), it was factored
  out into `hack/dm-verity-cmdline.sh`, shared by all four - the other
  three were re-verified to still pass unchanged after the refactor,
  not just assumed to.
  The ESP now lives on `image/disk/assemble.sh`'s single GPT disk too -
  the real, complete, single-disk shape a deployed node would have:
  partition 1 is the ESP, 2/3 are `BOOT-A-DATA`/`BOOT-A-HASH`, 4/5 are
  `BOOT-B-DATA`/`BOOT-B-HASH`, 6 is `STATE`. `image/disk/
  activate-slot.sh` rewrites **only** the ESP partition in place - a new
  UKI whose cmdline points at the other slot's data/hash partitions,
  `dd`'d at the ESP's own offset (found via `sgdisk -i 1`, same
  no-mount/no-loop-device pattern as everywhere else) - leaving both
  A/B slots' content and `STATE` completely untouched. This is
  deliberately the minimal, narrow operation a real
  `LifecycleService.Upgrade`/`Rollback` will eventually need at the
  image level: "make the other slot the one that boots" without
  disturbing anything else, most importantly `STATE` (PKI, applied
  config).
  Adding the ESP shifted every partition number by one, and surfaced a
  real bug the hard way: `rootfs/init/cmdline.go`'s `statePartitionDevice`
  still hardcoded `STATE` as partition 5 (now `BOOT-B-HASH`, not
  `STATE`), so `mountState` was silently mounting a dm-verity hash tree
  as if it were an ext4 filesystem - the mount failed, `mount()` only
  logs and falls back (never aborts the boot, see its own doc comment),
  so the visible symptom was PKI quietly regenerating a fresh CA on
  every single boot again, exactly like the *first* time this class of
  bug happened. Caught by actually switching to slot B and rebooting,
  not by inspection - fixed by updating the constant to `6` and adding
  `hack/qemu-uefi-ab-boot-test.sh`, which exists specifically to keep
  re-catching this: it boots slot A (fresh CA), calls
  `activate-slot.sh` to switch to slot B **in place**, boots again under
  real OVMF firmware with a single drive, and asserts both that the
  console's own `dm-mod.create=` line now references
  `/dev/vda4`/`/dev/vda5` (the switch actually took effect) and that
  haproxyosd's "first boot" log line does **not** reappear (`STATE`,
  and the CA on it, genuinely survived the switch).
  `LifecycleService.Rollback` is now real too: `internal/api/
  lifecycle.go` is the gRPC front end for exactly the ESP swap
  `activate-slot.sh` performs at build/install time, except it runs on
  an already-booted node. The target OS has no package manager, so it
  can never shell out to `ukify` the way `activate-slot.sh` does - this
  is precisely why that script now stages **both** slots' UKIs on the
  ESP, at fixed paths (`\HAPROXYOS\UKI-A.EFI`, `\HAPROXYOS\UKI-B.EFI`),
  alongside the active one (`\EFI\BOOT\BOOTX64.EFI`): at runtime,
  `Rollback` only needs to mount the ESP (`CONFIG_VFAT_FS` -
  `CONFIG_VFAT_FS=y` alone wasn't enough either, mounting failed
  outright with "codepage cp437 not found" until
  `CONFIG_NLS_CODEPAGE_437`/`CONFIG_NLS_ISO8859_1` were added too - each
  its own separate symbol from `FAT_DEFAULT_CODEPAGE`/
  `FAT_DEFAULT_IOCHARSET`, found by a real mount failing first) and copy
  the other slot's already-built UKI over `BOOTX64.EFI` - no PE
  manipulation, no build tooling, on the node at all.
  The cmdline-parsing logic (`dmVerityDataDevice`/`statePartitionDevice`)
  that used to live only in `rootfs/init` moved to a new shared package,
  `internal/bootslot` - `rootfs/init` and `internal/api/lifecycle.go`
  both need to answer "which slot am I running from, and where's the
  rest of the disk", and duplicating that logic a second time across a
  package boundary was the wrong call once there were two consumers of
  it, not just a hypothetical one. It also grew `ActiveSlot`/`OtherSlot`
  helpers `rootfs/init` never needed (STATE discovery doesn't care
  *which* slot, just that the device is a slot at all) but `Rollback`
  does (it needs to know current vs. target).
  Proven with a real gRPC call, not just that the underlying mechanism
  works when driven directly: `hack/qemu-lifecycle-rollback-test.sh`
  boots slot A, extracts `ca.crt`/`admin.crt`/`admin.key` straight from
  `disk.img`'s STATE partition via `debugfs` (haproxyosd only ever
  prints the admin cert/key to the console once, on first boot, and
  never the CA cert at all, by design - there's nothing on the console
  to trust a fresh external connection with), calls `haproxyosctl
  lifecycle rollback` over real mTLS, and - this is the one boot test in
  the whole project that does **not** pass `-no-reboot` to QEMU - watches
  the guest genuinely reboot itself inside the same QEMU process and
  come back up on slot B, with the boot marker appearing exactly twice,
  the second boot's own console cmdline referencing `/dev/vda4`, and
  PKI's "first boot" line appearing exactly once (`STATE` survived a
  real, API-driven reboot, not just a build-tool-driven one).
  Secure Boot signing/enforcement is now real too, proven both
  directions: `image/uki/assemble.sh` grew two optional trailing
  args (signing key/cert) - given both, `ukify build
  --secureboot-private-key`/`--secureboot-certificate` (which shells
  out to `sbsign`) signs the UKI; given neither, unsigned exactly as
  before, so every other boot test here is unaffected.
  `image/secureboot/gen-test-key.sh` generates a throwaway, self-signed
  RSA key + cert (never committed - a real project release key needs
  real key management: HSM, CI secret, offline root of trust, none of
  which a build script should generate on the fly) and
  `image/secureboot/enroll-vars.sh` enrolls it into a fresh OVMF vars
  file, Secure Boot on.
  A real bug caught the hard way, not by reading docs: `virt-fw-vars
  --enroll-cert <cert>` (the obvious "just enroll my cert" convenience
  shortcut) turned out to only populate `PK` and `KEK` - **never
  `db`**, the one list that actually authorizes *boot images* (`PK`/
  `KEK` only govern who can update the Secure Boot variables
  themselves) - so a correctly signed UKI, checked independently with
  `sbverify` and confirmed valid, still got refused with "Access
  Denied" by real firmware. Diagnosed by printing the resulting vars
  store (`virt-fw-vars -p`) and finding no `db` variable in it at all;
  fixed by switching to explicit `--set-pk`/`--add-kek`/`--add-db`
  (same cert, all three) instead of the shortcut. A second, purely
  environmental issue: the secboot-capable OVMF firmware binary
  (`OVMF_CODE_4M.secboot.fd`) produced **zero console output at all**
  under the plain `i440fx` machine type every other boot test in this
  project uses - identical command, only `-machine q35,smm=on
  -global driver=cfi.pflash01,property=secure,value=on` added, and it
  went from a silent hang to a normal boot; secure-boot-capable OVMF
  builds generally assume SMM-based flash variable protection, which
  needs `q35`.
  `hack/qemu-secureboot-test.sh` proves both directions with one real
  key: the signed UKI must boot; an *unsigned* UKI, on the exact same
  enrolled vars, must be refused by firmware itself (`grep`s the
  console for "Access Denied"), never even reaching the kernel.
  `LifecycleService.Upgrade` is now real too: it writes a *genuinely
  new* rootfs into the currently-inactive A/B slot from a local
  "release bundle" directory (`image/release/assemble.sh`:
  `rootfs.squashfs`/`rootfs.verity`/`uki-a.efi`/`uki-b.efi`), then
  switches the ESP and reboots - the same core trick `Rollback` uses
  (move an already-built UKI into place, never build one on the node),
  except the UKI comes from the bundle instead of from what
  `activate-slot.sh` already staged, because a genuinely new rootfs has
  a root hash nobody could have pre-staged at the original image's
  build time. `req.Source.Reference` is a local bundle directory path
  for now - real OCI/HTTPS distribution isn't built yet, a separate,
  distinct concern from the actual upgrade mechanics this proves.
  `internal/bootslot` grew `SlotDataDevice`/`SlotHashDevice`/
  `Disk` for this - the reverse direction from `ActiveSlot` (given a
  slot, find *its* partitions, not "which slot is currently running").
  Proven with a real gRPC call, via `hack/qemu-lifecycle-upgrade-test.sh`:
  boots slot A, builds a second rootfs with genuinely different content
  (different squashfs, different root hash), injects its release bundle
  into `disk.img`'s STATE partition via `debugfs` *before* the first
  boot (`haproxyosctl` and `haproxyosd` don't share a filesystem across
  this QEMU host/guest boundary, unlike this project's usual "share a
  filesystem" case - and writing to STATE from the host while the guest
  also has it mounted read-write would corrupt it, which is exactly why
  the injection happens before the first boot rather than concurrently
  with a running one), then drives a real `haproxyosctl lifecycle
  upgrade` call and watches the guest genuinely reboot itself into the
  new content, same `-no-reboot`-free pattern as the Rollback test.
  A real bug was found writing this test - not in `Upgrade` itself, but
  in the test's own first assumption: it gave the v2 rootfs a
  bootstrap HAProxy config bound to a different port, expecting that
  port to answer as proof v2 was running. It never did, because STATE
  is **one partition shared by both A/B slots**, not duplicated per
  slot, and `rootfs/init/main.go`'s `seedPersistentHaproxyCfg`
  deliberately never overwrites an already-persisted config - so slot
  B, booting after slot A already persisted its own config onto that
  shared STATE, just keeps serving what slot A left there. This is
  correct, intended behavior (an upgrade must never reset a node's
  live-applied HAProxy config back to some bootstrap default) - what
  was wrong was the test's verification method, not the production
  code. Fixed by proving genuinely new content took effect the same way
  `hack/qemu-uefi-ab-boot-test.sh` proves a slot switch did: reading the
  kernel's own "Kernel command line:" log line back and checking it
  references the new slot's partitions and root hash, not which HTTP
  port answers.
  `wait_for_health` is now real too: `Upgrade`, when asked, writes a
  persistent "boot pending confirmation" marker to STATE
  (`internal/bootcommit`) before switching the ESP and rebooting - the
  marker records the slot awaiting confirmation, which slot to fall
  back to, and (from `UpgradeRequest.health_timeout_seconds`) how long
  it gets. The *next* boot's `rootfs/init` (`checkBootCommit`) either
  gets that one confirmation attempt (decrementing the marker's
  `tries_left` before starting `haproxyosd`, so a *subsequent* boot into
  the same slot - if this one never confirms - finds it already
  exhausted and reverts immediately, without giving it a third try) or,
  finding tries already exhausted, reverts straight away without ever
  starting `haproxyosd` at all this boot.
  Confirmation itself is now a *real* HAProxy-level check, not an
  inference from process survival: `cmd/haproxyosd`, once it starts,
  checks for a pending marker and - in the background, so it never
  delays the gRPC server coming up - polls HAProxy's own stats socket
  (`internal/haproxy.Manager.ShowInfo`) via a new
  `internal/bootcommit.Confirm` until it succeeds several times in a
  row, then clears the marker itself. If that never happens within
  `HealthTimeoutSeconds`, `haproxyosd` reverts and reboots itself,
  directly - no RPC, no `rootfs/init` involvement needed for this path.
  A first version of this piggybacked on `Supervisor`'s own stability
  tracking instead (a proactive `OnStable` hook, firing once the
  `haproxyosd` *process* had merely stayed up for a while) - reused as
  the confirmation signal only briefly, and removed once real
  HAProxy-level confirmation landed: the two would have raced (a
  process-survival signal firing before, or after, the real health
  check, either clearing the marker prematurely on a genuinely broken
  HAProxy or double-reverting), and the weaker signal added nothing the
  stronger one didn't already cover.
  `rootfs/init`'s own `Supervisor.GiveUpAfter`/`OnGiveUp` stays, but
  narrows to the one thing `cmd/haproxyosd`'s own check structurally
  can't catch: `haproxyosd` crashing too fast, or too often, to ever
  reach the point of running its own confirmation loop at all - bounded
  *only* when a marker is pending (every other boot keeps the
  unconditional "restart forever" policy this package has always had,
  see its own doc comment). Without it, such a slot would sit
  unreachable forever: the cross-boot `tries_left` check only ever gets
  a chance to act on a *later* boot, which requires the machine to
  reboot again first. The two mechanisms are complementary, each the
  *only* one that can catch its respective failure - `Supervisor` has no
  visibility into HAProxy's health, and `haproxyosd` can't act if it
  never gets to run.
  Both revert paths share one `internal/bootrevert.To` helper
  (resolve the ESP device from `/proc/cmdline`, `internal/espswitch.
  Activate` the target slot, clear the marker - stopping short of the
  actual reboot, since `rootfs/init` blocks forever afterward as PID 1
  must, while `haproxyosd` just issues one and lets the whole machine go
  down with it), itself built on `internal/espswitch` - factored out of
  `Rollback`'s own inline ESP-swap logic once `rootfs/init`'s local
  revert became a second real consumer of the identical mechanism.
  None of this is visible over the original `Upgrade` call's own gRPC
  stream: by the time a revert might happen, that connection died with
  the first reboot, so a caller only ever sees the eventual outcome by
  reconnecting later (`SystemService.Version`, or simply which port
  answers), never a `"rolled-back"` stream message.
  Proven with a real gRPC call, three ways, via `hack/
  qemu-lifecycle-upgrade-health-test.sh`: boots slot A, then calls
  `Upgrade(wait_for_health=true)` three times against the same running
  node - a "good" bundle (just the existing v1 rootfs, repackaged;
  genuinely-new-content is what the *other* Upgrade test already
  proves, this one is purely about the health-check/revert mechanics)
  must show `"bootcommit: confirmed healthy"` and never revert; a
  "broken" bundle, a *fresh* rootfs with the host's own
  dynamically-linked `/bin/false` standing in for `haproxyosd` itself -
  since this rootfs ships no dynamic linker or libc at all (every real
  binary in it is statically linked, by design), `Supervisor` doesn't
  even get as far as a successful `exec`, hitting its spawn-failure path
  on every restart attempt, a realistic simulation of a badly built or
  wrong-architecture control-plane binary - must show the guest
  rebooting into it, then autonomously - **no RPC call from the test
  driving it** - `"giving up and reverting to slot B"`
  (`Supervisor.GiveUpAfter`, proving that backstop specifically); and a
  third, "haproxy-broken" bundle, with the *real* `haproxyosd` but
  `/bin/false` standing in for *haproxy* this time, must show
  `haproxyosd` itself coming up fine and logging its own confirmation
  attempt, then - again fully autonomous - `"bootcommit: rebooting to
  complete the revert"` (`cmd/haproxyosd`'s own check specifically, not
  the `Supervisor` backstop). All three reverts land back on whichever
  slot was active *when that particular Upgrade was called*, not a
  hardcoded fallback to the original slot A. Every boot is checked via
  the same real evidence `hack/qemu-uefi-ab-boot-test.sh` established:
  the kernel's own "Kernel command line:" log line, at specific boot
  numbers, referencing the expected slot's partitions and root hash.
  `LifecycleService.Install` is now real too: unlike `Rollback`/
  `Upgrade`, it has no existing partition table to build on, so it can't
  get away with only ever moving pre-built bytes into place - it lays
  out the *entire* disk itself, from a completely blank starting point.
  `sgdisk`/`mtools`/`mkfs.ext4` don't exist on the target OS any more
  than they do at runtime for `Rollback`/`Upgrade`, so this needed a
  genuinely Go-native GPT/FAT32/ext4 writer - found in
  `github.com/diskfs/go-diskfs` rather than hand-rolled: confirmed,
  empirically, before writing any production code, by building a full
  six-partition disk with it (GPT table, a real ext4 STATE filesystem,
  a real FAT32 ESP with a real UKI written into it) and booting the
  result under real OVMF - HTTP 200, PKI bootstrapped onto the
  go-diskfs-created STATE filesystem, on the first try after fixing one
  real thing that verification actually caught:
  `gpt.Table.ProtectiveMBR` defaults to `false`, and leaving it unset
  produces a GPT disk `sgdisk -p` reports as having a "corrupt MBR" (in
  practice: no protective MBR at all, all zero bytes at LBA 0) - an easy
  one-line fix (`ProtectiveMBR: true`) once caught, but exactly the kind
  of gap a library's own example code doesn't warn about.
  `internal/diskimage.Compute` is the pure-arithmetic half (given a
  disk's total byte size, lay out ESP/BOOT-A-DATA/BOOT-A-HASH/
  BOOT-B-DATA/BOOT-B-HASH sequentially and sector-aligned, matching
  `image/disk/assemble.sh`'s own fixed sizes and convention exactly,
  then give STATE whatever's left - unlike build time's fixed
  `STATE_MB`, meant for a deliberately small QEMU test disk, a real
  target disk's remaining space is put to use) - kept separate from
  `internal/api/install.go`'s actual go-diskfs calls specifically so the
  layout math has real unit tests without needing a disk or root to run
  them, the same reasoning `internal/bootslot`'s pure cmdline parsing
  was kept apart from the syscalls that act on what it parses.
  `Install` reads both slots' UKIs from the same kind of release bundle
  `Upgrade` already reads from (`image/release/assemble.sh`) - it never
  builds one either, just picks pre-built bytes for both slots this
  time instead of one. Both A/B slots get identical content on
  purpose: there's no "other slot" yet to leave untouched, the same
  starting point `image/disk/assemble.sh` itself produces at build
  time. Refuses two disks outright: one that already has a partition
  carrying one of HAProxyOS's own conventional GPT names (`ESP`,
  `STATE`, ...) - "looks like an existing install, use Upgrade/Rollback
  instead" - and the disk this node is itself currently booted from
  (repartitioning that out from under a running system would be
  catastrophic, and it's `Upgrade`'s territory anyway) - the latter
  reuses `internal/bootslot.Disk` against `/proc/cmdline`, the same
  parsing `Rollback`/`Upgrade` already trust. Doesn't reboot anything on
  success: the disk it just wrote isn't necessarily the one this node
  runs from at all (see the proto's own `disk` field comment), so
  getting a machine to actually boot from it is the caller/operator's
  job.
  Proven with a real gRPC call, via `hack/lifecycle-install-test.sh` -
  the one lifecycle test in this project where the RPC call itself
  doesn't run inside a VM: `Install` has no A/B/STATE machinery of its
  own to depend on (it's what *creates* that machinery), so `haproxyosd`
  runs natively on the test host, the same pattern `image-build.yml`'s
  own "HAProxy gRPC API integration test" step already uses, reading its
  bootstrapped PKI creds straight off `-pki-dir` rather than scraping a
  QEMU console log. Only the *result* is checked under a real VM - the
  same standard every other "is this actually bootable" claim in this
  project is held to. The test calls `Install` against a plain
  pre-allocated file (go-diskfs works identically against a real block
  device or a file, confirmed during the library verification above),
  confirms a second `Install` against the now-installed file is refused,
  boots the result under real OVMF and confirms real HTTP 200 plus a
  fresh PKI bootstrap, then - from *inside* that now-running instance,
  which has a genuine `dm-mod.create=` cmdline to check against, unlike
  the native process - confirms `Install` against `/dev/vda` (the disk
  it's actually booted from) is refused too. A bonus `Rollback` call
  proves slot B's identical copy is genuinely valid, not just slot A's.
  A real production signing key now exists too, closing out Phase 3:
  no HSM in this project's threat model (single maintainer, self-hosted
  CI), so the simplest thing genuinely safer than committing a key to
  git is what's used - `image/secureboot/gen-production-key.sh`
  generates it once, offline, by a human (20-year validity, unlike
  `gen-test-key.sh`'s 10 - rotating it means re-enrolling every already-
  deployed node's firmware by hand, so it's deliberately long-lived
  rather than something to renew casually); the private key lives only
  as a GitHub Actions encrypted secret (`SECUREBOOT_SIGNING_KEY`), never
  in the repo; the certificate half isn't sensitive (it has to be
  enrolled into every node's firmware `db` anyway) and is committed
  straight in at `image/secureboot/production-cert.pem`.
  `image-build.yml`'s own "production-signed release bundle" step needed
  no new signing code at all - `image/release/assemble.sh`'s existing
  optional `[signing-key] [signing-cert]` args (already built for
  `Upgrade`'s own release bundles) were enough, given a decoded,
  step-scoped temp copy of the secret. Runs only when the secret exists
  (skipped on a fork PR, or before the one-time key ceremony happens),
  and independently verifies both slots' signed UKIs with `sbverify`
  against the committed certificate before declaring success - proven
  for real locally before ever touching CI: both UKIs signed with the
  actual production key and `sbverify`-confirmed valid, using the exact
  same command the workflow step now runs.
- **Phase 4** (started): kernel/CIS hardening pass is real, SELinux
  policy is still open. Most of what a CIS benchmark's kernel-adjacent
  controls call out (loadable-module restrictions, `/dev/mem`, kexec,
  hibernation, USB/Firewire/staging drivers) was already true by
  construction from `allnoconfig` - this pass is specifically what was
  left: `kernel/configs/haproxyos_defconfig` grew a KSPP-style
  self-protection block (`STACKPROTECTOR_STRONG`, `SLAB_FREELIST_
  RANDOM`/`_HARDENED`, `HARDENED_USERCOPY`, `FORTIFY_SOURCE`,
  `INIT_ON_ALLOC`/`_FREE_DEFAULT_ON`, `CONFIG_SECURITY` + `SECURITY_
  YAMA`) verified, the same way every other Kconfig addition in this
  project has been, by diffing the *full* resolved config after merging
  - not just trusting a clean `merge_config.sh` run - to confirm
  `CONFIG_SECURITY=y` (the master gate every LSM needs, previously
  unset entirely) didn't silently pull in some *other* LSM along with
  Yama; the only side effects were an inert `CONFIG_INTEGRITY=y`
  framework dependency and a cosmetic `CONFIG_LSM=` default-ordering
  string naming LSMs that aren't actually compiled in. `INIT_ON_FREE`'s
  real, non-trivial perf cost (memory-zeroing on every kernel-side
  free, more noticeable than `INIT_ON_ALLOC`'s under allocation-heavy
  workloads) is accepted for now rather than pre-emptively tuned away,
  documented as the first thing to reconsider if a future real-traffic
  benchmark shows this project's network fast path regressing.
  `rootfs/init/main.go`'s new `hardenSysctls` is the runtime half - the
  tunable *values* (not features to compile in or out) that have no
  Kconfig home and need writing to `/proc/sys` at boot instead, since
  there's no `sysctl(8)`/procps on this rootfs at all: `kernel.
  dmesg_restrict`/`kptr_restrict` (hide the ring buffer and kernel
  pointers from anything without the matching capability - the
  unprivileged `haproxy` worker, uid 1000/chroot, specifically),
  `kernel.yama.ptrace_scope=2` (only `CAP_SYS_PTRACE` can attach -
  `haproxyosd`, root, still can; the worker no longer can, at all),
  and a set of anti-spoofing/anti-redirect/SYN-flood network sysctls
  chosen for what they mean to a reverse proxy specifically (`tcp_
  syncookies` isn't a generic checklist item on a box whose entire
  purpose is accepting inbound internet connections). A real gap was
  caught writing this, not assumed away: `net.ipv4.tcp_syncookies`
  doesn't exist as a `/proc/sys` node at all without `CONFIG_SYN_
  COOKIES`, which nothing else in the defconfig had pulled in - a real
  boot's console log showed exactly that one write failing ("no such
  file or directory") while every other sysctl, and the boot itself,
  looked completely fine either way, since a failed write here is
  logged but non-fatal by design (same tolerant pattern `mount()`
  already used for a missing STATE drive) - fixed by adding the one
  missing Kconfig symbol, not by working around the gap in Go.
  `hack/qemu-hardening-test.sh` exists specifically to keep re-catching
  this class of bug: it boots for real and checks every expected
  `"init: sysctl ..."` console line individually, *and* separately
  fails on any sysctl write that logged an error at all, expected or
  not - a passing HTTP check and a clean-looking boot log both proved
  insufficient on their own to catch the real `tcp_syncookies` gap.
  SELinux, first slice: kernel-side enablement only, deliberately
  scoped small and separate from writing an actual policy (a much
  larger, more specialized undertaking - type-enforcement rules for a
  from-scratch OS with no existing distro policy to build on - and one
  whose marginal value here is genuinely smaller than usual, given how
  much of what SELinux typically defends against, arbitrary shell/code
  execution and package tampering, this project's architecture already
  eliminates structurally: no shell, no package manager, dm-verity-
  verified read-only root). `CONFIG_SECURITY_SELINUX` turned out to
  `depend on SECURITY_NETWORK && AUDIT && NET && INET` in this kernel
  version - `CONFIG_AUDIT` wasn't just missing, it was explicitly
  unset, found by grepping the real dependency line rather than
  assumed - and pulling it in brought `AUDITSYSCALL`/`FSNOTIFY` along
  automatically via their own `select`s, same as `NETWORK_SECMARK`
  came along via SELinux's own. Verified via the same full-resolved-
  config-diff discipline as every prior Kconfig change: no other LSM
  (smack/apparmor/tomoyo) and no XFRM/IPSec surface got pulled in
  alongside it. Both filesystems that will ever hold a labeled file
  got their xattr support turned on too - `SQUASHFS_XATTR` for the
  read-only rootfs (`mksquashfs` already preserves source-file xattrs
  by default, so this is the only kernel-side piece that read-only
  half needs) and `EXT4_FS_SECURITY` for the writable STATE partition
  (distinct from the still-off `EXT4_FS_POSIX_ACL`) - though nothing
  writes a `security.selinux` xattr onto either filesystem yet, since
  there's no policy to derive one from. `SECURITY_SELINUX_DEVELOP`
  keeps the kernel in permissive mode (log, don't deny) by default,
  exactly right for a slice with no policy to enforce;
  `SECURITY_SELINUX_BOOTPARAM` adds a `selinux=0` cmdline escape hatch
  for the plain `-kernel`/`-append` boot tests (not for a real UKI
  boot - the cmdline there is baked in permanently at build time, no
  boot menu to edit it from). Proven on a real boot, not assumed from
  the Kconfig alone: the console log shows `LSM: initializing
  lsm=capability,yama,selinux` and `SELinux:  Initializing.`, and
  `make qemu-network-test`/`qemu-hardening-test`/`qemu-verity-boot-
  test`/`qemu-state-persist-test` all still pass unchanged - the new
  LSM, with no policy loaded, is currently a true no-op.

  SELinux, second slice: a real, hand-written minimal policy
  (`selinux/classes.conf` + `selinux/policy.conf`), compiled by
  `checkpolicy` (`selinux/Dockerfile`, `make selinux-policy`) and loaded
  by `rootfs/init/main.go`'s `loadSELinuxPolicy` right after mounting
  sysfs, writing the compiled bytes straight to `/sys/fs/selinux/load` -
  no `libselinux`/`load_policy`/policy-store userspace at all, matching
  the "no shell, no package manager" rule for the target. Not adapted
  from refpolicy - refpolicy assumes a systemd/udev-shaped, package-
  managed distro this project structurally isn't, and stripping it down
  would cost more than writing three domains from scratch. The
  bootstrap tool is `scripts/selinux/mdp/mdp`, a small program shipped
  *in the kernel source tree itself*, purpose-built for exactly this
  situation (a system with no existing distro policy to inherit
  class/permission definitions from) - `selinux/classes.conf` is its
  generated output (plain TE, no MLS - `mdp`'s `-m` flag enables MLS,
  which this project doesn't need: a single sensitivity level with real
  type-enforcement rules is enough, and skips ~2000 lines of per-class
  `mlsconstrain` boilerplate MLS would otherwise require).

  Three real domains for the three real processes this rootfs ever
  runs: `init_t` (PID 1, broadly privileged by necessity - mounts every
  filesystem, loads this very policy, is the last-resort revert path),
  `haproxyosd_t` (also root, moderately broad), and `haproxy_t` - the
  one domain confinement effort actually went into, since it's the only
  one that ever parses untrusted, internet-facing input: no
  `self:capability` wildcard, only the specific capabilities its own
  chroot/privilege-drop sequence needs. Labeling is xattr-based in
  principle (`fs_use_xattr` for both real filesystems, matching what
  `mdp` itself generates as correct for xattr-capable filesystems - both
  now carry `CONFIG_SQUASHFS_XATTR`/`CONFIG_EXT4_FS_SECURITY` from the
  first SELinux slice) but only the three real executables ever get an
  explicit xattr - `fs_use_xattr`'s own fallback (an inode with no
  `security.selinux` xattr gets the statement's own default context)
  means everything else on either filesystem just falls through to
  `squashfs_t`/`state_t`, no whole-rootfs labeling pass needed. The ESP
  (FAT, can't carry xattrs at all) is `genfscon`-labeled wholesale
  instead.

  Two real, non-obvious build-tooling gaps, both found by a real build
  failing, not by reading documentation: (1) Debian trixie's
  `checkpolicy` (3.8.1) links a `libsepol` that doesn't recognize five
  `policycap` names this kernel's own `mdp` output includes
  (`genfs_seclabel_symlinks`, `ioctl_skip_cloexec`, `netif_wildcard`,
  `genfs_seclabel_wildcard`, `functionfs_seclabel`) - real kernel/
  toolchain version skew, fixed by dropping those five (all optional
  kernel-side conveniences, never required for enforcement) rather than
  chasing a newer `checkpolicy`; (2) labeling the three executables by
  `setfattr`-ing `$WORKDIR` before `mksquashfs` runs - the obvious first
  approach - silently produced an *unlabeled* image every time: a real
  `setfattr` on the security namespace reports success and even reads
  back correctly with a direct `getfattr`, but `mksquashfs` itself,
  run as build user or root, never picks the xattr up into the built
  image at all (confirmed with an isolated single-file reproduction).
  Fixed by using `mksquashfs`'s own pseudo-file `x` action
  (`-p "path x security.selinux=<context>"`) instead - the same
  mechanism `/var/empty`'s mode-0000 entry already used to sidestep an
  analogous "can't read this back off a real inode" gap - which sets
  the attribute directly while writing the image rather than reading it
  off a source-tree inode first.

  The actual allow-rule set was never going to be right by construction
  - it was arrived at the same way every other piece of this project
  has been, by booting for real (permissive mode - the kernel's own
  `SECURITY_SELINUX_DEVELOP=y` default) and reading real `avc: denied`
  lines out of dmesg, fixing exactly what each one named, and
  reboot-and-recheck, five rounds deep before reaching a genuinely
  clean, zero-denial boot with real HTTP 200 traffic flowing. None of
  what turned up is guessable from reading a rule set in the abstract:
  `self:fd use` (a domain needs explicit permission to use its *own*
  already-open file descriptors, including ones inherited across an
  exec transition from a *different* domain -
  `allow haproxy_t init_t:fd use;`, since haproxy's console fd was
  opened two domain-hops earlier by `init_t` and never reopened);
  `filesystem:associate` on every single `fs_use`-labeled type against
  *itself* (not implicit just because source and target match - the
  very first `tmpfs` mount failed on exactly this); the process-
  transition permissions `noatsecure`/`rlimitinh`/`siginh`, needed on
  every `init_t -> haproxyosd_t -> haproxy_t` hop; and, once real
  network traffic started flowing, `peer`/`packet`/`netif`/`node` class
  rules for `policycap always_check_network` - inbound packets with no
  netlabel/secmark policy configured (this project has none) get the
  "unlabeled" initial SID's context on *every single packet*, checked
  against the receiving netif/node's own type, not just once at
  bind/connect time. `hack/qemu-selinux-test.sh` (`make
  qemu-selinux-test`) is what keeps re-proving this: two boots of the
  identical image, the shipped permissive default and a real
  `enforcing=1` override, both must show the policy loading, real HTTP
  200, and grep clean for `avc:.*denied` - a permissive-mode denial
  doesn't block anything, so only the second boot actually proves the
  rule set complete rather than merely quiet. Both did, on the first
  `enforcing=1` boot tried, once the permissive rounds above reached
  zero denials.

  SELinux enforcing by default, third slice: `image/uki/assemble.sh`'s
  baked-in cmdline now carries `enforcing=1` permanently - the actual
  production default, since there's no boot menu to add it from later
  and every real node boots via this exact UKI. `kernel/configs/
  haproxyos_defconfig`'s `SECURITY_SELINUX_DEVELOP=y` deliberately
  stays on regardless (keeps `/sys/fs/selinux/enforce` toggleable for
  debugging, and the kernel's own compiled-in default without this
  cmdline override would still be the safer permissive one - this UKI
  cmdline change is what actually makes enforcing real, not a kernel
  rebuild). Flipping this surfaced a real gap the earlier, narrower
  `hack/qemu-selinux-test.sh` boot (squashfs+verity only, no STATE
  drive attached at all) had never exercised: every one of this
  project's *other* real boot tests goes through a real, single-disk,
  genuinely-partitioned STATE ext4 mount instead, and that mount failed
  outright under real `enforcing=1` the first time it was tried
  end-to-end (`hack/qemu-lifecycle-rollback-test.sh`, caught before any
  of the others were even retried). The real cause was a wrong
  assumption about `fs_use_xattr`'s own fallback semantics: an inode
  with no `security.selinux` xattr set (every inode on
  `rootfs/state-image.sh`'s freshly-`mkfs.ext4`'d image, since nothing
  ever wrote one) does *not* fall back to the `fs_use_xattr` statement's
  own default context - it falls back to the "file" initial SID
  (`SECINITSID_FILE`, mapped to `squashfs_t` in this policy) instead,
  which only ever looked correct for the read-only rootfs because that
  mapping happened to already *be* `squashfs_t` there. The very first
  `os.MkdirAll("/etc/.state/pki")` on a freshly-mounted STATE partition
  was denied as a write to `squashfs_t`, not `state_t` -
  `"avc: denied { write } ... tcontext=...squashfs_t ... permissive=0"`.
  Fixed properly, not worked around: `rootfs/init/main.go`'s
  `mountState` now mounts STATE with the SELinux `context=` mount
  option (`mountData`, a new small variant of the shared `mount()`
  helper that also passes a mount-options string), forcing every file
  on that mount to a single fixed `state_t` context outright - the same
  role `genfscon` already plays for the ESP's non-xattr-capable FAT,
  used here instead of relying on `fs_use_xattr`'s per-inode fallback
  at all. That itself needed one more real permission
  (`filesystem:{relabelfrom,relabelto}` on `state_t` for `init_t` - the
  `context=` option is itself a relabel operation, caught by a second
  real denial once the first was fixed). Every UEFI-boot-dependent test
  in this project - `qemu-uefi-boot-test`, `qemu-uefi-ab-boot-test`,
  `qemu-lifecycle-rollback-test`, `qemu-lifecycle-upgrade-test`,
  `qemu-lifecycle-upgrade-health-test`, `lifecycle-install-test`,
  `qemu-secureboot-test` - was re-run locally after the fix and passes
  clean under real `enforcing=1`, not just the original
  `hack/qemu-selinux-test.sh` boot.
- **First alpha artifact** (before Phase 5): `image/kvm-proxmox/
  assemble.sh` converts `image/disk/assemble.sh`'s real, single-disk
  GPT image to qcow2 (`qemu-img convert -c`) - Proxmox's own preferred
  import/storage format. `image-build.yml`'s "Build an alpha Proxmox VM
  image" step builds one on every manual run and uploads it as a
  workflow artifact. Unsigned (Proxmox's OVMF has no cert of this
  project's own enrolled by default, so Secure Boot has to stay off in
  the VM's EFI disk settings regardless of whether the UKI itself is
  signed). Verified against the actual built qcow2, not just the raw
  image before conversion - real OVMF boot under `-machine q35`
  (Proxmox's own machine type), zero AVC denials under the now-default
  `enforcing=1`, real HTTP 200. See `image/kvm-proxmox/README.md` for
  the exact `qm create`/`qm importdisk` steps - needs `--serial0
  socket --vga serial0` specifically, since this rootfs has no VGA
  console at all, only serial.
- **Phase 5**: `NetworkService` - bird (BGP), keepalived (VRRP), nftables.
- **Phase 6**: companion website + dedicated Proxmox-hosted backend
  (separate container from the runner) + remote kernel-menuconfig UI -
  separate repository, separate plan.
