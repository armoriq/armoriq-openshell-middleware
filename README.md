# ArmorIQ middleware for NVIDIA OpenShell

Tool-level enforcement for agents running in
[OpenShell](https://github.com/NVIDIA/openshell) sandboxes, as a supervisor middleware.
On OpenShell v0.1.0 and later, which pass HTTP responses to middleware, it also holds
each sandbox to the plan its model issued, which is what intent enforcement needs. See
[Holding a sandbox to its model's plan](#holding-a-sandbox-to-its-models-plan).

Nothing in OpenShell is modified. No fork, nothing installed inside the sandbox,
and nothing added to the agent. This registers through OpenShell's own middleware
contract and is called before a sandbox's request leaves.

**Any agent, any tool.** The agent is not modified and never learns this exists, so
its framework, language and runtime are irrelevant. Enforcement happens at the
connection OpenShell already owns, and the tool name is read off the wire, so any
tool your MCP servers expose works whatever it is called. The examples below use
placeholder tool names; substitute your own.

## What it does

OpenShell calls this service at `PRE_CREDENTIALS`, before credentials are injected,
for every outbound HTTP request a sandbox makes. It answers allow or deny, and
OpenShell enforces that answer.

Two checks run per request:

1. **Is this tool one the sandbox was set up to use?** The tool set is declared in
   the sandbox policy and checked in process, typically under ten microseconds. A
   tool outside that set is refused without consulting any policy.

   The tool name is read from the request body. For MCP that is
   `params.name` on a `tools/call`, so **any tool your MCP servers expose works**,
   whatever they are named. Nothing is hardcoded to a particular provider.
2. **Does policy permit this agent to call it?** Evaluated by the ArmorIQ control
   plane, scoped to the agent the sandbox declares.

The first check is what makes this more than an authorization proxy. Asking whether
a policy permits a tool the sandbox never declared answers a different question from
the one being asked.

## Holding a sandbox to its model's plan

Both checks above are about what the sandbox may do. Neither sees what the agent set
out to do for the task in hand, because that first appears in the model's reply, which
arrives on a response.

With `-capture-plan`, the service also registers on the response path. It reads each
reply from the sandbox's model on its way back to the sandbox and records the tool calls
it asks for.
Every later request is checked against that plan as well, after the declared scope and
before policy:

| check | comes from | refuses |
| --- | --- | --- |
| declared scope | `declared_tools`, written once by whoever creates the sandbox | a tool the sandbox was never set up to use |
| captured plan | the model's latest reply, for this task | a tool the model did not ask for |
| policy | the ArmorIQ control plane | a tool the agent is not permitted to call |

A call the model did not ask for is refused with `intent_drift_from_plan`, without
consulting policy. A reply that asks for no tools is recorded as an empty plan, so a
call made after the model has finished is refused too. The reply is read before the
sandbox receives it, so the plan is in place before the agent can act on it.

**This needs OpenShell v0.1.0 or later**, the first release that dispatches
`HTTP_RESPONSE/PRE_RETURN`. Earlier gateways reject that binding at registration and then
refuse to start, so leave `-capture-plan` off on them.

List the model's hosts in `model_hosts`. A request to one of them is the agent asking
what to do, not a tool call, so it passes to OpenShell's network policy instead of being
refused as naming no tool. Plans are read only from replies those hosts send: a tool
server that returns a completion-shaped body cannot choose the plan. Include the model
host in the middleware's `endpoints.include` too, or its replies never reach the service.

Set `require_captured_plan: true` in a sandbox's middleware config to refuse tool calls
made before any model reply has been seen. Without it, a sandbox is held to its plan
once one exists, which is the safer default while rolling this out.

## Quick start

### 1. Build

```
go build -o openshell-middleware ./cmd/openshell-middleware
```

A single static binary, no runtime dependencies.

### 2. Get an API key

Create an organization at [platform.armoriq.ai](https://platform.armoriq.ai) and
issue an API key. The same organization is where policies are authored, so the key
and the policies that govern your agents live together.

A new organization has no policies and the default is to block, so add one before
your first run or every call is denied. A minimal policy that exercises both
decision paths, using two tool names as an example. **Substitute tools your own
agents actually call:**

```yaml
name: OpenShell trial
scope:
  target_type: org
default_action: block
rules:
  - tool: your_read_tool     # allowed, and declared by the sandbox below
    action: allow
  - tool: your_write_tool    # declared by the sandbox, but policy refuses it
    action: block
```

Then a third tool that the sandbox never declares at all shows the in-process
refusal, without a policy being consulted.

### 3. Run it

```
ARMORIQ_API_KEY=<key> ./openshell-middleware \
  -listen 0.0.0.0:50071 \
  -iap-url https://api.armoriq.ai \
  -timeout 2500ms
```

Start it **before** the gateway. OpenShell refuses to start when a registered
middleware is unavailable.

The key is read from the environment rather than a flag, because a flag is visible
in `ps` to every user on the host.

### 4. Register it with the gateway

In your gateway config, normally `~/.config/openshell/gateway.toml`:

```toml
[openshell]
version = 1

[[openshell.supervisor.middleware]]
name = "armoriq-intent"
grpc_endpoint = "http://172.17.0.1:50071"
allow_insecure_transport = true   # same host only; use TLS otherwise
max_payload_bytes = 262144
timeout = "3s"
```

**`grpc_endpoint` must be reachable from the gateway and from every sandbox
supervisor.** Supervisors run inside the container, so `127.0.0.1` there is the
container's own loopback and not your host. On a single box the Docker bridge
address works.

This is the most common setup mistake: the gateway connects fine and the deployment
looks healthy, while every sandbox request is denied with `binding_not_described`
because the supervisor could not reach the service.

**Raise the timeout above the default.** A policy decision takes roughly half a
second. The default binding timeout is 500ms and the failure mode is closed, so at
the default a real decision can be timed out and the request denied. The contract
accepts 10ms to 30s.

### 5. Attach it to a sandbox

In the sandbox policy:

```yaml
network_middlewares:
  armoriq:
    name: ArmorIQ intent enforcement
    middleware: armoriq-intent
    order: 10
    on_error: fail_closed
    config:
      agent_id: "<the agent this sandbox runs>"
      declared_tools: ["your_read_tool", "your_write_tool"]
      # model_hosts: ["api.openai.com"]  # with -capture-plan, see above
      # require_captured_plan: true      # only with -capture-plan
    endpoints:
      include: ["mcp.example.internal"]
```

`config` is delivered to the middleware on every request, so identity and the
declared tool set travel with the sandbox. No registration step and no lookup.

Apply it with `sandbox create --policy`. Some policy changes are refused on a live
sandbox, so creating with the policy avoids a class of errors unrelated to this
service.

## Flags

| flag | default | what it does |
| --- | --- | --- |
| `-listen` | `0.0.0.0:50071` | address to serve gRPC on |
| `-iap-url` | empty | control plane base url. Empty runs a skeleton verifier that decides nothing |
| `-unnamed` | `deny` | what to do with traffic that names no tool |
| `-agent-id` | empty | fallback agent for sandboxes whose policy declares none |
| `-timeout` | `450ms` | binding timeout to declare, between 10ms and 30s |
| `-max-body` | `262144` | largest body accepted buffered |
| `-audit` | `true` | record decisions the control plane does not see |
| `-capture-plan` | `false` | read model replies and hold each sandbox to the plan they issue. Needs OpenShell v0.1.0 or later |
| `-name` | `armoriq-intent` | manifest name, diagnostic only |
| `-deny` | `false` | deny everything, to prove the path end to end |
| `-dump-request` | `false` | log the whole evaluation proto |

## What the workload sees

Only a short reason code reaches the requester. Everything else stays in the logs.

| code | meaning |
| --- | --- |
| `intent_blocked_by_policy` | a policy refused it |
| `tool_not_in_plan` | not among the tools the sandbox declared |
| `intent_drift_from_plan` | not asked for by the model's latest reply, with `-capture-plan` |
| `intent_awaiting_approval` | the decision needs a human; the call does not proceed |
| `intent_plan_not_found` | the sandbox declares no agent, so no agent policy applies |
| `intent_action_not_named` | no tool could be identified in the request |
| `intent_verification_failed` | no decision could be obtained |

In the sandbox log these appear as `reason:middleware_denied:armoriq:<code>` with
`failed:false`, recorded as policy decisions rather than middleware errors.

## Timing

| path | measured |
| --- | --- |
| tool outside the declared set | 4 to 16 microseconds, never leaves the process |
| policy decision | roughly 300 to 650 milliseconds |
| first call from a cold process | the connection is opened at startup, so a TLS handshake is not on the critical path |

## Failure behaviour

**Control plane unreachable, slow, or the key is rejected.** The request is denied
with `intent_verification_failed`. This is a denial the service produces, not an
error it hands back, so it cannot be turned into an allow by setting
`on_error: fail_open`.

**The service itself is down.** OpenShell applies the binding's `on_error`. With
`fail_closed` the request is denied. Keep it `fail_closed`.

**Auditing fails.** Nothing happens to enforcement. Audit rows are buffered off the
decision path and a full buffer drops rows rather than delaying a decision.

## Limits worth knowing before you rely on it

- **It sees requests, not processes.** The evaluation carries no process identity,
  so nothing can be conditioned on which process inside the sandbox made the call.
- **The declared tool set is written by whoever creates the sandbox**, not declared
  by the agent. It is a scope boundary for the sandbox rather than a per task
  declaration.
- **Responses only with `-capture-plan` on v0.1.0 or later.** Without both, nothing
  returning to the sandbox is inspected. With them, model replies are read, never
  changed or blocked.
- **The plan is what the model asked for, not what the user asked for.** It catches an
  agent acting outside its own model's instructions. It does not catch a model that has
  itself been steered, for example by instructions injected into a tool result: the
  model's next reply then asks for the injected call, and that call is in the plan.
- **Replies are read when they are unencoded and not streamed.** OpenAI chat
  completions and Anthropic messages are understood. A gzip body or an event stream is
  delivered without its plan being read. The agent must name tools to the model the way
  it names them to its MCP server, which is what MCP clients do.
- **Traffic that names no tool** cannot be evaluated against a tool policy. The
  default is to deny it. `-unnamed allow` passes it through to your own network
  policy instead.
- **Your network policy runs first.** A request it rejects never reaches this
  service. It is an additional gate, not a replacement.

## Layout

```
cmd/openshell-middleware   the binary
pkg/openshell              the gRPC services and the decision logic
pkg/iapclient              the ArmorIQ control plane client
proto/openshell            OpenShell's middleware contract, vendored from the release in UPSTREAM_VERSION
```

The `Verifier` interface in `pkg/openshell` is the extension point if you want to
decide requests some other way while keeping the OpenShell plumbing.

## Verified against

OpenShell v0.1.2 as released, gateway and published supervisor image, Docker driver,
with the middleware registered over plaintext on the same host. The run in
[examples/intent-demo](examples/intent-demo) passes end to end: the model call passes,
its plan is captured from the reply, the call it asked for is decided by policy, and the
one it did not ask for is refused with `intent_drift_from_plan` without reaching policy.

Request-side enforcement was earlier verified on OpenShell 0.0.116 with gateway config
version 1.

## License

Apache 2.0. See [LICENSE](LICENSE).
