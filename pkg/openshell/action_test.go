package openshell

import "testing"

func evalWith(binary, method, host, path, query string, headers map[string]string, body string) *HttpRequestEvaluation {
	var hs []*HttpHeader
	for k, v := range headers {
		hs = append(hs, &HttpHeader{Name: k, Value: v})
	}
	return &HttpRequestEvaluation{
		Context: &RequestContext{
			SandboxId:          "sbx-1",
			OriginatingProcess: &Process{Binary: binary, Pid: 42},
		},
		Target:  &HttpRequestTarget{Scheme: "https", Host: host, Method: method, Path: path, Query: query},
		Headers: hs,
		Body:    []byte(body),
	}
}

// The case the whole integration rests on: a real MCP tools/call, exactly as an
// MCP client puts it on the wire, with no cooperation from the agent beyond
// speaking the protocol.
func TestRealMcpToolCallYieldsTheToolName(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":3,"method":"tools/call",` +
		`"params":{"name":"propose_oon_referral","arguments":{"member_id":"M-12","reason":"no in-network provider"}}}`
	a := ActionFrom(evalWith("/usr/local/bin/python3", "POST", "mcp.armorhealth.internal", "/mcp", "", nil, body))

	if a.Tool != "propose_oon_referral" {
		t.Fatalf("tool = %q, want propose_oon_referral", a.Tool)
	}
	if a.Source != "mcp" {
		t.Errorf("source = %q", a.Source)
	}
	if a.Arguments["member_id"] != "M-12" {
		t.Errorf("arguments not carried: %v", a.Arguments)
	}
	// The HTTP identity must survive alongside the tool name.
	if a.Binary != "/usr/local/bin/python3" || a.Method != "POST" || a.Host != "mcp.armorhealth.internal" {
		t.Errorf("http identity lost: %+v", a)
	}
}

// An MCP envelope method that is not a tool call must not be invented as a tool.
// Naming it "tools/list" would produce an action no policy author ever wrote.
func TestMcpProtocolMethodsAreNotTools(t *testing.T) {
	for _, m := range []string{"initialize", "tools/list", "ping", "resources/read"} {
		body := `{"jsonrpc":"2.0","id":1,"method":"` + m + `"}`
		a := ActionFrom(evalWith("/usr/bin/node", "POST", "mcp.internal", "/mcp", "", nil, body))
		if a.Named() {
			t.Errorf("%s was treated as tool %q", m, a.Tool)
		}
		if a.Source != "mcp_protocol" {
			t.Errorf("%s source = %q", m, a.Source)
		}
	}
}

// The case that matters for the honest answer: a plain API call names nothing,
// and we still get a complete HTTP identity to enforce on.
func TestPlainApiCallIsUnnamedButStillIdentified(t *testing.T) {
	a := ActionFrom(evalWith("/usr/bin/curl", "POST", "api.github.com", "/repos/acme/app/issues", "",
		nil, `{"title":"bug","body":"..."}`))

	if a.Named() {
		t.Fatalf("invented a tool name: %q", a.Tool)
	}
	if a.Binary != "/usr/bin/curl" || a.Method != "POST" ||
		a.Host != "api.github.com" || a.Path != "/repos/acme/app/issues" {
		t.Fatalf("http identity incomplete: %+v", a)
	}
	if a.Source != "unnamed" {
		t.Errorf("source = %q", a.Source)
	}
}

func TestFallbacksMatchTheProxyOrder(t *testing.T) {
	cases := []struct {
		name, want, source string
		eval               *HttpRequestEvaluation
	}{
		{"body tool field", "search", "body_tool",
			evalWith("/usr/bin/node", "POST", "h", "/x", "", nil, `{"tool":"search"}`)},
		{"non-mcp body method", "do_thing", "body_method",
			evalWith("/usr/bin/node", "POST", "h", "/x", "", nil, `{"method":"do_thing"}`)},
		{"query param", "read", "query",
			evalWith("/usr/bin/curl", "GET", "h", "/x", "tool=read", nil, "")},
		{"x-mcp-tool header", "write", "header",
			evalWith("/usr/bin/curl", "POST", "h", "/x", "", map[string]string{"X-MCP-Tool": "write"}, "")},
		{"tool path segment", "deploy", "path",
			evalWith("/usr/bin/curl", "POST", "h", "/tools/deploy", "", nil, "")},
	}
	for _, c := range cases {
		a := ActionFrom(c.eval)
		if a.Tool != c.want || a.Source != c.source {
			t.Errorf("%s: tool=%q source=%q, want %q/%q", c.name, a.Tool, a.Source, c.want, c.source)
		}
	}
}

// A body that is not JSON at all must not panic or produce a bogus name.
func TestNonJsonBodyIsSafe(t *testing.T) {
	for _, body := range []string{"", "not json", "<xml/>", "\x00\x01\x02"} {
		a := ActionFrom(evalWith("/usr/bin/curl", "PUT", "h", "/x", "", nil, body))
		if a.Named() {
			t.Errorf("body %q produced tool %q", body, a.Tool)
		}
	}
}

// Built from a request OpenShell actually sent us, copied out of the wire dump
// rather than written from the proto. Two things it pins down.
//
// There is no originating_process. Their field is documented "when available"
// and a curl in the sandbox does not make it available, so Binary is empty in
// production even though the proto has a place for it. Anything we enforce has
// to survive without it.
//
// The body does arrive, in full, and the tool name comes out of it. That is the
// part the integration rests on.
func TestTheRequestOpenshellActuallySends(t *testing.T) {
	req := &HttpRequestEvaluation{
		Phase: SupervisorMiddlewarePhase_SUPERVISOR_MIDDLEWARE_PHASE_PRE_CREDENTIALS,
		Context: &RequestContext{
			RequestId:   "59ba2d34-20ef-47b4-9d0d-5b8b745392e9",
			SandboxId:   "c46157b3-a7fa-454f-94a4-1f312baea298",
			SandboxName: "mwtest3",
			Workspace:   "default",
		},
		Target: &HttpRequestTarget{Scheme: "https", Host: "api.github.com", Port: 443, Method: "POST", Path: "/mcp"},
		Headers: []*HttpHeader{
			{Name: "user-agent", Value: "curl/8.5.0"},
			{Name: "accept", Value: "*/*"},
			{Name: "content-type", Value: "application/json"},
		},
		Body: []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call",` +
			`"params":{"name":"github_create_issue","arguments":{"repo":"armoriq/secrets","title":"x"}}}`),
		MiddlewareName: "armoriq-intent",
	}

	a := ActionFrom(req)
	if a.Tool != "github_create_issue" || a.Source != "mcp" {
		t.Fatalf("tool = %q via %q, want github_create_issue via mcp", a.Tool, a.Source)
	}
	if a.Arguments["repo"] != "armoriq/secrets" {
		t.Errorf("arguments not carried: %v", a.Arguments)
	}
	if a.Binary != "" {
		t.Errorf("Binary = %q, but OpenShell sends no originating_process; a test that "+
			"fills it in is testing us, not them", a.Binary)
	}
	if a.Method != "POST" || a.Host != "api.github.com" || a.Path != "/mcp" {
		t.Errorf("http identity wrong: %+v", a)
	}
}

// The same sandbox, same policy, without a tool name anywhere: a plain GET.
// This is the shape that forces the allow-or-deny decision on unnameable
// traffic, so it is worth having the real one on record.
func TestTheUnnamedRequestOpenshellActuallySends(t *testing.T) {
	req := &HttpRequestEvaluation{
		Phase: SupervisorMiddlewarePhase_SUPERVISOR_MIDDLEWARE_PHASE_PRE_CREDENTIALS,
		Context: &RequestContext{
			RequestId: "96e263d3-cd8d-4e77-bab4-59ddfc822974",
			SandboxId: "acc76442-fa79-4b7f-bcfb-699409f85d88",
			Workspace: "default",
		},
		Target:         &HttpRequestTarget{Scheme: "https", Host: "api.github.com", Port: 443, Method: "GET", Path: "/zen"},
		Headers:        []*HttpHeader{{Name: "user-agent", Value: "curl/8.5.0"}},
		MiddlewareName: "armoriq-intent",
	}

	a := ActionFrom(req)
	if a.Named() {
		t.Errorf("a plain GET must not be given a tool name, got %q via %q", a.Tool, a.Source)
	}
	if a.Method != "GET" || a.Host != "api.github.com" || a.Path != "/zen" {
		t.Errorf("http identity wrong: %+v", a)
	}
}
