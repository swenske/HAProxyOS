# image/kvm-proxmox

`assemble.sh` converts `image/disk/assemble.sh`'s real, single-disk GPT
image (ESP + both A/B slots + STATE) to qcow2 via `qemu-img convert -c`
- Proxmox's own preferred import/storage format. `make proxmox-image`
builds one end to end; `image-build.yml`'s "Build an alpha Proxmox VM
image (qcow2)" step does the same on `haproxyos-runner01` and uploads
it as a workflow artifact.

## Importing into Proxmox

The image is unsigned (no Secure Boot cert of this project's is
enrolled in Proxmox's own OVMF by default) and has no VGA/framebuffer
console at all - only a serial one (`console=ttyS0` baked into the UKI
cmdline). Both matter for how the VM is created:

```sh
qm create <vmid> --name haproxyos-alpha --memory 512 --cores 1 \
  --machine q35 --bios ovmf --efidisk0 <storage>:0,pre-enrolled-keys=0 \
  --net0 virtio,bridge=<bridge> \
  --serial0 socket --vga serial0

qm importdisk <vmid> haproxyos.qcow2 <storage>
qm set <vmid> --scsihw virtio-scsi-pci --virtio0 <storage>:vm-<vmid>-disk-1
qm set <vmid> --boot order=virtio0

qm start <vmid>
qm terminal <vmid>   # serial console - the GUI's noVNC console shows nothing
```

`pre-enrolled-keys=0` on the EFI disk is what leaves Secure Boot off
(Proxmox's own default OVMF vars otherwise enroll Microsoft's keys,
which don't match this project's own signing key anyway). `--serial0
socket --vga serial0` is what makes `qm terminal` show the actual
console - without it, the GUI's noVNC console stays blank (this rootfs
has no VGA console driver compiled in at all, by design, matching the
"ultra-light" boot-proof baseline).

First boot bootstraps a CA and prints the admin gRPC client cert/key to
that console **once** - see `cmd/haproxyosd/main.go` - copy it out
immediately, there's no shell to retrieve it later. From there,
`haproxyosctl pki generate-client-config` (or the printed cert/key
directly) drives everything else - see the root `CLAUDE.md`/`docs/
api-routes.md` for the full gRPC surface.
