package openshell

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/emptypb"
)

type stubVerifier struct {
	v   Verdict
	err error
}

func (s stubVerifier) Verify(context.Context, *HttpRequestEvaluation) (Verdict, error) {
	return s.v, s.err
}

func eval(sandbox string) *HttpRequestEvaluation {
	return &HttpRequestEvaluation{
		Context: &RequestContext{SandboxId: sandbox, RequestId: "req-1"},
		Target:  &HttpRequestTarget{Scheme: "https", Host: "api.example.com", Method: "GET", Path: "/v1/pods"},
	}
}

func TestDescribeDeclaresHttpRequestAtPreCredentials(t *testing.T) {
	s := New("armoriq-intent", "0.1.0", stubVerifier{v: Verdict{Allow: true}})
	m, err := s.Describe(context.Background(), &emptypb.Empty{})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if len(m.GetBindings()) != 1 {
		t.Fatalf("want 1 binding, got %d", len(m.GetBindings()))
	}
	b := m.GetBindings()[0]
	if b.GetOperation() != SupervisorMiddlewareOperation_SUPERVISOR_MIDDLEWARE_OPERATION_HTTP_REQUEST {
		t.Errorf("operation = %v", b.GetOperation())
	}
	if b.GetPhase() != SupervisorMiddlewarePhase_SUPERVISOR_MIDDLEWARE_PHASE_PRE_CREDENTIALS {
		t.Errorf("phase = %v", b.GetPhase())
	}
	// A binding may shorten the operator timeout but never extend it, so
	// declaring at or above their 500ms default buys nothing.
	if got := b.GetRequestTimeout().AsDuration().Milliseconds(); got > 500 {
		t.Errorf("declared timeout %dms exceeds the platform default", got)
	}
}

func TestAllowPasses(t *testing.T) {
	s := New("n", "v", stubVerifier{v: Verdict{Allow: true}})
	r, _ := s.EvaluateHttpRequest(context.Background(), eval("sbx-1"))
	if r.GetDecision() != Decision_DECISION_ALLOW {
		t.Fatalf("decision = %v", r.GetDecision())
	}
}

func TestDenyCarriesAStableCode(t *testing.T) {
	s := New("n", "v", stubVerifier{v: Verdict{Allow: false, Reason: "not in plan"}})
	r, _ := s.EvaluateHttpRequest(context.Background(), eval("sbx-1"))
	if r.GetDecision() != Decision_DECISION_DENY {
		t.Fatalf("decision = %v", r.GetDecision())
	}
	if r.GetReasonCode() != CodeIntentDenied {
		t.Errorf("reason_code = %q", r.GetReasonCode())
	}
}

// A verifier failure must be our deny, not a transport error. An error would be
// a middleware failure governed by on_error, which an operator may set to
// fail_open, and an unverifiable request would then be forwarded.
func TestVerifierErrorDeniesRatherThanErroring(t *testing.T) {
	s := New("n", "v", stubVerifier{err: errors.New("upstream down")})
	r, err := s.EvaluateHttpRequest(context.Background(), eval("sbx-1"))
	if err != nil {
		t.Fatalf("returned a transport error: %v", err)
	}
	if r.GetDecision() != Decision_DECISION_DENY || r.GetReasonCode() != CodeVerifierError {
		t.Fatalf("decision=%v code=%q", r.GetDecision(), r.GetReasonCode())
	}
}

func TestMissingSandboxIdIsDenied(t *testing.T) {
	s := New("n", "v", stubVerifier{v: Verdict{Allow: true}})
	for name, in := range map[string]*HttpRequestEvaluation{
		"nil request": nil,
		"no context":  {},
		"empty id":    {Context: &RequestContext{}},
	} {
		r, _ := s.EvaluateHttpRequest(context.Background(), in)
		if r.GetDecision() != Decision_DECISION_DENY || r.GetReasonCode() != CodeNoPlan {
			t.Errorf("%s: decision=%v code=%q", name, r.GetDecision(), r.GetReasonCode())
		}
	}
}

// OpenShell rejects a malformed reason_code by turning the whole result into a
// middleware failure. A verifier must not be able to cause that.
func TestMalformedVerifierCodeFallsBack(t *testing.T) {
	for _, bad := range []string{"Intent Denied", "9lives", "", "has-hyphen", strings.Repeat("a", 65)} {
		s := New("n", "v", stubVerifier{v: Verdict{Allow: false, Code: bad}})
		r, _ := s.EvaluateHttpRequest(context.Background(), eval("sbx-1"))
		if r.GetReasonCode() != CodeIntentDenied {
			t.Errorf("code %q was passed through as %q", bad, r.GetReasonCode())
		}
	}
}

func TestOurOwnCodesAreValid(t *testing.T) {
	for _, c := range []string{CodeIntentDenied, CodeNoPlan, CodeVerifierError} {
		if !reasonCodePattern.MatchString(c) {
			t.Errorf("%q would be rejected by OpenShell", c)
		}
	}
}

// Valid is a bool, so an empty response is a rejection with no reason. Their
// gateway then refuses the policy with "middleware config '<name>' is invalid:"
// and nothing after it, which is a genuinely hard failure to read.
func TestValidateConfigAccepts(t *testing.T) {
	s := New("n", "v", stubVerifier{v: Verdict{Allow: true}})
	r, err := s.ValidateConfig(context.Background(), &ValidateConfigRequest{MiddlewareName: "armoriq-intent"})
	if err != nil {
		t.Fatalf("ValidateConfig: %v", err)
	}
	if !r.GetValid() {
		t.Fatalf("config rejected, reason=%q", r.GetReason())
	}
}
