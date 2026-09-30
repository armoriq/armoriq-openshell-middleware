// Command openshell-middleware serves the ArmorIQ intent check to an OpenShell
// gateway and its sandbox supervisors.
//
// Deployment shape comes from their contract, not from ours. A middleware is
// registered with a `grpc_endpoint` URL and must be reachable over TCP from both
// the gateway and every sandbox supervisor, so this listens on a host and port
// rather than a Unix socket. Their gateway refuses to start when a registered
// service is unavailable, so start this first.
//
//	[[openshell.supervisor.middleware]]
//	name = "armoriq-intent"
//	grpc_endpoint = "http://127.0.0.1:50071"
//	allow_insecure_transport = true
//	max_payload_bytes = 262144
//	timeout = "500ms"
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/prototext"

	"github.com/armoriq/armoriq-openshell-middleware/pkg/iapclient"
	"github.com/armoriq/armoriq-openshell-middleware/pkg/openshell"
)

// apiKeyEnv holds the control plane key. Environment rather than a flag:
// flags are visible in ps.
const apiKeyEnv = "ARMORIQ_API_KEY"

// keepWarmEvery has to be shorter than the transport's idle timeout, or the
// connection we are trying to keep is dropped between pings.
const keepWarmEvery = 60 * time.Second

// auditFlushEvery bounds how long a decision waits before it is written. It is
// off the critical path, so this trades a little delay for far fewer requests.
const auditFlushEvery = 5 * time.Second

// auditBufferSize is how many decisions can wait to be written before rows are
// dropped. Dropping is the deliberate choice: a decision must never wait on its
// own record being filed.
const auditBufferSize = 1024

// runtime is what the chosen mode needs at run time.
type runtime struct {
	verifier openshell.Verifier
	client   *iapclient.Client
	audit    *iapclient.AuditBuffer
	mode     string
}

// allowAll is the skeleton verifier. It exists so registration can be proven end
// to end before intent verification is wired in, and it is deliberately loud
// about what it is.
type allowAll struct {
	deny bool
	dump bool
}

func (a allowAll) Verify(_ context.Context, req *openshell.HttpRequestEvaluation) (openshell.Verdict, error) {
	act := openshell.ActionFrom(req)
	if a.dump {
		log.Printf("raw request:\n%s", prototext.Format(req))
	}
	log.Printf("sandbox=%s %s %s%s binary=%s tool=%q via=%s",
		req.GetContext().GetSandboxId(), act.Method, act.Host, act.Path,
		act.Binary, act.Tool, act.Source)
	if a.deny {
		return openshell.Verdict{Allow: false, Code: "intent_not_authorized",
			Reason: "skeleton verifier denies everything"}, nil
	}
	return openshell.Verdict{Allow: true, Reason: "skeleton verifier allows everything"}, nil
}

func main() {
	addr := flag.String("listen", "0.0.0.0:50071", "host:port to serve gRPC on")
	name := flag.String("name", "armoriq-intent", "manifest name, diagnostic only")
	audience := flag.String("audience", "", "expected audience, empty skips the consistency check")
	timeout := flag.Duration("timeout", 450*time.Millisecond, "binding timeout to declare, 10ms to 30s")
	maxBody := flag.Uint64("max-body", 256<<10, "largest buffered body we accept")
	deny := flag.Bool("deny", false, "deny every request, to prove the path end to end")
	dump := flag.Bool("dump-request", false, "log the whole evaluation proto, to see which fields they populate")
	iapURL := flag.String("iap-url", "", "ArmorIQ control plane base url, empty runs the skeleton verifier")
	agentID := flag.String("agent-id", "", "fallback agent for sandboxes whose policy declares no agent_id")
	userEmail := flag.String("user-email", "", "requester passed on enforce, optional")
	unnamed := flag.String("unnamed", "deny", "what to do with traffic that names no tool: deny or allow")
	audit := flag.Bool("audit", true, "record decisions the control plane never sees")
	capturePlan := flag.Bool("capture-plan", false, "read model replies on the response path and hold each sandbox to the plan they issue; needs a gateway that dispatches HTTP_RESPONSE/PRE_RETURN")
	flag.Parse()

	// Their contract accepts 10ms to 30s and treats the operator's service
	// timeout as a ceiling, so a larger value here would be silently clamped.
	if *timeout < 10*time.Millisecond || *timeout > 30*time.Second {
		log.Fatalf("timeout %s is outside the 10ms to 30s range OpenShell accepts", *timeout)
	}

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}

	srv := grpc.NewServer()
	run := chooseVerifier(*iapURL, *agentID, *userEmail, *unnamed, *timeout, *deny, *dump, *audit)

	opts := []openshell.Option{
		openshell.WithTimeout(*timeout),
		openshell.WithMaxBody(*maxBody),
		openshell.WithAudience(*audience),
	}
	if run.client != nil {
		// Only when we are enforcing. The skeleton has no config to be wrong.
		opts = append(opts, openshell.WithConfigValidator(openshell.ValidateIntentConfig))
	}

	// One store, written by the response hook and read by the request hook.
	// Both are served from this process, which is what lets a plan read off a
	// reply govern the call that follows it.
	if *capturePlan {
		iv, ok := run.verifier.(*openshell.IntentVerifier)
		if !ok {
			log.Fatal("-capture-plan needs -iap-url: the skeleton verifier has nothing to hold a plan against")
		}
		capture := openshell.NewPlanCapture()
		iv.WithCapture(capture)
		opts = append(opts, openshell.WithResponseBinding())
		openshell.RegisterHttpResponsePreReturnServer(srv, openshell.NewResponseService(capture))
		run.mode += ", capturing plans from model replies"
	}

	svc := openshell.New(*name, version(), run.verifier, opts...)
	openshell.RegisterSupervisorMiddlewareServer(srv, svc)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Hold a connection open to the control plane. The first call out of a cold
	// process pays DNS and a TLS handshake, which is most of the budget.
	if run.client != nil {
		go run.client.KeepWarm(ctx, keepWarmEvery, func(err error) {
			log.Printf("keeping the control plane connection warm failed: %v", err)
		})
	}
	if run.audit != nil {
		go run.audit.Run(ctx, auditFlushEvery, func(err error) {
			log.Printf("audit: %v", err)
		})
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		log.Print("shutting down")
		cancel()
		srv.GracefulStop()
	}()

	log.Printf("listening on %s as %q, binding timeout %s, max body %d, mode %s",
		*addr, *name, *timeout, *maxBody, run.mode)
	if err := srv.Serve(lis); err != nil {
		log.Fatalf("serve: %v", err)
	}
}

// version is a placeholder until the build stamps one in. OpenShell uses it for
// diagnostics only.
func version() string { return "0.1.0-dev" }

// chooseVerifier picks between the skeleton and the real check. Without a
// control plane url there is nothing to ask, so the skeleton stays: it is how
// registration is proven without involving policy.
func chooseVerifier(iapURL, agentID, userEmail, unnamed string, timeout time.Duration, deny, dump, audit bool) runtime {
	if iapURL == "" {
		if deny {
			return runtime{verifier: allowAll{deny: true, dump: dump}, mode: "skeleton, DENY EVERYTHING"}
		}
		return runtime{verifier: allowAll{dump: dump}, mode: "skeleton, allow everything"}
	}

	// The key is read from the environment rather than a flag because a flag is
	// visible in ps to every user on the host.
	key := os.Getenv(apiKeyEnv)
	if key == "" {
		log.Fatalf("%s is not set, and -iap-url needs a key to call enforce with", apiKeyEnv)
	}

	var unnamedAction openshell.UnnamedAction
	switch unnamed {
	case "deny":
		unnamedAction = openshell.DenyUnnamed
	case "allow":
		unnamedAction = openshell.AllowUnnamed
	default:
		log.Fatalf("-unnamed must be deny or allow, got %q", unnamed)
	}

	// Leave room to answer. If the enforce call is allowed to run for the whole
	// binding budget, OpenShell gives up before we reply and the request is
	// denied by their timeout rather than by our decision.
	budget := timeout - 50*time.Millisecond
	if budget < 50*time.Millisecond {
		budget = 50 * time.Millisecond
	}

	client := iapclient.New(iapURL, key, budget)
	v, err := openshell.NewIntentVerifier(client,
		openshell.StaticIdentity{AgentID: agentID, UserEmail: userEmail},
		unnamedAction,
		budget/2,
	)
	if err != nil {
		log.Fatalf("verifier: %v", err)
	}

	r := runtime{verifier: v, client: client}
	if audit {
		r.audit = iapclient.NewAuditBuffer(client, auditBufferSize)
		v.WithAudit(r.audit)
	}
	r.mode = fmt.Sprintf("enforcing via %s, fallback agent %q, unnamed traffic %s, enforce budget %s, audit %v",
		iapURL, agentID, unnamed, budget, audit)
	return r
}
