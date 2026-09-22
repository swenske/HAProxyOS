// Command haproxyosctl is the HAProxyOS admin CLI, a thin client over the
// gRPC API served by haproxyosd (api/proto/haproxyos/v1alpha1). Command
// surface grows alongside its corresponding service implementation (see
// docs/architecture.md's roadmap) - "version" from Phase 0, "haproxy ..."
// from Phase 2.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
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
		usage()
		os.Exit(2)
	}

	// Phase 2: plain TCP, no mTLS yet - see cmd/haproxyosd and internal/pki.
	conn, err := grpc.NewClient(*endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("dial %s: %v", *endpoint, err)
	}
	defer conn.Close()

	switch cmd := flag.Arg(0); cmd {
	case "version":
		runVersion(conn)
	case "haproxy":
		runHAProxy(conn, flag.Args()[1:])
	default:
		fmt.Fprintf(os.Stderr, "haproxyosctl: unknown command %q\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: haproxyosctl [-endpoint host:port] <command>")
	fmt.Fprintln(os.Stderr, "commands:")
	fmt.Fprintln(os.Stderr, "  version                    print haproxyosctl's own version and the connected node's version")
	fmt.Fprintln(os.Stderr, "  haproxy show-info          HAProxy version/uptime/connections (stats socket)")
	fmt.Fprintln(os.Stderr, "  haproxy stats              raw 'show stat' CSV from the stats socket")
	fmt.Fprintln(os.Stderr, "  haproxy get-config         print the currently active haproxy.cfg")
	fmt.Fprintln(os.Stderr, "  haproxy apply-config FILE  validate + apply + seamlessly reload with FILE's contents")
}

func ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

func runVersion(conn *grpc.ClientConn) {
	fmt.Println("Client:", version)

	c, cancel := ctx()
	defer cancel()

	resp, err := haproxyosv1alpha1.NewSystemServiceClient(conn).Version(c, &emptypb.Empty{})
	if err != nil {
		log.Fatalf("Version: %v", err)
	}
	fmt.Println("Node:", resp.GetVersion())
}

func runHAProxy(conn *grpc.ClientConn, args []string) {
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	client := haproxyosv1alpha1.NewHAProxyServiceClient(conn)

	switch sub := args[0]; sub {
	case "show-info":
		c, cancel := ctx()
		defer cancel()
		info, err := client.ShowInfo(c, &emptypb.Empty{})
		if err != nil {
			log.Fatalf("ShowInfo: %v", err)
		}
		fmt.Printf("Version:      %s\n", info.GetVersion())
		fmt.Printf("Uptime:       %ds\n", info.GetUptimeSeconds())
		fmt.Printf("Connections:  %d / %d\n", info.GetCurrentConnections(), info.GetMaxConnections())

	case "stats":
		c, cancel := ctx()
		defer cancel()
		resp, err := client.Stats(c, &emptypb.Empty{})
		if err != nil {
			log.Fatalf("Stats: %v", err)
		}
		os.Stdout.Write(resp.GetRawCsv())

	case "get-config":
		c, cancel := ctx()
		defer cancel()
		resp, err := client.GetConfig(c, &emptypb.Empty{})
		if err != nil {
			log.Fatalf("GetConfig: %v", err)
		}
		os.Stdout.Write(resp.GetConfig())

	case "apply-config":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: haproxyosctl haproxy apply-config FILE")
			os.Exit(2)
		}
		data, err := os.ReadFile(args[1])
		if err != nil {
			log.Fatalf("read %s: %v", args[1], err)
		}

		c, cancel := ctx()
		defer cancel()
		stream, err := client.ApplyConfig(c, &haproxyosv1alpha1.ApplyConfigRequest{Config: data})
		if err != nil {
			log.Fatalf("ApplyConfig: %v", err)
		}
		var last *haproxyosv1alpha1.ApplyConfigResponse
		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				log.Fatalf("ApplyConfig: %v", err)
			}
			fmt.Printf("[%s] %s\n", resp.GetStage(), resp.GetMessage())
			last = resp
		}
		if last != nil && !last.GetAccepted() {
			os.Exit(1)
		}

	default:
		fmt.Fprintf(os.Stderr, "haproxyosctl haproxy: unknown subcommand %q\n", sub)
		usage()
		os.Exit(2)
	}
}
