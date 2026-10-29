#!/usr/bin/env python3
"""Conformance check of the ContextCache gateway with the official MCP Python
SDK as the client (pip install mcp). Exercises stdio in meta and all modes,
and Streamable HTTP with per-tenant bearer tokens. Prints a JSON report;
exits 1 if any check fails."""
import asyncio
import json
import subprocess
import sys
import time

from mcp import ClientSession, StdioServerParameters
from mcp import types
from mcp.client.stdio import stdio_client

try:  # SDK 2.x
    import httpx2
    from mcp.client.streamable_http import streamable_http_client

    def http_client(url, token):
        return streamable_http_client(url, http_client=httpx2.AsyncClient(headers={"Authorization": "Bearer " + token}))
except ImportError:  # SDK 1.x
    from mcp.client.streamable_http import streamablehttp_client

    def http_client(url, token):
        return streamablehttp_client(url, headers={"Authorization": "Bearer " + token})

CAT = sys.argv[1] if len(sys.argv) > 1 else "data/catalog.json"
results = []


def g(obj, *names):
    """attribute across SDK versions (1.x camelCase, 2.x snake_case)"""
    for n in names:
        if hasattr(obj, n):
            return getattr(obj, n)
    return None


def is_error(r):
    return bool(g(r, "isError", "is_error"))


def check(name, ok, detail=""):
    results.append({"check": name, "ok": bool(ok), "detail": str(detail)[:200]})


async def list_all(s):
    tools, cursor = [], None
    while True:
        r = await (s.list_tools(params=types.PaginatedRequestParams(cursor=cursor)) if cursor else s.list_tools())
        tools += r.tools
        cursor = g(r, "nextCursor", "next_cursor")
        if not cursor:
            return tools


def payload(r):
    sc = g(r, "structuredContent", "structured_content")
    if sc:
        return sc
    return json.loads(r.content[0].text)


async def meta_stdio():
    p = StdioServerParameters(command="bin/ccgw", args=["-farms", "4", "-catalog", CAT, "-mode", "meta", "-budget", "4000"])
    async with stdio_client(p) as (r, w):
        async with ClientSession(r, w) as s:
            init = await s.initialize()
            info, ver = g(init, "serverInfo", "server_info"), g(init, "protocolVersion", "protocol_version")
            tools_cap = g(init.capabilities, "tools")
            check("stdio initialize: server name", info.name == "contextcache-gateway", info)
            check("stdio initialize: protocol version negotiated", ver in ("2025-11-25", "2025-06-18", "2025-03-26"), ver)
            check("stdio initialize: tools.listChanged", tools_cap and g(tools_cap, "listChanged", "list_changed"), init.capabilities)
            tools = await list_all(s)
            check("meta mode lists 3 meta-tools", sorted(t.name for t in tools) == ["call_tool", "load_tool", "search_tools"], [t.name for t in tools])
            r = await s.call_tool("search_tools", {"query": "convert an amount of money between currencies", "k": 5})
            hits = payload(r)["results"]
            check("search_tools returns 5 results", len(hits) == 5, hits[:1])
            name = hits[0]["name"]
            r = await s.call_tool("load_tool", {"name": name})
            loaded = payload(r)
            check("load_tool returns the full schema", loaded["tool"]["name"] == name and "inputSchema" in loaded["tool"], loaded)
            tools = await list_all(s)
            check("loaded tool is listed afterwards", (name in [t.name for t in tools]) == loaded["listed"], [t.name for t in tools])
            r = await s.call_tool("call_tool", {"name": name, "arguments": {"amount": 300}})
            check("call_tool proxies to the upstream server", not is_error(r) and '"ok":true' in r.content[0].text, r.content[0].text)
            r = await s.call_tool(name, {"amount": 1})
            check("loaded tool callable by its own name", not is_error(r), r.content[0].text)
            r = await s.call_tool("call_tool", {"name": "nope__nothing", "arguments": {}})
            check("unknown tool is a tool error, not a protocol error", is_error(r), r.content[0].text)


async def all_stdio():
    p = StdioServerParameters(command="bin/ccgw", args=["-farms", "4", "-catalog", CAT, "-mode", "all"])
    async with stdio_client(p) as (r, w):
        async with ClientSession(r, w) as s:
            await s.initialize()
            t0 = time.time()
            tools = await list_all(s)
            check("all mode: paginated tools/list returns the whole catalog", len(tools) > 1000, len(tools))
            check("all mode: tool names are unique", len({t.name for t in tools}) == len(tools), "")
            r = await s.call_tool(tools[0].name, {})
            check("all mode: call by namespaced name", not is_error(r), r.content[0].text)


async def http_tenants():
    gw = subprocess.Popen(["bin/ccgw", "-farms", "4", "-catalog", CAT, "-mode", "meta", "-http", "127.0.0.1:8091",
                           "-tenants", "tokA=acme:farm0,farm1;tokB=globex:farm2"], stderr=subprocess.DEVNULL)
    try:
        time.sleep(1.5)
        async with http_client("http://127.0.0.1:8091/mcp", "tokA") as streams:
            r, w = streams[0], streams[1]
            async with ClientSession(r, w) as s:
                await s.initialize()
                res = payload(await s.call_tool("search_tools", {"query": "weather forecast", "k": 10}))["results"]
                check("http: tenant acme only sees its upstreams", all(h["name"].split("__")[0] in ("farm0", "farm1") for h in res), [h["name"] for h in res])
                r2 = await s.call_tool("call_tool", {"name": "farm2__x", "arguments": {}})
                check("http: tenant acme cannot call another tenant's upstream", is_error(r2), r2.content[0].text)
        failed = False
        try:
            async with http_client("http://127.0.0.1:8091/mcp", "wrong") as streams:
                r, w = streams[0], streams[1]
                async with ClientSession(r, w) as s:
                    await asyncio.wait_for(s.initialize(), 5)
        except BaseException:
            failed = True
        check("http: bad token is rejected", failed, "")
    finally:
        gw.kill()


async def main():
    for f in (meta_stdio, all_stdio, http_tenants):
        try:
            await f()
        except BaseException as e:  # a crash in one group should not hide the others
            check(f.__name__ + " ran", False, repr(e))
    import importlib.metadata as md
    print(json.dumps({"mcp_sdk_version": md.version("mcp"), "checks": results}, indent=1))
    sys.exit(0 if all(r["ok"] for r in results) else 1)


asyncio.run(main())
