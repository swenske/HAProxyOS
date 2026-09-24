// Command haproxyosd is the HAProxyOS control-plane daemon: it serves the
// SystemService/LifecycleService/HAProxyService/NetworkService gRPC API
// (see api/proto/haproxyos/v1alpha1) that haproxyosctl and any external
// controller use instead of SSH.
//
// mTLS is mandatory on every connection (internal/pki) - there is no
// plaintext or unauthenticated mode - and every RPC is role-checked
// against the calling certificate's roles (internal/api's
// UnaryAuthInterceptor/StreamAuthInterceptor, see internal/api/authz.go
// for the actual os:admin/os:reader split). On first boot (empty
// -pki-dir) a CA, this node's server certificate, and an initial admin
// client certificate are generated and the admin certificate/key are
// printed once, since there's no shell to retrieve them from later (see
// docs/architecture.md's "no shell" design goal) - copy them somewhere
// safe immediately. Phase 3's Install flow will replace this with a
// proper side channel.
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	haproxyosv1alpha1 "github.com/swenske/HAProxyOS/gen/haproxyos/v1alpha1"
	"github.com/swenske/HAProxyOS/internal/api"
	"github.com/swenske/HAProxyOS/internal/bootcommit"
	"github.com/swenske/HAProxyOS/internal/bootrevert"
	"github.com/swenske/HAProxyOS/internal/haproxy"
	"github.com/swenske/HAProxyOS/internal/pki"
)

// version is set via -ldflags "-X main.version=..." by the release build
// (see Makefile), left as "dev" for local builds.
var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print the daemon version and exit")
	addr := flag.String("addr", ":9505", "gRPC listen address")
	pkiDir := flag.String("pki-dir", "/etc/haproxyos/pki", "directory holding the node's CA/server/admin certificates (generated here on first boot)")
	haproxyBin := flag.String("haproxy-binary", "/usr/local/sbin/haproxy", "path to the haproxy binary")
	haproxyCfg := flag.String("haproxy-config", "/etc/haproxy/haproxy.cfg", "path to haproxy's active config file")
	haproxyPid := flag.String("haproxy-pid", "/run/haproxyos/haproxy.pid", "path to haproxy's pid file")
	haproxySock := flag.String("haproxy-stats-socket", "/run/haproxyos/haproxy-admin.sock", "path to haproxy's stats socket (must match the 'stats socket' line in haproxy-config)")
	haproxyChrootDir := flag.String("haproxy-chroot-dir", "/var/empty", "directory haproxy chroots into after binding listeners and dropping privileges (must match the 'chroot' line in haproxy-config); created here since this rootfs has no package manager to have provisioned it")
	flag.Parse()

	if *showVersion {
		fmt.Println("haproxyosd " + version)
		return
	}

	hostname, err := os.Hostname()
	if err != nil {
		hostname = "haproxyos"
	}

	pkiBootstrap, err := pki.LoadOrBootstrap(*pkiDir, hostname, nil)
	if err != nil {
		log.Fatalf("pki: %v", err)
	}
	if pkiBootstrap.AdminIssued {
		log.Printf("pki: first boot - generated a new CA and admin client certificate in %s", *pkiDir)
		// The CA cert isn't secret (it only lets a client verify the
		// server's identity, not authenticate as anyone) - printed
		// alongside the admin cert/key anyway, not just left to a
		// separate STATE-partition extraction, since a console reader
		// bootstrapping a node needs all three to actually connect
		// (haproxyosctl's -ca/-cert/-key) and splitting them across two
		// different recovery paths for one single one-time event was
		// real friction, not a meaningful security boundary - whoever
		// can read this console already has the admin cert/key printed
		// right below, which is the actually sensitive half.
		log.Printf("pki: CA CERTIFICATE (needed for haproxyosctl's -ca flag):\n%s", pkiBootstrap.CA.CertPEM)
		log.Printf("pki: ADMIN CERTIFICATE (save this now, it will not be printed again):\n%s%s", pkiBootstrap.AdminCertPEM, pkiBootstrap.AdminKeyPEM)
		// pkiDir may be the Phase 3 cont'd persistent STATE partition
		// (see rootfs/init/main.go's mountState) - force these bytes to
		// the underlying block device now rather than trusting they're
		// still there if the node loses power before some later,
		// unrelated sync happens to occur.
		syscall.Sync()
	}

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen on %s: %v", *addr, err)
	}

	// haproxy needs its pid-file/stats-socket directory (and this
	// daemon's own runtime dir in general) to exist - the target OS has
	// no package manager / installer to have created it ahead of time,
	// so this is the one place that responsibility can live.
	if err := os.MkdirAll(filepath.Dir(*haproxyPid), 0o755); err != nil {
		log.Fatalf("mkdir %s: %v", filepath.Dir(*haproxyPid), err)
	}

	// The chroot jail haproxy.cfg's `chroot` directive points into.
	// Nothing is ever accessed inside it after the chroot() call (every
	// file haproxy needs - config, maps, ACLs, certs - is opened before
	// it drops privileges), so it's created empty and inaccessible on
	// purpose: mode 0000, not even readable by its own owner.
	if err := os.MkdirAll(*haproxyChrootDir, 0o000); err != nil {
		log.Fatalf("mkdir %s: %v", *haproxyChrootDir, err)
	}

	haproxyMgr := haproxy.NewManager(*haproxyBin, *haproxyCfg, *haproxyPid, *haproxySock)

	// Start haproxy from whatever config is already on disk (the
	// bootstrap default at first boot - see rootfs/base/etc/haproxy -
	// or the last config ApplyConfig wrote), so a node runs HAProxy from
	// boot without needing an API call first. Not fatal: a dev build
	// without the haproxy binary in place should still serve the gRPC
	// API for everything else.
	if err := haproxyMgr.Reload(); err != nil {
		log.Printf("haproxy: initial start failed (continuing without it): %v", err)
	}

	// If rootfs/init's checkBootCommit left a pending wait_for_health
	// marker for this boot, confirm it against HAProxy's *actual*
	// health - not just "this daemon process is still running", which
	// says nothing about whether haproxy itself ever came up - and
	// revert automatically if it never does. Runs in the background:
	// gRPC must start regardless, and a marker (the rare case) shouldn't
	// delay it.
	if marker, err := bootcommit.Read(); err != nil {
		log.Printf("bootcommit: read marker: %v", err)
	} else if marker != nil {
		go confirmBootHealth(marker, haproxyMgr)
	}

	tlsConfig := pkiBootstrap.CA.ServerTLSConfig(pkiBootstrap.ServerCert)
	srv := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(tlsConfig)),
		grpc.UnaryInterceptor(api.UnaryAuthInterceptor),
		grpc.StreamInterceptor(api.StreamAuthInterceptor),
	)
	haproxyosv1alpha1.RegisterSystemServiceServer(srv, &api.System{BuildVersion: version, CA: pkiBootstrap.CA})
	haproxyosv1alpha1.RegisterLifecycleServiceServer(srv, &api.Lifecycle{})
	haproxyosv1alpha1.RegisterHAProxyServiceServer(srv, &api.HAProxy{Manager: haproxyMgr})
	haproxyosv1alpha1.RegisterNetworkServiceServer(srv, &api.Network{})

	log.Printf("haproxyosd %s listening on %s (mTLS required)", version, *addr)
	if err := srv.Serve(lis); err != nil {
		fmt.Fprintln(os.Stderr, "serve:", err)
		os.Exit(1)
	}
}

// healthPollInterval/healthStableChecks bound how quickly a genuinely
// healthy HAProxy gets confirmed: 3 consecutive successful checks,
// 500ms apart, is enough to rule out a single fluke (a check that
// raced startup, say) without adding a slow, arbitrary-feeling delay
// on top of an already-real signal.
const (
	healthPollInterval   = 500 * time.Millisecond
	healthStableChecks   = 3
	defaultHealthTimeout = 60 * time.Second // used if the marker's own HealthTimeoutSeconds is unset
)

// confirmBootHealth runs internal/bootcommit.Confirm against a real
// HAProxy health signal (ShowInfo succeeding means the stats socket is
// up and answering, which requires the haproxy process itself to
// actually be running - not just that this daemon, haproxyosd, is)
// and, on failure, reverts back to marker.RevertTo the same way
// rootfs/init's own boot-time revert path does (internal/bootrevert),
// then reboots. Meant to run in its own goroutine - it blocks for up to
// the marker's own health-timeout.
func confirmBootHealth(marker *bootcommit.Marker, mgr *haproxy.Manager) {
	log.Printf("bootcommit: confirming health for slot %s (revert to %s if it never comes up)", marker.Slot, marker.RevertTo)

	confirmed, err := bootcommit.Confirm(marker,
		func() error {
			_, err := mgr.ShowInfo()
			return err
		},
		func() error {
			if err := bootrevert.To(marker); err != nil {
				return err
			}
			syscall.Sync()
			log.Printf("bootcommit: rebooting to complete the revert to slot %s", marker.RevertTo)
			return syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART)
		},
		healthPollInterval, healthStableChecks, defaultHealthTimeout)

	if err != nil {
		// Either Clear() failed on the confirm path, or the revert
		// itself failed - either way, this node may now be stuck on an
		// unconfirmed slot with no automatic recovery left, worth
		// logging loudly rather than just silently returning.
		log.Printf("bootcommit: %v", err)
		return
	}
	if confirmed {
		log.Printf("bootcommit: confirmed healthy for slot %s", marker.Slot)
	}
	// !confirmed && err == nil: the revert (and reboot) succeeded -
	// nothing further to do, the machine is already on its way down.
}

