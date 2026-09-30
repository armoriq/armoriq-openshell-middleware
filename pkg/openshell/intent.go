package openshell

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/armoriq/armoriq-openshell-middleware/pkg/iapclient"
)

const (
	// CodeBlocked is a policy saying no. It is separate from CodeIntentDenied
	// so an operator can tell a policy denial from a plan miss.
	CodeBlocked = "intent_blocked_by_policy"
	// CodeHold is a decision that needs a human. The call does not proceed.
	CodeHold = "intent_awaiting_approval"
	// CodeUnknownDecision is an enforcement action we do not recognise. Their
	// normalizer promises three, so this firing means the contract moved.
	CodeUnknownDecision = "intent_decision_unknown"
	// CodeUnnamed is traffic we cannot name a tool for. Its own code so the
	// audit distinguishes "policy said no" from "we could not ask".
	CodeUnnamed = "intent_action_not_named"
)

// Identity is who a sandbox is acting as. The control plane scopes policy by
// agent, so without this every sandbox gets the same answer.
type Identity struct {
	AgentID   string
	UserEmail string
}

// Resolver maps a sandbox to the agent running in it.
//
// Their proto is explicit that sandbox_name and workspace are display only and
// that sandbox_id is the field to use for authorization and durable
// correlation, so this takes the id.
type Resolver interface {
	Resolve(ctx context.Context, sandboxID string) (Identity, error)
}

// StaticIdentity answers with the same identity for every sandbox. It is what
// we use until sandbox registration exists.
type StaticIdentity Identity

func (s StaticIdentity) Resolve(context.Context, string) (Identity, error) {
	return Identity(s), nil
}

// UnnamedAction is what to do with traffic that names no tool.
//
// The enforce endpoint requires a tool and rejects an empty one, so we cannot
// ask about this traffic at all. Both answers are defensible and the choice is
// an operator's, not ours: denying closes the hole and will block ordinary
// traffic nobody described, allowing leaves their own network policy as the
// only thing governing it.
type UnnamedAction int

const (
	// DenyUnnamed refuses traffic we cannot name. This is the default because
	// everything else about this path is fail closed.
	DenyUnnamed UnnamedAction = iota
	// AllowUnnamed passes it to their network policy.
	AllowUnnamed
)

// IntentVerifier answers from the ArmorIQ control plane.
type IntentVerifier struct {
	client   *iapclient.Client
	resolver Resolver
	unnamed  UnnamedAction

	// plans holds one minted intent token per sandbox, so the declared plan
	// costs a round trip when it changes rather than on every request.
	plans *planCache

	// minter is the same control plane with a longer timeout. Minting builds
	// proofs and is far slower than a decision, and it runs off the request
	// path, so holding it to the request budget would only make it fail.
	minter *iapclient.Client

	// audit records the decisions we make on our own. Decisions that went to
	// enforce are already persisted by the control plane, so recording those
	// here would double count them.
	audit *iapclient.AuditBuffer

	// capture is the plan the model most recently issued to each sandbox, read
	// off the response path. Nil when the gateway does not dispatch responses,
	// in which case only the declared scope and policy apply.
	capture *PlanCapture

	// slowAfter logs a decision that ate most of the budget. Being told late is
	// how a 500ms ceiling turns into denied traffic in production.
	slowAfter time.Duration
}

// planRenewAfter is how long a minted token is reused. The control plane issues
// for 15 minutes by default, so this renews well before expiry.
const planRenewAfter = 12 * time.Minute

// mintTimeout is what a background mint gets. It is generous on purpose: it
// never blocks a request, and a mint that fails leaves holds unapprovable.
const mintTimeout = 20 * time.Second

// NewIntentVerifier builds the verifier. A nil resolver is a programming error
// rather than a runtime one, so it is caught here.
func NewIntentVerifier(c *iapclient.Client, r Resolver, unnamed UnnamedAction, slowAfter time.Duration) (*IntentVerifier, error) {
	if c == nil {
		return nil, errors.New("openshell: nil enforce client")
	}
	if r == nil {
		return nil, errors.New("openshell: nil resolver")
	}
	return &IntentVerifier{
		client: c, resolver: r, unnamed: unnamed, slowAfter: slowAfter,
		plans:  newPlanCache(planRenewAfter),
		minter: c.Clone(mintTimeout),
	}, nil
}

// WithAudit records decisions the control plane never sees. Without it those
// denials exist only in OpenShell's log and ours.
func (v *IntentVerifier) WithAudit(b *iapclient.AuditBuffer) *IntentVerifier {
	v.audit = b
	return v
}

// WithCapture holds each sandbox to the plan its model last issued.
func (v *IntentVerifier) WithCapture(c *PlanCapture) *IntentVerifier {
	v.capture = c
	return v
}

// Verify asks the control plane about one request.
//
// Every path that does not produce a decision returns an error, which the
// service turns into a deny of our own rather than leaving it to the stage's
// on_error setting.
func (v *IntentVerifier) Verify(ctx context.Context, req *HttpRequestEvaluation) (Verdict, error) {
	act := ActionFrom(req)
	sandbox := req.GetContext().GetSandboxId()

	start := time.Now()
	verdict, id, asked, err := v.decide(ctx, req, act, sandbox)
	took := time.Since(start)
	v.logDecision(act, sandbox, id, verdict, err, took)
	if !asked && v.audit != nil {
		v.audit.Add(auditRow(act, sandbox, id, verdict, took))
	}
	return verdict, err
}

// decide returns the verdict, the identity it used, and whether it asked the
// control plane. The last one decides who records the decision.
func (v *IntentVerifier) decide(ctx context.Context, req *HttpRequestEvaluation, act Action, sandbox string) (Verdict, Identity, bool, error) {
	if !act.Named() {
		if v.unnamed == AllowUnnamed {
			return Verdict{Allow: true,
				Reason: fmt.Sprintf("no tool named in %s %s%s, passed to network policy", act.Method, act.Host, act.Path)}, Identity{}, false, nil
		}
		return Verdict{Allow: false, Code: CodeUnnamed,
			Reason: fmt.Sprintf("no tool named in %s %s%s", act.Method, act.Host, act.Path)}, Identity{}, false, nil
	}

	id, err := v.identify(ctx, req, sandbox)
	if err != nil {
		return Verdict{}, Identity{}, false, err
	}
	if id.AgentID == "" {
		// An unknown sandbox is not a permitted one. Without an agent the
		// control plane would answer with whatever the org allows in general,
		// which is a weaker rule than the one meant to apply here.
		return Verdict{Allow: false, Code: CodeNoPlan,
			Reason: fmt.Sprintf("sandbox %s declares no %s, so no agent policy can apply",
				sandbox, ConfigAgentID)}, id, false, nil
	}

	// The plan check comes first and never leaves the process. A tool the
	// sandbox never declared is drift, and asking a policy whether it is
	// permitted would answer a different question from the one being asked.
	declared := DeclaredToolsFromConfig(req.GetConfig())
	if len(declared) > 0 && !InPlan(declared, act.Tool) {
		return Verdict{Allow: false, Code: CodeNotInPlan,
			Reason: fmt.Sprintf("%s is not in the plan sandbox %s declared (%s)",
				act.Tool, sandbox, strings.Join(declared, ", "))}, id, false, nil
	}

	// The captured plan is narrower than the declared scope: it is what the
	// model asked for in its latest reply, for the task in hand. A call can be
	// inside the scope and allowed by policy and still be one the model never
	// asked for, and this is the only check that sees that.
	//
	// A policy that requires a plan is refused outright when this service is not
	// capturing any. Skipping the check would leave the policy believing it is
	// enforced when nothing could ever satisfy it.
	required := RequireCapturedPlan(req.GetConfig())
	if v.capture == nil && required {
		return Verdict{Allow: false, Code: CodeDrift,
			Reason: fmt.Sprintf("sandbox %s sets %s but this service is not capturing plans (-capture-plan is off)",
				sandbox, ConfigRequireCapturedPlan)}, id, false, nil
	}
	if v.capture != nil {
		plan, captured := v.capture.Lookup(sandbox)
		switch {
		case captured && !InPlan(plan, act.Tool):
			return Verdict{Allow: false, Code: CodeDrift,
				Reason: fmt.Sprintf("%s was not asked for by the model's last reply to sandbox %s, which asked for [%s]",
					act.Tool, sandbox, strings.Join(plan, ", "))}, id, false, nil
		case !captured && required:
			return Verdict{Allow: false, Code: CodeDrift,
				Reason: fmt.Sprintf("%s called before any model reply was seen for sandbox %s, and the policy sets %s",
					act.Tool, sandbox, ConfigRequireCapturedPlan)}, id, false, nil
		}
	}

	// The token carries the plan to the control plane. Without it there is no
	// plan id, so an approval can never attach and a hold could never clear.
	//
	// Looking it up never blocks. Minting costs about a second because the
	// control plane builds proofs, and putting that on the request path spends
	// the whole binding budget before the decision is even asked for, which
	// OpenShell then reports as a middleware timeout. The first requests from a
	// sandbox go without a token while one is minted behind them.
	var intentToken map[string]any
	if len(declared) > 0 {
		if t, ready := v.planToken(sandbox, id, declared); ready {
			intentToken = t.Raw
		}
	}

	res, err := v.client.Enforce(ctx, iapclient.Request{
		Tool:        act.Tool,
		Arguments:   act.Arguments,
		AgentID:     id.AgentID,
		UserEmail:   id.UserEmail,
		IntentToken: intentToken,
	})
	if err != nil {
		return Verdict{}, id, true, fmt.Errorf("enforce %q: %w", act.Tool, err)
	}

	if res.Allow() {
		return Verdict{Allow: true,
			Reason: fmt.Sprintf("%s allowed by %s", act.Tool, policyName(res))}, id, true, nil
	}

	switch res.Action {
	case iapclient.DecisionBlock:
		return Verdict{Allow: false, Code: CodeBlocked,
			Reason: fmt.Sprintf("%s blocked by %s: %s", act.Tool, policyName(res), res.Reason)}, id, true, nil
	case iapclient.DecisionHold:
		return Verdict{Allow: false, Code: CodeHold,
			Reason: fmt.Sprintf("%s held for approval by %s, plan %s: %s",
				act.Tool, policyName(res), res.Delegation.PlanID, res.Reason)}, id, true, nil
	default:
		// Either an action their normalizer should never emit, or allowed and
		// the action disagreeing. Both mean we stop.
		return Verdict{Allow: false, Code: CodeUnknownDecision,
			Reason: fmt.Sprintf("%s got allowed=%v action=%q, which is not a decision we act on",
				act.Tool, res.Allowed, res.Action)}, id, true, nil
	}
}

// identify prefers what the sandbox policy declares and falls back to the
// resolver field by field, so a policy that sets only one of them still gets
// the other.
func (v *IntentVerifier) identify(ctx context.Context, req *HttpRequestEvaluation, sandbox string) (Identity, error) {
	id := IdentityFromConfig(req.GetConfig())
	if id.AgentID != "" && id.UserEmail != "" {
		return id, nil
	}
	fallback, err := v.resolver.Resolve(ctx, sandbox)
	if err != nil {
		return Identity{}, fmt.Errorf("resolve sandbox %s: %w", sandbox, err)
	}
	if id.AgentID == "" {
		id.AgentID = fallback.AgentID
	}
	if id.UserEmail == "" {
		id.UserEmail = fallback.UserEmail
	}
	return id, nil
}

// planToken returns the token for a sandbox's declared plan when one is ready,
// and otherwise starts minting it in the background and reports not ready.
//
// It never blocks and never fails the caller. A sandbox's first few requests
// are enforced without a plan id, which costs approval attribution on those
// calls and nothing else: the plan check has already run locally and the policy
// check still applies.
func (v *IntentVerifier) planToken(sandbox string, id Identity, declared []string) (iapclient.Token, bool) {
	fp := planFingerprint(id.AgentID, declared)
	if t, ok := v.plans.get(sandbox, fp); ok {
		return t, true
	}
	if v.plans.claim(sandbox, fp) {
		go v.mint(sandbox, fp, id, declared)
	}
	return iapclient.Token{}, false
}

// mint declares the plan to the control plane. It runs on its own goroutine
// with its own timeout, detached from the request that triggered it.
func (v *IntentVerifier) mint(sandbox, fp string, id Identity, declared []string) {
	defer v.plans.release(sandbox, fp)

	ctx, cancel := context.WithTimeout(context.Background(), mintTimeout)
	defer cancel()

	start := time.Now()
	t, err := v.minter.IssueToken(ctx, iapclient.TokenRequest{
		UserID:    id.UserEmail,
		AgentID:   id.AgentID,
		UserEmail: id.UserEmail,
		Plan: iapclient.PlanFromTools(declared, map[string]any{
			"source":     "openshell-middleware",
			"sandbox_id": sandbox,
		}),
	})
	if err != nil {
		log.Printf("sandbox=%s plan token mint failed after %s, calls stay enforceable but a "+
			"hold on them could not be approved: %v", sandbox, time.Since(start), err)
		return
	}
	v.plans.put(sandbox, fp, t)
	log.Printf("sandbox=%s plan token ready in %s, plan=%s agent=%s tools=%d",
		sandbox, time.Since(start), t.PlanID, id.AgentID, len(declared))
}

// logDecision writes one line per request. Without it a denial is only visible
// in their audit log, which is the wrong place to debug our own behaviour from.
func (v *IntentVerifier) logDecision(act Action, sandbox string, id Identity, verdict Verdict, err error, took time.Duration) {
	what := "tool=" + act.Tool
	if !act.Named() {
		what = "unnamed"
	}
	if id.AgentID != "" {
		what += " agent=" + id.AgentID
	}
	slow := ""
	if v.slowAfter > 0 && took > v.slowAfter {
		slow = " SLOW"
	}
	switch {
	case err != nil:
		log.Printf("DENY sandbox=%s %s %s%s %s in %s%s: no decision, %v",
			sandbox, act.Method, act.Host, act.Path, what, took, slow, err)
	case verdict.Allow:
		log.Printf("ALLOW sandbox=%s %s %s%s %s in %s%s: %s",
			sandbox, act.Method, act.Host, act.Path, what, took, slow, verdict.Reason)
	default:
		log.Printf("DENY sandbox=%s %s %s%s %s in %s%s [%s]: %s",
			sandbox, act.Method, act.Host, act.Path, what, took, slow, verdict.Code, verdict.Reason)
	}
}

// policyName keeps a log line readable when no policy matched.
func policyName(r iapclient.Result) string {
	if r.MatchedPolicy.Name != "" {
		return r.MatchedPolicy.Name
	}
	if r.MatchedPolicy.PolicyID != "" {
		return r.MatchedPolicy.PolicyID
	}
	return "no matched policy"
}

// auditRow describes a decision we made without asking.
//
// Unnamed traffic has no tool, so the request itself is named in that field.
// Calling it something else would put a tool in the audit that nobody invoked.
func auditRow(act Action, sandbox string, id Identity, v Verdict, took time.Duration) iapclient.AuditRow {
	name := act.Tool
	input := map[string]any{}
	for k, val := range act.Arguments {
		input[k] = val
	}
	if !act.Named() {
		name = fmt.Sprintf("%s %s%s", act.Method, act.Host, act.Path)
		input["method"], input["host"], input["path"] = act.Method, act.Host, act.Path
	}

	status := "blocked"
	if v.Allow {
		status = "success"
	}

	return iapclient.AuditRow{
		SessionID: sandbox,
		AgentID:   id.AgentID,
		Action:    name,
		Tool:      name,
		Input:     input,
		Output: map[string]any{
			"allowed":     v.Allow,
			"reason_code": v.Code,
			"reason":      v.Reason,
			"sandbox_id":  sandbox,
			"decided_by":  "openshell-middleware",
		},
		Status:     status,
		DurationMs: took.Milliseconds(),
		ExecutedAt: time.Now().UTC().Format(time.RFC3339),
	}
}
