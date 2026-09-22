// Package api implements the HAProxyOS gRPC control-plane services
// declared in api/proto/haproxyos/v1alpha1. Every service is registered
// from day one (see cmd/haproxyosd); methods return codes.Unimplemented
// until their owning phase lands (see docs/architecture.md's roadmap).
package api

import (
	"context"
	"crypto/x509"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	haproxyosv1alpha1 "github.com/swenske/HAProxyOS/gen/haproxyos/v1alpha1"
	"github.com/swenske/HAProxyOS/internal/pki"
)

// System implements haproxyosv1alpha1.SystemServiceServer. Every method it
// doesn't override falls through to UnimplementedSystemServiceServer's
// generated codes.Unimplemented response.
type System struct {
	haproxyosv1alpha1.UnimplementedSystemServiceServer

	// BuildVersion is the daemon build version, set by cmd/haproxyosd
	// from build-time ldflags.
	BuildVersion string

	// CA issues the client certificates GenerateClientConfiguration hands
	// out. Reaching this RPC at all already required a valid client
	// certificate (mTLS is enforced on the whole listener, see
	// cmd/haproxyosd) - this is for rotating/reissuing credentials, not
	// bootstrapping the very first one (that comes from the admin
	// certificate LoadOrBootstrap prints on first boot).
	CA *pki.CA
}

func (s *System) Version(_ context.Context, _ *emptypb.Empty) (*haproxyosv1alpha1.VersionResponse, error) {
	return &haproxyosv1alpha1.VersionResponse{Version: s.BuildVersion}, nil
}

func (s *System) GenerateClientConfiguration(_ context.Context, req *haproxyosv1alpha1.GenerateClientConfigurationRequest) (*haproxyosv1alpha1.GenerateClientConfigurationResponse, error) {
	roles := req.GetRoles()
	if len(roles) == 0 {
		roles = []string{pki.RoleAdmin}
	}
	for _, role := range roles {
		// pki.RoleAdmin is the only role that exists so far (see
		// internal/pki's doc comment) - reject anything else rather than
		// silently issuing a certificate whose role nothing can act on.
		if role != pki.RoleAdmin {
			return nil, status.Errorf(codes.InvalidArgument, "unknown role %q (only %q exists so far)", role, pki.RoleAdmin)
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

	return &haproxyosv1alpha1.GenerateClientConfigurationResponse{
		Ca:  s.CA.CertPEM,
		Crt: certPEM,
		Key: keyPEM,
	}, nil
}
