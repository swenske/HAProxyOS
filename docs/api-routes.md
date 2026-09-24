# HAProxyOS — gRPC API catalog

Full contract lives in `api/proto/haproxyos/v1alpha1/*.proto` (generated
Go code in `gen/haproxyos/v1alpha1`, implementations in `internal/api`).
This is the human-readable index, adapted from Talos's own
`MachineService`/`LifecycleService` (verified directly against
`siderolabs/talos`'s `.proto` files on GitHub - `api/machine/machine.proto`,
`storage.proto`, `lifecycle.proto`) with everything Kubernetes/etcd-
specific dropped, and `HAProxyService`/`NetworkService` added as
HAProxyOS's own differentiating surface.

Status column: ✅ implemented · ⬜ contract defined, returns
`codes.Unimplemented` (see `internal/api`).

**mTLS is mandatory on every connection** (`internal/pki`, wired up in
`cmd/haproxyosd`) - there is no plaintext or unauthenticated mode. A
node generates its own CA + server certificate + an initial admin client
certificate on first boot; `GenerateClientConfiguration` issues
additional client certificates once you already have one.

**Every RPC is also role-checked** against the caller's certificate
(`internal/api/authz.go`, fail-closed - an RPC with no explicit entry
defaults to admin-only). Two roles: `os:admin` (everything) and
`os:reader` (the ✅ methods marked "read-only" in the tables below, plus
`Version`/`Hostname`/`Events`/`GetConfig`/`ValidateConfig`/`*Status`/
`*List`/`*Get` - status and observability RPCs only, not file/log/
packet-capture access or credential issuance, even though some of those
are also technically non-mutating).

## SystemService

| Method | Streaming | Status | Purpose |
|---|---|---|---|
| `Version` | | ✅ | Daemon version - connectivity check |
| `Hostname` | | ⬜ | |
| `Reboot` | | ⬜ | Power-cycle the machine |
| `Shutdown` | | ⬜ | |
| `Restart` | | ⬜ | Restart `haproxyosd` in place (not the machine) |
| `Reset` | | ⬜ | Wipe STATE/EPHEMERAL and reboot |
| `ApplyConfiguration` | server | ⬜ | Apply declarative config (`internal/config`), auto/no-reboot/reboot/try modes |
| `Events` | server | ⬜ | Internal event log |
| `Dmesg` | server | ⬜ | Kernel ring buffer |
| `Logs` | server | ⬜ | Managed-service logs |
| `Stats` | | ⬜ | Per-process CPU/memory |
| `SystemStat` | | ⬜ | Boot time, context switches |
| `Memory` | | ⬜ | |
| `CPUInfo` | | ⬜ | |
| `LoadAvg` | | ⬜ | |
| `DiskStats` | | ⬜ | |
| `DiskUsage` | server | ⬜ | |
| `NetworkDeviceStats` | | ⬜ | |
| `Netstat` | | ⬜ | |
| `Mounts` | | ⬜ | |
| `Processes` | | ⬜ | |
| `ServiceList` | | ⬜ | Managed services: `haproxy`, `bird`, `keepalived`, `haproxyosd` |
| `ServiceStart` / `Stop` / `Restart` | | ⬜ | |
| `List` | server | ⬜ | Scoped, read-only file listing - no shell |
| `Read` | server | ⬜ | Scoped, read-only file content |
| `Copy` | server | ⬜ | Tar stream of a path |
| `PacketCapture` | server | ⬜ | tcpdump-equivalent over gRPC |
| `MetaWrite` / `MetaDelete` | | ⬜ | META partition key/value entries |
| `GenerateClientConfiguration` | | ✅ | Issue an mTLS client cert (`internal/pki`) - 1 year validity, no rotation flow yet |

## LifecycleService

| Method | Streaming | Status | Purpose |
|---|---|---|---|
| `Install` | server | ⬜ | First install to a target disk |
| `Upgrade` | server | ✅ | Write a release bundle's rootfs to the inactive A/B slot, switch + reboot - `wait_for_health` auto-reverts if the new slot's HAProxy (real stats-socket check) never comes up healthy in time, or if haproxyosd itself never stays running long enough to check |
| `Rollback` | | ✅ | Switch back to the other A/B slot, reboot |

## HAProxyService

| Method | Streaming | Status | Purpose |
|---|---|---|---|
| `GetConfig` | | ✅ | Current `haproxy.cfg` |
| `ApplyConfig` | server | ✅ | Validate (`haproxy -c`) then seamless-reload (`-sf <pid>`) |
| `ValidateConfig` | | ✅ | Dry-run validation only |
| `Reload` | | ✅ | Seamless reload of the current config |
| `Stats` | | ✅ | Proxy of the HAProxy stats socket `show stat` (raw CSV) |
| `ShowInfo` | | ✅ | Proxy of `show info` (version/uptime/connections) |
| `BackendList` | | ⬜ | |
| `ServerSetState` | | ✅ | Runtime enable/drain/maint a backend server |
| `MapList` / `MapGet` / `MapUpdate` | | ✅ | Runtime maps - file-backed only (`map(<path>)` in the running config); upsert is delete-then-add since `set map` doesn't create missing keys |
| `ACLUpdate` | | ✅ | Runtime ACL pattern values - file-backed only (`acl ... -f <path>`), same delete-then-add upsert reasoning |
| `CertificateList` / `Upload` / `Delete` | | ✅ | HAProxy's cert store (`new`/`set`/`commit`/`del ssl cert`), plus optional binding into a `crt-list` already referenced by a `bind ... ssl crt-list <path>` in the running config (`add`/`del ssl crt-list`, with SNI filters) - `CertificateList` reports each cert's `Used`/`Unused` status |

## NetworkService (optional modules)

| Method | Streaming | Status | Purpose |
|---|---|---|---|
| `BGPStatus` / `BGPApplyConfig` | | ⬜ | bird - reports `MODULE_STATE_NOT_ENABLED` if bird isn't in this node's image |
| `VRRPStatus` / `VRRPApplyConfig` | | ⬜ | keepalived, same not-enabled convention |
| `FirewallList` / `FirewallApplyRuleset` | | ⬜ | nftables, same not-enabled convention |

## Deliberately not present

- `EtcdService` - HAProxyOS nodes don't form an etcd cluster.
- `ImageService`/container runtime RPCs - no container runtime on the
  target OS; HAProxy and the optional daemons are native processes
  supervised by the custom PID 1 (`rootfs/init`, Phase 1).
