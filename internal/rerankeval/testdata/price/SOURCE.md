# Brave public list price

The price contract only accepts the official page

`https://api-dashboard.search.brave.com/documentation/pricing`

and a single unambiguous Search card at `$5.00 per 1,000 requests`.

A live GET on 2026-08-12 returned HTTP 200 HTML. The published Svelte markup is not admissible:

- more than one `price-item` in the Search card, including non-breaking-space placeholders
- a second `$` token in the “free $5 in credits” line
- the amount and “per 1,000 requests” text are split by a decorative slash SVG

No `manifest.json` / `brave-search-pricing.html` artifact is committed. That would pretend the extractor accepted a page it rejected. GPU-hour operands remain absent.

Re-capture only when the official page again has one visible Search price item and one currency token. Artifact identity is still not a publisher signature.
