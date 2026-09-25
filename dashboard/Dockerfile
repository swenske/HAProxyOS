# syntax=docker/dockerfile:1
#
# Builds dashboardd (dashboard/backend) into a minimal runtime image -
# same multi-stage/FROM-scratch-export pattern as ../kernel/Dockerfile
# and ../selinux/Dockerfile, except this image actually runs (a service,
# not a build artifact to extract). The frontend SPA (dashboard/frontend)
# is *not* built here - its output is already committed to dashboard/
# backend/static (see that tree's own README for why, the same reasoning
# gen/janus/v1alpha1 is committed), so a plain `go build` here embeds
# exactly what a real deploy expects without needing a Node.js toolchain
# in this image's own build stage.
#
# No system CA bundle needed in the final image: dashboardd never
# dials anything over public-CA-verified TLS - every gRPC call it makes
# goes to a registered node's own gRPC server, verified against that
# node's own CA cert stored in the registry (see dashboard/backend/
# internal/nodeproxy's package doc comment), never the system trust
# store. CGO_ENABLED=0 keeps the binary static so a bare FROM scratch
# final stage can actually run it - confirmed locally (`docker build`
# + `docker run` against a real node) before this file was trusted.
FROM golang:1.26-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY gen/ gen/
COPY internal/ internal/
COPY dashboard/backend/ dashboard/backend/
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/dashboardd ./dashboard/backend

FROM scratch AS export
COPY --from=build /out/dashboardd /dashboardd
# /data is the persistent volume (node registry + this dashboard's own
# TLS identity - see dashboard/backend/main.go's own -data-dir default)
# - declared here so a bare `docker run` without an explicit -v still
# gets an anonymous volume rather than writing into the container's own
# throwaway layer.
VOLUME /data
# :8080 - the main port (node list/add/remove, behind admin auth - see
# internal/auth). :8443 - node self-registration (see internal/pending
# and register.go), a fixed port, not a pool, since it's one endpoint
# shared by every registering node, not per-node like what follows.
# 9500-9599 - the per-node mTLS-gated listener pool (see dashboard/
# backend/main.go's portRangeStart/End) - Docker's static -p mapping
# doesn't cover a dynamic pool well, so a real run needs either
# --network host or a pre-mapped "-p 9500-9599:9500-9599" - see
# dashboard/README.md for the full run command.
EXPOSE 8080 8443 9500-9599
ENTRYPOINT ["/dashboardd"]
