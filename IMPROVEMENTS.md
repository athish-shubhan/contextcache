# Improvements log

## Round 1 (2026-11-05): tests for the MCP layer and gateway, tenant and timeout fixes, CI

### What changed

1. **Tests for the two packages that had none.** `mcp/` (protocol, stdio and Streamable HTTP transports, client)
   and `gateway/` (meta-tools, loaded-tool cache, tenants, tracing) had 0% coverage although they are what the
   README's conformance and latency numbers are about. New tests run real `ServeStdio` / `HTTPHandler` servers over
   in-memory pipes and `httptest`:
   - `mcp/mcp_test.go`: version negotiation, initialize, `tools/list` pagination over 250 tools, `tools/call`,
     50 concurrent calls multiplexed by id, server notifications reaching the client, parse errors answered with
     id `null`, notifications not answered, context cancellation, HTTP sessions (issue, reuse, 404 without id,
     401 for bad or missing token, 403 for another tenant, 202 for notifications, 400 for bad JSON, DELETE).
   - `gateway/gateway_test.go`: search -> load -> call through two upstreams, `list_changed` sent once per change,
     upstream receives the original (un-namespaced) name, trace line contents, a tool bigger than the budget is
     returned but not listed, tenant isolation for search, load, call_tool and direct `tools/call` (meta and all
     modes), all-mode pagination and bad cursors, bad meta-tool arguments.
2. **Bug: any tenant could end another tenant's HTTP session.** `DELETE` with someone else's `Mcp-Session-Id`
   returned 200 and closed the session; POST already checked the tenant, DELETE did not. It now returns 403, and
   DELETE of an unknown session returns 404 instead of 200.
3. **Bug: `Client.ListTools` looped forever** if an upstream kept returning the same `nextCursor`; the gateway
   would hang at start-up. A repeated cursor is now an error.
4. **No timeout on upstream tool calls.** Over stdio the request context never ends, so one hung upstream tool
   blocked its caller forever. `gateway.Config.CallTimeout` (CLI `-call-timeout`, default 60s, 0 = none) turns
   that into a tool error. The HTTP server now sets `ReadHeaderTimeout` and `IdleTimeout`.
5. **`ccgw -tenants` parsing.** A trailing `;` created an entry with an empty token, so requests with no
   `Authorization` header were authenticated (as an empty tenant with no tools). Entries are now validated
   (token, tenant and at least one upstream required, duplicate tokens rejected) and error messages do not echo
   tokens. `-mode` other than `all`/`meta` is rejected.
6. Smaller fixes found by the linter: `toolfarm` panicked on an out-of-range `tools/list` cursor (now
   InvalidParams); `memrevise` ignored write and close errors on the dataset files; `ccbench` ignored flag-parse and
   warm-up errors; one dead assignment and one unused field removed.
7. **CI** (`.github/workflows/ci.yml`): gofmt, `go vet`, `go test -race`, a stdio smoke test of the real `ccgw` and
   `toolfarm` binaries on the committed catalog, golangci-lint v2.5.0 (`.golangci.yml`: default linters, errcheck
   off in tests and for Close/Print).
8. **README**: a Quick start that runs from a clean clone with no BFCL download, Postgres or model.

### Why it matters

Tenant isolation and the meta-tool flow are the gateway's main claims and had no tests; two of the bugs above
(cross-tenant DELETE, unbounded upstream calls) are the kind a reviewer of a multi-tenant gateway looks for first.

### How it was verified

```
$ go test -count=1 -v ./... | grep -E '^--- ' | awk '{print $2}' | sort | uniq -c    # main
      7 PASS:
$ go test -count=1 -v ./... | grep -E '^--- ' | awk '{print $2}' | sort | uniq -c    # this branch
     23 PASS:
$ go test -race -count=1 ./...          # all packages ok
$ golangci-lint run ./...               # main: 32 issues; branch: 0 issues
$ go vet ./... && gofmt -l .            # clean
```

Coverage (`go test -cover ./...`): `mcp` 0% -> 83.4%, `gateway` 0% -> 95.2%, `cmd/ccgw` 0% -> 26.1%;
`memory` 85.2% -> 84.9% and `retrieve` 37.1% -> 37.0% (one uncovered `defer` closure each).

New tests against main's `mcp/` and `gateway/` code:

```
--- FAIL: TestListToolsRejectsRepeatedCursor (5.00s)
    mcp_test.go:203: a server repeating its cursor must not loop forever: context deadline exceeded
--- FAIL: TestHTTPSessionsAndTenants (0.00s)
    mcp_test.go:297: cross-tenant DELETE: 200
gateway/gateway_test.go:300:48: unknown field CallTimeout in struct literal of type Config
```

End to end with the real binaries (the command in the README Quick start):

```
$ ... | bin/ccgw -farms 2 -mode meta
{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25", ... "This gateway fronts 1520 tools. ..."}}
{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"{\"results\":[{\"name\":\"farm1__Weather_1_GetWeather\", ...
```

MemRevise touches `memory/` code changed here (a refactor to a `switch`, a dead assignment, `Rollback` error
handling), so I re-ran it, including the Postgres store against a local Postgres 16:

```
$ go build -o /tmp/memrevise ./cmd/memrevise
$ /tmp/memrevise -out /tmp/mr.json -data /tmp/mrdata -dsn 'postgres://postgres@127.0.0.1:55432/contextcache?sslmode=disable'
bi-temporal                          overall 1.000
key-value, latest only               overall 0.494
timestamped log (no valid time)      overall 0.860
similarity, top 1                    overall 0.181
similarity, most recent of top 5     overall 0.177
bi-temporal (Postgres)               overall 1.000
```

All six stores match `results/memrevise.json` exactly, per query kind; event and query counts and the
85,684-token median history also match.

### Not verified

- The two dense (GloVe) MemRevise rows and the overload, pgvector, skills, cache, latency and conformance
  benchmarks were not re-run: they need the BFCL checkout, the GloVe download, pgvector and the MCP Python SDK.
  None of their code paths changed except flag-parse and warm-up error checks in `ccbench`. No README number was
  changed.

### Files touched

`mcp/server.go`, `mcp/client.go`, `mcp/mcp_test.go` (new), `gateway/gateway.go`, `gateway/gateway_test.go` (new),
`cmd/ccgw/main.go`, `cmd/ccgw/main_test.go` (new), `cmd/toolfarm/main.go`, `cmd/memrevise/main.go`,
`cmd/ccbench/{cache,latency,overload,pgvector,skills}.go`, `memory/{memrevise,stores,pg}.go`, `retrieve/pgvector.go`,
`admit/admit.go`, `.github/workflows/ci.yml` (new), `.golangci.yml` (new), `README.md`, `IMPROVEMENTS.md` (new).
