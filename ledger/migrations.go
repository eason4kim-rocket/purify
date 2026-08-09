package ledger

// migrations are deliberately append-only. Each entry is applied in its own
// transaction and recorded in schema_migrations before the database is used.
var migrations = []string{
	`CREATE TABLE verifications (
		id TEXT PRIMARY KEY,
		verification_id TEXT NOT NULL,
		claim_index INTEGER NOT NULL CHECK (claim_index >= 0),
		url TEXT NOT NULL,
		host TEXT NOT NULL,
		final_url TEXT,
		path TEXT NOT NULL,
		old_value TEXT NOT NULL CHECK (json_valid(old_value)),
		new_value TEXT CHECK (new_value IS NULL OR json_valid(new_value)),
		outcome TEXT NOT NULL CHECK (outcome IN ('confirmed', 'changed', 'gone')),
		gone_scope TEXT CHECK (gone_scope IS NULL OR gone_scope IN ('field', 'page')),
		page_similarity REAL CHECK (
			page_similarity IS NULL OR
			(page_similarity >= 0.0 AND page_similarity <= 1.0)
		),
		old_snapshot_id TEXT NOT NULL,
		new_snapshot_id TEXT,
		old_receipt TEXT,
		receipt TEXT,
		schema_hash TEXT,
		template_cluster_id TEXT,
		extractor_id TEXT,
		verified_at TEXT NOT NULL,
		UNIQUE (verification_id, claim_index)
	);

	CREATE INDEX idx_verifications_url_time
		ON verifications(url, verified_at DESC);
	CREATE INDEX idx_verifications_host_time
		ON verifications(host, verified_at DESC);
	CREATE INDEX idx_verifications_old_snapshot
		ON verifications(old_snapshot_id);
	CREATE INDEX idx_verifications_new_snapshot
		ON verifications(new_snapshot_id)
		WHERE new_snapshot_id IS NOT NULL;
	CREATE INDEX idx_verifications_outcome_time
		ON verifications(outcome, verified_at DESC);
	CREATE INDEX idx_verifications_replay
		ON verifications(
			host,
			schema_hash,
			template_cluster_id,
			extractor_id,
			outcome,
			verified_at DESC
		);`,
	`CREATE TABLE outbox_events (
		id TEXT PRIMARY KEY,
		verification_id TEXT NOT NULL,
		event_type TEXT NOT NULL,
		destination_url TEXT NOT NULL,
		secret TEXT NOT NULL DEFAULT '',
		payload TEXT NOT NULL CHECK (json_valid(payload)),
		created_at TEXT NOT NULL,
		attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
		last_attempt_at TEXT,
		last_error TEXT NOT NULL DEFAULT '',
		next_attempt_at TEXT NOT NULL,
		delivered_at TEXT,
		failed_at TEXT,
		UNIQUE (event_type, verification_id),
		CHECK (delivered_at IS NULL OR failed_at IS NULL),
		CHECK (failed_at IS NULL OR last_error <> ''),
		CHECK (
			(attempt_count = 0 AND last_attempt_at IS NULL AND last_error = '') OR
			(attempt_count > 0 AND last_attempt_at IS NOT NULL AND last_error <> '')
		)
	);

	CREATE INDEX idx_outbox_events_pending
		ON outbox_events(next_attempt_at, created_at, id)
		WHERE delivered_at IS NULL AND failed_at IS NULL;
	CREATE INDEX idx_outbox_events_verification
		ON outbox_events(verification_id);`,
}
