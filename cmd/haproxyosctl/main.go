// Command haproxyosctl is the HAProxyOS admin CLI, a thin client over the
// gRPC API served by haproxyosd (api/proto/haproxyos/v1alpha1), always
// over mTLS (internal/pki) - there is no insecure fallback. Command
// surface grows alongside its corresponding service implementation (see
// docs/architecture.md's roadmap).
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/types/known/emptypb"

	haproxyosv1alpha1 "github.com/swenske/HAProxyOS/gen/haproxyos/v1alpha1"
	"github.com/swenske/HAProxyOS/internal/pki"
)

var version = "dev"

func main() {
	endpoint := flag.String("endpoint", "127.0.0.1:9505", "haproxyosd gRPC endpoint")
	// Defaults match haproxyosd's own default -pki-dir - convenient when
	// haproxyosctl runs on the same filesystem as the daemon (local/dev
	// use); a real remote operator passes their own issued certificate
	// (see "haproxy show-info" etc. needing a cert from
	// GenerateClientConfiguration first, or the admin cert haproxyosd
	// printed on its first boot).
	caFile := flag.String("ca", "/etc/haproxyos/pki/ca.crt", "path to the CA certificate")
	certFile := flag.String("cert", "/etc/haproxyos/pki/admin.crt", "path to the client certificate")
	keyFile := flag.String("key", "/etc/haproxyos/pki/admin.key", "path to the client private key")
	flag.Parse()

	if flag.NArg() == 0 {
		usage()
		os.Exit(2)
	}

	conn, err := dial(*endpoint, *caFile, *certFile, *keyFile)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	switch cmd := flag.Arg(0); cmd {
	case "version":
		runVersion(conn)
	case "haproxy":
		runHAProxy(conn, flag.Args()[1:])
	case "pki":
		runPKI(conn, flag.Args()[1:])
	default:
		fmt.Fprintf(os.Stderr, "haproxyosctl: unknown command %q\n", cmd)
		usage()
		os.Exit(2)
	}
}

func dial(endpoint, caFile, certFile, keyFile string) (*grpc.ClientConn, error) {
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read CA certificate %s: %w", caFile, err)
	}
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		return nil, fmt.Errorf("read client certificate %s: %w", certFile, err)
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("read client key %s: %w", keyFile, err)
	}

	tlsConfig, err := pki.ClientTLSConfig(caPEM, certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("build TLS config: %w", err)
	}

	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", endpoint, err)
	}
	return conn, nil
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: haproxyosctl [-endpoint host:port] <command>")
	fmt.Fprintln(os.Stderr, "commands:")
	fmt.Fprintln(os.Stderr, "  version                    print haproxyosctl's own version and the connected node's version")
	fmt.Fprintln(os.Stderr, "  haproxy show-info          HAProxy version/uptime/connections (stats socket)")
	fmt.Fprintln(os.Stderr, "  haproxy stats              raw 'show stat' CSV from the stats socket")
	fmt.Fprintln(os.Stderr, "  haproxy get-config         print the currently active haproxy.cfg")
	fmt.Fprintln(os.Stderr, "  haproxy apply-config FILE  validate + apply + seamlessly reload with FILE's contents")
	fmt.Fprintln(os.Stderr, "  haproxy map-list                  list file-backed maps known to the running config")
	fmt.Fprintln(os.Stderr, "  haproxy map-get MAP                dump MAP's key/value entries")
	fmt.Fprintln(os.Stderr, "  haproxy map-set MAP KEY VALUE      upsert one entry in MAP")
	fmt.Fprintln(os.Stderr, "  haproxy map-delete MAP KEY          delete one entry from MAP")
	fmt.Fprintln(os.Stderr, "  haproxy acl-add ACL VALUE           add one pattern value to ACL")
	fmt.Fprintln(os.Stderr, "  haproxy acl-delete ACL VALUE        delete one pattern value from ACL")
	fmt.Fprintln(os.Stderr, "  haproxy cert-list                   list certificates in HAProxy's cert store")
	fmt.Fprintln(os.Stderr, "  haproxy cert-upload [-crt-list PATH] [-sni host1,host2] NAME FILE  upload a PEM cert+key bundle as NAME, optionally binding it into crt-list PATH")
	fmt.Fprintln(os.Stderr, "  haproxy cert-delete [-crt-list PATH] NAME  delete a certificate (unbinding from crt-list PATH first if given)")
	fmt.Fprintln(os.Stderr, "  pki generate-client-config [-role os:admin|os:reader] DIR  issue a new client certificate, write ca.crt/client.crt/client.key to DIR")
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

func runPKI(conn *grpc.ClientConn, args []string) {
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}

	switch sub := args[0]; sub {
	case "generate-client-config":
		fs := flag.NewFlagSet("pki generate-client-config", flag.ExitOnError)
		role := fs.String("role", "os:admin", "role to request (os:admin or os:reader - see internal/api/authz.go)")
		_ = fs.Parse(args[1:])
		if fs.NArg() != 1 {
			fmt.Fprintln(os.Stderr, "usage: haproxyosctl pki generate-client-config [-role os:admin|os:reader] DIR")
			os.Exit(2)
		}
		dir := fs.Arg(0)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			log.Fatalf("mkdir %s: %v", dir, err)
		}

		c, cancel := ctx()
		defer cancel()
		resp, err := haproxyosv1alpha1.NewSystemServiceClient(conn).GenerateClientConfiguration(c, &haproxyosv1alpha1.GenerateClientConfigurationRequest{
			Roles: []string{*role},
		})
		if err != nil {
			log.Fatalf("GenerateClientConfiguration: %v", err)
		}

		for name, data := range map[string][]byte{"ca.crt": resp.GetCa(), "client.crt": resp.GetCrt(), "client.key": resp.GetKey()} {
			mode := os.FileMode(0o644)
			if name == "client.key" {
				mode = 0o600
			}
			if err := os.WriteFile(dir+"/"+name, data, mode); err != nil {
				log.Fatalf("write %s: %v", name, err)
			}
		}
		fmt.Printf("Wrote %s/{ca.crt,client.crt,client.key}\n", dir)

	default:
		fmt.Fprintf(os.Stderr, "haproxyosctl pki: unknown subcommand %q\n", sub)
		usage()
		os.Exit(2)
	}
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

	case "map-list":
		c, cancel := ctx()
		defer cancel()
		resp, err := client.MapList(c, &emptypb.Empty{})
		if err != nil {
			log.Fatalf("MapList: %v", err)
		}
		for _, name := range resp.GetMaps() {
			fmt.Println(name)
		}

	case "map-get":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: haproxyosctl haproxy map-get MAP")
			os.Exit(2)
		}
		c, cancel := ctx()
		defer cancel()
		resp, err := client.MapGet(c, &haproxyosv1alpha1.MapGetRequest{Map: args[1]})
		if err != nil {
			log.Fatalf("MapGet: %v", err)
		}
		for k, v := range resp.GetEntries() {
			fmt.Printf("%s %s\n", k, v)
		}

	case "map-set":
		if len(args) != 4 {
			fmt.Fprintln(os.Stderr, "usage: haproxyosctl haproxy map-set MAP KEY VALUE")
			os.Exit(2)
		}
		c, cancel := ctx()
		defer cancel()
		_, err := client.MapUpdate(c, &haproxyosv1alpha1.MapUpdateRequest{Map: args[1], Key: args[2], Value: args[3]})
		if err != nil {
			log.Fatalf("MapUpdate: %v", err)
		}

	case "map-delete":
		if len(args) != 3 {
			fmt.Fprintln(os.Stderr, "usage: haproxyosctl haproxy map-delete MAP KEY")
			os.Exit(2)
		}
		c, cancel := ctx()
		defer cancel()
		_, err := client.MapUpdate(c, &haproxyosv1alpha1.MapUpdateRequest{Map: args[1], Key: args[2], Delete: true})
		if err != nil {
			log.Fatalf("MapUpdate: %v", err)
		}

	case "acl-add":
		if len(args) != 3 {
			fmt.Fprintln(os.Stderr, "usage: haproxyosctl haproxy acl-add ACL VALUE")
			os.Exit(2)
		}
		c, cancel := ctx()
		defer cancel()
		_, err := client.ACLUpdate(c, &haproxyosv1alpha1.ACLUpdateRequest{Acl: args[1], Value: args[2]})
		if err != nil {
			log.Fatalf("ACLUpdate: %v", err)
		}

	case "acl-delete":
		if len(args) != 3 {
			fmt.Fprintln(os.Stderr, "usage: haproxyosctl haproxy acl-delete ACL VALUE")
			os.Exit(2)
		}
		c, cancel := ctx()
		defer cancel()
		_, err := client.ACLUpdate(c, &haproxyosv1alpha1.ACLUpdateRequest{Acl: args[1], Value: args[2], Delete: true})
		if err != nil {
			log.Fatalf("ACLUpdate: %v", err)
		}

	case "cert-list":
		c, cancel := ctx()
		defer cancel()
		resp, err := client.CertificateList(c, &emptypb.Empty{})
		if err != nil {
			log.Fatalf("CertificateList: %v", err)
		}
		for _, cert := range resp.GetCertificates() {
			fmt.Printf("%s\tnotAfter=%s\tstatus=%s\n", cert.GetName(), cert.GetNotAfter(), cert.GetStatus())
		}

	case "cert-upload":
		fs := flag.NewFlagSet("haproxy cert-upload", flag.ExitOnError)
		crtList := fs.String("crt-list", "", "bind into this crt-list (a 'bind ... ssl crt-list <path>' already in the running config) - leave empty to only upload, not bind")
		sni := fs.String("sni", "", "comma-separated SNI names to scope the binding to (only meaningful with -crt-list)")
		_ = fs.Parse(args[1:])
		if fs.NArg() != 2 {
			fmt.Fprintln(os.Stderr, "usage: haproxyosctl haproxy cert-upload [-crt-list PATH] [-sni host1,host2] NAME FILE")
			os.Exit(2)
		}
		name, file := fs.Arg(0), fs.Arg(1)
		data, err := os.ReadFile(file)
		if err != nil {
			log.Fatalf("read %s: %v", file, err)
		}
		var sniList []string
		if *sni != "" {
			sniList = strings.Split(*sni, ",")
		}
		c, cancel := ctx()
		defer cancel()
		_, err = client.CertificateUpload(c, &haproxyosv1alpha1.CertificateUploadRequest{
			Name: name, PemBundle: data, CrtList: *crtList, Sni: sniList,
		})
		if err != nil {
			log.Fatalf("CertificateUpload: %v", err)
		}

	case "cert-delete":
		fs := flag.NewFlagSet("haproxy cert-delete", flag.ExitOnError)
		crtList := fs.String("crt-list", "", "unbind from this crt-list before deleting - required if the certificate is still bound anywhere")
		_ = fs.Parse(args[1:])
		if fs.NArg() != 1 {
			fmt.Fprintln(os.Stderr, "usage: haproxyosctl haproxy cert-delete [-crt-list PATH] NAME")
			os.Exit(2)
		}
		c, cancel := ctx()
		defer cancel()
		_, err := client.CertificateDelete(c, &haproxyosv1alpha1.CertificateDeleteRequest{Name: fs.Arg(0), CrtList: *crtList})
		if err != nil {
			log.Fatalf("CertificateDelete: %v", err)
		}

	default:
		fmt.Fprintf(os.Stderr, "haproxyosctl haproxy: unknown subcommand %q\n", sub)
		usage()
		os.Exit(2)
	}
}
