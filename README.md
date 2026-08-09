<div align="center">

# Purify

**Web Scraping API for AI Agents.**
One binary. Zero dependencies. Built-in MCP server.

The Go alternative to Firecrawl / Crawl4AI — no Python, no Node.js, no Redis. Just `docker compose up` and go.

<img src="https://img.shields.io/github/license/Easonliuliang/purify?style=flat-square&color=22C55E" alt="License" />
<img src="https://img.shields.io/github/stars/Easonliuliang/purify?style=flat-square&color=22C55E" alt="Stars" />
<img src="https://img.shields.io/github/v/release/Easonliuliang/purify?style=flat-square&color=22C55E" alt="Release" />

[Get Free API Key](https://purify.verifly.pro) · [API Docs](#api) · [MCP Server](#mcp-server) · [Self-Host](#self-hosting)

</div>

---

## See it work

```bash
# Start Purify
docker compose up -d

# Scrape a page
curl -s -X POST http://localhost:8080/api/v1/scrape \
  -H "Content-Type: application/json" \
  -d '{"url": "https://news.ycombinator.com"}' | jq .
```

```json
{
  "success": true,
  "content": "# Hacker News\n\n1. Show HN: ...",
  "tokens": {
    "original_estimate": 11708,
    "cleaned_estimate": 5572,
    "savings_percent": 52.4
  },
  "timing": {
    "total_ms": 400
  }
}
```

Two commands. Clean Markdown back in under a second.

---

## How it compares

| | Purify | Firecrawl | Crawl4AI | Jina Reader |
|---|---|---|---|---|
| Language | **Go** | TypeScript | Python | N/A (cloud) |
| Self-host | **Single binary** | 5+ containers (Redis, PG, Playwright…) | pip + Playwright | No self-host docs |
| MCP server | **Built-in (6 tools)** | Community-maintained | No | No |
| Token savings | **52–99%** | ~70–80% | ~75–85% | ~60–70% |
| Recursive crawling | Yes | Yes | Yes | No |
| Batch scrape | Yes | Yes | No | No |
| License | **Apache 2.0** | AGPL-3.0 | Apache 2.0 | Partial open source |
| Price (50k req/mo) | **$29/mo** | $49/mo | Free (local) | $49/mo |

## Quick start

**Option A: Docker**

```bash
docker compose up -d
```

**Option B: Build from source**

```bash
git clone https://github.com/Easonliuliang/purify.git
cd purify && make build
PURIFY_AUTH_ENABLED=false ./bin/purify
```

**Option C: Hosted API (no setup)**

```bash
curl -s -X POST https://purify.verifly.pro/api/v1/scrape \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"url": "https://news.ycombinator.com"}' | jq .content
```

Get a free API key at [purify.verifly.pro](https://purify.verifly.pro) — 1,000 requests/month, no credit card.

## Token savings — real numbers

Measured with [tiktoken](https://github.com/openai/tiktoken) (GPT-4 tokenizer). Purify strips navigation, ads, scripts, and styling — your LLM only sees the content.

| Website | Raw HTML | After Purify | Savings | Latency |
|---|---|---|---|---|
| GitHub repo page | 99,181 | 1,370 | **98.6%** | 1.1s |
| New York Times | 103,744 | 2,130 | **98.0%** | 1.1s |
| Anthropic API Docs | 129,066 | 4,837 | **96.3%** | 1.8s |
| Next.js blog (React SPA) | 87,231 | 4,271 | **95.1%** | 5.0s |
| BBC News homepage | 97,540 | 6,969 | **92.9%** | 2.5s |
| arXiv paper (DeepSeek-R1) | 26,684 | 3,129 | **88.3%** | 0.5s |
| Wikipedia (LLM) | 245,276 | 76,325 | **68.9%** | 1.5s |
| Hacker News | 11,708 | 5,572 | **52.4%** | 0.4s |
| sspai.com | 32,895 | 187 | **99.4%** | 1.2s |
| Xiaohongshu (RedNote) | 158,742 | 353 | **99.8%** | 1.0s |

> Low-savings sites (Hacker News, paulgraham.com) are already minimal — almost pure text with no cruft to remove. That's a feature, not a bug.

## Use cases

- **AI agents** — Give your agent web access via MCP or REST API
- **RAG pipelines** — Scrape docs, get clean Markdown, embed into your vector DB
- **Trading bots** — Scrape prediction markets and news with sub-500ms latency
- **Research assistants** — Read and summarize any web page

## MCP server

Purify includes a built-in MCP server with **6 tools**:

| Tool | Description |
|---|---|
| `scrape_url` | Scrape a single page, return clean content |
| `verify_fact` | Revisit evidence-backed claims and return `confirmed`, `changed`, or `gone` |
| `batch_scrape` | Scrape multiple URLs in parallel |
| `crawl_site` | Recursively crawl a website (BFS) |
| `map_site` | Discover all URLs on a site |
| `extract_data` | Extract structured data with LLM (BYOK) |

### Setup

Add to your Claude Desktop config (`claude_desktop_config.json`):

```json
{
  "mcpServers": {
    "purify": {
      "command": "purify-mcp",
      "env": {
        "PURIFY_API_URL": "https://purify.verifly.pro",
        "PURIFY_API_KEY": "your-api-key"
      }
    }
  }
}
```

For self-hosted instances, set `PURIFY_API_URL` to `http://localhost:8080`.

Then ask Claude:
- *"Scrape https://paulgraham.com/greatwork.html and summarize it."*
- *"Crawl the Next.js docs site, max 20 pages."*
- *"Extract the product name and price from this page: ..."*

## API

### POST /api/v1/scrape

Scrape a single page and return cleaned content. Supports JSON response or SSE streaming.

```json
{
  "url": "https://example.com/article",
  "output_format": "markdown",
  "extract_mode": "readability"
}
```

| Parameter | Type | Default | Description |
|---|---|---|---|
| `url` | string | *required* | Target URL |
| `output_format` | string | `markdown` | `markdown`, `html`, `text`, or `markdown_citations` |
| `extract_mode` | string | `readability` | `readability`, `raw`, `pruning`, or `auto` |
| `timeout` | int | `30` | Timeout in seconds (1–120) |
| `stealth` | bool | `false` | Anti-detection mode |
| `headers` | object | — | Custom HTTP headers |
| `cookies` | array | — | Cookies to set before navigation |
| `actions` | array | — | Browser interactions (click, scroll, wait, etc.) |
| `include_tags` | array | — | CSS selectors to keep |
| `exclude_tags` | array | — | CSS selectors to remove |
| `css_selector` | string | — | Extract only matching elements |
| `max_age` | int | `0` | Cache max age in ms (0 = no cache) |

Response:

```json
{
  "success": true,
  "status_code": 200,
  "final_url": "https://example.com/article",
  "content": "# Article Title\n\nClean markdown content...",
  "metadata": {
    "title": "Article Title",
    "author": "Author Name",
    "language": "en",
    "source_url": "https://example.com/article",
    "fetch_method": "http"
  },
  "links": {
    "internal": [{"href": "/about", "text": "About"}],
    "external": [{"href": "https://github.com/...", "text": "GitHub"}]
  },
  "images": [{"src": "https://example.com/hero.jpg", "alt": "Hero"}],
  "tokens": {
    "original_estimate": 32895,
    "cleaned_estimate": 187,
    "savings_percent": 99.43
  },
  "timing": {
    "total_ms": 1172,
    "navigation_ms": 1162,
    "cleaning_ms": 9
  },
  "engine_used": "http"
}
```

#### SSE streaming

Add `Accept: text/event-stream` header to receive Server-Sent Events instead of JSON:

```bash
curl -X POST https://purify.verifly.pro/api/v1/scrape \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -H "Content-Type: application/json" \
  -H "Accept: text/event-stream" \
  -d '{"url": "https://example.com"}'
```

Events: `scrape.started` → `scrape.navigated` → `scrape.completed` (or `scrape.error`).

#### Citation format

Use `"output_format": "markdown_citations"` to convert inline links to academic-style references:

```markdown
See [Google][1] and [GitHub][2]

---
[1]: https://google.com
[2]: https://github.com
```

### POST /api/v1/batch/scrape

Scrape multiple URLs in parallel. Returns a job ID for async polling.

```json
{
  "urls": ["https://a.com", "https://b.com", "https://c.com"],
  "options": {"output_format": "markdown"},
  "webhook_url": "https://your-server.com/callback",
  "webhook_secret": "your-hmac-secret"
}
```

Poll status: `GET /api/v1/batch/:id`

### POST /api/v1/crawl

Recursively crawl a website starting from a URL.

```json
{
  "url": "https://docs.example.com",
  "max_depth": 3,
  "max_pages": 100,
  "scope": "subdomain",
  "webhook_url": "https://your-server.com/callback",
  "webhook_secret": "your-hmac-secret"
}
```

Poll status: `GET /api/v1/crawl/:id`

### POST /api/v1/map

Discover all URLs on a site without scraping content.

```json
{"url": "https://example.com"}
```

### POST /api/v1/extract

Structured data extraction using your own LLM key (BYOK). Purify validates the
result against JSON Schema and makes at most one repair attempt. Set
`evidence` to attach the immutable page snapshot and a source anchor for every
JSON leaf value.

```bash
curl -X POST https://purify.verifly.pro/api/v1/extract \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "url": "https://example.com/product",
    "schema": {
      "type": "object",
      "properties": {
        "name": {"type": "string"},
        "price": {"type": "number"},
        "features": {"type": "array", "items": {"type": "string"}}
      },
      "required": ["name", "price"],
      "additionalProperties": false
    },
    "llm_api_key": "your-openai-key",
    "evidence": true
  }'
```

Legacy shorthand schemas such as `{"name":"string","price":"number"}`
remain accepted and are normalized to JSON Schema server-side.

An evidence response adds fields without changing the existing response:

```json
{
  "success": true,
  "data": {
    "name": "Pro Plan",
    "price": 29.99
  },
  "snapshot_id": "sha256:9f2c...",
  "unlocated_rate": 0,
  "basis": {
    "price": {
      "quote": "$29.99",
      "text_range": [1204, 1210],
      "selector": ".pricing-card .amount",
      "method": "exact",
      "snapshot_id": "sha256:9f2c...",
      "fetched_at": "2026-08-09T08:00:00Z"
    }
  },
  "receipts": {
    "price": "eyJhbGciOiJFZERTQSIsImtpZCI6Ii4uLiJ9..."
  },
  "metadata": {
    "title": "Plans",
    "source_url": "https://example.com/product",
    "fetch_method": "http"
  },
  "tokens": {
    "original_estimate": 1000,
    "cleaned_estimate": 250,
    "savings_percent": 75
  },
  "timing": {
    "total_ms": 820,
    "navigation_ms": 600,
    "cleaning_ms": 20,
    "extraction_ms": 200
  }
}
```

`text_range` is a half-open `[start,end)` range of UTF-8 byte offsets into the
cleaned content. `unlocated_rate` is the fraction of JSON leaf values Purify
could not locate. When the single repair attempt still cannot satisfy the
schema, the endpoint returns the best data with `partial: true` and a
`violations` array. With `evidence` omitted or false, `snapshot_id`,
`unlocated_rate`, `basis`, and signed receipt fields are omitted. Evidence mode
requires `PURIFY_SNAPSHOT_ENABLED=true`; otherwise the endpoint returns
`EVIDENCE_UNAVAILABLE`.

### POST /api/v1/verify

Revisit a source page and re-verify facts against their original, immutable
snapshot. This route is protected by the same API-key authentication and rate
limit as `/scrape` and `/extract`, and requires snapshot storage to be enabled.

The request accepts exactly one claim input form:

- `url` plus a non-empty `claims` array; or
- one signed `receipt`. In receipt mode the URL is restored from the receipt,
  although `url` may be supplied as a consistency check.

`claims` and `receipt` are a strict XOR: sending both, or neither, returns
`INVALID_INPUT`. Each explicit claim's `value` must be a JSON string, number,
or boolean, with an evidence anchor copied from an earlier evidence-enabled
extraction.
All claims in one request must reference the same old snapshot. Explicit mode
accepts at most 100 unique claim paths and 512 KiB of aggregate path, value,
quote, and selector data. Anchors must use `exact`, `normalized`, `fuzzy`, or
`compiled`; `unlocated` evidence cannot be re-verified. Receipt tokens are
limited to 2 MiB.

Claims mode:

```bash
curl -X POST https://purify.verifly.pro/api/v1/verify \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "url": "https://example.com/product",
    "claims": [
      {
        "path": "price",
        "value": 29.99,
        "anchor": {
          "quote": "$29.99",
          "text_range": [1204, 1210],
          "selector": ".pricing-card .amount",
          "method": "exact",
          "snapshot_id": "sha256:9f2c7150b5d6c4834a8b73c39ad2b678d08e234951d0990b0c59f30c41ec33e1",
          "fetched_at": "2026-08-09T08:00:00Z"
        }
      }
    ]
  }'
```

Receipt mode restores one authenticated claim without making the caller
reconstruct its anchor:

```bash
curl -X POST https://purify.verifly.pro/api/v1/verify \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"receipt":"eyJhbGciOiJFZERTQSIsImtpZCI6Ii4uLiJ9..."}'
```

A changed claim returns its current scalar value, refreshed evidence, and a
new signed receipt:

```json
{
  "verification_id": "ab7d7e33e0af4e378f5351226adfb8e2",
  "url": "https://example.com/product",
  "final_url": "https://example.com/product",
  "status_code": 200,
  "results": [
    {
      "path": "price",
      "status": "changed",
      "new_value": 31.99,
      "evidence": {
        "quote": "$31.99",
        "text_range": [1220, 1226],
        "selector": ".pricing-card .amount",
        "method": "exact",
        "snapshot_id": "sha256:e4a9c531ae63b63a35f62783064387da778d19f34d0671781d70a5ec3bb051a2",
        "fetched_at": "2026-08-09T08:05:00Z"
      },
      "receipt": "eyJhbGciOiJFZERTQSIsImtpZCI6Ii4uLiJ9..."
    }
  ],
  "page_similarity": 0.96875,
  "snapshot_id": "sha256:e4a9c531ae63b63a35f62783064387da778d19f34d0671781d70a5ec3bb051a2",
  "verified_at": "2026-08-09T08:05:00Z"
}
```

The verdict set is deliberately limited to three states:

| Status | Meaning | Result fields |
|---|---|---|
| `confirmed` | The selector still yields the same normalized scalar, or the selector moved but the old quote still aligns | Refreshed `evidence` and `receipt`; no `new_value` |
| `changed` | The selector yields a different scalar | `new_value`, refreshed `evidence`, and `receipt` |
| `gone` | The field cannot be located, or the source page definitely returned 404/410 | `gone_scope` is `field` or `page`; no evidence or receipt |

`page_similarity` is `1 - DOM SimHash distance / 64`. It is a page-change
signal, not a fourth verdict, and is present only when the revisit produced a
successful page body.

A source HTTP 404 or 410 is a successful verification observation, not an API
transport error. The `/verify` request itself returns HTTP 200, every claim is
`gone` with `gone_scope: "page"`, and `page_similarity` is omitted:

```json
{
  "verification_id": "ab7d7e33e0af4e378f5351226adfb8e2",
  "url": "https://example.com/product",
  "final_url": "https://example.com/product",
  "status_code": 404,
  "results": [
    {
      "path": "price",
      "status": "gone",
      "gone_scope": "page"
    }
  ],
  "snapshot_id": "sha256:e4a9c531ae63b63a35f62783064387da778d19f34d0671781d70a5ec3bb051a2",
  "verified_at": "2026-08-09T08:05:00Z"
}
```

Every result is written to the SQLite verification ledger before the response
is returned.

#### Durable `fact.changed` webhook

Either request mode may include `webhook_url` and an optional
`webhook_secret`. A secret without a URL is invalid. When at least one claim is
`changed`, Purify commits one aggregate `fact.changed` event to the durable
outbox in the same transaction as the verification rows:

```json
{
  "receipt": "eyJhbGciOiJFZERTQSIsImtpZCI6Ii4uLiJ9...",
  "webhook_url": "https://hooks.example.com/purify",
  "webhook_secret": "your-hmac-secret"
}
```

The delivered event body has this shape:

```json
{
  "type": "fact.changed",
  "job_id": "ab7d7e33e0af4e378f5351226adfb8e2",
  "timestamp": 1786262700,
  "data": {
    "verification_id": "ab7d7e33e0af4e378f5351226adfb8e2",
    "url": "https://example.com/product",
    "final_url": "https://example.com/product",
    "verified_at": "2026-08-09T08:05:00Z",
    "changes": [
      {
        "path": "price",
        "old_value": 29.99,
        "new_value": 31.99,
        "evidence": {
          "quote": "$31.99",
          "text_range": [1220, 1226],
          "selector": ".pricing-card .amount",
          "method": "exact",
          "snapshot_id": "sha256:e4a9c531ae63b63a35f62783064387da778d19f34d0671781d70a5ec3bb051a2",
          "fetched_at": "2026-08-09T08:05:00Z"
        },
        "receipt": "eyJhbGciOiJFZERTQSIsImtpZCI6Ii4uLiJ9..."
      }
    ]
  }
}
```

Delivery includes `Content-Type: application/json` and an
`X-Purify-Event-ID` equal to `verification_id`. When a secret is supplied,
`X-Purify-Signature` is `sha256=<hex>` for
`HMAC-SHA256(webhook_secret, exact_request_body)`.

The response confirms the durable commit, not downstream delivery. A worker
resumes pending events after restart and makes at most four delivery attempts,
with 1s, 5s, and 30s backoffs after retryable failures. Transport failures and
HTTP 408, 425, 429, and 5xx are retryable; other non-2xx responses are
permanent. Redirects are not followed, and webhook destinations must resolve
only to public IP addresses.

#### Authentication and errors

When `PURIFY_AUTH_ENABLED=true`, use either `Authorization: Bearer <key>` or
`X-API-Key: <key>`. Receipt signature verification at
`/api/v1/receipts/verify` remains public; fact re-verification at
`/api/v1/verify` is protected.

The verify handler rejects unknown JSON fields and request bodies larger than
4 MiB. Handler-level failures use the stable envelope
`{"error":{"code":"...","message":"..."}}`; authentication and rate-limit
failures use the existing common API middleware response.

| HTTP | Code | Meaning |
|---:|---|---|
| 400 | `INVALID_INPUT` | Malformed JSON, invalid claim, or a `claims`/`receipt` XOR violation |
| 400 | `INVALID_RECEIPT` | Receipt signature, payload, or optional URL consistency check is invalid |
| 401 | `UNAUTHORIZED` | Missing or invalid API key |
| 413 | `INVALID_INPUT` | Request body exceeds 4 MiB |
| 429 | `RATE_LIMITED` | Per-key or per-IP rate limit exceeded |
| 500/503 | `INTERNAL_ERROR` | Signing, ledger recording, or an unexpected internal failure |
| 502 | `NAVIGATION_FAILED` | The source could not be revisited or returned an unusable status other than 404/410 |
| 503 | `EVIDENCE_UNAVAILABLE` | Verification is disabled or a required durable snapshot is unavailable |
| 504 | `SCRAPE_TIMEOUT` | Verification was canceled or exceeded its deadline |

#### MCP `verify_fact`

The MCP server exposes claims mode through `verify_fact`. Both arguments are
required: `url` is the source URL and `claims` is a JSON-encoded **string**
containing the same non-empty claim array accepted by the HTTP API. The tool
accepts up to 16 KiB for `url` and 512 KiB for the claims string.

```json
{
  "name": "verify_fact",
  "arguments": {
    "url": "https://example.com/product",
    "claims": "[{\"path\":\"price\",\"value\":29.99,\"anchor\":{\"quote\":\"$29.99\",\"text_range\":[1204,1210],\"selector\":\".pricing-card .amount\",\"method\":\"exact\",\"snapshot_id\":\"sha256:9f2c7150b5d6c4834a8b73c39ad2b678d08e234951d0990b0c59f30c41ec33e1\",\"fetched_at\":\"2026-08-09T08:00:00Z\"}}]"
  }
}
```

Configure `PURIFY_API_URL` and `PURIFY_API_KEY` as shown in
[MCP server setup](#setup). The tool calls the authenticated HTTP endpoint and
returns the complete `VerifyResponse` as structured content, with pretty JSON
as its text fallback. Non-2xx API responses become MCP tool errors in
`[CODE] message` form when the API supplied a structured error. Receipt mode
and webhook options are available through the HTTP endpoint, not this MCP
tool.

### Public receipt verification

Receipt verification and the active public key are free public endpoints; they
do not require an API key.

```bash
curl -X POST https://purify.verifly.pro/api/v1/receipts/verify \
  -H "Content-Type: application/json" \
  -d '{"receipt":"eyJhbGciOiJFZERTQSIsImtpZCI6Ii4uLiJ9..."}'
```

```json
{
  "valid": true,
  "payload": {
    "v": "purify-receipt/1",
    "url": "https://example.com/product",
    "path": "price",
    "value": 29.99,
    "anchor": {
      "quote": "$29.99",
      "text_range": [1204, 1210],
      "method": "exact",
      "snapshot_id": "sha256:9f2c...",
      "fetched_at": "2026-08-09T08:00:00Z"
    },
    "issued_at": "2026-08-09T08:00:01Z",
    "kid": "0123456789abcdef"
  }
}
```

`GET /api/v1/receipts/pubkey` returns the current Ed25519 public key as an
RFC 8037-compatible OKP JWK. Verification failures return HTTP 200 with
`{"valid":false,"error":{"code":"INVALID_RECEIPT",...}}`; malformed request
bodies return HTTP 400.

### Webhook callbacks

Batch and Crawl endpoints support job webhook notifications. `/verify` uses
the durable `fact.changed` outbox described above. Purify signs the exact POST
body with HMAC-SHA256 in the `X-Purify-Signature` header when a secret is set.

Events: `batch.completed`, `crawl.page`, `crawl.completed`, `crawl.failed`,
`fact.changed`

Verify the signature:
```
HMAC-SHA256(webhook_secret, request_body) == X-Purify-Signature (sha256=<hex>)
```

### GET /api/v1/health

Returns server status and uptime.

## Configuration

All configuration via environment variables:

| Variable | Default | Description |
|---|---|---|
| `PURIFY_HOST` | `0.0.0.0` | Listen address |
| `PURIFY_PORT` | `8080` | Listen port |
| `PURIFY_AUTH_ENABLED` | `true` | Enable API key authentication |
| `PURIFY_API_KEYS` | — | Comma-separated valid API keys |
| `PURIFY_MAX_PAGES` | `10` | Max concurrent browser tabs |
| `PURIFY_DEFAULT_TIMEOUT` | `30s` | Default scrape timeout |
| `PURIFY_RATE_RPS` | `5` | Rate limit (requests/sec/key) |
| `PURIFY_RATE_BURST` | `10` | Rate limit burst |
| `PURIFY_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `PURIFY_DATA_DIR` | `./data` | Durable snapshots, signing key, SQLite verification ledger, and webhook outbox |
| `PURIFY_SNAPSHOT_ENABLED` | `true` | Persist content-addressed HTML snapshots |
| `PURIFY_SIGNING_KEY` | generated | Optional 32-byte Ed25519 seed encoded as hex |

## Self-hosting

Purify is a single Go binary. No Docker, Redis, or external database is
required. The data directory stores compressed snapshots, the stable
receipt-signing identity, the verification ledger, and pending webhook outbox
events; persist it across restarts.

```bash
# Local development (no auth)
PURIFY_AUTH_ENABLED=false ./bin/purify

# Production (with API key)
PURIFY_API_KEYS=your-secret-key ./bin/purify
```

Runs on any $5/month VPS. No usage limits when self-hosted.

### System requirements

- Any Linux, macOS, or Windows machine
- ~15 MB for the binary, plus snapshot storage in `PURIFY_DATA_DIR`
- ~30 MB RAM idle

## Pricing

| | Free | Pro |
|---|---|---|
| Price | $0/mo | $29/mo |
| Requests | 1,000/mo | 50,000/mo |
| Concurrent | 2 | 10 |
| MCP server | ✓ | ✓ |
| Structured extraction | ✓ | ✓ |

**[Get started free →](https://purify.verifly.pro)**

## Contributing

Contributions welcome. Please open an issue first to discuss what you'd like to change.

## License

[Apache 2.0](LICENSE) — use it however you want, commercially or otherwise. No AGPL restrictions.
