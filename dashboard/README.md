# Janus Controller

A web UI for managing one or more Janus nodes: register a node by
name/address, view its stats (RAM/CPU/disk, active boot slot,
kernel/HAProxy version), and drive its config (apply a new HAProxy
config, manage maps/ACLs/certificates, drain/ready/maint individual
backend servers).

See the local `docs/plan` history (rebranding/dashboard/client-native
initiative) for the full architecture and why it's shaped the way it
is - the short version: your browser authenticates to *this dashboard*
per node using a TLS client certificate issued by that node's own PKI
(never uploaded - selected from what your browser already has
installed), while the dashboard itself talks to the real node using a
separate service credential it generates for itself once, when you add
the node. A TLS server can verify a client holds a private key, it can
never extract that key - so your browser's certificate can never be
reused to dial the node directly, only to prove to the dashboard that
you're allowed to look at that node's own view.

**Status:** not yet published anywhere - build and run it locally for
now. Publishing an image (to the maintainer's own Docker Hub) is
deferred until the project's rename decision lands, so an image isn't
built under a name that immediately needs republishing under another
one. This is a local build/run howto in the meantime.

## Build

```sh
make dashboard-image      # from the repo root - builds dashboard/Dockerfile
```

This builds a single, self-contained image (`janus-controller`,
`FROM scratch` - see `Dockerfile`'s own comment for why no system CA
bundle or shell is needed inside it). It does **not** rebuild the
frontend SPA from source - `dashboard/backend/static` is already
committed (same convention `gen/janus/v1alpha1` uses), so this
build needs no Node.js toolchain. If you've changed `dashboard/frontend`
source, run `make dashboard-frontend-build` first and commit the
result before building the image.

## Run

The dashboard needs three things exposed:

- **`:8080`** (configurable via `-addr`) - the main UI. Behind a single
  admin password, forced setup on first visit (see `internal/auth`) -
  nothing else here needs a credential, just registered names/
  addresses.
- **`:8443`** (configurable via `-register-addr`) - where a node
  self-registers (see `internal/pending`); self-announced nodes land in
  a "pending" queue, approved or rejected by hand in the UI, not
  admitted automatically.
- **`9500-9599`** - a *pool* of per-node HTTPS listeners, one per
  registered node, each requiring a TLS client certificate issued by
  that node's own CA. This is a dynamic range, not a single fixed
  port (browser TLS client-certificate selection is negotiated per
  *origin*, so managing more than one node needs a distinct origin -
  port - per node) - Docker's static `-p host:container` mapping
  doesn't represent a range cleanly, so a real run needs either
  `--network host` or a pre-mapped range:

```sh
docker run -d \
  --name janus-controller \
  -p 8080:8080 \
  -p 8443:8443 \
  -p 9500-9599:9500-9599 \
  -v janus-controller-data:/data \
  janus-controller
```

`-v .../data` is a real requirement, not optional: it's where the node
registry and the dashboard's own TLS identity persist across restarts
(`-data-dir`, default `/data`) - without it, every restart forgets
every registered node and re-issues a new dashboard identity, which
also invalidates any per-node listener certificate your browser
already trusted.

The `docker run` example above uses Docker's default bridge networking,
which means this process's own view of its network interfaces (used to
build its TLS identity certificate's SAN list, see
`loadOrCreateDashboardIdentity`) is the container's internal bridge IP,
not whatever address a node or browser actually reaches `-p 8443:8443`/
`-p 9500-9599:9500-9599` through from outside. A browser tolerates this
for the per-node view (it lets you click through the mismatch); a
self-registering node's own HTTP client does not - it will refuse the
handshake outright. If nodes will self-register through a Docker
bridge, NAT, or a port-forwarded address, pass that address explicitly:

```sh
docker run -d \
  --name janus-controller \
  -p 8080:8080 \
  -p 8443:8443 \
  -p 9500-9599:9500-9599 \
  -v janus-controller-data:/data \
  janus-controller -advertise-address YOUR.PUBLIC.IP.HERE
```

Only read the first time the identity is generated (it's cached to
`-data-dir` afterward) - delete `<data-dir>/dashboard-identity.{crt,key}`
and restart to regenerate it after changing this.

Then open `http://<host>:8080/` and add a node: you'll need its
display name, its gRPC address (`ip:9505` by default), its `ca.crt`
(public, not sensitive), and a client credential that already has
`os:admin` on that node - either the one `janusd` printed to its
console on first boot, or one you generated yourself via `janusctl
pki generate-client-config`. That credential is used exactly once, to
call `GenerateClientConfiguration` and obtain a fresh service
credential for the dashboard's own use - it is never written to disk
itself (see the architecture note above).

## Build (backend only, no image)

For local development without Docker:

```sh
make dashboard-build   # -> bin/dashboardd
./bin/dashboardd -addr :8080 -data-dir ./dashboard-data
```

## Verification

`hack/qemu-dashboard-test.sh` (`make qemu-dashboard-test`) drives the
whole add/list/relay/mTLS-gate/delete/restart-persistence flow, plus
the ops/config relay (`ShowInfo`/`GetConfig`/`ApplyConfig`/`BackendList`/
maps/ACLs/certificates), against a real booted node - not a mock. The
Docker image itself is verified by building it and running a real
container against it (`docker build` + `docker run` + a real `curl`),
same discipline as everything else in this project.
