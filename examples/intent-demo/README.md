# Intent demo

A sandbox asks its model what to do, makes the call the model asked for, then makes one
the model did not ask for. The first is decided by policy. The second is refused with
`intent_drift_from_plan` before policy is asked, even though the sandbox declared that
tool and policy would allow it.

Needs OpenShell v0.1.0 or later, and the middleware run with `-capture-plan`.

| file | what it is |
| --- | --- |
| `model.py` | a stand-in model: an OpenAI-shaped chat completion that asks for one tool, `github_list_issues`. Fixed, so every run is the same |
| `policy.yaml` | the sandbox policy: network rules for the model and the tool host, and the middleware attached to both with `model_hosts` naming the model |
| `agent.sh` | the agent as a script |

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
openshell sandbox exec --name intent --no-tty -- bash -c "$(cat agent.sh)" < /dev/null
```

Expected output:

```text
1. ask the model: list the open issues in armoriq/demo
   model asked for "name": "github_list_issues"
2. the call the model asked for
github_list_issues  http=200
3. a call the model did not ask for
github_create_issue  http=403
```

The sandbox reaches the host as `host.openshell.internal` when the gateway callback is an
IP address or `localhost`, which is the default on Linux. On Docker Desktop, point the
policy, `model_hosts` and `MODEL` at the host's LAN address instead.

The workload image needs `curl` at `/usr/bin/curl`, the path the policy allows.
