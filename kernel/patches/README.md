# Kernel patches

Empty for now (Phase 0). When a patch against the pinned kernel version
(`versions.mk`) is needed, add it here as a `git format-patch`-style file
named `NNNN-short-description.patch`, applied in numeric order by
`kernel/Dockerfile`'s future build stage (Phase 1).
