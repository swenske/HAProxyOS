// Command haproxyosd is the HAProxyOS control-plane daemon: it serves the
// SystemService/LifecycleService/HAProxyService/NetworkService gRPC API
// (see api/proto/haproxyos/v1alpha1) that haproxyosctl and any external
// controller use instead of SSH.
//
// Phase 0: plain TCP, no mTLS yet (see internal/pki, roadmap Phase 2/3 in
// docs/architecture.md). Do not expose this build's port beyond a trusted
// network.
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"

	"google.golang.org/grpc"

	haproxyosv1alpha1 "github.com/swenske/HAProxyOS/gen/haproxyos/v1alpha1"
	"github.com/swenske/HAProxyOS/internal/api"
)

// version is set via -ldflags "-X main.version=..." by the release build
// (see Makefile), left as "dev" for local builds.
var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print the daemon version and exit")
	addr := flag.String("addr", ":9505", "gRPC listen address")
	flag.Parse()

	if *showVersion {
		fmt.Println("haproxyosd " + version)
		return
	}

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen on %s: %v", *addr, err)
	}

	srv := grpc.NewServer()
	haproxyosv1alpha1.RegisterSystemServiceServer(srv, &api.System{BuildVersion: version})
	haproxyosv1alpha1.RegisterLifecycleServiceServer(srv, &api.Lifecycle{})
	haproxyosv1alpha1.RegisterHAProxyServiceServer(srv, &api.HAProxy{})
	haproxyosv1alpha1.RegisterNetworkServiceServer(srv, &api.Network{})

	log.Printf("haproxyosd %s listening on %s", version, *addr)
	if err := srv.Serve(lis); err != nil {
		fmt.Fprintln(os.Stderr, "serve:", err)
		os.Exit(1)
	}
}
