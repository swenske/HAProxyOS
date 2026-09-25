// Package nodeproxy runs one dedicated HTTPS listener per registered
// node, on that node's own allocated port (see dashboard/backend/
// internal/store) - the browser's TLS client-certificate negotiation
// happens per origin (host:port), so managing more than one node with
// native browser cert selection needs a distinct origin per node, not
// one shared listener (see the rebranding/dashboard plan's own
// architecture section for the full reasoning).
//
// The client certificate a browser presents here only ever proves the
// browser is allowed into *this* node's view (it's checked against that
// node's own CA) - it is never, and cryptographically can never be,
// reused to talk to the real node (a TLS server can verify a client
// holds a private key, it can never extract that key). All real gRPC
// calls to the node use the store.Node's own dedicated service
// credential instead.
package nodeproxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/pki"

	"github.com/swenske/Janus/dashboard/backend/internal/store"
)

// staticFiles is the entire per-node dashboard view - deliberately
// plain HTML/JS, no build step, no framework (unlike dashboard/
// frontend's own React SPA, which lives on a completely different
// origin - see this package's own doc comment for why the two can't
// just be the same thing).
//
//go:embed static
var staticFiles embed.FS

// Listener is one running per-node HTTPS endpoint.
type Listener struct {
	node   *store.Node
	server *http.Server
}

// Start begins serving node's dashboard view on its own allocated port
// immediately - callers should treat a returned error as "this node's
// listener never came up", not something to retry inline.
func Start(node *store.Node, dashboardServerCert tls.Certificate) (*Listener, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(node.CACertPEM) {
		return nil, fmt.Errorf("node %s: no valid certificates in stored ca.crt", node.ID)
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{dashboardServerCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS13,
	}

	view, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return nil, fmt.Errorf("static assets: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/info", func(w http.ResponseWriter, r *http.Request) {
		handleInfo(w, r, node)
	})
	registerOpsRoutes(mux, node)
	mux.Handle("/", http.FileServerFS(view))

	addr := fmt.Sprintf(":%d", node.Port)
	ln, err := tls.Listen("tcp", addr, tlsConfig)
	if err != nil {
		return nil, fmt.Errorf("node %s: listen on %s: %w", node.ID, addr, err)
	}

	srv := &http.Server{Handler: mux}
	go func() {
		// ErrServerClosed is the expected outcome of Stop() below, not a
		// real failure - nothing else to do with a listener error after
		// the fact except let it die; the node just stops being
		// reachable through the dashboard until re-added.
		_ = srv.Serve(ln)
	}()

	return &Listener{node: node, server: srv}, nil
}

// Stop shuts this node's listener down - used both for explicit node
// removal and (in a future slice) restarting a listener after its
// service credential is rotated.
func (l *Listener) Stop(ctx context.Context) error {
	return l.server.Shutdown(ctx)
}

// dialNode opens a real gRPC connection to node using its own stored
// service credential - never the browser's client certificate (see the
// package doc comment for why that's not just a design choice but a
// cryptographic impossibility).
func dialNode(node *store.Node) (*grpc.ClientConn, error) {
	tlsConfig, err := pki.ClientTLSConfig(node.CACertPEM, node.ServiceCertPEM, node.ServiceKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("build TLS config: %w", err)
	}
	return grpc.NewClient(node.Address, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
}

// infoResponse is the dashboard's single-node view payload - the same
// set of SystemService RPCs hack/qemu-system-info-test.sh already proves
// return real values from a real boot (see internal/api/system_stats.go).
type infoResponse struct {
	Version       string       `json:"version"`
	GoVersion     string       `json:"go_version"`
	KernelVersion string       `json:"kernel_version"`
	ActiveSlot    string       `json:"active_slot"`
	Memory        *memoryInfo  `json:"memory"`
	CPUs          []cpuInfo    `json:"cpus"`
	LoadAvg       *loadAvgInfo `json:"load_avg"`
	Disks         []diskInfo   `json:"disks"`
}

type memoryInfo struct {
	TotalBytes     uint64 `json:"total_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`
	CachedBytes    uint64 `json:"cached_bytes"`
}

type cpuInfo struct {
	Processor uint32  `json:"processor"`
	ModelName string  `json:"model_name"`
	MHz       float64 `json:"mhz"`
}

type loadAvgInfo struct {
	Load1  float64 `json:"load1"`
	Load5  float64 `json:"load5"`
	Load15 float64 `json:"load15"`
}

type diskInfo struct {
	DeviceName     string `json:"device_name"`
	ReadCompleted  uint64 `json:"read_completed"`
	WriteCompleted uint64 `json:"write_completed"`
}

func handleInfo(w http.ResponseWriter, r *http.Request, node *store.Node) {
	conn, err := dialNode(node)
	if err != nil {
		http.Error(w, fmt.Sprintf("dial node: %v", err), http.StatusBadGateway)
		return
	}
	defer conn.Close()

	client := janusv1alpha1.NewSystemServiceClient(conn)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	ver, err := client.Version(ctx, &emptypb.Empty{})
	if err != nil {
		http.Error(w, fmt.Sprintf("Version: %v", err), http.StatusBadGateway)
		return
	}
	mem, err := client.Memory(ctx, &emptypb.Empty{})
	if err != nil {
		http.Error(w, fmt.Sprintf("Memory: %v", err), http.StatusBadGateway)
		return
	}
	cpu, err := client.CPUInfo(ctx, &emptypb.Empty{})
	if err != nil {
		http.Error(w, fmt.Sprintf("CPUInfo: %v", err), http.StatusBadGateway)
		return
	}
	load, err := client.LoadAvg(ctx, &emptypb.Empty{})
	if err != nil {
		http.Error(w, fmt.Sprintf("LoadAvg: %v", err), http.StatusBadGateway)
		return
	}
	disks, err := client.DiskStats(ctx, &emptypb.Empty{})
	if err != nil {
		http.Error(w, fmt.Sprintf("DiskStats: %v", err), http.StatusBadGateway)
		return
	}

	resp := infoResponse{
		Version:       ver.GetVersion(),
		GoVersion:     ver.GetGoVersion(),
		KernelVersion: ver.GetKernelVersion(),
		ActiveSlot:    ver.GetActiveSlot(),
		Memory: &memoryInfo{
			TotalBytes:     mem.GetTotalBytes(),
			AvailableBytes: mem.GetAvailableBytes(),
			CachedBytes:    mem.GetCachedBytes(),
		},
		LoadAvg: &loadAvgInfo{Load1: load.GetLoad1(), Load5: load.GetLoad5(), Load15: load.GetLoad15()},
	}
	for _, c := range cpu.GetCpus() {
		resp.CPUs = append(resp.CPUs, cpuInfo{Processor: c.GetProcessor(), ModelName: c.GetModelName(), MHz: c.GetMhz()})
	}
	for _, d := range disks.GetDisks() {
		resp.Disks = append(resp.Disks, diskInfo{
			DeviceName:     d.GetDeviceName(),
			ReadCompleted:  d.GetReadCompleted(),
			WriteCompleted: d.GetWriteCompleted(),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
