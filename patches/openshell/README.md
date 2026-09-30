# OpenShell patch: dispatch HTTP responses to PRE_RETURN middleware

This is the patch the "with patches" demo runs on. It is not upstream, and it is
not a proposal for how OpenShell should implement response dispatch. It is the
smallest change that lets a middleware see a response before the sandbox does, so
the value of that can be shown on a real gateway.

Base: NVIDIA/OpenShell at `7d5b2e4` (`ci: consolidate release binary builds (#3405)`).

It is two files, one per crate, applied in order:

| file | crate | change |
| --- | --- | --- |
| `0001-middleware-dispatch-http-response-pre-return.patch` | `openshell-supervisor-middleware` | 3 files, 516 added, 11 removed |
| `0002-network-hold-responses-for-pre-return-stages.patch` | `openshell-supervisor-network` | 4 files, 624 added |

```shell
git -C openshell checkout 7d5b2e4
git -C openshell apply ../armoriq-openshell-middleware/patches/openshell/0001-middleware-dispatch-http-response-pre-return.patch
git -C openshell apply ../armoriq-openshell-middleware/patches/openshell/0002-network-hold-responses-for-pre-return-stages.patch
```

`0002` uses what `0001` adds, so it does not apply or build on its own.

## What stock OpenShell already has

The contract is complete in `proto/supervisor_middleware.proto`: the
`HttpResponsePreReturn` service, the `HTTP_RESPONSE` operation, the `PRE_RETURN`
phase, and every preflight, body, transform and trailer message. The gRPC client is
constructed on the same channel as the request client, and
`RemoteMiddlewareService::open_http_response_pre_return` makes a real call.
`ChainRunner::describe_http_response_chain` already selects stages for the binding.

## What 0001 adds

1. `supported_binding` accepts `HTTP_RESPONSE/PRE_RETURN` instead of rejecting it as
   not yet supported. The test that guarded the rejection now asserts acceptance.
2. `GrpcMiddlewareService` and `MiddlewareDispatch` route
   `open_http_response_pre_return` through to the client that already exists. These
   were the two missing hops between the chain and that client.
3. `ChainRunner::observe_http_response`, in a new `response.rs`, drives one stream
   per stage: preflight, one whole body unit, trailers, session end.

Nothing calls the runner yet, so with only `0001` applied the gateway accepts a
response-bound middleware and never dispatches to it.

## What 0002 adds

`relay_response` holds a response when a response-bound stage is attached, offers it
to the stages through the runner, and only then writes it to the sandbox, byte for
byte. The REST and route-selected relay paths select the stages after the request
chain allows the request.

The hold is the part that matters for intent. If the response were written first and
observed after, the workload could act on the reply before the stage had read it,
and its next request would be judged against stale state.

## Scope

It implements the observe subset of the contract. A stage may skip, or inspect with
`WHOLE_BODY_BYTES` and pass the body through. Block delivery, transforms, header and
trailer mutations, and `STREAM_BYTES` are reported as stage failures and the response
is delivered unchanged. Because nothing is ever blocked, `on_error` is not applied on
this path.

A response is held only when it can be offered whole: a known length within the
stage limit, or a chunked body that turns out to fit. Encoded bodies, event streams,
and bodies over the limit are relayed exactly as before. A chunked body that crosses
the limit mid-read is delivered intact from what was already read.

Only the REST and route-selected relay paths are wired. The JSON-RPC, GraphQL,
passthrough and plain forward-proxy relays are unchanged and never hold a response.

With no middleware advertising the response binding, every path behaves exactly as
it does unpatched.

## Tests

`0001` flips the gate test and adds four for the runner: whole body before return,
skip sends no body, whole-body mode withheld over capacity, block delivery reported
not applied.

`0002` adds seven for the relay, including one that holds the stage mid-answer and
asserts the client has received nothing.

With only `0001` applied, the middleware crate passes 105 of 105 and the network
crate its existing 1252 of 1252.

With both applied, `cargo test --lib` passes 105 of 105 in the middleware crate and
1259 of 1259 in the network crate (the 1252 that existed before, plus the seven).
Clippy is clean on both with the workspace lints, and `openshell-supervisor` and
`openshell-server` build.

`src/response.rs` carries only an SPDX identifier, not NVIDIA's copyright line, since
it was not written by NVIDIA. OpenShell's license-header check will flag that if the
patch is ever taken further.
