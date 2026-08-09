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
	`CREATE TABLE extractors (
		id TEXT NOT NULL PRIMARY KEY CHECK (
			length(id) = 36 AND
			substr(id, 9, 1) = '-' AND substr(id, 14, 1) = '-' AND
			substr(id, 19, 1) = '-' AND substr(id, 24, 1) = '-' AND
			length(replace(id, '-', '')) = 32 AND
			id NOT GLOB '*[^0-9a-f-]*'
		),
		host TEXT NOT NULL CHECK (
			length(host) BETWEEN 1 AND 253 AND
			host = lower(host) AND host = trim(host)
		),
		schema_json TEXT NOT NULL CHECK (
			json_valid(schema_json) AND json_type(schema_json) = 'object'
		),
		schema_hash TEXT NOT NULL CHECK (
			length(schema_hash) = 64 AND schema_hash NOT GLOB '*[^0-9a-f]*'
		),
		template_cluster_id TEXT NOT NULL CHECK (
			length(template_cluster_id) = 64 AND
			template_cluster_id NOT GLOB '*[^0-9a-f]*'
		),
		template_simhash BLOB NOT NULL CHECK (
			typeof(template_simhash) = 'blob' AND length(template_simhash) = 8 AND
			template_simhash <> zeroblob(8)
		),
		ir TEXT NOT NULL CHECK (json_valid(ir) AND json_type(ir) = 'object'),
		ir_hash TEXT NOT NULL CHECK (
			length(ir_hash) = 64 AND ir_hash NOT GLOB '*[^0-9a-f]*'
		),
		ir_format_version INTEGER NOT NULL CHECK (ir_format_version > 0),
		version INTEGER NOT NULL CHECK (version > 0),
		validation_report TEXT NOT NULL CHECK (
			json_valid(validation_report) AND json_type(validation_report) = 'object'
		),
		validation REAL NOT NULL CHECK (validation >= 0.0 AND validation <= 1.0),
		state TEXT NOT NULL CHECK (state IN ('active', 'stale', 'retired')),
		empty_window TEXT NOT NULL DEFAULT '[]' CHECK (
			json_valid(empty_window) AND json_type(empty_window) = 'array' AND
			json_array_length(empty_window) <= 20
		),
		stale_reason TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL CHECK (
			typeof(created_at) = 'text' AND length(created_at) = 30 AND
			substr(created_at, 5, 1) = '-' AND substr(created_at, 8, 1) = '-' AND
			substr(created_at, 11, 1) = 'T' AND substr(created_at, 14, 1) = ':' AND
			substr(created_at, 17, 1) = ':' AND substr(created_at, 20, 1) = '.' AND
			substr(created_at, 30, 1) = 'Z' AND julianday(created_at) IS NOT NULL
		),
		last_used_at TEXT,
		updated_at TEXT NOT NULL CHECK (
			typeof(updated_at) = 'text' AND length(updated_at) = 30 AND
			substr(updated_at, 5, 1) = '-' AND substr(updated_at, 8, 1) = '-' AND
			substr(updated_at, 11, 1) = 'T' AND substr(updated_at, 14, 1) = ':' AND
			substr(updated_at, 17, 1) = ':' AND substr(updated_at, 20, 1) = '.' AND
			substr(updated_at, 30, 1) = 'Z' AND julianday(updated_at) IS NOT NULL
		),
		CHECK (updated_at >= created_at),
		CHECK (last_used_at IS NULL OR (
			typeof(last_used_at) = 'text' AND length(last_used_at) = 30 AND
			substr(last_used_at, 5, 1) = '-' AND substr(last_used_at, 8, 1) = '-' AND
			substr(last_used_at, 11, 1) = 'T' AND substr(last_used_at, 14, 1) = ':' AND
			substr(last_used_at, 17, 1) = ':' AND substr(last_used_at, 20, 1) = '.' AND
			substr(last_used_at, 30, 1) = 'Z' AND julianday(last_used_at) IS NOT NULL AND
			last_used_at >= created_at AND last_used_at <= updated_at
		)),
		CHECK (
			(state = 'stale' AND stale_reason IN (
				'template_drift', 'required_empty_rate', 'superseded'
			)) OR
			(state = 'active' AND stale_reason = '') OR
			(state = 'retired' AND (
				stale_reason = '' OR stale_reason IN (
					'template_drift', 'required_empty_rate', 'superseded'
				)
			))
		),
		CHECK (
			state <> 'active' OR (
				validation >= 0.9 AND
				json_type(validation_report, '$.can_enable') = 'true' AND
				json_extract(validation_report, '$.can_enable') IS 1
			)
		),
		UNIQUE (id, schema_hash),
		UNIQUE (host, schema_hash, template_cluster_id, version)
	) STRICT;

	CREATE UNIQUE INDEX idx_extractors_active_cluster
		ON extractors(host, schema_hash, template_cluster_id)
		WHERE state = 'active';
	CREATE INDEX idx_extractors_lookup
		ON extractors(host, schema_hash, state, template_cluster_id, version DESC);
	CREATE INDEX idx_extractors_ir_hash
		ON extractors(ir_hash);

	CREATE TRIGGER trg_extractors_state_transition
	BEFORE UPDATE OF state, stale_reason ON extractors
	WHEN NOT (
		(OLD.state = NEW.state AND OLD.stale_reason = NEW.stale_reason) OR
		(OLD.state = 'active' AND NEW.state = 'stale' AND
			NEW.stale_reason IN ('template_drift', 'required_empty_rate', 'superseded')) OR
		(OLD.state = 'active' AND NEW.state = 'retired' AND NEW.stale_reason = '') OR
		(OLD.state = 'stale' AND NEW.state = 'retired' AND
			NEW.stale_reason = OLD.stale_reason)
	)
	BEGIN
		SELECT RAISE(ABORT, 'invalid extractor state transition');
	END;

	CREATE TRIGGER trg_extractors_immutable_revision
	BEFORE UPDATE ON extractors
	WHEN
		OLD.id IS NOT NEW.id OR
		OLD.host IS NOT NEW.host OR
		OLD.schema_json IS NOT NEW.schema_json OR
		OLD.schema_hash IS NOT NEW.schema_hash OR
		OLD.template_cluster_id IS NOT NEW.template_cluster_id OR
		OLD.template_simhash IS NOT NEW.template_simhash OR
		OLD.ir IS NOT NEW.ir OR
		OLD.ir_hash IS NOT NEW.ir_hash OR
		OLD.ir_format_version IS NOT NEW.ir_format_version OR
		OLD.version IS NOT NEW.version OR
		OLD.validation_report IS NOT NEW.validation_report OR
		OLD.validation IS NOT NEW.validation OR
		OLD.created_at IS NOT NEW.created_at
	BEGIN
		SELECT RAISE(ABORT, 'extractor revision is immutable');
	END;

	CREATE TRIGGER trg_extractors_delete_retired_only
	BEFORE DELETE ON extractors
	WHEN OLD.state <> 'retired'
	BEGIN
		SELECT RAISE(ABORT, 'only retired extractors can be deleted');
	END;

	CREATE TRIGGER trg_extractors_empty_window_insert
	BEFORE INSERT ON extractors
	WHEN EXISTS (
		SELECT 1 FROM json_each(NEW.empty_window)
		WHERE type <> 'text' OR value NOT IN ('succeeded', 'required_empty')
	)
	BEGIN
		SELECT RAISE(ABORT, 'invalid extractor empty window');
	END;

	CREATE TRIGGER trg_extractors_empty_window_update
	BEFORE UPDATE OF empty_window ON extractors
	WHEN EXISTS (
		SELECT 1 FROM json_each(NEW.empty_window)
		WHERE type <> 'text' OR value NOT IN ('succeeded', 'required_empty')
	)
	BEGIN
		SELECT RAISE(ABORT, 'invalid extractor empty window');
	END;

	CREATE TABLE extractor_page_bindings (
		page_hash TEXT NOT NULL CHECK (
			length(page_hash) = 64 AND page_hash NOT GLOB '*[^0-9a-f]*'
		),
		schema_hash TEXT NOT NULL CHECK (
			length(schema_hash) = 64 AND schema_hash NOT GLOB '*[^0-9a-f]*'
		),
		extractor_id TEXT NOT NULL,
		bound_at TEXT NOT NULL CHECK (
			typeof(bound_at) = 'text' AND length(bound_at) = 30 AND
			substr(bound_at, 5, 1) = '-' AND substr(bound_at, 8, 1) = '-' AND
			substr(bound_at, 11, 1) = 'T' AND substr(bound_at, 14, 1) = ':' AND
			substr(bound_at, 17, 1) = ':' AND substr(bound_at, 20, 1) = '.' AND
			substr(bound_at, 30, 1) = 'Z' AND julianday(bound_at) IS NOT NULL
		),
		last_seen_at TEXT NOT NULL CHECK (
			typeof(last_seen_at) = 'text' AND length(last_seen_at) = 30 AND
			substr(last_seen_at, 5, 1) = '-' AND substr(last_seen_at, 8, 1) = '-' AND
			substr(last_seen_at, 11, 1) = 'T' AND substr(last_seen_at, 14, 1) = ':' AND
			substr(last_seen_at, 17, 1) = ':' AND substr(last_seen_at, 20, 1) = '.' AND
			substr(last_seen_at, 30, 1) = 'Z' AND julianday(last_seen_at) IS NOT NULL
		),
		CHECK (last_seen_at >= bound_at),
		PRIMARY KEY (page_hash, schema_hash),
		FOREIGN KEY (extractor_id, schema_hash)
			REFERENCES extractors(id, schema_hash) ON DELETE RESTRICT
	) STRICT;

	CREATE INDEX idx_extractor_page_bindings_extractor
		ON extractor_page_bindings(extractor_id);`,
}
