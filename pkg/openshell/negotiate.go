package openshell

import (
	"fmt"
	"slices"
)

// OpenShell's extension protocol, from openshell-core's extension_protocol.rs.
// A gateway on another major version cannot safely talk to us, and the contract
// capability is required in both directions.
const (
	ProtocolMajor      = 1
	ProtocolMinor      = 0
	ContractCapability = "openshell.supervisor-middleware.contract"
)

// ImplementationName is how the gateway reports us in `openshell gateway info`.
const ImplementationName = "armoriq/openshell-middleware"

// peerMetadata is what we advertise in the manifest. A gateway that negotiates
// rejects a manifest without it and then refuses to start.
func peerMetadata(version string) *PeerMetadata {
	return &PeerMetadata{
		ProtocolVersion:       &ProtocolVersion{Major: ProtocolMajor, Minor: ProtocolMinor},
		ImplementationName:    ImplementationName,
		ImplementationVersion: version,
		SupportedCapabilities: []string{ContractCapability},
		RequiredCapabilities:  []string{ContractCapability},
	}
}

// checkGateway refuses a gateway we cannot serve correctly. Their contract asks
// middleware to reject unmet requirements before accepting traffic, and Describe
// is the only call made before traffic.
//
// A gateway that sends no metadata predates negotiation. It also never reads
// ours, so it is served as before rather than refused.
func checkGateway(gw *PeerMetadata) error {
	if gw == nil {
		return nil
	}
	v := gw.GetProtocolVersion()
	if v == nil {
		return fmt.Errorf("gateway %q sent no protocol version", gw.GetImplementationName())
	}
	if v.GetMajor() != ProtocolMajor {
		return fmt.Errorf("gateway %q speaks middleware protocol %d.%d, this service speaks %d.%d",
			gw.GetImplementationName(), v.GetMajor(), v.GetMinor(), ProtocolMajor, ProtocolMinor)
	}
	if !slices.Contains(gw.GetSupportedCapabilities(), ContractCapability) {
		return fmt.Errorf("gateway %q does not support %s", gw.GetImplementationName(), ContractCapability)
	}
	for _, c := range gw.GetRequiredCapabilities() {
		if c != ContractCapability {
			return fmt.Errorf("gateway %q requires %s, which this service does not implement",
				gw.GetImplementationName(), c)
		}
	}
	return nil
}
