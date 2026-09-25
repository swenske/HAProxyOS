# dashboard/frontend

React + Vite SPA served by `dashboardd` (`dashboard/backend`) on its
main port: the node list and the add-node form. Not the per-node
dashboard view - that lives on each node's own dedicated, mTLS-gated
port and is a separate, deliberately plain HTML/JS page embedded in
`dashboard/backend/internal/nodeproxy` (no build step there - see that
package's own doc comment for why the two views can't just be the same
SPA).

## Build

```sh
npm ci
npm run build
```

`vite.config.js` builds straight into `../backend/static` -
`dashboard/backend/main.go`'s `go:embed` needs the built assets there
at `go build` time. The build output is committed to the repo (like
`gen/janus/v1alpha1`) so a plain `go build ./...` never needs a
Node.js toolchain just to compile - regenerate it (via `make
dashboard-frontend-build`, from the repo root) whenever frontend source
changes.
