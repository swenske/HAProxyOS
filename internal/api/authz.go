// Role enforcement: mTLS (internal/pki, wired up in cmd/janusd)
// proves *who* is calling - this file decides *what* they're allowed to
// call, based on the role(s) carried in their verified client
// certificate's Subject.Organization.
//
// requiredRoles is fail-closed by design: a method with no entry defaults
// to admin-only rather than being silently open. Every RPC in
// api/proto/janus/v1alpha1 is listed below deliberately, so a new RPC
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

	"github.com/swenske/Janus/internal/pki"
)

var adminOnly = []string{pki.RoleAdmin}
var adminOrReader = []string{pki.RoleAdmin, pki.RoleReader}

var requiredRoles = map[string][]string{
	// SystemService
	"/janus.v1alpha1.SystemService/Version":                     adminOrReader,
	"/janus.v1alpha1.SystemService/Hostname":                    adminOrReader,
	"/janus.v1alpha1.SystemService/Reboot":                      adminOnly,
	"/janus.v1alpha1.SystemService/Shutdown":                    adminOnly,
	"/janus.v1alpha1.SystemService/Restart":                     adminOnly,
	"/janus.v1alpha1.SystemService/Reset":                       adminOnly,
	"/janus.v1alpha1.SystemService/ApplyConfiguration":          adminOnly,
	"/janus.v1alpha1.SystemService/Events":                      adminOrReader,
	"/janus.v1alpha1.SystemService/Dmesg":                       adminOnly, // kernel log can leak boot secrets/paths
	"/janus.v1alpha1.SystemService/Logs":                        adminOnly, // service logs can leak request data
	"/janus.v1alpha1.SystemService/Stats":                       adminOrReader,
	"/janus.v1alpha1.SystemService/SystemStat":                  adminOrReader,
	"/janus.v1alpha1.SystemService/Memory":                      adminOrReader,
	"/janus.v1alpha1.SystemService/CPUInfo":                     adminOrReader,
	"/janus.v1alpha1.SystemService/LoadAvg":                     adminOrReader,
	"/janus.v1alpha1.SystemService/DiskStats":                   adminOrReader,
	"/janus.v1alpha1.SystemService/DiskUsage":                   adminOnly, // walks arbitrary paths
	"/janus.v1alpha1.SystemService/NetworkDeviceStats":          adminOrReader,
	"/janus.v1alpha1.SystemService/Netstat":                     adminOrReader,
	"/janus.v1alpha1.SystemService/Mounts":                      adminOrReader,
	"/janus.v1alpha1.SystemService/Processes":                   adminOrReader,
	"/janus.v1alpha1.SystemService/ServiceList":                 adminOrReader,
	"/janus.v1alpha1.SystemService/ServiceStart":                adminOnly,
	"/janus.v1alpha1.SystemService/ServiceStop":                 adminOnly,
	"/janus.v1alpha1.SystemService/ServiceRestart":              adminOnly,
	"/janus.v1alpha1.SystemService/List":                        adminOnly, // filesystem access
	"/janus.v1alpha1.SystemService/Read":                        adminOnly, // can read secrets/keys
	"/janus.v1alpha1.SystemService/Copy":                        adminOnly, // can read secrets/keys
	"/janus.v1alpha1.SystemService/PacketCapture":               adminOnly, // can capture unencrypted traffic
	"/janus.v1alpha1.SystemService/MetaWrite":                   adminOnly,
	"/janus.v1alpha1.SystemService/MetaDelete":                  adminOnly,
	"/janus.v1alpha1.SystemService/GenerateClientConfiguration": adminOnly, // issuing credentials is itself a privileged operation

	// LifecycleService - installing/upgrading/rolling back the machine
	// is always privileged, no reader carve-out.
	"/janus.v1alpha1.LifecycleService/Install":  adminOnly,
	"/janus.v1alpha1.LifecycleService/Upgrade":  adminOnly,
	"/janus.v1alpha1.LifecycleService/Rollback": adminOnly,

	// HAProxyService
	"/janus.v1alpha1.HAProxyService/GetConfig":         adminOrReader,
	"/janus.v1alpha1.HAProxyService/ApplyConfig":       adminOnly,
	"/janus.v1alpha1.HAProxyService/ValidateConfig":    adminOrReader, // no side effects - validates the caller's own input
	"/janus.v1alpha1.HAProxyService/Reload":            adminOnly,
	"/janus.v1alpha1.HAProxyService/Stats":             adminOrReader,
	"/janus.v1alpha1.HAProxyService/ShowInfo":          adminOrReader,
	"/janus.v1alpha1.HAProxyService/BackendList":       adminOrReader,
	"/janus.v1alpha1.HAProxyService/ServerSetState":    adminOnly,
	"/janus.v1alpha1.HAProxyService/MapList":           adminOrReader,
	"/janus.v1alpha1.HAProxyService/MapGet":            adminOrReader,
	"/janus.v1alpha1.HAProxyService/MapUpdate":         adminOnly,
	"/janus.v1alpha1.HAProxyService/ACLUpdate":         adminOnly,
	"/janus.v1alpha1.HAProxyService/CertificateList":   adminOrReader, // names/expiry only, not key material
	"/janus.v1alpha1.HAProxyService/CertificateUpload": adminOnly,
	"/janus.v1alpha1.HAProxyService/CertificateDelete": adminOnly,

	// NetworkService
	"/janus.v1alpha1.NetworkService/BGPStatus":            adminOrReader,
	"/janus.v1alpha1.NetworkService/BGPApplyConfig":       adminOnly,
	"/janus.v1alpha1.NetworkService/VRRPStatus":           adminOrReader,
	"/janus.v1alpha1.NetworkService/VRRPApplyConfig":      adminOnly,
	"/janus.v1alpha1.NetworkService/FirewallList":         adminOrReader,
	"/janus.v1alpha1.NetworkService/FirewallApplyRuleset": adminOnly,
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
