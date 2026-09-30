package openshell

import (
	"context"
	"slices"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func gateway(major uint32, supported, required []string) *MiddlewareDescribeRequest {
	return &MiddlewareDescribeRequest{Gateway: &PeerMetadata{
		ProtocolVersion:       &ProtocolVersion{Major: major},
		ImplementationName:    "openshell/gateway",
		SupportedCapabilities: supported,
		RequiredCapabilities:  required,
	}}
}

func TestManifestCarriesTheMetadataTheGatewayRequires(t *testing.T) {
	m, err := New("x", "0.2.0", allowVerifier{}).Describe(context.Background(),
		gateway(1, []string{ContractCapability}, []string{ContractCapability}))
	if err != nil {
		t.Fatal(err)
	}
	ext := m.GetExtension()
	if ext.GetProtocolVersion().GetMajor() != ProtocolMajor {
		t.Errorf("protocol major %d, want %d", ext.GetProtocolVersion().GetMajor(), ProtocolMajor)
	}
	// The gateway requires the contract capability, and refuses to start when
	// we do not advertise it.
	if !slices.Contains(ext.GetSupportedCapabilities(), ContractCapability) {
		t.Errorf("supported %v, missing %s", ext.GetSupportedCapabilities(), ContractCapability)
	}
	if ext.GetImplementationName() == "" || ext.GetImplementationVersion() != "0.2.0" {
		t.Errorf("implementation %q %q", ext.GetImplementationName(), ext.GetImplementationVersion())
	}
}

func TestGatewaysWeCannotServeAreRefusedAtDescribe(t *testing.T) {
	for name, req := range map[string]*MiddlewareDescribeRequest{
		"other major":         gateway(2, []string{ContractCapability}, nil),
		"no contract":         gateway(1, nil, nil),
		"unknown requirement": gateway(1, []string{ContractCapability}, []string{ContractCapability, "openshell.supervisor-middleware.future"}),
		"no version at all":   {Gateway: &PeerMetadata{ImplementationName: "openshell/gateway"}},
	} {
		_, err := New("x", "v", allowVerifier{}).Describe(context.Background(), req)
		if status.Code(err) != codes.FailedPrecondition {
			t.Errorf("%s: got %v, want FailedPrecondition", name, err)
		}
	}
}

func TestGatewayThatPredatesNegotiationIsStillServed(t *testing.T) {
	if _, err := New("x", "v", allowVerifier{}).Describe(context.Background(), &MiddlewareDescribeRequest{}); err != nil {
		t.Fatalf("older gateway refused: %v", err)
	}
}
