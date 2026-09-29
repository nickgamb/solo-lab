#!/usr/bin/env python3
"""Minimal MCP-over-HTTP probe (stdlib only) for verify scripts.

  mcp-probe.py URL list                  [--token T] [--header name=value]
  mcp-probe.py URL call TOOL '{"a":1}'   [--token T] [--header name=value]

A token or header value of "-" is read from stdin, one line each in the
order they appear, so credentials stay off the command line.

Speaks the session protocol (initialize -> Mcp-Session-Id) that agentgateway
serves, and falls back to the sessionless 2026-07-28 envelope when a server
rejects initialize. Prints one JSON line: {"http": code, "result"/"error": ...}.
"""
import json
import sys
import urllib.error
import urllib.request

args = sys.argv[1:]
token = None
extra = {}
stdin_value = lambda v: sys.stdin.readline().rstrip("\n") if v == "-" else v
if "--token" in args:
    i = args.index("--token"); token = stdin_value(args[i + 1]); del args[i:i + 2]
while "--header" in args:  # --header name=value (repeatable)
    i = args.index("--header"); k, v = args[i + 1].split("=", 1); extra[k] = stdin_value(v); del args[i:i + 2]
url, op, *rest = args
NEW = "2026-07-28"
META = {"io.modelcontextprotocol/protocolVersion": NEW,
        "io.modelcontextprotocol/clientCapabilities": {}}


def post(body, session=None, version=None, method=None, name=None):
    h = {"content-type": "application/json", "accept": "application/json, text/event-stream"}
    if token: h["authorization"] = f"Bearer {token}"
    if session: h["mcp-session-id"] = session
    if version: h["mcp-protocol-version"] = version
    if method: h["mcp-method"] = method
    if name: h["mcp-name"] = name
    h.update(extra)
    req = urllib.request.Request(url, json.dumps(body).encode(), h, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            return r.status, r.headers, r.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.headers, e.read().decode()
    except (urllib.error.URLError, ConnectionError, TimeoutError) as e:
        # A refused/reset connection is an answer too (in the mesh: an L4 deny).
        print(json.dumps({"http": 0, "error": f"connection failed: {e}"}))
        sys.exit(0)


def parse(text):
    for line in text.splitlines():
        if line.startswith("data:"):
            text = line[5:].strip()
    try:
        return json.loads(text)
    except Exception:
        return {"raw": text[:300]}


if op == "list":
    method, params, name = "tools/list", {}, None
else:
    method, name = "tools/call", rest[0]
    params = {"name": rest[0], "arguments": json.loads(rest[1]) if len(rest) > 1 else {}}

code, hdrs, text = post({"jsonrpc": "2.0", "id": 0, "method": "initialize", "params": {
    "protocolVersion": "2025-06-18", "capabilities": {},
    "clientInfo": {"name": "lab-probe", "version": "1"}}}, method="initialize")
session = hdrs.get("mcp-session-id") if code == 200 else None
if code == 200:
    ver = parse(text).get("result", {}).get("protocolVersion", "2025-06-18")
    post({"jsonrpc": "2.0", "method": "notifications/initialized"}, session, ver, "notifications/initialized")
    code, _, text = post({"jsonrpc": "2.0", "id": 1, "method": method, "params": params},
                         session, ver, method, name)
elif code in (401, 403):
    print(json.dumps({"http": code, "error": text[:300]})); sys.exit(0)
else:
    params["_meta"] = META
    code, _, text = post({"jsonrpc": "2.0", "id": 1, "method": method, "params": params},
                         None, NEW, method, name)
out = parse(text)
res = out.get("result", out)
if isinstance(res, dict) and "tools" in res:
    res = [t["name"] for t in res["tools"]]
elif isinstance(res, dict) and "content" in res:
    res = {"isError": res.get("isError", False),
           "text": " ".join(c.get("text", "") for c in res["content"])[:600]}
print(json.dumps({"http": code, "result": res}))
