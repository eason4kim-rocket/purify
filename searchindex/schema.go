package searchindex

const schemaMigrationV1 = `
CREATE TABLE pages (
	id INTEGER PRIMARY KEY,
	url TEXT NOT NULL UNIQUE,
	root TEXT NOT NULL,
	title TEXT NOT NULL DEFAULT '',
	body TEXT NOT NULL DEFAULT '',
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

-- Both tables use unicode61. Chinese reaches it through the indexText bigram
-- rewrite in lang.go rather than through the trigram tokenizer, which cannot
-- match the two-character words that dominate real Chinese queries.
CREATE VIRTUAL TABLE pages_fts_en USING fts5(
	title, body, content='pages', content_rowid='id', tokenize='unicode61');
CREATE VIRTUAL TABLE pages_fts_zh USING fts5(
	title, body, content='pages', content_rowid='id', tokenize='unicode61');

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
