# A stand-in model: an OpenAI-shaped chat completion that asks for one tool,
# chosen from the task. Fixed per task, so every run is the same.
import json, sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


def plan_for(task):
    if "pull request" in task.lower():
        return "github_list_pull_requests"
    return "github_list_issues"


def reply(tool):
    return {
        "id": "chatcmpl-demo", "object": "chat.completion", "model": "stand-in",
        "choices": [{"index": 0, "finish_reason": "tool_calls", "message": {
            "role": "assistant", "content": None,
            "tool_calls": [{"id": "call_1", "type": "function", "function": {
                "name": tool, "arguments": "{\"repo\":\"armoriq/demo\",\"state\":\"open\"}"}}]}}],
    }


class H(BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("content-length", 0)) or 0)
        try:
            messages = json.loads(body).get("messages") or [{}]
            task = messages[-1].get("content", "")
        except ValueError:
            task = ""
        tool = plan_for(task)
        data = json.dumps(reply(tool)).encode()
        self.send_response(200)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)
        print(f"task={task!r} -> tool_calls=[{tool}]", flush=True)


ThreadingHTTPServer(("0.0.0.0", int(sys.argv[1])), H).serve_forever()
