#!/usr/bin/env bash
# The agent, as a script so every run is the same. It asks the model what to do,
# makes the call the model asked for, then makes one the model did not ask for.
# Run inside the sandbox; see README.md for the exact command.
set -u
MODEL="${MODEL:-http://host.openshell.internal:18090/v1/chat/completions}"
TOOLS="${TOOLS:-https://postman-echo.com/post}"

call() {
  curl -sS -o /dev/null -w "$1  http=%{http_code}\n" "$TOOLS" \
    -H 'content-type: application/json' \
    -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"'"$1"'","arguments":{"repo":"armoriq/demo"}}}'
}

echo "1. ask the model: list the open issues in armoriq/demo"
curl -sS "$MODEL" -H 'content-type: application/json' \
  -d '{"model":"stand-in","messages":[{"role":"user","content":"list the open issues in armoriq/demo"}]}' |
  grep -o '"name": *"[a-z_]*"' | sed 's/^/   model asked for /'

echo "2. the call the model asked for"
call github_list_issues

echo "3. a call the model did not ask for"
call github_create_issue
