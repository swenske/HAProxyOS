package main

import "testing"

func TestDmVerityDataDevice(t *testing.T) {
	cases := []struct {
		name    string
		cmdline string
		want    string
		wantOK  bool
	}{
		{
			name: "single-disk GPT+ESP layout (image/disk/assemble.sh, hack/qemu-ab-boot-test.sh slot A)",
			// The exact string a real boot produced (see rootfs/init's
			// own development notes) - not a hand-simplified stand-in.
			cmdline: `console=ttyS0 panic=-1 dm-mod.create="vroot,,,ro,0 15608 verity 1 /dev/vda2 /dev/vda3 4096 4096 1951 1 sha256 58b57af748b7f4ff334c9f9612c2c62a7dd28d3753c70ed03cd409fa7b7d30e4 eee8298dddb05a935d91fc88bdca004d1e3baec7dccd0a03da9a552d4142212f" root=/dev/dm-0 rootfstype=squashfs ro`,
			want:    "/dev/vda2",
			wantOK:  true,
		},
		{
			name:    "single-disk GPT+ESP layout, slot B",
			cmdline: `console=ttyS0 dm-mod.create="vroot,,,ro,0 15608 verity 1 /dev/vda4 /dev/vda5 4096 4096 1951 1 sha256 abc def" root=/dev/dm-0`,
			want:    "/dev/vda4",
			wantOK:  true,
		},
		{
			name:    "separate-virtio-blk-drives harness (hack/qemu-verity-boot-test.sh) - whole-disk device",
			cmdline: `console=ttyS0 panic=-1 dm-mod.create="vroot,,,ro,0 15600 verity 1 /dev/vda /dev/vdb 4096 4096 1950 1 sha256 hash salt" root=/dev/dm-0 rootfstype=squashfs ro ip=dhcp`,
			want:    "/dev/vda",
			wantOK:  true,
		},
		{
			name:    "no dm-mod.create= at all (Phase 1/2 initramfs boots)",
			cmdline: `console=ttyS0 panic=-1 ip=dhcp`,
			wantOK:  false,
		},
		{
			name:    "empty cmdline",
			cmdline: "",
			wantOK:  false,
		},
		{
			name:    "unterminated quote",
			cmdline: `dm-mod.create="vroot,,,ro,0 15600 verity 1 /dev/vda /dev/vdb`,
			wantOK:  false,
		},
		{
			name:    "not enough comma-separated fields",
			cmdline: `dm-mod.create="vroot,,,ro"`,
			wantOK:  false,
		},
		{
			name:    "table isn't a verity target",
			cmdline: `dm-mod.create="lroot,,,rw,0 4096 linear 8:16 0"`,
			wantOK:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := dmVerityDataDevice(tc.cmdline)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (got %q)", ok, tc.wantOK, got)
			}
			if ok && got != tc.want {
				t.Fatalf("data device = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStatePartitionDevice(t *testing.T) {
	cases := []struct {
		name    string
		dataDev string
		want    string
		wantOK  bool
	}{
		{name: "slot A data partition", dataDev: "/dev/vda2", want: "/dev/vda6", wantOK: true},
		{name: "slot B data partition", dataDev: "/dev/vda4", want: "/dev/vda6", wantOK: true},
		{name: "double-digit partition number", dataDev: "/dev/vda12", want: "/dev/vda6", wantOK: true},
		{name: "whole-disk device (separate-drives harness)", dataDev: "/dev/vda", wantOK: false},
		{name: "empty", dataDev: "", wantOK: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := statePartitionDevice(tc.dataDev)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (got %q)", ok, tc.wantOK, got)
			}
			if ok && got != tc.want {
				t.Fatalf("state device = %q, want %q", got, tc.want)
			}
		})
	}
}
