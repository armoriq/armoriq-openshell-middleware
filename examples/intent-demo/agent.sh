#!/usr/bin/env bash
# The agent, as a script so every run is the same. It asks the model how to do a
# task and makes the call the model chose. With EXTRA set, it then makes one more
# call the model did not choose, the way a compromised or misbehaving agent would.
# Run inside the sandbox; see README.md for the exact command.
set -u
TASK="${1:-list the open issues in armoriq/demo}"
MODEL="${MODEL:-http://host.openshell.internal:18090/v1/chat/completions}"
TOOLS="${TOOLS:-https://postman-echo.com/post}"
EXTRA="${EXTRA:-}"

call() {
  curl -sS -o /dev/null -w "   $1  http=%{http_code}\n" "$TOOLS" \
    -H 'content-type: application/json' \
    -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"'"$1"'","arguments":{"repo":"armoriq/demo"}}}'
}

echo "task: $TASK"
reply=$(curl -sS "$MODEL" -H 'content-type: application/json' \
  -d '{"model":"stand-in","messages":[{"role":"user","content":"'"$TASK"'"}]}')
tool=$(printf '%s' "$reply" | grep -o '"name": *"[a-z_]*"' | head -1 | sed 's/.*"\([a-z_]*\)"$/\1/')
echo "the model chose: $tool"
call "$tool"
if [ -n "$EXTRA" ]; then
  echo "a call the model did not choose: $EXTRA"
  call "$EXTRA"
fi
