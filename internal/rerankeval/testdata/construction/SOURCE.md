# Construction corpus (seed order)

This directory holds a provider-free relevance construction set.

- `cases.jsonl` and `docs.jsonl` are emitted from `scripts/rerankharvest/seeds.json`.
- `provider_rank` is the seed file order (`baseline=seed_order`).
- Pages are operator-curated public encyclopedia and standards snapshots, not Search Results.
- This set must not be cited as a Brave (or any search-provider) comparison.
- Recordings, scorecards, and production certification are intentionally absent.

Regenerate only by writing a new output directory:

```text
go run ./scripts/rerankharvest -seeds scripts/rerankharvest/seeds.json -out /tmp/rerank-construction
```

Do not replace these files in place from a live fetch unless the snapshots are re-reviewed.
