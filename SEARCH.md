# Purify Search Workstream

Status: scoped for implementation
Repository: `Easonliuliang/purify`
Working branch: `codex/search-api-v1`

## Decision

Purify Search is developed and committed as an independent product workstream in this repository. It will not share commits, folders, or runtime responsibilities with LCI/lithium, the assimilation engine, DataOS, or the security product line.

Purify remains the product. Search extends its existing URL-to-clean-content foundation with a query-to-ranked-results entry point.

## Product goal

Give an AI agent one stable API that can:

1. search the web from a natural-language query;
2. return ranked, source-attributed results;
3. optionally use Purify to fetch and clean the top results;
4. expose the same capability through HTTP and MCP.

The first version is a focused Search API, not a general agent platform.

## MVP contract

The initial endpoint is:

```text
POST /api/v1/search
```

Proposed request fields:

```json
{
  "query": "latest browser automation benchmarks",
  "limit": 10,
  "domains": ["example.com"],
  "freshness": "month",
  "include_content": false
}
```

Proposed response shape:

```json
{
  "query": "latest browser automation benchmarks",
  "results": [
    {
      "title": "Result title",
      "url": "https://example.com/article",
      "snippet": "Relevant excerpt",
      "score": 0.91,
      "published_at": "2026-08-01T00:00:00Z",
      "content": "Optional Purify-cleaned content"
    }
  ]
}
```

The public contract must not expose provider-specific response objects. Provider metadata may be retained internally for diagnostics.

## Code boundaries

Planned ownership:

```text
search/                  query orchestration and provider interface
search/providers/        isolated provider adapters
models/search.go         public request and response models
api/handler/search.go    HTTP validation and response handling
api/router.go            /api/v1/search registration
cmd/purify-mcp/          search_web MCP tool
```

Existing packages remain the source of truth for enrichment:

```text
scraper/                 page retrieval
cleaner/                 readable content and citations
engine/                  HTTP/browser execution
```

Search may call those packages. It must not fork or duplicate their behavior.

## Delivery sequence

### M0 — Contract and core

- Add Search request/response models.
- Define a provider-neutral interface.
- Implement validation, normalization, deduplication, and deterministic unit tests.
- Register the HTTP route with the existing authentication and rate-limit behavior.

### M1 — Search results

- Add the first provider adapter using configuration-based credentials.
- Map provider output into the stable Purify result model.
- Add timeout, cancellation, error mapping, and provider contract tests.

### M2 — Purify enrichment

- Support `include_content` for a bounded number of top results.
- Reuse the existing scraper and cleaner pipeline.
- Preserve result order and report partial enrichment failures without discarding successful search results.

### M3 — Agent interface

- Add an MCP `search_web` tool using the same service and models.
- Document HTTP and MCP examples.

### M4 — Production readiness

- Add metrics, structured diagnostics, rate-limit cost rules, and deployment configuration.
- Run local integration tests and a hosted smoke test.
- Publish only after the health check and an authenticated Search request pass.

## Explicit non-goals

- security monitoring, vulnerability feeds, or security-specific APIs;
- LCI/lithium workflows;
- assimilation-engine workflows;
- social-data ingestion or a general DataOS;
- autonomous agent planning, memory, or task execution;
- breaking or renaming the existing Purify scraping APIs.

## Commit map

Implementation is split into reviewable commits:

1. `docs(search): establish standalone product line`
2. `feat(search): define request models and provider contract`
3. `feat(search): add search service and result normalization`
4. `feat(search): expose search HTTP endpoint`
5. `feat(search): add first provider adapter`
6. `feat(search): enrich search results with purify`
7. `feat(search): expose search through mcp`
8. `docs(search): document configuration and usage`

Each commit must pass the repository tests and contain no changes from another product line.

## Definition of done for v1

- `POST /api/v1/search` returns a stable, provider-neutral result format.
- Authentication, rate limiting, timeouts, and errors follow existing Purify conventions.
- Optional content enrichment uses the current Purify scraper/cleaner path.
- HTTP and MCP share one Search service rather than separate implementations.
- Unit and integration tests pass with no live credential required for the default test suite.
- Documentation includes setup, examples, limits, and failure behavior.
- The production health endpoint and one authenticated Search smoke test both pass.
