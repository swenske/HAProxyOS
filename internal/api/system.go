// Package api implements the HAProxyOS gRPC control-plane services
// declared in api/proto/haproxyos/v1alpha1. Every service is registered
// from day one (see cmd/haproxyosd); methods return codes.Unimplemented
// until their owning phase lands (see docs/architecture.md's roadmap).
package api

import (
	"context"

	"google.golang.org/protobuf/types/known/emptypb"

	haproxyosv1alpha1 "github.com/swenske/HAProxyOS/gen/haproxyos/v1alpha1"
)

// System implements haproxyosv1alpha1.SystemServiceServer. Every method it
// doesn't override falls through to UnimplementedSystemServiceServer's
// generated codes.Unimplemented response - only Version is implemented in
// Phase 0, as an end-to-end connectivity check.
type System struct {
	haproxyosv1alpha1.UnimplementedSystemServiceServer

	// BuildVersion is the daemon build version, set by cmd/haproxyosd
	// from build-time ldflags.
	BuildVersion string
}

func (s *System) Version(_ context.Context, _ *emptypb.Empty) (*haproxyosv1alpha1.VersionResponse, error) {
	return &haproxyosv1alpha1.VersionResponse{Version: s.BuildVersion}, nil
}
