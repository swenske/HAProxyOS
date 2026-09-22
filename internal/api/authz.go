// Role enforcement: mTLS (internal/pki, wired up in cmd/haproxyosd)
// proves *who* is calling - this file decides *what* they're allowed to
// call, based on the role(s) carried in their verified client
// certificate's Subject.Organization.
//
// requiredRoles is fail-closed by design: a method with no entry defaults
// to admin-only rather than being silently open. Every RPC in
// api/proto/haproxyos/v1alpha1 is listed below deliberately, so a new RPC
// that forgets to be added here is caught immediately (it'll be
// admin-only until someone decides otherwise, never accidentally
// reader-accessible).
//
// RoleReader is scoped to observability/status only. Notably NOT
// reader-accessible: List/Read/Copy/Dmesg/Logs/DiskUsage/PacketCapture -
// these don't mutate anything, but they can expose sensitive file
// contents or traffic, which is a different risk than "can this identity
// see a metric."
package api

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/swenske/HAProxyOS/internal/pki"
)

var adminOnly = []string{pki.RoleAdmin}
var adminOrReader = []string{pki.RoleAdmin, pki.RoleReader}

var requiredRoles = map[string][]string{
	// SystemService
	"/haproxyos.v1alpha1.SystemService/Version":                     adminOrReader,
	"/haproxyos.v1alpha1.SystemService/Hostname":                    adminOrReader,
	"/haproxyos.v1alpha1.SystemService/Reboot":                      adminOnly,
	"/haproxyos.v1alpha1.SystemService/Shutdown":                    adminOnly,
	"/haproxyos.v1alpha1.SystemService/Restart":                     adminOnly,
	"/haproxyos.v1alpha1.SystemService/Reset":                       adminOnly,
	"/haproxyos.v1alpha1.SystemService/ApplyConfiguration":          adminOnly,
	"/haproxyos.v1alpha1.SystemService/Events":                      adminOrReader,
	"/haproxyos.v1alpha1.SystemService/Dmesg":                       adminOnly, // kernel log can leak boot secrets/paths
	"/haproxyos.v1alpha1.SystemService/Logs":                        adminOnly, // service logs can leak request data
	"/haproxyos.v1alpha1.SystemService/Stats":                       adminOrReader,
	"/haproxyos.v1alpha1.SystemService/SystemStat":                  adminOrReader,
	"/haproxyos.v1alpha1.SystemService/Memory":                      adminOrReader,
	"/haproxyos.v1alpha1.SystemService/CPUInfo":                     adminOrReader,
	"/haproxyos.v1alpha1.SystemService/LoadAvg":                     adminOrReader,
	"/haproxyos.v1alpha1.SystemService/DiskStats":                   adminOrReader,
	"/haproxyos.v1alpha1.SystemService/DiskUsage":                   adminOnly, // walks arbitrary paths
	"/haproxyos.v1alpha1.SystemService/NetworkDeviceStats":          adminOrReader,
	"/haproxyos.v1alpha1.SystemService/Netstat":                     adminOrReader,
	"/haproxyos.v1alpha1.SystemService/Mounts":                      adminOrReader,
	"/haproxyos.v1alpha1.SystemService/Processes":                   adminOrReader,
	"/haproxyos.v1alpha1.SystemService/ServiceList":                 adminOrReader,
	"/haproxyos.v1alpha1.SystemService/ServiceStart":                adminOnly,
	"/haproxyos.v1alpha1.SystemService/ServiceStop":                 adminOnly,
	"/haproxyos.v1alpha1.SystemService/ServiceRestart":              adminOnly,
	"/haproxyos.v1alpha1.SystemService/List":                        adminOnly, // filesystem access
	"/haproxyos.v1alpha1.SystemService/Read":                        adminOnly, // can read secrets/keys
	"/haproxyos.v1alpha1.SystemService/Copy":                        adminOnly, // can read secrets/keys
	"/haproxyos.v1alpha1.SystemService/PacketCapture":               adminOnly, // can capture unencrypted traffic
	"/haproxyos.v1alpha1.SystemService/MetaWrite":                   adminOnly,
	"/haproxyos.v1alpha1.SystemService/MetaDelete":                  adminOnly,
	"/haproxyos.v1alpha1.SystemService/GenerateClientConfiguration": adminOnly, // issuing credentials is itself a privileged operation

	// LifecycleService - installing/upgrading/rolling back the machine
	// is always privileged, no reader carve-out.
	"/haproxyos.v1alpha1.LifecycleService/Install":  adminOnly,
	"/haproxyos.v1alpha1.LifecycleService/Upgrade":  adminOnly,
	"/haproxyos.v1alpha1.LifecycleService/Rollback": adminOnly,

	// HAProxyService
	"/haproxyos.v1alpha1.HAProxyService/GetConfig":         adminOrReader,
	"/haproxyos.v1alpha1.HAProxyService/ApplyConfig":       adminOnly,
	"/haproxyos.v1alpha1.HAProxyService/ValidateConfig":    adminOrReader, // no side effects - validates the caller's own input
	"/haproxyos.v1alpha1.HAProxyService/Reload":            adminOnly,
	"/haproxyos.v1alpha1.HAProxyService/Stats":             adminOrReader,
	"/haproxyos.v1alpha1.HAProxyService/ShowInfo":          adminOrReader,
	"/haproxyos.v1alpha1.HAProxyService/BackendList":       adminOrReader,
	"/haproxyos.v1alpha1.HAProxyService/ServerSetState":    adminOnly,
	"/haproxyos.v1alpha1.HAProxyService/MapList":           adminOrReader,
	"/haproxyos.v1alpha1.HAProxyService/MapGet":            adminOrReader,
	"/haproxyos.v1alpha1.HAProxyService/MapUpdate":         adminOnly,
	"/haproxyos.v1alpha1.HAProxyService/ACLUpdate":         adminOnly,
	"/haproxyos.v1alpha1.HAProxyService/CertificateList":   adminOrReader, // names/expiry only, not key material
	"/haproxyos.v1alpha1.HAProxyService/CertificateUpload": adminOnly,
	"/haproxyos.v1alpha1.HAProxyService/CertificateDelete": adminOnly,

	// NetworkService
	"/haproxyos.v1alpha1.NetworkService/BGPStatus":            adminOrReader,
	"/haproxyos.v1alpha1.NetworkService/BGPApplyConfig":       adminOnly,
	"/haproxyos.v1alpha1.NetworkService/VRRPStatus":           adminOrReader,
	"/haproxyos.v1alpha1.NetworkService/VRRPApplyConfig":      adminOnly,
	"/haproxyos.v1alpha1.NetworkService/FirewallList":         adminOrReader,
	"/haproxyos.v1alpha1.NetworkService/FirewallApplyRuleset": adminOnly,
}

// UnaryAuthInterceptor enforces requiredRoles for unary RPCs.
func UnaryAuthInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if err := checkRole(ctx, info.FullMethod); err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

// StreamAuthInterceptor enforces requiredRoles for streaming RPCs.
func StreamAuthInterceptor(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	if err := checkRole(ss.Context(), info.FullMethod); err != nil {
		return err
	}
	return handler(srv, ss)
}

func checkRole(ctx context.Context, fullMethod string) error {
	required, ok := requiredRoles[fullMethod]
	if !ok {
		// Fail closed: an RPC we forgot to classify is admin-only, never
		// silently open to readers.
		required = adminOnly
	}

	p, ok := peer.FromContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "no peer information")
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(tlsInfo.State.PeerCertificates) == 0 {
		return status.Error(codes.Unauthenticated, "no client certificate presented")
	}
	peerRoles := tlsInfo.State.PeerCertificates[0].Subject.Organization

	for _, want := range required {
		for _, have := range peerRoles {
			if have == want {
				return nil
			}
		}
	}
	return status.Errorf(codes.PermissionDenied, "%s requires role %v, certificate has %v", fullMethod, required, peerRoles)
}
