package openshell

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/structpb"
)

const openaiToolCall = `{"object":"chat.completion","choices":[{"message":{"role":"assistant",
	"tool_calls":[{"type":"function","function":{"name":"github_list_issues","arguments":"{}"}}]}}]}`

const openaiTextOnly = `{"object":"chat.completion","choices":[{"message":{"role":"assistant",
	"content":"There are three open issues."}}]}`

const anthropicToolUse = `{"type":"message","role":"assistant","content":[
	{"type":"text","text":"Listing them."},
	{"type":"tool_use","id":"t1","name":"github_list_issues","input":{}}]}`

// mcpResult is what the tool itself sends back. It passes through the same
// response hook as the model's reply.
const mcpResult = `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"issue #4"}]}}`

func TestModelReplyReadsBothProviders(t *testing.T) {
	for name, body := range map[string]string{"openai": openaiToolCall, "anthropic": anthropicToolUse} {
		tools, ok := ModelReply([]byte(body))
		if !ok || len(tools) != 1 || tools[0] != "github_list_issues" {
			t.Errorf("%s: got %v, %v", name, tools, ok)
		}
	}
}

func TestToolResultIsNotMistakenForAReply(t *testing.T) {
	// If this reads as a reply that asked for nothing, the plan is cleared the
	// moment the agent makes its first call, and the next legitimate call is
	// refused as drift.
	if tools, ok := ModelReply([]byte(mcpResult)); ok {
		t.Fatalf("tool result read as a model reply asking for %v", tools)
	}
}

func TestTextOnlyReplyIsAnEmptyPlan(t *testing.T) {
	tools, ok := ModelReply([]byte(openaiTextOnly))
	if !ok || len(tools) != 0 {
		t.Fatalf("got %v, %v, want an empty plan", tools, ok)
	}
}

func TestCallTheModelNeverAskedForIsDrift(t *testing.T) {
	v, seen := verifierAgainst(t, 200, map[string]any{"allowed": true, "enforcementAction": "allow"})
	c := NewPlanCapture()
	v.WithCapture(c)
	c.Record("sbx-1", []string{"github_list_issues"})

	got, err := v.Verify(context.Background(), mcpCall("github_create_issue"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Allow || got.Code != CodeDrift {
		t.Fatalf("got %+v, want a %s denial", got, CodeDrift)
	}
	// Policy would have said yes. Drift is decided before it is asked.
	if seen.Tool != "" {
		t.Errorf("asked the control plane about %q, drift should never leave the process", seen.Tool)
	}
}

func TestCallInThePlanStillGoesToPolicy(t *testing.T) {
	v, seen := verifierAgainst(t, 200, map[string]any{"allowed": true, "enforcementAction": "allow"})
	c := NewPlanCapture()
	v.WithCapture(c)
	c.Record("sbx-1", []string{"github_create_issue"})

	got, err := v.Verify(context.Background(), mcpCall("github_create_issue"))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Allow {
		t.Fatalf("denied a call the model asked for: %+v", got)
	}
	if seen.Tool != "github_create_issue" {
		t.Errorf("being in the plan must not skip policy, control plane saw %q", seen.Tool)
	}
}

func TestCallAfterTheModelFinishedIsDrift(t *testing.T) {
	v, _ := verifierAgainst(t, 200, map[string]any{"allowed": true, "enforcementAction": "allow"})
	c := NewPlanCapture()
	v.WithCapture(c)
	c.Record("sbx-1", nil)

	got, _ := v.Verify(context.Background(), mcpCall("github_list_issues"))
	if got.Allow || got.Code != CodeDrift {
		t.Fatalf("got %+v after a text-only reply, want %s", got, CodeDrift)
	}
}

func TestNoPlanYetFallsThroughUnlessRequired(t *testing.T) {
	v, _ := verifierAgainst(t, 200, map[string]any{"allowed": true, "enforcementAction": "allow"})
	v.WithCapture(NewPlanCapture())

	got, _ := v.Verify(context.Background(), mcpCall("github_list_issues"))
	if !got.Allow {
		t.Fatalf("no plan captured and none required, want policy to decide, got %+v", got)
	}

	req := mcpCall("github_list_issues")
	req.Config, _ = structpb.NewStruct(map[string]any{ConfigRequireCapturedPlan: true})
	got, _ = v.Verify(context.Background(), req)
	if got.Allow || got.Code != CodeDrift {
		t.Fatalf("got %+v with %s set, want %s", got, ConfigRequireCapturedPlan, CodeDrift)
	}
}

func TestPlanExpires(t *testing.T) {
	c := NewPlanCapture()
	now := time.Now()
	c.now = func() time.Time { return now }
	c.Record("sbx-1", []string{"github_list_issues"})

	now = now.Add(capturedPlanTTL + time.Second)
	if _, ok := c.Lookup("sbx-1"); ok {
		t.Fatal("expired plan still held")
	}
}

func TestPlansAreScopedToTheirSandbox(t *testing.T) {
	c := NewPlanCapture()
	c.Record("sbx-1", []string{"github_list_issues"})
	if _, ok := c.Lookup("sbx-2"); ok {
		t.Fatal("one sandbox's plan governed another")
	}
}

func TestConfigAcceptsRequireCapturedPlan(t *testing.T) {
	ok, _ := structpb.NewStruct(map[string]any{ConfigRequireCapturedPlan: true})
	if err := ValidateIntentConfig(ok); err != nil {
		t.Errorf("rejected a bool: %v", err)
	}
	bad, _ := structpb.NewStruct(map[string]any{ConfigRequireCapturedPlan: "yes"})
	if err := ValidateIntentConfig(bad); err == nil {
		t.Error("accepted a string for a bool key")
	}
}

func TestResponseBindingIsOptIn(t *testing.T) {
	m, _ := New("x", "v", allowVerifier{}).Describe(context.Background(), &MiddlewareDescribeRequest{})
	if len(m.GetBindings()) != 1 {
		t.Fatalf("default manifest has %d bindings; stock OpenShell refuses to start on the response one", len(m.GetBindings()))
	}
	m, _ = New("x", "v", allowVerifier{}, WithResponseBinding()).Describe(context.Background(), &MiddlewareDescribeRequest{})
	if len(m.GetBindings()) != 2 {
		t.Fatalf("got %d bindings with the response binding enabled", len(m.GetBindings()))
	}
	b := m.GetBindings()[1]
	if b.GetOperation() != SupervisorMiddlewareOperation_SUPERVISOR_MIDDLEWARE_OPERATION_HTTP_RESPONSE ||
		b.GetPhase() != SupervisorMiddlewarePhase_SUPERVISOR_MIDDLEWARE_PHASE_PRE_RETURN {
		t.Errorf("second binding is %v/%v", b.GetOperation(), b.GetPhase())
	}
}

type allowVerifier struct{}

func (allowVerifier) Verify(context.Context, *HttpRequestEvaluation) (Verdict, error) {
	return Verdict{Allow: true}, nil
}

// responseStream opens a real bidi stream to a ResponseService, the way the
// supervisor does.
func responseStream(t *testing.T, c *PlanCapture) grpc.BidiStreamingClient[HttpResponseEvent, HttpResponseEventResult] {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	RegisterHttpResponsePreReturnServer(srv, NewResponseService(c))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	stream, err := NewHttpResponsePreReturnClient(conn).Evaluate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return stream
}

func modelHostConfig() *structpb.Struct {
	c, _ := structpb.NewStruct(map[string]any{ConfigModelHosts: []any{"model.internal"}})
	return c
}

func preflight(status uint32, contentType string, modes ...HttpResponseBodyMode) *HttpResponseEvent {
	return preflightFrom("model.internal", status, contentType, modes...)
}

func preflightFrom(host string, status uint32, contentType string, modes ...HttpResponseBodyMode) *HttpResponseEvent {
	return &HttpResponseEvent{Event: &HttpResponseEvent_Preflight{Preflight: &HttpResponsePreflight{
		Context:            &RequestContext{SandboxId: "sbx-1"},
		Config:             modelHostConfig(),
		Target:             &HttpRequestTarget{Host: host, Path: "/v1/chat/completions"},
		StatusCode:         status,
		Headers:            []*HttpHeader{{Name: "Content-Type", Value: contentType}},
		PermittedBodyModes: modes,
	}}}
}

func TestResponseStreamCapturesThePlanAndPassesThrough(t *testing.T) {
	c := NewPlanCapture()
	s := responseStream(t, c)

	if err := s.Send(preflight(200, "application/json",
		HttpResponseBodyMode_HTTP_RESPONSE_BODY_MODE_HEADERS_ONLY,
		HttpResponseBodyMode_HTTP_RESPONSE_BODY_MODE_WHOLE_BODY_BYTES)); err != nil {
		t.Fatal(err)
	}
	res, err := s.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if res.GetPreflightResult().GetInspect().GetBodyMode() != HttpResponseBodyMode_HTTP_RESPONSE_BODY_MODE_WHOLE_BODY_BYTES {
		t.Fatalf("preflight = %v, want inspect with the whole body", res.GetPreflightResult())
	}

	if err := s.Send(&HttpResponseEvent{Event: &HttpResponseEvent_Body{Body: &HttpResponseBodyUnit{
		Sequence: 1, Payload: &HttpResponseBodyUnit_Data{Data: []byte(openaiToolCall)}, EndOfStream: true,
	}}}); err != nil {
		t.Fatal(err)
	}
	res, err = s.Recv()
	if err != nil {
		t.Fatal(err)
	}
	body := res.GetBodyResult()
	if body.GetSequence() != 1 || body.GetPassThrough() == nil {
		t.Fatalf("body result = %v, want pass_through for sequence 1", body)
	}

	if err := s.Send(&HttpResponseEvent{Event: &HttpResponseEvent_Trailers{Trailers: &HttpResponseTrailers{}}}); err != nil {
		t.Fatal(err)
	}
	if res, err = s.Recv(); err != nil || res.GetTrailersResult() == nil {
		t.Fatalf("trailers result = %v, %v", res, err)
	}

	plan, ok := c.Lookup("sbx-1")
	if !ok || len(plan) != 1 || plan[0] != "github_list_issues" {
		t.Fatalf("captured %v, %v", plan, ok)
	}
}

func TestResponsesThatCannotBeAReplyAreSkipped(t *testing.T) {
	whole := HttpResponseBodyMode_HTTP_RESPONSE_BODY_MODE_WHOLE_BODY_BYTES
	for name, ev := range map[string]*HttpResponseEvent{
		"error status":  preflight(500, "application/json", whole),
		"not json":      preflight(200, "text/html", whole),
		"streamed":      preflight(200, "text/event-stream", whole),
		"no whole body": preflight(200, "application/json", HttpResponseBodyMode_HTTP_RESPONSE_BODY_MODE_HEADERS_ONLY),
	} {
		s := responseStream(t, NewPlanCapture())
		if err := s.Send(ev); err != nil {
			t.Fatal(err)
		}
		res, err := s.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if res.GetPreflightResult().GetSkip() == nil {
			t.Errorf("%s: got %v, want skip", name, res.GetPreflightResult())
		}
	}
}

func TestRequiringAPlanWithoutCapturingFailsClosed(t *testing.T) {
	// No WithCapture: the service is not reading replies at all.
	v, seen := verifierAgainst(t, 200, map[string]any{"allowed": true, "enforcementAction": "allow"})

	req := mcpCall("github_list_issues")
	req.Config, _ = structpb.NewStruct(map[string]any{ConfigRequireCapturedPlan: true})
	got, err := v.Verify(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got.Allow || got.Code != CodeDrift {
		t.Fatalf("got %+v, want %s: a required plan that can never be captured must not pass", got, CodeDrift)
	}
	if seen.Tool != "" {
		t.Errorf("asked the control plane about %q", seen.Tool)
	}
}

func TestAReplyFromAToolHostIsNeverThePlan(t *testing.T) {
	// A tool server that returns a completion-shaped body must not get to choose
	// the plan the sandbox is then held to.
	c := NewPlanCapture()
	s := responseStream(t, c)
	if err := s.Send(preflightFrom("postman-echo.com", 200, "application/json",
		HttpResponseBodyMode_HTTP_RESPONSE_BODY_MODE_WHOLE_BODY_BYTES)); err != nil {
		t.Fatal(err)
	}
	res, err := s.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if res.GetPreflightResult().GetSkip() == nil {
		t.Fatalf("preflight from a tool host = %v, want skip", res.GetPreflightResult())
	}
	if _, ok := c.Lookup("sbx-1"); ok {
		t.Fatal("a plan was recorded from a host that is not the model")
	}
}

func TestAskingTheModelIsNotAToolCall(t *testing.T) {
	v, seen := verifierAgainst(t, 200, map[string]any{"allowed": true, "enforcementAction": "allow"})
	v.WithCapture(NewPlanCapture())
	req := &HttpRequestEvaluation{
		Context: &RequestContext{SandboxId: "sbx-1"},
		Config:  modelHostConfig(),
		Target:  &HttpRequestTarget{Scheme: "https", Host: "model.internal", Method: "POST", Path: "/v1/chat/completions"},
		Headers: []*HttpHeader{{Name: "content-type", Value: "application/json"}},
		Body:    []byte(`{"model":"m","messages":[{"role":"user","content":"list the issues"}]}`),
	}
	got, err := v.Verify(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Allow {
		t.Fatalf("the agent's call to its own model was refused: %+v", got)
	}
	if seen.Tool != "" {
		t.Errorf("a model call went to the control plane as tool %q", seen.Tool)
	}
}

func TestConfigAcceptsModelHosts(t *testing.T) {
	ok, _ := structpb.NewStruct(map[string]any{ConfigModelHosts: []any{"api.openai.com"}})
	if err := ValidateIntentConfig(ok); err != nil {
		t.Errorf("rejected a host list: %v", err)
	}
	bad, _ := structpb.NewStruct(map[string]any{ConfigModelHosts: "api.openai.com"})
	if err := ValidateIntentConfig(bad); err == nil {
		t.Error("accepted a string where a list is required")
	}
}
