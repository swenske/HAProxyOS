<div align="center">
  <img src="https://raw.githubusercontent.com/swenske/Janus/main/brand/png/janus-logo-mono-h256.png" alt="Janus" height="80" />
</div>

# Janus Controller

A web UI for managing one or more [Janus](https://github.com/swenske/Janus)
nodes: register a node by name/address (or let it self-register), view
its stats (RAM/CPU/disk, active boot slot, kernel/HAProxy version), and
drive its config (apply a new HAProxy config, manage maps/ACLs/
certificates, drain/ready/maint individual backend servers).

Your browser authenticates to *this dashboard* per node using a TLS
client certificate issued by that node's own PKI (never uploaded -
selected from what your browser already has installed), while the
dashboard itself talks to the real node using a separate service
credential it generates for itself once, when you add the node - your
own credential is never written to disk.

See the [full README](https://github.com/swenske/Janus/blob/main/dashboard/README.md)
for the complete architecture, self-registration flow, and node-side
setup.

## How to run

**The container must run with `--network host`.** It listens on three
ports, one of which (the per-node listener pool) is a dynamic range -
host networking makes every one of them directly reachable at the
host's own address, with no port mapping and no extra configuration.

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

`-v .../data` is required, not optional: it's where the node registry
and the dashboard's own TLS identity persist across restarts.

Then open **`https://<host>:8080/`** (HTTPS only - a self-signed
certificate is generated on first run, so your browser will ask you to
click through a trust warning once, the same as it would for any
self-hosted admin tool) and set the admin password on first visit.

To use your own certificate instead of the auto-generated one, mount it
in and pass `-tls-cert`/`-tls-key`:

```sh
docker run -d \
  --name janus-controller \
  --network host \
  -v janus-controller-data:/data \
  -v /path/to/certs:/certs:ro \
  swenske/janus-controller -tls-cert /certs/fullchain.pem -tls-key /certs/privkey.pem
```

## Tags

- `latest` - the most recent build from `main`.
- `<git-sha>` - a specific commit, for pinning.
