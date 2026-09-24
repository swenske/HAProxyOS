# HAProxyOS

An ultra-light, immutable, API-driven Linux distribution built from scratch
(LFS-style), inspired by [Talos Linux](https://github.com/siderolabs/talos),
centered on [HAProxy](https://www.haproxy.org/) as the primary
reverse-proxy/load-balancer. No SSH, no interactive shell, no package
manager on the running system - everything is driven through a gRPC API
secured with mTLS.

Optional network features, opt-in per node: BGP via
[bird](https://bird.network.cz/), VRRP via
[keepalived](https://www.keepalived.org/), firewalling via nftables.

**Status: early scaffolding.** There is no bootable image yet. See
[`docs/architecture.md`](docs/architecture.md) for the design and roadmap,
and [`docs/api-routes.md`](docs/api-routes.md) for the gRPC API catalog.

## Repository layout

- `api/proto/haproxyos/v1alpha1/` - the gRPC contract (source of truth).
- `cmd/haproxyosd/`, `cmd/haproxyosctl/` - control-plane daemon and CLI.
- `internal/api/` - gRPC service implementations.
- `kernel/`, `pkgs/`, `rootfs/`, `image/` - the from-scratch OS build
  system (Dockerfile-per-component, Talos-`pkgs`-style).
- `dashboard/` - the management dashboard (web UI for one or more
  nodes). See [`dashboard/README.md`](dashboard/README.md) to build and
  run it locally.
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
