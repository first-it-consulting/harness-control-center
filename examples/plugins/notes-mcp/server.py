"""A minimal MCP server over stdio (JSON-RPC 2.0, one message per line).

It keeps notes in memory, for as long as its job runs. No network.
"""
import json
import sys

NOTES = {}
TOOLS = [
    {"name": "add_note", "description": "Store a note and return its id.",
     "inputSchema": {"type": "object", "properties": {"text": {"type": "string"}}, "required": ["text"]}},
    {"name": "list_notes", "description": "List all notes.",
     "inputSchema": {"type": "object", "properties": {}}},
    {"name": "delete_note", "description": "Delete a note by id.",
     "inputSchema": {"type": "object", "properties": {"id": {"type": "integer"}}, "required": ["id"]}},
]


def text(s, error=False):
    return {"content": [{"type": "text", "text": s}], "isError": error}


def call(name, args):
    if name == "add_note":
        nid = len(NOTES) + 1
        NOTES[nid] = str(args.get("text", ""))
        return text(f"stored note {nid}")
    if name == "list_notes":
        return text("\n".join(f"{k}: {v}" for k, v in NOTES.items()) or "no notes")
    if name == "delete_note":
        nid = args.get("id")
        if NOTES.pop(nid, None) is None:
            return text(f"no note {nid}", error=True)
        return text(f"deleted note {nid}")
    return text(f"unknown tool {name}", error=True)


def handle(msg):
    method, params = msg.get("method"), msg.get("params") or {}
    if method == "initialize":
        return {"protocolVersion": params.get("protocolVersion", "2025-06-18"),
                "capabilities": {"tools": {}}, "serverInfo": {"name": "notes-mcp", "version": "0.1.0"}}
    if method == "ping":
        return {}
    if method == "tools/list":
        return {"tools": TOOLS}
    if method == "tools/call":
        return call(params.get("name"), params.get("arguments") or {})
    raise LookupError(method)


for line in sys.stdin:
    try:
        msg = json.loads(line)
    except ValueError:
        continue
    if "id" not in msg:
        continue  # a notification
    try:
        out = {"jsonrpc": "2.0", "id": msg["id"], "result": handle(msg)}
    except LookupError as e:
        out = {"jsonrpc": "2.0", "id": msg["id"], "error": {"code": -32601, "message": f"method not found: {e}"}}
    sys.stdout.write(json.dumps(out) + "\n")
    sys.stdout.flush()
