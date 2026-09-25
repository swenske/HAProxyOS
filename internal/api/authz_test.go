package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// TestRequiredRolesCoversEveryRPC registers every service against a real
// *grpc.Server and cross-checks its actual method list against
// requiredRoles, in both directions - catches an RPC added to a .proto
// without a corresponding entry here (would silently fail closed to
// admin-only, which is safe but easy to miss), and a stale entry left
// behind after an RPC is renamed or removed.
func TestRequiredRolesCoversEveryRPC(t *testing.T) {
	srv := grpc.NewServer()
	janusv1alpha1.RegisterSystemServiceServer(srv, &System{})
	janusv1alpha1.RegisterLifecycleServiceServer(srv, &Lifecycle{})
	janusv1alpha1.RegisterHAProxyServiceServer(srv, &HAProxy{})
	janusv1alpha1.RegisterNetworkServiceServer(srv, &Network{})

	seen := map[string]bool{}
	for serviceName, info := range srv.GetServiceInfo() {
		for _, m := range info.Methods {
			full := "/" + serviceName + "/" + m.Name
			seen[full] = true
			if _, ok := requiredRoles[full]; !ok {
				t.Errorf("RPC %s has no entry in requiredRoles", full)
			}
		}
	}
	for full := range requiredRoles {
		if !seen[full] {
			t.Errorf("requiredRoles has a stale entry for %s (not a registered RPC)", full)
		}
	}
}

func TestCheckRole(t *testing.T) {
	requiredRoles["/test.Service/AdminOnly"] = adminOnly
	requiredRoles["/test.Service/AdminOrReader"] = adminOrReader
	t.Cleanup(func() {
		delete(requiredRoles, "/test.Service/AdminOnly")
		delete(requiredRoles, "/test.Service/AdminOrReader")
	})

	tests := []struct {
		name      string
		method    string
		peerRoles []string
		noPeer    bool
		wantCode  codes.Code
	}{
		{name: "admin can call admin-only", method: "/test.Service/AdminOnly", peerRoles: []string{"os:admin"}, wantCode: codes.OK},
		{name: "reader cannot call admin-only", method: "/test.Service/AdminOnly", peerRoles: []string{"os:reader"}, wantCode: codes.PermissionDenied},
		{name: "reader can call admin-or-reader", method: "/test.Service/AdminOrReader", peerRoles: []string{"os:reader"}, wantCode: codes.OK},
		{name: "unknown role is rejected everywhere", method: "/test.Service/AdminOrReader", peerRoles: []string{"os:mystery"}, wantCode: codes.PermissionDenied},
		{name: "unlisted method fails closed to admin-only", method: "/test.Service/NeverClassified", peerRoles: []string{"os:reader"}, wantCode: codes.PermissionDenied},
		{name: "unlisted method allows admin", method: "/test.Service/NeverClassified", peerRoles: []string{"os:admin"}, wantCode: codes.OK},
		{name: "no client certificate at all", method: "/test.Service/AdminOrReader", noPeer: true, wantCode: codes.Unauthenticated},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if !tc.noPeer {
				ctx = peer.NewContext(ctx, &peer.Peer{
					Addr: &net.TCPAddr{},
					AuthInfo: credentials.TLSInfo{
						State: tls.ConnectionState{
							PeerCertificates: []*x509.Certificate{
								{Subject: pkix.Name{Organization: tc.peerRoles}},
							},
						},
					},
				})
			}

			err := checkRole(ctx, tc.method)
			if tc.wantCode == codes.OK {
				if err != nil {
					t.Fatalf("expected success, got %v", err)
				}
				return
			}
			if status.Code(err) != tc.wantCode {
				t.Fatalf("expected code %v, got %v (%v)", tc.wantCode, status.Code(err), err)
			}
		})
	}
}
