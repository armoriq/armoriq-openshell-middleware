package openshell

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/armoriq/armoriq-openshell-middleware/pkg/iapclient"
)

func cfg(t *testing.T, kv map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(kv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// enforceSpy stands up a control plane that allows everything and records what
// it was asked.
func enforceSpy(t *testing.T) (*httptest.Server, *iapclient.Request) {
	t.Helper()
	seen := &iapclient.Request{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(seen)
		_ = json.NewEncoder(w).Encode(map[string]any{"allowed": true, "enforcementAction": "allow"})
	}))
	t.Cleanup(srv.Close)
	return srv, seen
}

// The sandbox policy is where identity comes from, so what it declares has to
// be what the control plane is asked about.
func TestPolicyDeclaredIdentityIsWhatWeAskAbout(t *testing.T) {
	srv, seen := enforceSpy(t)
	v, err := NewIntentVerifier(iapclient.New(srv.URL, "k", time.Second),
		StaticIdentity{AgentID: "flag-default"}, DenyUnnamed, 0)
	if err != nil {
		t.Fatal(err)
	}

	req := mcpCall("github_create_issue")
	req.Config = cfg(t, map[string]any{
		ConfigAgentID:   "agent-from-policy",
		ConfigUserEmail: "requester@example.com",
	})

	got, err := v.Verify(context.Background(), req)
	if err != nil || !got.Allow {
		t.Fatalf("verify: %+v %v", got, err)
	}
	if seen.AgentID != "agent-from-policy" {
		t.Errorf("agent = %q, the policy's declaration must win over the flag", seen.AgentID)
	}
	if seen.UserEmail != "requester@example.com" {
		t.Errorf("user email = %q", seen.UserEmail)
	}
}

// Two sandboxes, same image, different policies. This is the case that makes
// per agent rules mean anything.
func TestTwoSandboxesGetTheirOwnAgent(t *testing.T) {
	srv, seen := enforceSpy(t)
	v, err := NewIntentVerifier(iapclient.New(srv.URL, "k", time.Second),
		StaticIdentity{}, DenyUnnamed, 0)
	if err != nil {
		t.Fatal(err)
	}

	for _, agent := range []string{"agent-a", "agent-b"} {
		req := mcpCall("github_create_issue")
		req.Config = cfg(t, map[string]any{ConfigAgentID: agent})
		if _, err := v.Verify(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		if seen.AgentID != agent {
			t.Errorf("asked about %q, want %q", seen.AgentID, agent)
		}
	}
}

// A policy that sets only the email still gets the agent from the fallback.
func TestIdentityFallsBackFieldByField(t *testing.T) {
	srv, seen := enforceSpy(t)
	v, err := NewIntentVerifier(iapclient.New(srv.URL, "k", time.Second),
		StaticIdentity{AgentID: "flag-agent", UserEmail: "flag@example.com"}, DenyUnnamed, 0)
	if err != nil {
		t.Fatal(err)
	}

	req := mcpCall("github_create_issue")
	req.Config = cfg(t, map[string]any{ConfigUserEmail: "policy@example.com"})

	if _, err := v.Verify(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if seen.AgentID != "flag-agent" || seen.UserEmail != "policy@example.com" {
		t.Errorf("agent=%q email=%q", seen.AgentID, seen.UserEmail)
	}
}

// With no agent anywhere the control plane would answer with whatever the org
// allows in general, which is not the rule meant to apply. Deny instead, and
// never make the call.
func TestNoAgentAnywhereIsADeny(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		_ = json.NewEncoder(w).Encode(map[string]any{"allowed": true, "enforcementAction": "allow"})
	}))
	defer srv.Close()

	v, err := NewIntentVerifier(iapclient.New(srv.URL, "k", time.Second),
		StaticIdentity{}, DenyUnnamed, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, verr := v.Verify(context.Background(), mcpCall("github_create_issue"))
	if verr != nil {
		t.Fatal(verr)
	}
	if got.Allow || got.Code != CodeNoPlan {
		t.Fatalf("want deny with %s, got %+v", CodeNoPlan, got)
	}
	if called {
		t.Error("asked the control plane about a sandbox with no agent")
	}
}

func TestIntentConfigSchema(t *testing.T) {
	if err := ValidateIntentConfig(cfg(t, map[string]any{
		ConfigAgentID: "a", ConfigUserEmail: "b@c.d",
	})); err != nil {
		t.Errorf("a valid config was refused: %v", err)
	}
	// No config at all is valid: the flag default still applies.
	if err := ValidateIntentConfig(nil); err != nil {
		t.Errorf("an empty config was refused: %v", err)
	}
	// The typo case. Accepting this would leave the sandbox with no identity
	// and nobody would find out until the wrong policy had been applied.
	err := ValidateIntentConfig(cfg(t, map[string]any{"agentid": "a"}))
	if err == nil {
		t.Fatal("a misspelled key was accepted")
	}
	if !strings.Contains(err.Error(), ConfigAgentID) {
		t.Errorf("the reason should say what the right key is: %q", err)
	}
	if err := ValidateIntentConfig(cfg(t, map[string]any{ConfigAgentID: 42.0})); err == nil {
		t.Error("a non-string agent id was accepted")
	}
}

// A rejected config has to come back as valid:false with a reason. Returning an
// error instead would be reported as a middleware fault rather than as the
// policy mistake it is.
func TestServiceReportsAConfigMistake(t *testing.T) {
	svc := New("armoriq-intent", "test", allowEverything{}, WithConfigValidator(ValidateIntentConfig))

	res, err := svc.ValidateConfig(context.Background(), &ValidateConfigRequest{
		Config: cfg(t, map[string]any{"agentid": "a"}),
	})
	if err != nil {
		t.Fatalf("validation must not return a transport error: %v", err)
	}
	if res.GetValid() {
		t.Fatal("a bad config was accepted")
	}
	if res.GetReason() == "" {
		t.Error("a rejection with no reason shows up as 'is invalid:' with nothing after it")
	}
}

type allowEverything struct{}

func (allowEverything) Verify(context.Context, *HttpRequestEvaluation) (Verdict, error) {
	return Verdict{Allow: true}, nil
}
