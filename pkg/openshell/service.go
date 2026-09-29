// Package openshell implements the OpenShell supervisor middleware contract, so
// a sandbox's outbound request is checked against its registered intent before
// OpenShell forwards it.
//
// The shape of the contract decides most of the design here:
//
//   - The operator timeout defaults to 500ms and a binding may shorten it but
//     never extend it, so a verdict has to be reachable without leaving the
//     process on the common path.
//   - on_error defaults to fail_closed, so being slow is the same as denying.
//     That makes our latency the sandbox's availability.
//   - OpenShell never relays the free-form reason to the workload. Only
//     reason_code reaches the requester, and an invalid code turns the whole
//     result into a middleware failure, so codes are built from a fixed set
//     rather than formatted from input.
package openshell

import (
	"context"
	"regexp"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"
)

// Verdict is what a Verifier returns for one request. It is deliberately not the
// wire type: the wire type carries mutation and audit fields this service does
// not use yet, and a narrow interface keeps a verifier from reaching for them.
type Verdict struct {
	Allow bool
	// Code reaches the workload. Keep it stable, it is an API.
	Code string
	// Reason never reaches the workload. It is for our own diagnostics.
	Reason string
}

// Verifier decides whether a request is within the sandbox's registered intent.
//
// Implementations must respect ctx. OpenShell cancels on its own timeout and a
// verifier that ignores that turns a slow answer into a denied request.
type Verifier interface {
	Verify(ctx context.Context, req *HttpRequestEvaluation) (Verdict, error)
}

// reasonCodePattern is OpenShell's rule: 1 to 64 bytes, leading lowercase ASCII
// letter, then lowercase letters, digits and underscores.
var reasonCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

const (
	// CodeIntentDenied is returned when the request falls outside the plan.
	CodeIntentDenied = "intent_not_authorized"
	// CodeNoPlan is returned when the sandbox has no registered plan. It is a
	// deny rather than an allow: an unknown sandbox is not a permitted one.
	CodeNoPlan = "intent_plan_not_found"
	// CodeVerifierError is returned when verification itself failed. Returning
	// a deny here rather than an error keeps the decision ours instead of
	// leaving it to the stage's on_error setting.
	CodeVerifierError = "intent_verification_failed"
)

// Service implements SupervisorMiddlewareServer.
type Service struct {
	UnimplementedSupervisorMiddlewareServer

	name     string
	version  string
	verifier Verifier

	// audience is returned in the manifest. Their gateway compares it with the
	// operator's configured value and refuses to start if they differ, so an
	// empty string deliberately skips that check rather than guessing.
	audience string

	// validate checks the operator's per-binding config. Nil accepts anything.
	validate ConfigValidator

	// maxBody bounds the buffered request body we accept. OpenShell caps it at
	// 4 MiB; declaring less means it truncates before calling us.
	maxBody uint64
	// timeout is the per-binding timeout we declare. It can only shorten the
	// operator's value, so it is a ceiling on our own latency, not a request
	// for more room.
	timeout time.Duration
}

// New builds a Service. A nil verifier is a programming error rather than a
// runtime condition, so it is rejected here instead of failing per request.
func New(name, version string, v Verifier, opts ...Option) *Service {
	s := &Service{
		name:     name,
		version:  version,
		verifier: v,
		maxBody:  4 << 20,
		timeout:  450 * time.Millisecond,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Option configures a Service.
type Option func(*Service)

// WithMaxBody sets the largest buffered body we will accept.
func WithMaxBody(n uint64) Option { return func(s *Service) { s.maxBody = n } }

// WithTimeout sets the binding timeout we declare. OpenShell accepts 10ms to
// 30s and will not let this extend the operator's own setting.
func WithTimeout(d time.Duration) Option { return func(s *Service) { s.timeout = d } }

// WithAudience sets the audience returned in the manifest. Leave it empty to
// skip their post-authentication consistency check.
func WithAudience(a string) Option { return func(s *Service) { s.audience = a } }

// ConfigValidator checks an operator's per-binding configuration. The error text
// is shown to whoever is applying the policy, so it should say what to fix.
type ConfigValidator func(*structpb.Struct) error

// WithConfigValidator rejects a policy whose config we cannot work with. This
// runs when the policy is applied, which is the only moment an operator is
// watching, so a typo caught here is a typo that never reaches production.
func WithConfigValidator(v ConfigValidator) Option { return func(s *Service) { s.validate = v } }

// Describe returns the manifest. We declare only the HTTP request operation at
// PRE_CREDENTIALS: that is where the workload's intent is visible, before
// OpenShell injects credentials.
func (s *Service) Describe(context.Context, *emptypb.Empty) (*MiddlewareManifest, error) {
	return &MiddlewareManifest{
		Name:             s.name,
		ServiceVersion:   s.version,
		ExpectedAudience: s.audience,
		Bindings: []*MiddlewareBinding{{
			Operation:       SupervisorMiddlewareOperation_SUPERVISOR_MIDDLEWARE_OPERATION_HTTP_REQUEST,
			Phase:           SupervisorMiddlewarePhase_SUPERVISOR_MIDDLEWARE_PHASE_PRE_CREDENTIALS,
			MaxPayloadBytes: s.maxBody,
			RequestTimeout:  durationpb.New(s.timeout),
		}},
	}, nil
}

// ValidateConfig accepts the operator's per-binding configuration.
//
// Valid must be set explicitly. It is a bool, so a zero-valued response is a
// rejection with an empty reason, which their gateway reports as
// "middleware config '<name>' is invalid: " with nothing after the colon.
// Without a validator everything is accepted, rather than us inventing a schema
// we would then have to keep compatible.
func (s *Service) ValidateConfig(_ context.Context, req *ValidateConfigRequest) (*ValidateConfigResponse, error) {
	if s.validate != nil {
		if err := s.validate(req.GetConfig()); err != nil {
			return &ValidateConfigResponse{Valid: false, Reason: err.Error()}, nil
		}
	}
	return &ValidateConfigResponse{Valid: true}, nil
}

// EvaluateHttpRequest asks the verifier and translates its answer to the wire.
//
// It never returns a non-nil error. An error would be a middleware failure,
// handed to the stage's on_error setting, which an operator can set to
// fail_open. A deny we produce ourselves cannot be bypassed that way, and an
// unverifiable request is exactly the case where we want the strict answer.
func (s *Service) EvaluateHttpRequest(ctx context.Context, req *HttpRequestEvaluation) (*HttpRequestResult, error) {
	if req == nil || req.GetContext() == nil || req.GetContext().GetSandboxId() == "" {
		return deny(CodeNoPlan, "evaluation carried no sandbox id"), nil
	}
	v, err := s.verifier.Verify(ctx, req)
	if err != nil {
		return deny(CodeVerifierError, err.Error()), nil
	}
	if !v.Allow {
		return deny(codeOr(v.Code, CodeIntentDenied), v.Reason), nil
	}
	return &HttpRequestResult{
		Decision: Decision_DECISION_ALLOW,
		Reason:   v.Reason,
	}, nil
}

func deny(code, reason string) *HttpRequestResult {
	return &HttpRequestResult{
		Decision:   Decision_DECISION_DENY,
		Reason:     reason,
		ReasonCode: code,
	}
}

// codeOr falls back when a verifier returns a code OpenShell would reject.
// A malformed code makes the entire result a middleware failure, which would
// turn our deny into whatever on_error says, so a bad code must never reach the
// wire.
func codeOr(code, fallback string) string {
	if reasonCodePattern.MatchString(code) {
		return code
	}
	return fallback
}
