// Package api implements the Janus gRPC control-plane services
// declared in api/proto/janus/v1alpha1. Every service is registered
// from day one (see cmd/janusd); methods return codes.Unimplemented
// until their owning phase lands (see docs/architecture.md's roadmap).
package api

import (
	"context"
	"crypto/x509"
	"os"
	"runtime"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/bootslot"
	"github.com/swenske/Janus/internal/pki"
)

// System implements janusv1alpha1.SystemServiceServer. Every method it
// doesn't override falls through to UnimplementedSystemServiceServer's
// generated codes.Unimplemented response.
type System struct {
	janusv1alpha1.UnimplementedSystemServiceServer

	// BuildVersion is the daemon build version, set by cmd/janusd
	// from build-time ldflags.
	BuildVersion string

	// CA issues the client certificates GenerateClientConfiguration hands
	// out. Reaching this RPC at all already required a valid client
	// certificate (mTLS is enforced on the whole listener, see
	// cmd/janusd) - this is for rotating/reissuing credentials, not
	// bootstrapping the very first one (that comes from the admin
	// certificate LoadOrBootstrap prints on first boot).
	CA *pki.CA
}

func (s *System) Version(_ context.Context, _ *emptypb.Empty) (*janusv1alpha1.VersionResponse, error) {
	return &janusv1alpha1.VersionResponse{
		Version:       s.BuildVersion,
		GoVersion:     runtime.Version(),
		KernelVersion: readKernelVersion(),
		ActiveSlot:    currentActiveSlot(),
	}, nil
}

// readKernelVersion reads /proc/sys/kernel/osrelease (e.g. "6.18.53") -
// the same value `uname -r` reports, simpler to read than parsing
// /proc/version's full free-form string. Empty on any read error - this
// field is informational, never worth failing Version over.
func readKernelVersion() string {
	data, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// currentActiveSlot best-effort resolves which A/B slot this boot came
// from, the same way internal/api/lifecycle.go's resolveBootContext
// does - but never errors: an initramfs-only test boot, or any other
// non-A/B boot, legitimately has no slot to report, and that's not a
// reason to fail Version (unlike Rollback, which genuinely can't
// proceed without one).
func currentActiveSlot() string {
	cmdline, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return ""
	}
	dataDev, ok := bootslot.DataDevice(string(cmdline))
	if !ok {
		return ""
	}
	slot, ok := bootslot.ActiveSlot(dataDev)
	if !ok {
		return ""
	}
	return slot
}

func (s *System) GenerateClientConfiguration(_ context.Context, req *janusv1alpha1.GenerateClientConfigurationRequest) (*janusv1alpha1.GenerateClientConfigurationResponse, error) {
	roles := req.GetRoles()
	if len(roles) == 0 {
		roles = []string{pki.RoleAdmin}
	}
	for _, role := range roles {
		// Reject anything not in internal/pki's known role set rather
		// than silently issuing a certificate whose role nothing checks
		// for (see internal/api/authz.go's requiredRoles).
		if role != pki.RoleAdmin && role != pki.RoleReader {
			return nil, status.Errorf(codes.InvalidArgument, "unknown role %q (known roles: %q, %q)", role, pki.RoleAdmin, pki.RoleReader)
		}
	}

	certPEM, keyPEM, err := s.CA.Issue(pki.IssueOptions{
		CommonName:  "client",
		Roles:       roles,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "issue client certificate: %v", err)
	}

	return &janusv1alpha1.GenerateClientConfigurationResponse{
		Ca:  s.CA.CertPEM,
		Crt: certPEM,
		Key: keyPEM,
	}, nil
}
