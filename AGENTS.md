# Purify Repository Instructions

## Product boundary

- This repository is the standalone Purify product.
- The active product workstream is an agent-first Search API built on Purify's existing scraping and cleaning capabilities.
- Do not add LCI/lithium, assimilation-engine, DataOS, security-feed, or unrelated product code here.
- Do not modify sibling repositories as part of Purify work.
- Do not copy an implementation from another product repository. Build Purify Search against Purify's own interfaces and conventions.

## Search architecture

- Keep the existing scrape, extract, batch, crawl, and map APIs backward compatible.
- Put query-to-results orchestration in a dedicated `search` package.
- Isolate provider-specific code behind a small provider interface.
- Reuse Purify's scraper and cleaner only for optional result enrichment; do not duplicate them inside the search package.
- Keep request and response types in `models`, HTTP wiring in `api/handler` and `api/router.go`, and MCP wiring in `cmd/purify-mcp`.
- Never commit credentials, provider keys, production data, or generated secrets.

## Commit discipline

- Work on a Purify-only feature branch. Search work uses `codex/search-api-v1` unless the user selects another branch.
- Each commit must contain one reviewable Purify Search concern. Do not combine unrelated cleanup or another product line.
- Use scoped commit subjects such as `feat(search):`, `fix(search):`, `test(search):`, `docs(search):`, or `refactor(search):`.
- Before every code commit, run `go test ./...` and `git diff --check`.
- Before committing, inspect `git status --short` and exclude unrelated files.
- Do not push, merge, tag, deploy, or modify GitHub settings unless the user explicitly asks.
