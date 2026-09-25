package pki

import "net"

// LocalIPs returns every non-loopback unicast IP address currently
// assigned to a network interface - meant to be added as extra SANs on
// a self-signed server certificate alongside 127.0.0.1/localhost, so a
// client dialing over the host's real address (not just loopback) can
// verify the handshake. Two real consumers as of this writing:
// cmd/janusd (the node's own server cert, keyed to its real LAN
// IP - found missing when a dashboard add-node call over a node's real
// IP failed TLS verification outright) and dashboard/backend (the
// dashboard's own per-node-listener identity, keyed to whatever host a
// browser reaches it through - localhost most commonly, but not
// exclusively).
//
// Best-effort only: an error here (no network yet, a container with no
// real NICs) just means an empty list, never a fatal error - the caller
// still has 127.0.0.1/localhost to fall back on.
func LocalIPs() []net.IP {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var ips []net.IP
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() {
			continue
		}
		ips = append(ips, ipNet.IP)
	}
	return ips
}
