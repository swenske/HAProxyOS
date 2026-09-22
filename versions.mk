# Single source of truth for every pinned upstream version used by the
# build system (kernel/, pkgs/*). Bumping a version here is the entire
# "montée de version" workflow for Phase 0/1: edit the number (+ sha256),
# open a PR, let ci.yml build the control plane and image-build.yml (manual)
# build+boot-test the image before merging.
#
# Versions below were checked live against upstream on 2026-09-22:
#   - kernel: https://www.kernel.org/releases.json, latest "longterm" branch
#   - haproxy: https://www.haproxy.org/, latest stable branch
# bird/keepalived are optional (Phase 5, NetworkService) and not yet
# pinned - confirm actual target versions before that phase starts.

KERNEL_VERSION  := 6.18.53
HAPROXY_VERSION := 3.4.0

# Optional network features (Phase 5) - versions TBD.
BIRD_VERSION       :=
KEEPALIVED_VERSION :=
