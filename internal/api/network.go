package api

import haproxyosv1alpha1 "github.com/swenske/HAProxyOS/gen/haproxyos/v1alpha1"

// Network implements haproxyosv1alpha1.NetworkServiceServer. Not
// implemented yet - see roadmap Phase 5 (optional bird/keepalived/nftables
// modules) in docs/architecture.md.
type Network struct {
	haproxyosv1alpha1.UnimplementedNetworkServiceServer
}
