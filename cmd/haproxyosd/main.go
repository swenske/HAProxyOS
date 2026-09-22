// Command haproxyosd is the HAProxyOS control-plane daemon: it serves the
// SystemService/LifecycleService/HAProxyService/NetworkService gRPC API
// (see api/proto/haproxyos/v1alpha1) that haproxyosctl and any external
// controller use instead of SSH.
//
// mTLS is mandatory on every connection (internal/pki) - there is no
// plaintext or unauthenticated mode. On first boot (empty -pki-dir) a
// CA, this node's server certificate, and an initial admin client
// certificate are generated and the admin certificate/key are printed
// once, since there's no shell to retrieve them from later (see
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

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	haproxyosv1alpha1 "github.com/swenske/HAProxyOS/gen/haproxyos/v1alpha1"
	"github.com/swenske/HAProxyOS/internal/api"
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
		log.Printf("pki: ADMIN CERTIFICATE (save this now, it will not be printed again):\n%s%s", pkiBootstrap.AdminCertPEM, pkiBootstrap.AdminKeyPEM)
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

	tlsConfig := pkiBootstrap.CA.ServerTLSConfig(pkiBootstrap.ServerCert)
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsConfig)))
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

