package api

import haproxyosv1alpha1 "github.com/swenske/HAProxyOS/gen/haproxyos/v1alpha1"

// HAProxy implements haproxyosv1alpha1.HAProxyServiceServer. Not
// implemented yet - see roadmap Phase 2 in docs/architecture.md.
type HAProxy struct {
	haproxyosv1alpha1.UnimplementedHAProxyServiceServer
}
