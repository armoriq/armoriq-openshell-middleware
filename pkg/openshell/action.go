package openshell

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
)

// Action is what one sandbox request is, as far as enforcement is concerned.
//
// The HTTP fields are always present: OpenShell's supervisor fills them in before
// calling us, so an agent cannot decline to provide them or forge them. Tool and
// Arguments are present only when the traffic names a tool, which in practice
// means MCP, and they are what let an existing tool-shaped policy apply unchanged.
type Action struct {
	// Always available.
	Binary string // originating executable, e.g. /usr/bin/curl
	Method string
	Host   string
	Path   string
	Query  string

	// Present only when the request names a tool.
	Tool      string
	Arguments map[string]any

	// Source records how the tool name was found, for diagnostics and so a
	// decision can say why it was enforceable.
	Source string
}

// Named reports whether a tool name was recoverable. When false the request can
// still be enforced, but only on the HTTP shape.
func (a Action) Named() bool { return a.Tool != "" }

// jsonRPC is the subset of a JSON-RPC 2.0 envelope we care about. MCP rides on
// this, so a tools/call carries the tool name in params.name.
type jsonRPC struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	} `json:"params"`
	// Non-MCP callers sometimes name the tool directly.
	Tool     string         `json:"tool"`
	ToolName string         `json:"toolName"`
	Args     map[string]any `json:"arguments"`
}

// mcpProtocolMethods are MCP envelope methods that are not themselves tool
// calls. Treating "tools/list" as a tool named "tools/list" would invent an
// action that no policy author ever wrote a rule for.
var mcpProtocolMethods = map[string]bool{
	"initialize": true, "initialized": true,
	"notifications/initialized": true, "notifications/init": true,
	"tools/list": true, "tools/call": true,
	"prompts/list": true, "prompts/get": true,
	"resources/list": true, "resources/read": true,
	"resources/templates/list": true,
	"completion/complete":      true, "logging/setLevel": true, "ping": true,
}

var toolPathPattern = regexp.MustCompile(`/tools?/([^/]+)`)

// ActionFrom builds an Action from one OpenShell evaluation.
//
// The order of the lookups mirrors the proxy's extractToolNameFromRequest, so a
// request enforced through our proxy and the same request enforced through
// OpenShell resolve to the same tool name.
func ActionFrom(req *HttpRequestEvaluation) Action {
	t := req.GetTarget()
	a := Action{
		Binary: req.GetContext().GetOriginatingProcess().GetBinary(),
		Method: t.GetMethod(),
		Host:   t.GetHost(),
		Path:   t.GetPath(),
		Query:  t.GetQuery(),
		Source: "unnamed",
	}

	if body := req.GetBody(); len(body) > 0 {
		var rpc jsonRPC
		if err := json.Unmarshal(body, &rpc); err == nil {
			switch {
			case rpc.Method == "tools/call" && rpc.Params.Name != "":
				a.Tool, a.Arguments, a.Source = rpc.Params.Name, rpc.Params.Arguments, "mcp"
				return a
			case rpc.Method != "" && mcpProtocolMethods[rpc.Method]:
				// An MCP envelope method that is not a tool call. Deliberately
				// left unnamed rather than named after the envelope.
				a.Source = "mcp_protocol"
				return a
			case rpc.Method != "":
				a.Tool, a.Arguments, a.Source = rpc.Method, rpc.Args, "body_method"
				return a
			case rpc.Tool != "":
				a.Tool, a.Arguments, a.Source = rpc.Tool, rpc.Args, "body_tool"
				return a
			case rpc.ToolName != "":
				a.Tool, a.Arguments, a.Source = rpc.ToolName, rpc.Args, "body_tool"
				return a
			}
		}
	}

	if q, err := url.ParseQuery(a.Query); err == nil {
		if v := q.Get("tool"); v != "" {
			a.Tool, a.Source = v, "query"
			return a
		}
	}

	for _, h := range req.GetHeaders() {
		switch strings.ToLower(h.GetName()) {
		case "x-mcp-tool", "x-tool-name":
			if h.GetValue() != "" {
				a.Tool, a.Source = h.GetValue(), "header"
				return a
			}
		}
	}

	if m := toolPathPattern.FindStringSubmatch(a.Path); m != nil {
		a.Tool, a.Source = m[1], "path"
		return a
	}

	return a
}
