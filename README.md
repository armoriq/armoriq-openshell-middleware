# ArmorIQ middleware for NVIDIA OpenShell

Intent enforcement for agents running in [OpenShell](https://github.com/NVIDIA/openshell)
sandboxes, as a supervisor middleware.

Nothing in OpenShell is modified. No fork, nothing installed inside the sandbox,
and nothing added to the agent. This registers through OpenShell's own middleware
contract and is called before a sandbox's request leaves.

## What it does

OpenShell calls this service at `PRE_CREDENTIALS`, before credentials are injected,
for every outbound HTTP request a sandbox makes. It answers allow or deny, and
OpenShell enforces that answer.

Two checks run per request:

1. **Is this tool one the sandbox was set up to use?** The tool set is declared in
   the sandbox policy and checked in process, typically under ten microseconds. A
   tool outside that set is refused without consulting any policy.
2. **Does policy permit this agent to call it?** Evaluated by the ArmorIQ control
   plane, scoped to the agent the sandbox declares.

The first check is what makes this more than an authorization proxy. Asking whether
a policy permits a tool the sandbox never declared answers a different question from
the one being asked.

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
decision paths:

```yaml
name: OpenShell trial
scope:
  target_type: org
default_action: block
rules:
  - tool: github_list_issues
    action: allow
  - tool: github_create_issue
    action: block
```

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
      declared_tools: ["github_list_issues", "github_create_issue"]
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
| `-name` | `armoriq-intent` | manifest name, diagnostic only |
| `-deny` | `false` | deny everything, to prove the path end to end |
| `-dump-request` | `false` | log the whole evaluation proto |

## What the workload sees

Only a short reason code reaches the requester. Everything else stays in the logs.

| code | meaning |
| --- | --- |
| `intent_blocked_by_policy` | a policy refused it |
| `tool_not_in_plan` | not among the tools the sandbox declared |
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
- **Requests only.** Responses are not inspected, so data returning to the sandbox
  is not covered.
- **Traffic that names no tool** cannot be evaluated against a tool policy. The
  default is to deny it. `-unnamed allow` passes it through to your own network
  policy instead.
- **Your network policy runs first.** A request it rejects never reaches this
  service. It is an additional gate, not a replacement.

## Layout

```
cmd/openshell-middleware   the binary
pkg/openshell              the gRPC service and the decision logic
pkg/iapclient              the ArmorIQ control plane client
proto/openshell            OpenShell's middleware contract, vendored
```

The `Verifier` interface in `pkg/openshell` is the extension point if you want to
decide requests some other way while keeping the OpenShell plumbing.

## Verified against

OpenShell gateway config version 1, with the middleware registered over plaintext
loopback on the same host, sandboxes created through `sandbox create --policy`, and
MCP `tools/call` traffic over HTTP.

## License

Apache 2.0. See [LICENSE](LICENSE).
