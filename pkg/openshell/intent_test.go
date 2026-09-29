package openshell

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/armoriq/armoriq-openshell-middleware/pkg/iapclient"
)

// mcpCall is a tools/call the way an MCP client puts it on the wire, wrapped in
// the request shape OpenShell actually sends.
func mcpCall(tool string) *HttpRequestEvaluation {
	return &HttpRequestEvaluation{
		Context: &RequestContext{SandboxId: "sbx-1", RequestId: "req-1"},
		Target:  &HttpRequestTarget{Scheme: "https", Host: "mcp.internal", Method: "POST", Path: "/mcp"},
		Headers: []*HttpHeader{{Name: "content-type", Value: "application/json"}},
		Body: []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` +
			tool + `","arguments":{"repo":"armoriq/secrets"}}}`),
	}
}

// verifierAgainst stands up a fake control plane returning body, and returns a
// verifier pointed at it plus the last request it received.
func verifierAgainst(t *testing.T, status int, body any) (*IntentVerifier, *iapclient.Request) {
	t.Helper()
	seen := &iapclient.Request{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/iap/sdk/enforce" {
			t.Errorf("called %s, want /iap/sdk/enforce", r.URL.Path)
		}
		if got := r.Header.Get("authorization"); got != "Bearer k" {
			t.Errorf("authorization = %q", got)
		}
		_ = json.NewDecoder(r.Body).Decode(seen)
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)

	v, err := NewIntentVerifier(iapclient.New(srv.URL, "k", 2*time.Second),
		StaticIdentity{AgentID: "agent-7"}, DenyUnnamed, 0)
	if err != nil {
		t.Fatal(err)
	}
	return v, seen
}

func TestAllowedToolGoesThrough(t *testing.T) {
	v, seen := verifierAgainst(t, 200, map[string]any{
		"allowed": true, "enforcementAction": "allow",
		"matchedPolicy": map[string]any{"name": "github-write"},
	})

	got, err := v.Verify(context.Background(), mcpCall("github_create_issue"))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Allow {
		t.Fatalf("denied an allowed tool: %+v", got)
	}
	// The question we asked has to carry the tool, its arguments and the agent,
	// or the control plane is scoring a different call than the one being made.
	if seen.Tool != "github_create_issue" {
		t.Errorf("asked about tool %q", seen.Tool)
	}
	if seen.Arguments["repo"] != "armoriq/secrets" {
		t.Errorf("arguments not forwarded: %v", seen.Arguments)
	}
	if seen.AgentID != "agent-7" {
		t.Errorf("agent id not forwarded: %q", seen.AgentID)
	}
}

func TestBlockedToolIsDeniedWithItsOwnCode(t *testing.T) {
	v, _ := verifierAgainst(t, 200, map[string]any{
		"allowed": false, "enforcementAction": "block", "reason": "writes need approval",
		"matchedPolicy": map[string]any{"name": "no-writes"},
	})

	got, err := v.Verify(context.Background(), mcpCall("github_delete_repo"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Allow || got.Code != CodeBlocked {
		t.Fatalf("want deny with %s, got %+v", CodeBlocked, got)
	}
	if !strings.Contains(got.Reason, "no-writes") {
		t.Errorf("reason should name the policy for our own logs: %q", got.Reason)
	}
}

func TestHoldDoesNotLetTheCallProceed(t *testing.T) {
	v, _ := verifierAgainst(t, 200, map[string]any{
		"allowed": false, "enforcementAction": "hold", "requiresApproval": true,
		"delegationContext": map[string]any{"planId": "plan-9"},
	})

	got, err := v.Verify(context.Background(), mcpCall("wire_transfer"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Allow || got.Code != CodeHold {
		t.Fatalf("a hold must not proceed, got %+v", got)
	}
}

// Their normalizer derives allowed strictly from the action so this pairing
// cannot happen. If it ever does, it is the silent-bypass bug their contract
// exists to prevent, and we stop rather than take the allowed field's word.
func TestAllowedTrueWithAGateActionIsRefused(t *testing.T) {
	for _, action := range []string{"hold", "block", "step_up", ""} {
		v, _ := verifierAgainst(t, 200, map[string]any{
			"allowed": true, "enforcementAction": action,
		})
		got, err := v.Verify(context.Background(), mcpCall("wire_transfer"))
		if err != nil {
			t.Fatalf("action %q: %v", action, err)
		}
		if got.Allow {
			t.Errorf("action %q with allowed:true was let through", action)
		}
	}
}

// An action outside the terminal set means their contract moved. Fail closed
// and say which one it was.
func TestUnknownActionFailsClosed(t *testing.T) {
	v, _ := verifierAgainst(t, 200, map[string]any{
		"allowed": false, "enforcementAction": "escalate_to_council",
	})
	got, err := v.Verify(context.Background(), mcpCall("wire_transfer"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Allow || got.Code != CodeUnknownDecision {
		t.Fatalf("want %s, got %+v", CodeUnknownDecision, got)
	}
}

// A control plane we cannot reach must not become an allow. Returning an error
// hands the service a deny of our own rather than leaving it to on_error, which
// an operator may have set to fail_open.
func TestControlPlaneFailureIsNotAnAllow(t *testing.T) {
	v, _ := verifierAgainst(t, 500, map[string]any{"error": "boom"})
	got, err := v.Verify(context.Background(), mcpCall("github_create_issue"))
	if err == nil {
		t.Fatalf("a 500 must not produce a verdict, got %+v", got)
	}
	if got.Allow {
		t.Error("a failed call came back as allow")
	}
}

func TestRejectedApiKeyIsNotAnAllow(t *testing.T) {
	v, _ := verifierAgainst(t, 401, map[string]any{"message": "Invalid API key"})
	got, err := v.Verify(context.Background(), mcpCall("github_create_issue"))
	if err == nil || got.Allow {
		t.Fatalf("a rejected key must fail closed, got %+v err=%v", got, err)
	}
}

// A cancelled context is OpenShell's timeout arriving. It must not be an allow,
// and it must not be swallowed.
func TestTimeoutIsNotAnAllow(t *testing.T) {
	// The handler needs its own way out. Closing the server waits for it, and
	// the client giving up does not by itself end the handler.
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer func() { close(release); srv.Close() }()

	v, err := NewIntentVerifier(iapclient.New(srv.URL, "k", 50*time.Millisecond),
		StaticIdentity{AgentID: "agent-7"}, DenyUnnamed, 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()

	got, verr := v.Verify(ctx, mcpCall("github_create_issue"))
	if verr == nil || got.Allow {
		t.Fatalf("a timeout must fail closed, got %+v err=%v", got, verr)
	}
}

// Traffic naming no tool never reaches the control plane: the endpoint requires
// a tool and rejects an empty one. Both configured answers are exercised here
// because the choice between them is still open.
func TestUnnamedTrafficFollowsTheConfiguredAnswer(t *testing.T) {
	plainGet := &HttpRequestEvaluation{
		Context: &RequestContext{SandboxId: "sbx-1"},
		Target:  &HttpRequestTarget{Scheme: "https", Host: "api.github.com", Method: "GET", Path: "/zen"},
	}

	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(400)
	}))
	defer srv.Close()

	for _, tc := range []struct {
		mode  UnnamedAction
		allow bool
	}{{DenyUnnamed, false}, {AllowUnnamed, true}} {
		v, err := NewIntentVerifier(iapclient.New(srv.URL, "k", time.Second),
			StaticIdentity{AgentID: "agent-7"}, tc.mode, 0)
		if err != nil {
			t.Fatal(err)
		}
		got, verr := v.Verify(context.Background(), plainGet)
		if verr != nil {
			t.Fatal(verr)
		}
		if got.Allow != tc.allow {
			t.Errorf("mode %v: allow = %v, want %v", tc.mode, got.Allow, tc.allow)
		}
		if !tc.allow && got.Code != CodeUnnamed {
			t.Errorf("mode %v: code = %q, want %s", tc.mode, got.Code, CodeUnnamed)
		}
	}
	if called {
		t.Error("unnamed traffic was sent to enforce, which rejects an empty tool")
	}
}

// Every code we can put on the wire has to satisfy OpenShell's rule, or the
// whole result is a middleware failure and our deny becomes whatever on_error
// says.
func TestOurCodesAreAcceptableToOpenshell(t *testing.T) {
	for _, c := range []string{CodeBlocked, CodeHold, CodeUnknownDecision, CodeUnnamed,
		CodeIntentDenied, CodeNoPlan, CodeVerifierError} {
		if !reasonCodePattern.MatchString(c) {
			t.Errorf("%q would be rejected by OpenShell", c)
		}
	}
}
