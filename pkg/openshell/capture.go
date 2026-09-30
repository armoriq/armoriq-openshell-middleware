package openshell

import (
	"encoding/json"
	"sort"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/structpb"
)

// CodeDrift is a tool call the model's own reply did not ask for.
//
// It is a separate code from CodeNotInPlan. That one is the sandbox's declared
// scope, which an operator writes once when the sandbox is created. This one is
// what the model chose for the task in hand, which is the only place the
// agent's intent shows up.
const CodeDrift = "intent_drift_from_plan"

// ConfigRequireCapturedPlan refuses tool calls made before any model reply has
// been seen for the sandbox. Without it, a sandbox is held to its captured plan
// only once one exists, which is the safer default for rolling this out.
const ConfigRequireCapturedPlan = "require_captured_plan"

// capturedPlanTTL bounds how long a plan is honoured. An agent that goes quiet
// and comes back should be working from a fresh reply, not the last one.
const capturedPlanTTL = 10 * time.Minute

// RequireCapturedPlan reads ConfigRequireCapturedPlan. Absent means false.
func RequireCapturedPlan(cfg *structpb.Struct) bool {
	v, ok := cfg.GetFields()[ConfigRequireCapturedPlan]
	return ok && v.GetBoolValue()
}

// ModelReply returns the tool names a model reply asks the agent to call, and
// whether the body was a model reply at all.
//
// The second value matters more than the first. Responses from the tools
// themselves pass through the same hook, and treating an MCP result as a model
// reply that asked for nothing would clear the plan the moment the agent made
// its first call.
//
// It understands OpenAI chat completions and Anthropic messages.
func ModelReply(body []byte) ([]string, bool) {
	var r struct {
		Object  string `json:"object"`
		Type    string `json:"type"`
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					Function struct {
						Name string `json:"name"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Content []struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"content"`
	}
	if json.Unmarshal(body, &r) != nil {
		return nil, false
	}

	seen := map[string]bool{}
	switch {
	case r.Object == "chat.completion":
		for _, c := range r.Choices {
			for _, tc := range c.Message.ToolCalls {
				if tc.Function.Name != "" {
					seen[tc.Function.Name] = true
				}
			}
		}
	case r.Type == "message":
		for _, c := range r.Content {
			if c.Type == "tool_use" && c.Name != "" {
				seen[c.Name] = true
			}
		}
	default:
		return nil, false
	}

	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, true
}

// PlanCapture holds the most recent plan a model issued to each sandbox.
//
// The response hook writes it and the request hook reads it. Both run in this
// process and key by sandbox_id, which OpenShell sets on both evaluations.
type PlanCapture struct {
	mu    sync.Mutex
	plans map[string]capturedPlan
	ttl   time.Duration
	now   func() time.Time
}

type capturedPlan struct {
	tools []string
	at    time.Time
}

// NewPlanCapture returns an empty store.
func NewPlanCapture() *PlanCapture {
	return &PlanCapture{
		plans: make(map[string]capturedPlan),
		ttl:   capturedPlanTTL,
		now:   time.Now,
	}
}

// Record replaces a sandbox's plan with the tools a model reply asked for.
//
// A reply that asks for no tools is recorded too, as an empty plan. The model
// finishing with text is an instruction to stop, and a tool call after it is
// one the model did not ask for.
func (p *PlanCapture) Record(sandboxID string, tools []string) {
	if sandboxID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.plans[sandboxID] = capturedPlan{tools: tools, at: p.now()}
}

// Lookup returns the sandbox's current plan, and false when none has been
// captured or the last one has expired.
func (p *PlanCapture) Lookup(sandboxID string) ([]string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.plans[sandboxID]
	if !ok {
		return nil, false
	}
	if p.now().Sub(e.at) > p.ttl {
		delete(p.plans, sandboxID)
		return nil, false
	}
	return e.tools, true
}
