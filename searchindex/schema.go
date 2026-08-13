package searchindex

// The schema is pre-release: it is edited in place rather than migrated, so a
// tokenizer or layout change means rebuilding the index file.
//
// Measured on a real 86-page crawl, storing plain body text cost 149% of the
// text while the inverted index alone cost 39%. Pages therefore keep a
// zstd-compressed body plus a short plain lead, and the FTS tables are
// contentless. That also removes the external-content coupling: the indexed
// token stream no longer has to equal the stored text, which is what a
// rewritten (segmented) Chinese stream needs.
const schemaMigrationV1 = `
CREATE TABLE pages (
	id INTEGER PRIMARY KEY,
	url TEXT NOT NULL UNIQUE,
	root TEXT NOT NULL,
	title TEXT NOT NULL DEFAULT '',
	lead TEXT NOT NULL DEFAULT '',
	body_z BLOB,
	lang TEXT NOT NULL,
	fetched_at INTEGER NOT NULL,
	content_hash TEXT NOT NULL,
	etag TEXT NOT NULL DEFAULT '',
	last_mod TEXT NOT NULL DEFAULT '',
	needs_render INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX pages_root ON pages(root);
CREATE INDEX pages_hash ON pages(content_hash);
CREATE INDEX pages_render ON pages(needs_render) WHERE needs_render = 1;

CREATE VIRTUAL TABLE pages_fts_en USING fts5(
	title, body, content='', contentless_delete=1, tokenize='unicode61');
CREATE VIRTUAL TABLE pages_fts_zh USING fts5(
	title, body, content='', contentless_delete=1, tokenize='unicode61');

CREATE TABLE frontier (
	url TEXT PRIMARY KEY,
	root TEXT NOT NULL,
	state TEXT NOT NULL,
	leased_at INTEGER NOT NULL DEFAULT 0,
	attempts INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX frontier_pick ON frontier(state, root);
`

var migrations = []string{schemaMigrationV1}
