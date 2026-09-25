package api

import janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"

// Network implements janusv1alpha1.NetworkServiceServer. Not
// implemented yet - see roadmap Phase 5 (optional bird/keepalived/nftables
// modules) in docs/architecture.md.
type Network struct {
	janusv1alpha1.UnimplementedNetworkServiceServer
}
