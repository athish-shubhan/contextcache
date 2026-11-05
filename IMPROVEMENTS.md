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

## Round 2 (2026-11-05): overload benchmark reproduced, ranker and catalog tests, bounded HTTP sessions

Stacked on round 1 (`improve/round-1`).

### What changed

1. **Tests for the rankers behind the headline recall numbers** (`retrieve/`): BM25 scores checked against a hand
   calculation (k1 = 1.2, b = 0.75, the exact idf formula), including length normalisation and duplicate query
   terms; trigram cosine is 1 for an identical document and in (0, 1] otherwise; reciprocal-rank fusion scores
   are exactly `sum 1/(K + rank)`, ties ordered by ID, `Depth` and `k` respected; tokenizer edge cases; empty
   indexes.
2. **Tests for `catalog/`** (0% before): schema fix-ups, BFCL loading on a synthetic data directory (newest file
   version only, `simple` does not pick up `live_simple`, one tool per name, answers mapped, irrelevance queries
   kept with no tool, missing category is an error), Save/Load round trip, and a check that the committed
   `data/catalog.json` has unique tool ids, every needle query points at a tool, and the counts match the README
   (1,520 tools, 1,911 needle queries).
3. **Bug: `fixSchema` skipped parameters named `type`.** It treated every key `"type"` as a type and did not walk
   into it, so inside `properties` a parameter called `type` kept BFCL's `dict` / `float` / `tuple` / `list`
   types. Only a string-valued `"type"` is treated as a type now. The committed catalog has no affected tool
   (checked: 0 unconverted types), so no result changes.
4. **Bounded HTTP sessions.** Clients need not `DELETE` their session, so every `initialize` stayed in memory for
   the life of the gateway. `mcp.NewHTTPHandler` takes `HTTPOptions{IdleTimeout, MaxSessions}`; `HTTPHandler`
   (used by `ccgw`) defaults to 30 idle minutes and 10,000 sessions. Expired sessions are reported to the
   handler's `Closed`; an `initialize` over the cap gets 503. Tested with a fake clock.

### How it was verified

Overload benchmark (README "Tool overload" table) re-run from the committed catalog, no download needed for the
lexical rankers:

```
$ go build -o /tmp/ccbench ./cmd/ccbench
$ time /tmp/ccbench overload -catalog data/catalog.json -out /tmp/overload.json
real	12m26.722s
N=1520 bm25     R@1 0.567 R@5 0.797 MRR 0.670 tokens all 187421 top5 675
N=1520 trigram  R@1 0.520 R@5 0.779 MRR 0.634 tokens all 187421 top5 634
N=1520 hybrid   R@1 0.571 R@5 0.807 MRR 0.678 tokens all 187421 top5 662
```

Compared with `results/overload.json`: 21 rows (7 catalog sizes x BM25, trigram, hybrid), 105 fields, **0
differences**. Recall@5 at 100 / 500 / 1,520 tools: BM25 94.1 / 88.4 / 79.7%, trigram 94.2 / 87.1 / 77.9%,
hybrid 95.2 / 89.8 / 80.7%, all as in the README; 187,421 tokens to list every tool, as in the README. This round
changes no ranker code, so the result holds for this branch.

```
$ go test -count=1 -v ./... | grep -E '^--- ' | awk '{print $2}' | sort | uniq -c    # round 1 -> this round
     23 PASS: -> 33 PASS:
$ go test -race -count=1 ./...      # all ok
$ golangci-lint run ./...           # 0 issues
```

Coverage: `catalog` 0% -> 92.1%, `mcp` 83.4% -> 85.3%, `retrieve` 37.0% -> 39.0% (the rest of `retrieve` is the
GloVe and pgvector code). The new `TestFixSchema` on the old `fixSchema` leaves `type:map[... n:map[type:float]] type:dict]`.

### Not verified

The dense (GloVe) and hybrid+dense columns, pgvector, skills, cache, latency and conformance benchmarks were not
re-run (GloVe download, pgvector and the MCP Python SDK needed).

### Files touched

`retrieve/retrieve_test.go`, `catalog/catalog.go`, `catalog/catalog_test.go` (new), `mcp/server.go`, `mcp/mcp_test.go`,
`IMPROVEMENTS.md`.

## Round 3 (2026-11-05): dense columns reproduced; dense retriever edge cases fixed; cache invariants

Stacked on round 2 (`improve/round-2`).

### What changed

1. **The dense (GloVe) columns of the overload benchmark now reproduce.** Rounds 1 and 2 could not re-run them.
   With `glove-wiki-gigaword-100` (the file `bench/run_all.sh` downloads, 134 MB), round 2's code reproduces the
   committed `results/overload.json` **exactly**: all 35 rows (7 catalog sizes x 5 rankers), every recall@k, MRR
   and token field.
2. **Bug: the dense ranker padded its results with uninformative hits.** When no query word is in the GloVe
   vocabulary the query vector is zero, and `Dense.Rank` returned every document with score 0, in ID order; it
   also returned documents with no known word (zero vectors) at score 0, ranked above negatively correlated ones.
   In the dense hybrid those became fused votes for alphabetically early tools. Both are now left out, as BM25
   leaves out documents that share no term. With 10 tools this was visible: dense recall@10 was 100% only because
   all 10 tools were always returned; 12 of the 1,911 queries (0.6%) have no word GloVe knows.
3. **Bug: `Fit` could make every embedding NaN.** Power iteration divided by the norm of the update; when it was 0
   (data orthogonal to the start vector) the principal component, and with it every embedding, became NaN. It now
   stops instead. `LoadGloVe` reported nothing for a malformed number and loaded 0; it now fails with the line.
4. **Tests**: `retrieve/dense_test.go` (gzipped and plain GloVe with a header, vocabulary filtering keeping file
   ranks, malformed number, empty file, ranking, unknown-word queries, zero-vector documents, unit norm with the
   fitted direction removed, embedding cache, the degenerate `Fit`, the dense hybrid); `admit`: invariants for
   LRU, LFU and W-TinyLFU on random streams (usage equals the sum of held costs and stays within budget, no
   duplicates, `Contains` agrees with `Items`, `Access` reports whether the item was held, an item larger than the
   budget is never held). No admission bug was found.

### Re-measured results (item 2 changes them)

```
$ go build -o /tmp/ccbench ./cmd/ccbench
$ /tmp/ccbench overload -catalog data/catalog.json -glove glove-wiki-gigaword-100.gz -out /tmp/overload.json
```

Round 2's code vs the committed file: 0 of 35 rows differ. This round vs the committed file: the dense and
hybrid+dense rows change, BM25 / trigram / hybrid rows are identical. `results/overload.json` is replaced by this
run and the 14 affected rows of `results/SUMMARY.md` were rewritten with `plot.py`'s format (matplotlib is not
installed here, so `results/overload.png` was not regenerated; it plots recall@5, which moves by at most 0.3
points at 10 and 50 tools). Old -> new, recall@1 / @5 / @10:

| N | ranker | old | new |
|---|---|---|---|
| 10 | dense | 90.6 / 98.6 / 100.0% | 90.4 / 98.3 / 99.4% |
| 10 | hybrid+dense | 94.8 / 99.0 / 100.0% | 94.7 / 98.6 / 99.4% |
| 50 | dense | 79.3 / 94.0 / 96.4% | 79.2 / 93.9 / 96.3% |
| 50 | hybrid+dense | 88.9 / 96.7 / 97.4% | 88.9 / 96.6 / 97.3% |
| 100 | dense | 72.5 / 91.3 / 94.2% | 72.5 / 91.3 / 94.1% |
| 100 | hybrid+dense | 84.7 / 95.7 / 96.8% | 84.7 / 95.7 / 96.7% |
| 200 | dense, hybrid+dense | recall@10 91.7%, 95.9% | 91.6%, 95.8% |
| 500 to 1,520 | dense, hybrid+dense | | recall unchanged; top-5 and search-result tokens 3 to 4 lower |

**No number in the README changes** (its dense numbers are recall@5 at 100 / 500 / 1,520 and the 1,520-tool
comparisons, all unchanged; its token column is the hybrid ranker's).

### How it was verified

```
$ go test -count=1 -v ./... | grep -E '^--- ' | awk '{print $2}' | sort | uniq -c    # round 2 -> this round
     33 PASS: -> 38 PASS:
$ go test -race -count=1 ./... ; golangci-lint run ./...    # all ok; 0 issues
```

Coverage: `retrieve` 39.0% -> 81.7%, `admit` 78.8% -> 90.5%. On round 2's `dense.go` the new tests fail:
`a malformed number must be an error naming the line: <nil>`,
`hits [{weather.forecast 0.21} {empty 0} {finance.convert -0.21}]`, `Fit produced NaN embeddings`.

### Not verified

The pgvector benchmark uses `NewDense` as its exact reference and was not re-run (no pgvector here); its overlap
figures could shift by the same small amount.

### Files touched

`retrieve/dense.go`, `retrieve/dense_test.go` (new), `admit/admit_test.go`, `results/overload.json`,
`results/SUMMARY.md`, `IMPROVEMENTS.md`.

## Round 4 (2026-11-05): pgvector benchmark reproduced; both Postgres stores tested in CI

Stacked on round 3 (`improve/round-3`).

### What changed

1. **pgvector benchmark reproduced.** With pgvector 0.6.0 (the README's version, apt package `postgresql-16-pgvector`) on Postgres 16, the README's pgvector paragraph holds:

   | | committed `results/pgvector.json` | round 2 code | round 3 code | this round |
   |---|---|---|---|---|
   | HNSW top-10 overlap with exact search | 99.18% | 99.17% | 99.18% | 99.17% |
   | recall@5, pgvector / exact | 65.04% / 65.25% | 65.10% / 65.25% | 65.10% / 65.25% | 65.10% / 65.25% |
   | query p50 / p99 | 0.61 / 1.80 ms | 0.67 / 1.16 ms | 0.72 / 1.21 ms | 0.64 / 1.41 ms |
   | in-memory exact p50 | 0.49 ms | 0.39 ms | 0.45 ms | 0.40 ms |

   Exact recall is identical; pgvector recall differs by 0.06 points because HNSW construction is not
   deterministic; latencies are this VM's. `results/pgvector.json` is left as committed.
2. **`PGVector` hardened.** The table name is formatted into SQL; it must now be a plain identifier (a name like
   `tools; DROP TABLE x` is refused). `Insert` checks each vector's dimension, skips zero vectors (cosine distance to
   them is undefined; round 3 made the in-memory ranker skip them, so both now rank the same set) and returns how
   many it stored; `Search` returns nothing for a zero query; `Statement.Close` errors are checked. In this catalog
   all 1,520 tool vectors are non-zero, so the benchmark above is unaffected (`vectors_stored: 1520`).
3. **Both Postgres code paths are now tested, and CI runs them.** `retrieve/pgvector_test.go` (insert, index,
   cosine ranking, zero query, wrong dimension, unsafe table names) and `memory/pg_test.go`, which replays a
   generated MemRevise dataset (3 users, 1,200 events, 180 queries) into the Postgres bi-temporal store and the
   in-memory one and requires both to give the reference answer to every query. They run when
   `CONTEXTCACHE_TEST_DSN` is set; CI now starts a `pgvector/pgvector:pg16` service and sets it.

### How it was verified

```
$ go build -o /tmp/ccbench ./cmd/ccbench
$ /tmp/ccbench pgvector -catalog data/catalog.json -glove glove-wiki-gigaword-100.gz -dsn postgres://...:55432/contextcache
pgvector: overlap 0.992, R@5 0.651 (exact 0.653), p50 0.64 ms
$ go test -count=1 -v ./... | grep -E '^--- ' | awk '{print $2}' | sort | uniq -c                          # round 3 -> this round
     38 PASS: -> 39 PASS:, 2 SKIP:
$ CONTEXTCACHE_TEST_DSN=postgres://...:55432/contextcache go test -count=1 -v ./... | grep -E '^--- ' | ...  # with Postgres 16 + pgvector 0.6.0
     41 PASS:
$ go test -race -count=1 ./... ; golangci-lint run ./...    # all ok; 0 issues
```

### Not verified

The skills, cache, latency and conformance benchmarks were not re-run (the conformance check needs the MCP Python
SDK; the others only had error-handling changes in round 1).

### Files touched

`retrieve/pgvector.go`, `retrieve/pgvector_test.go` (new), `memory/pg_test.go` (new), `cmd/ccbench/pgvector.go`,
`.github/workflows/ci.yml`, `IMPROVEMENTS.md`.
