# Intent demo

A sandbox asks its model how to do a task and makes the call the model chose. Then it
makes one more call the model did not choose. That call is refused with
`intent_drift_from_plan` before policy is asked. Run a second task where the model does
choose that same tool, and the same call is decided by policy and allowed.

Same tool, same agent, same policy. The only difference is whether the model asked for
it, which is the difference between intent and policy.

Needs OpenShell v0.1.0 or later, and the middleware run with `-capture-plan`.

| file | what it is |
| --- | --- |
| `model.py` | a stand-in model: an OpenAI-shaped chat completion that asks for one tool, chosen from the task. Fixed per task, so every run is the same |
| `policy.yaml` | the sandbox policy: network rules for the model and the tool host, and the middleware attached to both with `model_hosts` naming the model |
| `agent.sh` | the agent as a script. It takes the task as its argument; with `EXTRA` set it makes one call the model did not choose |

## Run it

On the host, start the stand-in model, then the middleware with `-capture-plan`, then the
gateway:

```shell
python3 model.py 18090
```

Create the sandbox and run the agent. `< /dev/null` matters: `sandbox exec` otherwise
waits on stdin after the script finishes.

```shell
openshell sandbox create --name intent --policy policy.yaml --detach -- sleep infinity

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

The `403` body carries `"reason_code":"intent_drift_from_plan"`. Policy must allow
`github_list_pull_requests` for the agent, or the second task fails for a different
reason.

The sandbox reaches the host as `host.openshell.internal` when the gateway callback is an
IP address or `localhost`, which is the default on Linux. On Docker Desktop, point the
policy, `model_hosts` and `MODEL` at the host's LAN address instead.

The workload image needs `curl` at `/usr/bin/curl`, the path the policy allows.
