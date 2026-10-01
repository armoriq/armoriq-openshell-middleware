# Intent demo

A sandbox asks its model how to do a task and makes the call the model chose. Then it
makes one more call the model did not choose. That call is refused with
`intent_drift_from_plan` before policy is asked. Run a second task where the model does
choose that same tool, and the same call is decided by policy and allowed.

Same tool, same agent, same policy. The only difference is whether the model asked for
it, which is the difference between intent and policy.

| file | what it is |
| --- | --- |
| `model.py` | a stand-in model: an OpenAI-shaped chat completion that asks for one tool, chosen from the task. Fixed per task, so every run is the same |
| `policy.yaml` | the sandbox policy: network rules for the model and the tool host, and the middleware attached to both with `model_hosts` naming the model |
| `agent.sh` | the agent as a script. It takes the task as its argument; with `EXTRA` set it makes one call the model did not choose |

No call reaches GitHub. The tool calls are MCP-shaped `tools/call` requests sent to
`postman-echo.com`, which echoes them back, so the tool names are only names.

## What you need

- A Linux host with Docker and OpenShell v0.1.0 or later, gateway on the host with the
  Docker driver. Verified on v0.1.2.
- Go 1.24 or later and Python 3.
- An ArmorIQ API key, from an organization at
  [platform.armoriq.ai](https://platform.armoriq.ai).

## 1. Add the policy on the ArmorIQ platform

In the organization that issued the key:

```yaml
name: OpenShell intent demo
scope:
  target_type: org
default_action: block
rules:
  - tool: github_list_issues
    action: allow
  - tool: github_list_pull_requests
    action: allow
  - tool: github_create_issue
    action: block
```

`github_list_pull_requests` must be allowed. Otherwise the second task is refused by
policy, and the demo no longer shows the difference between intent and policy.

## 2. Start the stand-in model

From this directory:

```shell
python3 model.py 18090
```

## 3. Run the middleware with `-capture-plan`

From the repository root:

```shell
go build -o openshell-middleware ./cmd/openshell-middleware

ARMORIQ_API_KEY=<key> ./openshell-middleware \
  -listen 0.0.0.0:50071 \
  -iap-url https://api.armoriq.ai \
  -timeout 2500ms \
  -capture-plan
```

## 4. Register it with the gateway

In the gateway config, normally `~/.config/openshell/gateway.toml`:

```toml
[openshell]
version = 2

[[openshell.supervisor.middleware]]
name = "armoriq-intent"
grpc_endpoint = "http://172.17.0.1:50071"
allow_insecure_transport = true
max_payload_bytes = 262144
timeout = "3s"
```

`172.17.0.1` is the default Docker bridge address. Sandbox supervisors run inside
containers, so `127.0.0.1` would be their own loopback, not the host.

Restart the gateway so it loads the registration, then check that it negotiated:

```shell
openshell gateway info | grep -A1 "(supervisor-middleware)"
```

`armoriq-intent` should be listed with `(protocol 1.0)`, next to OpenShell's built-in
`openshell/regex`.

## 5. Create the sandbox

From this directory:

```shell
openshell sandbox create --name intent \
  --from ghcr.io/nvidia/openshell-community/sandboxes/base:latest \
  --policy policy.yaml --no-tty --detach -- sleep infinity < /dev/null

openshell sandbox list    # wait until intent is Ready
```

`--from` matters: the default image in v0.1.2 has no `curl`, and the policy allows
`/usr/bin/curl`.

## 6. Run the two tasks

`< /dev/null` matters: `sandbox exec` otherwise waits on stdin after the script
finishes.

```shell
openshell sandbox exec --name intent --no-tty -- env EXTRA=github_list_pull_requests \
  bash -c "$(cat agent.sh)" _ "list the open issues in armoriq/demo" < /dev/null

openshell sandbox exec --name intent --no-tty -- \
  bash -c "$(cat agent.sh)" _ "list the open pull requests in armoriq/demo" < /dev/null
```

Expected output, as run on OpenShell v0.1.2:

```text
task: list the open issues in armoriq/demo
the model chose: github_list_issues
   github_list_issues  http=200
a call the model did not choose: github_list_pull_requests
   github_list_pull_requests  http=403

task: list the open pull requests in armoriq/demo
the model chose: github_list_pull_requests
   github_list_pull_requests  http=200
```

## 7. Where the refusal is recorded

- The `403` body carries `"reason_code":"intent_drift_from_plan"`.
- The middleware's log shows the plan captured from each model reply, and the refusal
  with `[intent_drift_from_plan]` and the plan it was checked against.
- OpenShell's own audit trail records it as a policy decision:

  ```shell
  openshell logs intent --source sandbox -n 200 | grep engine:middleware
  ```

  The refused call carries `reason:middleware_denied:armoriq:intent_drift_from_plan`.

The refused call never reaches the control plane, so the ArmorIQ platform shows only the
two allowed decisions.

## Notes

- The sandbox reaches the host as `host.openshell.internal` when the gateway callback is
  an IP address or `localhost`, which is the default on Linux. On Docker Desktop, point
  the policy, `model_hosts` and `MODEL` at the host's LAN address instead.
- The stand-in replies uncompressed and unstreamed. A real model client usually asks for
  compressed replies, and OpenShell does not pass a compressed body to middleware, so
  the plan would not be read. See "Limits" in the [main README](../../README.md).
- Clean up with `openshell sandbox delete intent`.
