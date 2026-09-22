// Command haproxyosctl is the HAProxyOS admin CLI, a thin client over the
// gRPC API served by haproxyosd (api/proto/haproxyos/v1alpha1). Phase 0
// only wires up "version" as an end-to-end connectivity check; the rest of
// the command surface lands alongside its corresponding service
// implementation (see docs/architecture.md's roadmap).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"

	haproxyosv1alpha1 "github.com/swenske/HAProxyOS/gen/haproxyos/v1alpha1"
)

var version = "dev"

func main() {
	endpoint := flag.String("endpoint", "127.0.0.1:9505", "haproxyosd gRPC endpoint")
	flag.Parse()

	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: haproxyosctl [-endpoint host:port] <command>")
		fmt.Fprintln(os.Stderr, "commands:")
		fmt.Fprintln(os.Stderr, "  version   print haproxyosctl's own version and the connected node's version")
		os.Exit(2)
	}

	switch cmd := flag.Arg(0); cmd {
	case "version":
		runVersion(*endpoint)
	default:
		fmt.Fprintf(os.Stderr, "haproxyosctl: unknown command %q\n", cmd)
		os.Exit(2)
	}
}

func runVersion(endpoint string) {
	fmt.Println("Client:", version)

	// Phase 0: plain TCP, no mTLS yet - see cmd/haproxyosd.
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("dial %s: %v", endpoint, err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := haproxyosv1alpha1.NewSystemServiceClient(conn).Version(ctx, &emptypb.Empty{})
	if err != nil {
		log.Fatalf("Version: %v", err)
	}
	fmt.Println("Node:", resp.GetVersion())
}
