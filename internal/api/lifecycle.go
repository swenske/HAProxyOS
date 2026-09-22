package api

import haproxyosv1alpha1 "github.com/swenske/HAProxyOS/gen/haproxyos/v1alpha1"

// Lifecycle implements haproxyosv1alpha1.LifecycleServiceServer. Not
// implemented yet - see roadmap Phase 3 (A/B immutability, dm-verity,
// secure boot) in docs/architecture.md.
type Lifecycle struct {
	haproxyosv1alpha1.UnimplementedLifecycleServiceServer
}
