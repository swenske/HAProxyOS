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

**Status:** published to Docker Hub as
[`swenske/janus-controller`](https://hub.docker.com/r/swenske/janus-controller)
(`:latest` and per-commit `:<sha>` tags) - `.github/workflows/
image-build.yml`'s own manually-dispatched, self-hosted "Push the
dashboard image to Docker Hub" step builds and pushes it, gated behind
a `DOCKERHUB_TOKEN` repository secret so a fork PR (or a run before
that secret exists) just skips the push, not the build. Building and
running locally, as below, still works exactly the same either way.

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

**The container must run with `--network host`.** It listens on three
distinct ports, one of which (the per-node listener pool, below) is a
*dynamic range*, not a single fixed port - browser TLS client-
certificate selection is negotiated per *origin* (host:port), so
managing more than one node needs a distinct origin per node. Docker's
static `-p host:container` mapping can't represent that cleanly (mapping
a 100-port range works but is awkward, and still leaves this process
unable to see its own real, externally-reachable address on its network
interfaces - see `-advertise-address` further down for that specific
symptom). `--network host` sidesteps all of it at once: every port
`dashboardd` binds is immediately reachable at the host's own address,
with no mapping and no address-detection gap.

- **`:8080`** (configurable via `-addr`) - the main UI, **HTTPS only**.
  Behind a single admin password, forced setup on first visit (see
  `internal/auth`).
- **`:8443`** (configurable via `-register-addr`) - where a node
  self-registers (see `internal/pending`); self-announced nodes land in
  a "pending" queue, approved or rejected by hand in the UI, not
  admitted automatically.
- **`9500-9599`** - a *pool* of per-node HTTPS listeners, one per
  registered node, each requiring a TLS client certificate issued by
  that node's own CA.

```sh
docker run -d \
  --name janus-controller \
  --network host \
  -v janus-controller-data:/data \
  swenske/janus-controller
```

Or with Compose:

```yaml
services:
  janus-controller:
    image: swenske/janus-controller
    container_name: janus-controller
    network_mode: host
    restart: unless-stopped
    volumes:
      - janus-controller-data:/data

volumes:
  janus-controller-data:
```

(Built and running locally instead of pulled from Docker Hub? Swap the
image for the locally-built `janus-controller` tag - see Build above.)

`-v .../data` is a real requirement, not optional: it's where the node
registry and the dashboard's own TLS identity persist across restarts
(`-data-dir`, default `/data`) - without it, every restart forgets
every registered node and re-issues a new dashboard identity, which
also invalidates any per-node listener certificate your browser
already trusted.

### TLS identity

Every port above shares one TLS server certificate
(`loadOrCreateDashboardIdentity`) - by default a self-signed one,
generated on first run and persisted to `-data-dir`, so your browser's
one-time trust click-through (same as any per-node view already needs)
survives restarts. Its SAN list covers `localhost`/`127.0.0.1`/`::1`
plus every real address this process can see on its own network
interfaces - with `--network host`, that's the host's actual LAN
address(es) directly, no extra configuration needed. Pass
`-advertise-address YOUR.IP.OR.HOSTNAME` (comma-separated for more than
one) only if you're *not* using `--network host` and this process can't
otherwise see the address a node or browser will actually reach it
through (Docker bridge networking's own internal IP is a real example -
a self-registering node's plain `net/http` client, unlike a browser,
can't click through a hostname/SAN mismatch, so it would refuse the
handshake outright without this). Only read the first time the identity
is generated - delete `<data-dir>/dashboard-identity.{crt,key}` and
restart to regenerate it after changing this.

To use your own certificate instead (a Let's Encrypt one, or one from
an internal CA) - modifiable at any time, unlike the auto-generated
one - mount it in and point `-tls-cert`/`-tls-key` at it:

```sh
docker run -d \
  --name janus-controller \
  --network host \
  -v janus-controller-data:/data \
  -v /path/to/certs:/certs:ro \
  swenske/janus-controller -tls-cert /certs/fullchain.pem -tls-key /certs/privkey.pem
```

Both flags must be set together; when set, `-advertise-address` and the
auto-generated identity are skipped entirely - that certificate's own
SAN list is then your responsibility.

### Adding a node

Open `https://<host>:8080/` (note **https** - the click-through warning
on first visit is expected with the default self-signed certificate)
and add a node: you'll need its display name, its gRPC address
(`ip:9505` by default), its `ca.crt` (public, not sensitive), and a
client credential that already has `os:admin` on that node - either the
one `janusd` printed to its console on first boot, or one you generated
yourself via `janusctl pki generate-client-config`. That credential is
used exactly once, to call `GenerateClientConfiguration` and obtain a
fresh service credential for the dashboard's own use - it is never
written to disk itself (see the architecture note above).

A node can also register itself with this Controller automatically at
first boot instead, if it was provisioned with `janusctl lifecycle
install`'s `-controller-address`/`-controller-ca` flags - it then shows
up in a "pending" queue for you to approve or reject, rather than being
added by hand. The "Provision a new node" panel in the UI has the exact
address/CA certificate/command to use for this, pre-filled from this
Controller's own configuration (double-check the suggested address
against your real network before using it - this process can't always
tell what address a node will actually be able to reach it at, most
notably under Docker bridge networking, see the `-advertise-address`
note above).

## Build (backend only, no image)

For local development without Docker:

```sh
make dashboard-build   # -> bin/dashboardd
./bin/dashboardd -addr :8080 -data-dir ./dashboard-data
```

Open `https://localhost:8080/` (not `http://`) - a self-signed certificate is generated into `./dashboard-data` on first run.

## Verification

`hack/qemu-dashboard-test.sh` (`make qemu-dashboard-test`) drives the
whole add/list/relay/mTLS-gate/delete/restart-persistence flow, plus
the ops/config relay (`ShowInfo`/`GetConfig`/`ApplyConfig`/`BackendList`/
maps/ACLs/certificates), against a real booted node - not a mock. The
Docker image itself is verified by building it and running a real
container against it (`docker build` + `docker run` + a real `curl`),
same discipline as everything else in this project.
