# A stand-in model: an OpenAI-shaped chat completion that asks for exactly one
# tool, github_list_issues. Fixed, so every run is identical.
import json, sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

REPLY = {
    "id": "chatcmpl-demo", "object": "chat.completion", "model": "stand-in",
    "choices": [{"index": 0, "finish_reason": "tool_calls", "message": {
        "role": "assistant", "content": None,
        "tool_calls": [{"id": "call_1", "type": "function", "function": {
            "name": "github_list_issues", "arguments": "{\"repo\":\"armoriq/demo\",\"state\":\"open\"}"}}]}}],
}

class H(BaseHTTPRequestHandler):
    def do_POST(self):
        self.rfile.read(int(self.headers.get("content-length", 0)) or 0)
        data = json.dumps(REPLY).encode()
        self.send_response(200); self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(data))); self.end_headers(); self.wfile.write(data)
        print("reply sent: tool_calls=[github_list_issues]", flush=True)

ThreadingHTTPServer(("0.0.0.0", int(sys.argv[1])), H).serve_forever()
