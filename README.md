<p align="center">
  <img src="brand/logo/janus-logo-mono-fond-sombre.svg" alt="Janus" />
</p>

# Janus

An ultra-light, immutable, API-driven Linux distribution built from scratch
(LFS-style), inspired by [Talos Linux](https://github.com/siderolabs/talos),
centered on [HAProxy](https://www.haproxy.org/) as the primary
reverse-proxy/load-balancer. No SSH, no interactive shell, no package
manager on the running system - everything is driven through a gRPC API
secured with mTLS. **Janus Controller** (see [`dashboard/`](dashboard/)) is
the companion management dashboard for running one or more nodes.

Optional network features, opt-in per node: BGP via
[bird](https://bird.network.cz/), VRRP via
[keepalived](https://www.keepalived.org/), firewalling via nftables.

> **Janus** is the Roman god of beginnings and endings, of choices, of
> passage, and of doors - traditionally shown with two faces looking in
> opposite directions at once. This project is named for that duality
> (frontend/backend, ingress/egress, the two faces of a reverse proxy),
> not for any connection to the HAProxy project. **Janus has no
> affiliation with, and is not supported, endorsed, or reviewed by,
> HAProxy Technologies or the maintainers of HAProxy** - it is an
> independent project that happens to run HAProxy as its data plane, the
> same way it could run any other proxy. "HAProxy" above and throughout
> this repository refers strictly to the upstream software.

**Status: early alpha.** Real bootable images exist (kernel hardening,
dm-verity, A/B updates, Secure Boot, SELinux enforcing by default) and an
unsigned alpha qcow2 is built on every image workflow run - see
[`docs/architecture.md`](docs/architecture.md) for the design and roadmap,
and [`docs/api-routes.md`](docs/api-routes.md) for the gRPC API catalog.
Nothing here is published or versioned for general use yet.

## Repository layout

- `api/proto/janus/v1alpha1/` - the gRPC contract (source of truth).
- `cmd/janusd/`, `cmd/janusctl/` - control-plane daemon and CLI.
- `internal/api/` - gRPC service implementations.
- `kernel/`, `pkgs/`, `rootfs/`, `image/` - the from-scratch OS build
  system (Dockerfile-per-component, Talos-`pkgs`-style).
- `dashboard/` - **Janus Controller**, the management dashboard (web UI
  for one or more nodes). See [`dashboard/README.md`](dashboard/README.md)
  to build and run it locally.
- `hack/` - local dev tooling (QEMU test harness).

## Building the control plane

```sh
make build   # binaries in ./bin
make test
make lint
make proto   # regenerate gen/ from api/proto/**.proto (requires buf)
```

## License

[MIT](LICENSE)
