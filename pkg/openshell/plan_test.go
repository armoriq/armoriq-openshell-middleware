package openshell

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/armoriq/armoriq-openshell-middleware/pkg/iapclient"
)

// controlPlane is a fake that mints tokens and allows every enforce call, and
// records what it was asked.
type controlPlane struct {
	mu        sync.Mutex
	mints     int
	enforces  []iapclient.Request
	tokenErr  bool
	mintDelay time.Duration
	srv       *httptest.Server
}

func newControlPlane(t *testing.T) *controlPlane {
	t.Helper()
	c := &controlPlane{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/iap/sdk/token":
			c.mu.Lock()
			c.mints++
			fail := c.tokenErr
			delay := c.mintDelay
			n := c.mints
			c.mu.Unlock()
			if delay > 0 {
				time.Sleep(delay)
			}
			if fail {
				w.WriteHeader(500)
				_, _ = w.Write([]byte(`{"message":"minting is down"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success":          true,
				"plan_id":          "11111111-1111-1111-1111-00000000000" + string(rune('0'+n%10)),
				"plan_hash":        "hash-of-the-plan",
				"intent_reference": "ref-1",
				"token":            map[string]any{"signature": "sig"},
			})
		case "/iap/sdk/enforce":
			var got iapclient.Request
			_ = json.NewDecoder(r.Body).Decode(&got)
			c.mu.Lock()
			c.enforces = append(c.enforces, got)
			c.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"allowed": true, "enforcementAction": "allow",
			})
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *controlPlane) seen() (int, []iapclient.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mints, append([]iapclient.Request(nil), c.enforces...)
}

func planVerifier(t *testing.T, c *controlPlane) *IntentVerifier {
	t.Helper()
	v, err := NewIntentVerifier(iapclient.New(c.srv.URL, "k", 3*time.Second),
		StaticIdentity{}, DenyUnnamed, 0)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func withPlan(tool string, agent string, declared []string) *HttpRequestEvaluation {
	req := mcpCall(tool)
	fields := map[string]any{ConfigAgentID: agent}
	if declared != nil {
		list := make([]any, len(declared))
		for i, d := range declared {
			list[i] = d
		}
		fields[ConfigDeclaredTools] = list
	}
	req.Config, _ = structpb.NewStruct(fields)
	return req
}

// The case the whole claim rests on. A tool the sandbox never declared is drift,
// and it is refused without asking a policy, because "may this agent ever do
// this" is a different question from "did this sandbox say it would".
func TestDriftIsRefusedWithoutAskingPolicy(t *testing.T) {
	cp := newControlPlane(t)
	v := planVerifier(t, cp)

	got, err := v.Verify(context.Background(),
		withPlan("github_delete_repo", "agent-7", []string{"github_list_issues", "github_create_issue"}))
	if err != nil {
		t.Fatal(err)
	}
	if got.Allow || got.Code != CodeNotInPlan {
		t.Fatalf("want deny with %s, got %+v", CodeNotInPlan, got)
	}
	if !strings.Contains(got.Reason, "github_list_issues") {
		t.Errorf("the reason should say what was declared: %q", got.Reason)
	}
	mints, enforces := cp.seen()
	if len(enforces) != 0 {
		t.Errorf("drift was sent to the policy check: %+v", enforces)
	}
	if mints != 0 {
		t.Errorf("drift minted a token, which is a round trip for a decision already made")
	}
}

// Minting is slow because the control plane builds proofs. It must not sit on
// the request path: doing so spends the binding budget before the decision is
// even asked for, and OpenShell reports that as a middleware timeout.
func TestTheFirstCallIsNotBlockedByMinting(t *testing.T) {
	cp := newControlPlane(t)
	cp.mintDelay = 2 * time.Second
	v := planVerifier(t, cp)

	start := time.Now()
	got, err := v.Verify(context.Background(),
		withPlan("github_create_issue", "agent-7", []string{"github_create_issue"}))
	took := time.Since(start)

	if err != nil || !got.Allow {
		t.Fatalf("verify: %+v %v", got, err)
	}
	if took > time.Second {
		t.Errorf("the call waited %s for a token, which would blow the binding budget", took)
	}
	_, enforces := cp.seen()
	if len(enforces) != 1 {
		t.Fatalf("got %d enforce calls, want 1", len(enforces))
	}
	if enforces[0].IntentToken != nil {
		t.Error("the first call carried a token that could not have been minted yet")
	}
}

// Once the mint lands, the token goes with every subsequent call, which is what
// gives the control plane a plan id to attach an approval to.
func TestTheTokenIsUsedOnceItIsReady(t *testing.T) {
	cp := newControlPlane(t)
	v := planVerifier(t, cp)
	req := func() *HttpRequestEvaluation {
		return withPlan("github_create_issue", "agent-7", []string{"github_create_issue"})
	}

	if _, err := v.Verify(context.Background(), req()); err != nil {
		t.Fatal(err)
	}
	waitForMint(t, cp, 1)

	if _, err := v.Verify(context.Background(), req()); err != nil {
		t.Fatal(err)
	}
	_, enforces := cp.seen()
	last := enforces[len(enforces)-1].IntentToken
	if last == nil {
		t.Fatal("a later call still carried no token")
	}
	if last["plan_id"] == nil || last["plan_id"] == "" {
		t.Errorf("token carried no plan_id: %v", last)
	}
	if last["plan"] == nil {
		t.Errorf("token carried no plan: %v", last)
	}
}

// A burst from one sandbox must produce one mint, not one per request.
func TestABurstMintsOnce(t *testing.T) {
	cp := newControlPlane(t)
	cp.mintDelay = 300 * time.Millisecond
	v := planVerifier(t, cp)

	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = v.Verify(context.Background(),
				withPlan("github_create_issue", "agent-7", []string{"github_create_issue"}))
		}()
	}
	wg.Wait()
	waitForMint(t, cp, 1)
	time.Sleep(200 * time.Millisecond)

	if mints, _ := cp.seen(); mints != 1 {
		t.Errorf("a burst of 12 produced %d mints, want 1", mints)
	}
}

// Changing the declared plan on a live sandbox has to mint again, or the next
// task would be enforced against the previous task's plan id.
func TestANewPlanMintsANewToken(t *testing.T) {
	cp := newControlPlane(t)
	v := planVerifier(t, cp)

	if _, err := v.Verify(context.Background(),
		withPlan("github_create_issue", "agent-7", []string{"github_create_issue"})); err != nil {
		t.Fatal(err)
	}
	waitForMint(t, cp, 1)

	if _, err := v.Verify(context.Background(),
		withPlan("github_create_issue", "agent-7", []string{"github_create_issue", "github_comment"})); err != nil {
		t.Fatal(err)
	}
	waitForMint(t, cp, 2)

	if mints, _ := cp.seen(); mints != 2 {
		t.Errorf("a changed plan produced %d mints, want 2", mints)
	}
}

// Minting failing must never deny. The plan check has already run locally and
// the policy check still applies, so only approval attribution is lost.
func TestAMintFailureNeverDenies(t *testing.T) {
	cp := newControlPlane(t)
	cp.tokenErr = true
	v := planVerifier(t, cp)

	for i := 0; i < 3; i++ {
		got, err := v.Verify(context.Background(),
			withPlan("github_create_issue", "agent-7", []string{"github_create_issue"}))
		if err != nil {
			t.Fatal(err)
		}
		if !got.Allow {
			t.Fatalf("a mint failure denied a call the policy allows: %+v", got)
		}
		time.Sleep(50 * time.Millisecond)
	}
	_, enforces := cp.seen()
	if len(enforces) != 3 {
		t.Errorf("policy was consulted %d times, want 3", len(enforces))
	}
}

// A policy that declares no plan keeps the old behaviour: policy only, no mint.
func TestNoDeclaredPlanMeansPolicyOnly(t *testing.T) {
	cp := newControlPlane(t)
	v := planVerifier(t, cp)

	got, err := v.Verify(context.Background(), withPlan("anything_at_all", "agent-7", nil))
	if err != nil || !got.Allow {
		t.Fatalf("verify: %+v %v", got, err)
	}
	time.Sleep(100 * time.Millisecond)
	mints, enforces := cp.seen()
	if mints != 0 {
		t.Errorf("minted a token with no plan declared")
	}
	if len(enforces) != 1 || enforces[0].IntentToken != nil {
		t.Errorf("expected one enforce call with no token, got %+v", enforces)
	}
}

// waitForMint blocks until the fake control plane has minted n tokens.
func waitForMint(t *testing.T, cp *controlPlane, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if mints, _ := cp.seen(); mints >= n {
			// The put happens just after the response, so let it land.
			time.Sleep(30 * time.Millisecond)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d mints", n)
}

func TestDeclaredToolsSchema(t *testing.T) {
	ok, _ := structpb.NewStruct(map[string]any{
		ConfigAgentID: "a", ConfigDeclaredTools: []any{"t1", "t2"},
	})
	if err := ValidateIntentConfig(ok); err != nil {
		t.Errorf("a valid plan was refused: %v", err)
	}
	notList, _ := structpb.NewStruct(map[string]any{ConfigDeclaredTools: "t1"})
	if ValidateIntentConfig(notList) == nil {
		t.Error("a string was accepted where a list belongs")
	}
	notStrings, _ := structpb.NewStruct(map[string]any{ConfigDeclaredTools: []any{1.0}})
	if ValidateIntentConfig(notStrings) == nil {
		t.Error("a non-string tool name was accepted")
	}
}
