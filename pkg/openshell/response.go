package openshell

import (
	"errors"
	"io"
	"log"
	"strings"

	"google.golang.org/grpc"
)

// ResponseService implements HttpResponsePreReturnServer.
//
// It reads replies from the sandbox's model hosts on their way back to the
// sandbox and records the tool calls they ask for. Every other response is
// skipped at the head. It never changes or blocks a response. Reading the plan
// is the whole job here: enforcing it happens on the request that follows,
// where the request hook already sits and already has the tool name.
//
// Registering this against stock OpenShell makes the gateway refuse to start,
// because manifest validation rejects the HTTP_RESPONSE/PRE_RETURN binding as
// not yet supported. It needs a gateway that dispatches the response path.
type ResponseService struct {
	UnimplementedHttpResponsePreReturnServer

	capture *PlanCapture
}

// NewResponseService returns a service that records into capture.
func NewResponseService(capture *PlanCapture) *ResponseService {
	return &ResponseService{capture: capture}
}

// Evaluate runs one stage for one response: preflight, then at most one whole
// body, then trailers. Each of those gets exactly one result, in order.
func (r *ResponseService) Evaluate(stream grpc.BidiStreamingServer[HttpResponseEvent, HttpResponseEventResult]) error {
	var sandbox, host, target string
	var modelHosts []string
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}

		switch e := ev.GetEvent().(type) {
		case *HttpResponseEvent_Preflight:
			p := e.Preflight
			sandbox = p.GetContext().GetSandboxId()
			host = p.GetTarget().GetHost()
			target = host + p.GetTarget().GetPath()
			modelHosts = ModelHostsFromConfig(p.GetConfig())
			result := preflightResult(p)
			if !IsModelHost(modelHosts, host) {
				result = skipResult()
			}
			if err := stream.Send(result); err != nil {
				return err
			}

		case *HttpResponseEvent_Body:
			b := e.Body
			if tools, ok := ModelReply(b.GetData()); ok {
				r.capture.Record(sandbox, tools)
				log.Printf("sandbox=%s plan captured from %s: [%s]",
					sandbox, target, strings.Join(tools, ", "))
			}
			if err := stream.Send(&HttpResponseEventResult{
				Result: &HttpResponseEventResult_BodyResult{BodyResult: &HttpResponseBodyResult{
					Sequence: b.GetSequence(),
					Action:   &HttpResponseBodyResult_PassThrough{PassThrough: &HttpResponseBodyPassThrough{}},
				}},
			}); err != nil {
				return err
			}

		case *HttpResponseEvent_Trailers:
			if err := stream.Send(&HttpResponseEventResult{
				Result: &HttpResponseEventResult_TrailersResult{TrailersResult: &HttpResponseTrailersResult{}},
			}); err != nil {
				return err
			}

		case *HttpResponseEvent_SessionEnd:
			return nil
		}
	}
}

// preflightResult asks for the whole body only when it could be a model reply.
//
// Everything else is skipped at the head, so tool responses, errors and
// anything streamed are delivered without being buffered. OpenShell already
// leaves WHOLE_BODY_BYTES out of the permitted modes for encoded, partial and
// oversized bodies, so its absence covers those too.
func skipResult() *HttpResponseEventResult {
	return &HttpResponseEventResult{Result: &HttpResponseEventResult_PreflightResult{
		PreflightResult: &HttpResponsePreflightResult{
			Action: &HttpResponsePreflightResult_Skip{Skip: &HttpResponsePreflightSkip{}},
		},
	}}
}

// preflightResult is only asked for replies from a model host.
func preflightResult(p *HttpResponsePreflight) *HttpResponseEventResult {
	skip := skipResult()

	if p.GetStatusCode() < 200 || p.GetStatusCode() > 299 {
		return skip
	}
	if !jsonContent(p.GetHeaders()) {
		return skip
	}
	whole := false
	for _, m := range p.GetPermittedBodyModes() {
		if m == HttpResponseBodyMode_HTTP_RESPONSE_BODY_MODE_WHOLE_BODY_BYTES {
			whole = true
		}
	}
	if !whole {
		return skip
	}

	return &HttpResponseEventResult{Result: &HttpResponseEventResult_PreflightResult{
		PreflightResult: &HttpResponsePreflightResult{
			Action: &HttpResponsePreflightResult_Inspect{Inspect: &HttpResponsePreflightInspect{
				BodyMode: HttpResponseBodyMode_HTTP_RESPONSE_BODY_MODE_WHOLE_BODY_BYTES,
			}},
		},
	}}
}

func jsonContent(headers []*HttpHeader) bool {
	for _, h := range headers {
		if strings.EqualFold(h.GetName(), "content-type") {
			return strings.Contains(strings.ToLower(h.GetValue()), "json")
		}
	}
	return false
}
