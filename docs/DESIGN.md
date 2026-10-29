# ContextCache design

## 1. The context window as a cache

An agent connected to hundreds of MCP tools pays for every tool schema it carries in its context, on every turn, and gets worse at picking the right tool as the list grows (RAG-MCP measured 13.62% selection accuracy among many distractors, 43.13% with retrieval first). ContextCache treats what enters the context as a caching problem with two inputs:

- **what this turn needs**, which only the current request reveals: answered by retrieval over tool descriptions;
- **what this session keeps needing**, which history reveals: answered by an admission-controlled cache of loaded tools with a token budget.

## 2. The gateway

`ccgw` is an MCP gateway written from the specification (`mcp/`, JSON-RPC 2.0; stdio and Streamable HTTP in JSON-response mode; protocol versions 2024-11-05 to 2025-11-25). It starts or connects to upstream MCP servers, lists their tools (following `nextCursor`), and namespaces them as `<upstream>__<tool>`.

Two modes:

- **all**: every upstream tool appears in `tools/list` (paginated). This is today's default behaviour of MCP clients and the baseline.
- **meta**: `tools/list` returns three tools plus whatever this session has loaded:
  - `search_tools(query, k)`: hybrid retrieval over tool names, descriptions and parameter names; returns names, one-line descriptions and each schema's token cost;
  - `load_tool(name)`: returns the full schema and offers the tool to the session's cache; if the set of listed tools changes the gateway sends `notifications/tools/list_changed`;
  - `call_tool(name, arguments)`: proxies to the upstream server. A listed tool can also be called by its own name.

Per-tenant authorisation (HTTP): a bearer token maps to a tenant, and a tenant sees, searches and calls only its upstreams. Every tool call is traced as one JSON line (session, tenant, tool, upstream, upstream latency, outcome), the shape an OpenTelemetry exporter or Coze Loop's trace ingestion would take.

## 3. Retrieval

- **BM25** over words, with camelCase and snake_case split (tool names are identifiers);
- **TF-IDF over character trigrams** with cosine similarity, robust to word forms ("factorials", "forecasted") and partial identifiers;
- **reciprocal rank fusion** of the two (k = 60);
- **dense** (`retrieve/dense.go`): no neural encoder could be downloaded, so a text is embedded with static 100-d GloVe vectors and smooth inverse frequency weighting (Arora et al., ICLR 2017: the weighted mean of word vectors, weight a / (a + p(w)), with the corpus' first principal component removed), compared by cosine. The same vectors are stored in Postgres with pgvector (HNSW, m = 16, ef_construction = 64) to measure an index against exact search (`retrieve/pgvector.go`);
- **hybrid + dense**: reciprocal rank fusion of BM25, trigrams and dense.

Result: dense alone loses to BM25 at every N, and adding it to the hybrid changes recall@5 by under a point. Tool names and descriptions are identifier-heavy and queries usually share exact terms with the right tool, which favours lexical matching over an averaged word vector. A modern sentence encoder plugs into the same `Ranker` interface and pgvector store; this result is about static vectors only.

## 4. Admission: why W-TinyLFU and not LRU

The cache's capacity is a token budget and each item costs its schema's tokens. Tool usage in a session is skewed (a few tools every few turns) with one-off scans (a tool used once for an unusual request). LRU admits every one-off tool and evicts a frequently used one to make room. W-TinyLFU (Einziger et al. 2017; Caffeine) keeps a small LRU window for new items and admits a candidate from the window into the main segment only if a count-min sketch says it is used more often than the items it would evict; the sketch is halved periodically so popularity can change. Here it is cost-aware: a candidate must beat every victim needed to make room.

## 5. Benchmarks

### Tool overload (needle)

Tools and queries come from the Berkeley Function Calling Leaderboard (BFCL; categories simple, multiple, live_simple and live_multiple): every function definition is a tool, every question whose single ground-truth call names one of its functions is a query with a known correct tool. For each query and each N in 10 ... all, the correct tool plus N-1 random distractors are indexed and ranked. Reported: recall@k and MRR per ranker, prompt tokens with all N schemas versus the top-k schemas versus the meta-tool overhead (cl100k tokens of the tool JSON as sent to a model).

This measures the retrieval stage. It does not measure an LLM's final selection accuracy, which needs a model; recall@k is an upper bound on selection accuracy for an agent that only sees the top k.

### Session cache

A 30,000-turn session: each turn needs one tool, drawn with Zipf(1.0) popularity over the tools that have queries, and comes with one of that tool's real BFCL queries; every 5,000 turns ten of the 50 most popular tools swap with random ones (trends change). For a token budget B, a strategy puts schemas in context before the turn; a hit means the needed tool was there. Strategies: retrieval only (fill B with top hits), cache only (LRU, LFU, W-TinyLFU), and retrieval plus a cache, half the budget each.

### Gateway overhead

The same `tools/call` straight to one upstream server and through the gateway in front of 10 upstreams (all over stdio, one process each), sequential and with 8 concurrent callers; `search_tools` over the whole catalog; `load_tool`. Also the tokens of `tools/list` in all mode versus meta mode.

### Conformance

`bench/conformance.py` drives the gateway with the official MCP Python SDK as the client, over stdio (meta and all modes) and Streamable HTTP with two tenants.

### MemRevise

See `memory/memrevise.go`. Per user: a year of 3,000 events (55% chatter that shares vocabulary with the facts, the rest sets, retroactive corrections and deletions over 30 attributes such as "optimisation goal for eu-fra during auctions"), then 100 queries of four kinds:

| kind | question | correct answer |
|---|---|---|
| current | what is X now | the latest valid value |
| deleted | what is X now, after X was deleted | NONE |
| as_of | what was X on date D | the value valid at D, given everything known now (including later corrections) |
| as_recorded | on date D, what did we have on record for X | the value valid at D according to what had been recorded by D |

Answers come from an independent reference replay (per-attribute valid-time timelines rebuilt for each transaction time), which shares no code with the stores. Stores get structured payloads, so no extraction or reading by a model is involved: the benchmark isolates storage and retrieval semantics.

Stores: bi-temporal (in memory and in Postgres), latest-value key-value, a timestamped log that treats record time as valid time, and similarity retrieval over memory texts (top 1, or most recent of the top 5) without temporal fields, once with BM25 and once with the dense GloVe SIF vectors. The similarity stores answer with the value of the retrieved event if it sets or corrects a value, and NONE otherwise.

## 6. Bi-temporal model

Every version of a fact is a row with a valid-time interval `[valid_from, valid_to)` (when it was true in the world) and a transaction-time interval `[recorded_at, superseded_at)` (when the system believed it). A change at valid time `c` (a set, a correction dated in the past, or a delete) closes every current row still valid at or after `c` by setting `superseded_at`, re-inserts the part before `c` as a new version, and (unless it is a delete) inserts the new value valid from `c`. Nothing else is ever updated. Queries pick the row whose two intervals contain the asked valid time and transaction time. In Postgres the change is one transaction with a data-modifying CTE (`UPDATE ... RETURNING` feeding `INSERT ... SELECT`).

Why bi-temporal: "what was true on March 1" and "what did we believe on March 1" are different questions once corrections exist, and an agent that explains a past decision needs the second. Deletions are honoured because a deleted fact simply has no row valid now; old text that is still similar to the query cannot resurrect it, unlike similarity retrieval over memory texts.

## 7. Interview defence notes

- *Why bi-temporal?* Corrections. Without valid time, a correction recorded today rewrites history as of today; without transaction time, you cannot reproduce what the agent knew when it acted.
- *How do you keep deleted facts deleted?* A delete closes the fact's valid interval at the delete time; queries filter by interval, not by similarity. The deletion is itself recorded (transaction time), so "what did we have on record before the delete" still works. The dense similarity baseline shows the failure mode: the old statement of a deleted value stays the nearest neighbour, and only 19% of deleted-fact queries came back empty.
- *Admission policy, and why not LRU?* Frequency matters more than recency for tools; one-off tools must not evict tools used every few turns. W-TinyLFU gives frequency-based admission with bounded memory and aging.
- *How is MemRevise different from LoCoMo and LongMemEval?* It targets revision semantics (current versus past versus as-recorded versus deleted) with exact ground truth from a reference replay, not single final values judged by a model; its histories are long enough (tens of thousands of tokens per user) that putting everything in context stops being an option at scale, and its distractors share vocabulary with the facts.
