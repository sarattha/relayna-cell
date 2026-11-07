"""Minimal OpenAI-compatible tool loop; all generated commands execute in Cell."""
import json
import subprocess
import sys
import urllib.request

endpoint, model = sys.argv[1:3]
request = urllib.request.Request(
    endpoint.rstrip("/") + "/chat/completions",
    data=json.dumps({
        "model": model,
        "messages": [{"role": "user", "content": "Use the shell tool to print hello from cell."}],
        "tools": [{"type": "function", "function": {
            "name": "shell", "description": "Execute a shell command inside the sandbox",
            "parameters": {"type": "object", "properties": {"command": {"type": "string"}}, "required": ["command"]},
        }}],
    }).encode(),
    headers={"Content-Type": "application/json"},
)
with urllib.request.urlopen(request, timeout=60) as response:
    message = json.load(response)["choices"][0]["message"]
for call in message.get("tool_calls", []):
    if call["function"]["name"] != "shell":
        raise ValueError("unsupported tool")
    command = json.loads(call["function"]["arguments"])["command"]
    print("Executing model tool:", command, flush=True)
    subprocess.run(["/bin/sh", "-c", command], check=True, timeout=30)
print("agent complete", flush=True)
