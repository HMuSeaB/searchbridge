# searchbridge

<p align="center"><a href="README.md">简体中文</a> • <b>English</b></p>

A local gateway that **adds native `web_search` to any Anthropic Messages-compatible endpoint**: multi-backend search chain, automatic failover, and result curation.

When clients like ZCode / Claude Code perform a web search, they send a side request carrying only the `web_search` server tool, expecting the model provider to execute the search. Many custom endpoints (e.g. StepFun) don't support that server tool, while tools like [wbmux](https://github.com/HMuSeaB/wbmux) provide a search endpoint that only handles search requests and can't serve as the main endpoint.

searchbridge bridges the two, and doesn't depend on any single search source:

```
                 ┌─────────────────────────────┐
 ZCode / Claude  │  Plain chat request          │
 Code            │   (no web_search tool)       │
 (anthropic-     │             │                │
  messages)      │             ▼                │
                 │    Upstream model endpoint   │
                 │      (e.g. StepFun)          │
                 │                             │
                 │  Web-search side request     │
                 │   (with web_search tool)     │
                 │             │                │
                 │             ▼                │
                 │  Search backend chain        │
                 │  + curation layer            │
                 │  remote → ddg → bing → ...   │
                 │  failover · consensus · junk │
                 │  filtering                   │
                 └─────────────────────────────┘
```

- **Plain chat**: forwarded byte-for-byte to your upstream model endpoint, streaming or not. Client-supplied auth headers are replaced with your configured upstream key.
- **Web search**: requests carrying the `web_search` server tool are routed through a prioritized backend chain. A failing backend is skipped for a 10-minute cooldown; only when all backends fail is an error returned. Results are synthesized into a standard Anthropic Message (or an SSE event stream for streaming clients: `message_start` → `server_tool_use` → `web_search_tool_result` → `message_delta` → `message_stop`).
- **Result curation**: raw search results are full of junk (SEO farms, gambling sites, irrelevant content) that pollutes model answers. The curation layer does four things to every backend's results: spam-pattern filtering (gambling/adult/registration-bait keywords), relevance scoring and grading (query-term coverage across title/snippet/URL, with CJK bigram tokenization so no segmenter is needed), deduplication (normalized URL + normalized title), and ranking. All-junk results trigger the next backend but **never** a cooldown (the backend may be fine for other queries).
- **Merge mode**: with `SB_SEARCH_MERGE=1`, multiple backends (3 by default) are queried in parallel and results returned by several engines get a consensus boost — cross-engine consensus is the most effective signal against a single engine's junk or bias.
- **JSON endpoint**: `GET/POST /v1/search?q=<query>&n=5`, wire-compatible with wbmux's `/v1/search`, with curation stats attached (`stats: {total, good, partial, junk, deduped}`) — ready for scripts, MCP servers, Raycast, and friends.

Zero third-party dependencies, single binary, configured entirely through environment variables.

## Quick Start

```bash
go build -o searchbridge .

export SB_LISTEN="127.0.0.1:8787"
export SB_UPSTREAM_URL="https://api.stepfun.com/step_plan"   # chat passthrough target
export SB_UPSTREAM_API_KEY="sk-..."                          # upstream key (x-api-key)
# the search backend chain defaults to remote,ddg,bing — zero config needed

./searchbridge
```

Then point your client's model endpoint at searchbridge and enable "Native web search" in the model capabilities.

### ZCode

Settings → Model Providers → Custom model:

| Setting | Value |
|---|---|
| Base URL | `http://127.0.0.1:8787` |
| API Key | any placeholder (replaced with `SB_UPSTREAM_API_KEY`) |
| Model capabilities → Native web search | **Enabled** |

### Claude Code

```bash
export ANTHROPIC_BASE_URL="http://127.0.0.1:8787"
export ANTHROPIC_API_KEY="dummy"
claude
```

## Configuration

### Basic

| Variable | Default | Description |
|---|---|---|
| `SB_LISTEN` | `127.0.0.1:8787` | Listen address |
| `SB_UPSTREAM_URL` | empty | Chat passthrough target (Anthropic Messages-compatible endpoint) |
| `SB_UPSTREAM_API_KEY` | empty | Upstream key, sent as the `x-api-key` header |
| `SB_UPSTREAM_BEARER` | empty | Set to `1` to send the upstream key as `Authorization: Bearer` |

### Search backend chain

`SB_SEARCH_BACKENDS` defines the backends and their priority (comma-separated, order = try order), default `remote,ddg,bing`:

| Backend | Cost | Description | Related config |
|---|---|---|---|
| `remote` | free* | wbmux-style Anthropic search endpoint (reuses logged-in credentials) | `SB_SEARCH_URL`, `SB_SEARCH_API_KEY`, `SB_SEARCH_MODEL` |
| `ddg` | free | DuckDuckGo Lite, no key needed (reachable overseas) | `SB_DDG_URL` |
| `bing` | free | Bing web search, defaults to `cn.bing.com` for China | `SB_BING_HOST` |
| `searxng` | free | Self-hosted / public SearXNG instance (JSON API must be enabled) | `SB_SEARXNG_URL` |
| `tavily` | free tier 1000/mo | Tavily Search API | `SB_TAVILY_API_KEY`, `SB_TAVILY_URL` |
| `brave` | paid | Brave Search API (free tier removed in 2026; $5/1k queries) | `SB_BRAVE_API_KEY`, `SB_BRAVE_URL` |

\* `remote` is free as long as the wbmux desktop app is running.

Other search settings:

| Variable | Default | Description |
|---|---|---|
| `SB_SEARCH_URL` | `http://127.0.0.1:55464/v1/search/messages` | `remote` backend URL; set to `off` to disable |
| `SB_SEARCH_API_KEY` | empty | `remote` backend key (wbmux loopback is passwordless) |
| `SB_SEARCH_MODEL` | `web-search` | Model name used when calling `remote` |
| `SB_SEARCH_COOLDOWN_MIN` | `10` | Cooldown minutes after a backend fails |
| `SB_SEARCH_CACHE_MIN` | `30` | Result cache minutes (same query + count hits cache) |
| `SB_SEARCH_MERGE` | empty | Set to `1` for merge mode: parallel query + consensus weighting |
| `SB_SEARCH_MERGE_WIDTH` | `3` | Backends queried simultaneously in merge mode |
| `SB_SEARCH_HEDGE_MS` | `1000` | Hedge window (ms): if the first backend hasn't answered, the next one starts in parallel and the first usable result wins; set to `0` for plain sequential failover |

Example — free backends only, searxng first:

```bash
export SB_SEARCH_BACKENDS="searxng,ddg,bing"
export SB_SEARXNG_URL="http://127.0.0.1:8080"
```

Example — merge mode (three engines in parallel, consensus-weighted, quality first):

```bash
export SB_SEARCH_MERGE=1
export SB_SEARCH_MERGE_WIDTH=3
```

Example — hedge mode (on by default, latency first): put the most comprehensive
source at the head of the chain and the fastest at the tail; if nothing comes
back within the window, they race:

```bash
export SB_SEARCH_BACKENDS="remote,ddg,bing"   # hedge mode by default
export SB_SEARCH_HEDGE_MS=1000                # start ddg if the first backend is silent for 1s
```

### Three chain modes

| Mode | Trigger | Behavior | Best for |
|---|---|---|---|
| merge | `SB_SEARCH_MERGE=1` | Fixed parallel query of N backends, consensus-weighted merge of all results | Quality first; extra upstream calls don't matter |
| hedge (default) | `SB_SEARCH_HEDGE_MS>0` (default 1000) | Sequential start; if the first backend exceeds the window the next starts in parallel, **first usable result wins**, losers are cancelled | Latency first (default) |
| failover | `SB_SEARCH_HEDGE_MS=0` | Try the next only on failure / all-junk | Quota-sensitive (exactly one upstream call per backend) |

Hedge's latency win comes from the long tail of the chain's latency distribution: when the head source (e.g. wbmux) has a multi-second p99, a sub-second tail source wins the race often, and end-to-end latency drops from "the head's full latency" to "window + next backend's latency". Measured (local network, chain `remote,ddg,bing`): sequentially `remote` alone takes 3.8–4.2s; under hedge the winner lands in 0.8–2.5s. The trade-off is a possible extra upstream call — metered backends (tavily/brave) may bill the loser too, so put them at the tail.

Backend errors or all-junk results **fill the next slot immediately** (that's failover, not hedging — it doesn't wait for the hedge timer); all-junk never triggers cooldown, only errors and empty results do.

## Curation layer

Raw search results mix in plenty of junk — SEO farms, gambling sites, pages unrelated to the query. Feeding those straight to the model pollutes the answer. After each backend returns, the curation layer does four things in order:

1. **Spam filtering**: results whose title/snippet/URL hit strong patterns (gambling, adult content, registration bait) are dropped outright (the pattern list is deliberately conservative — better to miss than to misfire).
2. **Relevance scoring and grading**: the query is tokenized into the full phrase (weight 3), Latin/digit words and CJK runs (weight 2), and CJK bigrams (weight 1); coverage across title/snippet/URL is computed. `good` = coverage ≥ 0.5 or all main terms hit; `partial` = a main term hit but coverage short; `junk` = no hit at all, or **bigram-only hits** (weak signals like 「特性」 mismatch across unrelated contexts too often to count as relevant), dropped.
3. **Deduplication**: exact normalized-URL dedup (scheme/www/query/trailing slash stripped, path kept) plus **near-duplicate titles** — engines often give the same page different URLs but title variants (the longer one with a 「（完整版）」-style suffix), so titles are compared by token-set containment (smaller set covered ≥ 0.85; titles under 3 tokens are left to exact dedup) and one copy is kept.
4. **Ranking**: relevance score descending, then re-ranked with three quality signals, `good` first, `partial` only as backfill when good results are fewer than the limit:
   - **Authoritative domain boost ×1.25**: first-party docs and source repos of languages/frameworks/databases/cloud vendors, Stack Overflow, and similar recognized sources (go.dev, docs.python.org, github.com, developer.mozilla.org, …). Boost only, never filtering — missing one real domain costs more than a missed boost, and no list can be exhaustive.
   - **Recency weighting**: when the query carries recency intent (「最新」, "latest", 「近期」…), results mentioning the current or previous year get ×1.2, results mentioning only older years ×0.85. Asking for "the latest" shouldn't return a three-year-old tutorial.
   - **Cross-engine consensus** (merge mode): results returned by k backends get ×(1+0.5(k-1)).

Chain behavior: when a backend's results are **all junk** the next backend is tried, but no cooldown is entered (the backend may still be fine for other queries); only errors and empty results cool down. In merge mode, per-backend results are merged by URL and the consensus boost applies there too.

## Routing detection

A request is treated as a web-search request when any tool in its `tools` array matches either condition:

- `name == "web_search"` (the standard name for Anthropic server tools)
- `type` starts with `web_search_` (e.g. `web_search_20250305`, `web_search_20260209`)

Client-defined tools (e.g. `WebSearch` with an `input_schema`) are unaffected and still handled as plain chat.

## Endpoints

| Path | Behavior |
|---|---|
| `POST /v1/messages` | Routes by detection (chat / search) |
| `POST /v1/messages/count_tokens` | Forwarded upstream |
| `GET /v1/models` | Forwarded upstream |
| `GET/POST /v1/search?q=&n=` | Structured JSON search via the backend chain |
| `GET /healthz` | Health check, returns current routes and the backend chain |

## Development

```bash
go test ./...   # run tests (backend parsing, chain failover/cooldown/cache, routing, SSE synthesis)
go vet ./...    # static analysis
```

## License

MIT
