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
	`CREATE TABLE compiler_samples (
		host TEXT NOT NULL CHECK (
			typeof(host) = 'text' AND length(host) BETWEEN 1 AND 253 AND
			host = lower(host) AND host = trim(host)
		),
		schema_hash TEXT NOT NULL CHECK (
			typeof(schema_hash) = 'text' AND length(schema_hash) = 64 AND
			schema_hash NOT GLOB '*[^0-9a-f]*'
		),
		content_profile TEXT NOT NULL CHECK (
			typeof(content_profile) = 'text' AND
			content_profile = 'extract-default-v1'
		),
		template_cluster_id TEXT NOT NULL CHECK (
			typeof(template_cluster_id) = 'text' AND
			length(template_cluster_id) = 64 AND
			template_cluster_id NOT GLOB '*[^0-9a-f]*'
		),
		cluster_simhash BLOB NOT NULL CHECK (
			typeof(cluster_simhash) = 'blob' AND
			length(cluster_simhash) = 8 AND
			cluster_simhash <> zeroblob(8)
		),
		sample_simhash BLOB NOT NULL CHECK (
			typeof(sample_simhash) = 'blob' AND
			length(sample_simhash) = 8 AND
			sample_simhash <> zeroblob(8)
		),
		page_hash TEXT NOT NULL CHECK (
			typeof(page_hash) = 'text' AND length(page_hash) = 64 AND
			page_hash NOT GLOB '*[^0-9a-f]*'
		),
		snapshot_id TEXT NOT NULL CHECK (
			typeof(snapshot_id) = 'text' AND length(snapshot_id) = 71 AND
			substr(snapshot_id, 1, 7) = 'sha256:' AND
			substr(snapshot_id, 8) NOT GLOB '*[^0-9a-f]*'
		),
		fetched_at TEXT NOT NULL CHECK (
			typeof(fetched_at) = 'text' AND length(fetched_at) = 30 AND
			substr(fetched_at, 5, 1) = '-' AND substr(fetched_at, 8, 1) = '-' AND
			substr(fetched_at, 11, 1) = 'T' AND substr(fetched_at, 14, 1) = ':' AND
			substr(fetched_at, 17, 1) = ':' AND substr(fetched_at, 20, 1) = '.' AND
			substr(fetched_at, 30, 1) = 'Z' AND julianday(fetched_at) IS NOT NULL
		),
		first_seen_at TEXT NOT NULL CHECK (
			typeof(first_seen_at) = 'text' AND length(first_seen_at) = 30 AND
			substr(first_seen_at, 5, 1) = '-' AND substr(first_seen_at, 8, 1) = '-' AND
			substr(first_seen_at, 11, 1) = 'T' AND substr(first_seen_at, 14, 1) = ':' AND
			substr(first_seen_at, 17, 1) = ':' AND substr(first_seen_at, 20, 1) = '.' AND
			substr(first_seen_at, 30, 1) = 'Z' AND julianday(first_seen_at) IS NOT NULL
		),
		last_seen_at TEXT NOT NULL CHECK (
			typeof(last_seen_at) = 'text' AND length(last_seen_at) = 30 AND
			substr(last_seen_at, 5, 1) = '-' AND substr(last_seen_at, 8, 1) = '-' AND
			substr(last_seen_at, 11, 1) = 'T' AND substr(last_seen_at, 14, 1) = ':' AND
			substr(last_seen_at, 17, 1) = ':' AND substr(last_seen_at, 20, 1) = '.' AND
			substr(last_seen_at, 30, 1) = 'Z' AND julianday(last_seen_at) IS NOT NULL
		),
		CHECK (last_seen_at >= first_seen_at),
		PRIMARY KEY (host, schema_hash, content_profile, page_hash)
	) STRICT, WITHOUT ROWID;

	CREATE INDEX idx_compiler_samples_compile
		ON compiler_samples(
			host, schema_hash, content_profile, template_cluster_id,
			fetched_at DESC, page_hash, snapshot_id
		);
	CREATE INDEX idx_compiler_samples_cluster
		ON compiler_samples(
			host, schema_hash, content_profile, template_cluster_id,
			cluster_simhash
		);

	CREATE TRIGGER trg_compiler_samples_fixed_cluster_insert
	BEFORE INSERT ON compiler_samples
	WHEN EXISTS (
		SELECT 1 FROM compiler_samples existing
		WHERE existing.host = NEW.host
			AND existing.schema_hash = NEW.schema_hash
			AND existing.content_profile = NEW.content_profile
			AND (
				(existing.template_cluster_id = NEW.template_cluster_id AND
					existing.cluster_simhash IS NOT NEW.cluster_simhash) OR
				(existing.cluster_simhash = NEW.cluster_simhash AND
					existing.template_cluster_id IS NOT NEW.template_cluster_id)
			)
	)
	BEGIN
		SELECT RAISE(ABORT, 'compiler sample cluster representative is not fixed');
	END;

	CREATE TRIGGER trg_compiler_samples_identity_immutable
	BEFORE UPDATE ON compiler_samples
	WHEN
		OLD.host IS NOT NEW.host OR
		OLD.schema_hash IS NOT NEW.schema_hash OR
		OLD.content_profile IS NOT NEW.content_profile OR
		OLD.template_cluster_id IS NOT NEW.template_cluster_id OR
		OLD.cluster_simhash IS NOT NEW.cluster_simhash OR
		OLD.sample_simhash IS NOT NEW.sample_simhash OR
		OLD.page_hash IS NOT NEW.page_hash OR
		OLD.snapshot_id IS NOT NEW.snapshot_id OR
		OLD.fetched_at IS NOT NEW.fetched_at OR
		OLD.first_seen_at IS NOT NEW.first_seen_at
	BEGIN
		SELECT RAISE(ABORT, 'compiler sample identity is immutable');
	END;

	CREATE TRIGGER trg_compiler_samples_last_seen_monotonic
	BEFORE UPDATE OF last_seen_at ON compiler_samples
	WHEN NEW.last_seen_at < OLD.last_seen_at
	BEGIN
		SELECT RAISE(ABORT, 'compiler sample last_seen_at cannot move backward');
	END;`,
	`CREATE TABLE compiler_attempts (
		host TEXT NOT NULL CHECK (
			typeof(host) = 'text' AND length(host) BETWEEN 1 AND 253 AND
			host = lower(host) AND host = trim(host)
		),
		schema_hash TEXT NOT NULL CHECK (
			typeof(schema_hash) = 'text' AND length(schema_hash) = 64 AND
			schema_hash NOT GLOB '*[^0-9a-f]*'
		),
		content_profile TEXT NOT NULL CHECK (
			typeof(content_profile) = 'text' AND
			content_profile = 'extract-default-v1'
		),
		template_cluster_id TEXT NOT NULL CHECK (
			typeof(template_cluster_id) = 'text' AND
			length(template_cluster_id) = 64 AND
			template_cluster_id NOT GLOB '*[^0-9a-f]*'
		),
		cluster_simhash BLOB NOT NULL CHECK (
			typeof(cluster_simhash) = 'blob' AND length(cluster_simhash) = 8 AND
			cluster_simhash <> zeroblob(8)
		),
		attempted_revision TEXT NOT NULL DEFAULT '' CHECK (
			attempted_revision = '' OR (
				length(attempted_revision) = 64 AND
				attempted_revision NOT GLOB '*[^0-9a-f]*'
			)
		),
		outcome TEXT NOT NULL DEFAULT '' CHECK (
			outcome IN ('', 'success', 'weak', 'no_candidate', 'transient')
		),
		reason TEXT NOT NULL DEFAULT '' CHECK (
			reason IN (
				'', 'compiled', 'validation_below_threshold',
				'samples_unavailable', 'sample_provenance_mismatch',
				'sample_fingerprint_mismatch', 'no_extractor_candidate',
				'catalog_unavailable', 'snapshot_unavailable', 'compile_failed',
				'save_failed', 'task_timeout', 'task_canceled', 'panic_recovered'
			)
		),
		cooldown_until TEXT CHECK (
			cooldown_until IS NULL OR (
				typeof(cooldown_until) = 'text' AND length(cooldown_until) = 30 AND
				substr(cooldown_until, 5, 1) = '-' AND substr(cooldown_until, 8, 1) = '-' AND
				substr(cooldown_until, 11, 1) = 'T' AND substr(cooldown_until, 14, 1) = ':' AND
				substr(cooldown_until, 17, 1) = ':' AND substr(cooldown_until, 20, 1) = '.' AND
				substr(cooldown_until, 30, 1) = 'Z' AND julianday(cooldown_until) IS NOT NULL
			)
		),
		lease_id TEXT CHECK (
			lease_id IS NULL OR (
				length(lease_id) = 36 AND
				substr(lease_id, 9, 1) = '-' AND substr(lease_id, 14, 1) = '-' AND
				substr(lease_id, 19, 1) = '-' AND substr(lease_id, 24, 1) = '-' AND
				length(replace(lease_id, '-', '')) = 32 AND
				lease_id NOT GLOB '*[^0-9a-f-]*'
			)
		),
		lease_revision TEXT CHECK (
			lease_revision IS NULL OR (
				length(lease_revision) = 64 AND
				lease_revision NOT GLOB '*[^0-9a-f]*'
			)
		),
		lease_until TEXT CHECK (
			lease_until IS NULL OR (
				typeof(lease_until) = 'text' AND length(lease_until) = 30 AND
				substr(lease_until, 5, 1) = '-' AND substr(lease_until, 8, 1) = '-' AND
				substr(lease_until, 11, 1) = 'T' AND substr(lease_until, 14, 1) = ':' AND
				substr(lease_until, 17, 1) = ':' AND substr(lease_until, 20, 1) = '.' AND
				substr(lease_until, 30, 1) = 'Z' AND julianday(lease_until) IS NOT NULL
			)
		),
		created_at TEXT NOT NULL CHECK (
			typeof(created_at) = 'text' AND length(created_at) = 30 AND
			substr(created_at, 5, 1) = '-' AND substr(created_at, 8, 1) = '-' AND
			substr(created_at, 11, 1) = 'T' AND substr(created_at, 14, 1) = ':' AND
			substr(created_at, 17, 1) = ':' AND substr(created_at, 20, 1) = '.' AND
			substr(created_at, 30, 1) = 'Z' AND julianday(created_at) IS NOT NULL
		),
		updated_at TEXT NOT NULL CHECK (
			typeof(updated_at) = 'text' AND length(updated_at) = 30 AND
			substr(updated_at, 5, 1) = '-' AND substr(updated_at, 8, 1) = '-' AND
			substr(updated_at, 11, 1) = 'T' AND substr(updated_at, 14, 1) = ':' AND
			substr(updated_at, 17, 1) = ':' AND substr(updated_at, 20, 1) = '.' AND
			substr(updated_at, 30, 1) = 'Z' AND julianday(updated_at) IS NOT NULL
		),
		CHECK (updated_at >= created_at),
		CHECK (cooldown_until IS NULL OR cooldown_until > updated_at),
		CHECK (lease_until IS NULL OR lease_until > updated_at),
		CHECK (
			(outcome = '' AND attempted_revision = '' AND reason = '' AND cooldown_until IS NULL) OR
			(outcome = 'success' AND attempted_revision <> '' AND reason = 'compiled' AND cooldown_until IS NULL) OR
			(outcome = 'weak' AND attempted_revision <> '' AND reason = 'validation_below_threshold' AND cooldown_until IS NOT NULL) OR
			(outcome = 'no_candidate' AND attempted_revision <> '' AND reason IN (
				'samples_unavailable', 'sample_provenance_mismatch',
				'sample_fingerprint_mismatch', 'no_extractor_candidate'
			) AND cooldown_until IS NOT NULL) OR
			(outcome = 'transient' AND attempted_revision <> '' AND reason IN (
				'catalog_unavailable', 'snapshot_unavailable', 'compile_failed',
				'save_failed', 'task_timeout', 'task_canceled', 'panic_recovered'
			) AND cooldown_until IS NOT NULL)
		),
		CHECK (
			(lease_id IS NULL AND lease_revision IS NULL AND lease_until IS NULL) OR
			(lease_id IS NOT NULL AND lease_revision IS NOT NULL AND lease_until IS NOT NULL)
		),
		PRIMARY KEY (host, schema_hash, content_profile, template_cluster_id)
	) STRICT, WITHOUT ROWID;

	CREATE INDEX idx_compiler_attempts_cooldown
		ON compiler_attempts(cooldown_until, updated_at)
		WHERE cooldown_until IS NOT NULL;
	CREATE INDEX idx_compiler_attempts_lease
		ON compiler_attempts(lease_until, updated_at)
		WHERE lease_until IS NOT NULL;
	CREATE INDEX idx_compiler_attempts_updated
		ON compiler_attempts(updated_at, host, schema_hash, content_profile, template_cluster_id);

	CREATE TRIGGER trg_compiler_attempts_identity_immutable
	BEFORE UPDATE ON compiler_attempts
	WHEN
		OLD.host IS NOT NEW.host OR
		OLD.schema_hash IS NOT NEW.schema_hash OR
		OLD.content_profile IS NOT NEW.content_profile OR
		OLD.template_cluster_id IS NOT NEW.template_cluster_id OR
		OLD.cluster_simhash IS NOT NEW.cluster_simhash OR
		OLD.created_at IS NOT NEW.created_at
	BEGIN
		SELECT RAISE(ABORT, 'compiler attempt identity is immutable');
	END;

	CREATE TRIGGER trg_compiler_attempts_updated_monotonic
	BEFORE UPDATE OF updated_at ON compiler_attempts
	WHEN NEW.updated_at < OLD.updated_at
	BEGIN
		SELECT RAISE(ABORT, 'compiler attempt updated_at cannot move backward');
	END;`,
	`CREATE TABLE extractor_heal_runs (
		id TEXT NOT NULL PRIMARY KEY CHECK (
			length(id) = 36 AND
			substr(id, 9, 1) = '-' AND substr(id, 14, 1) = '-' AND
			substr(id, 19, 1) = '-' AND substr(id, 24, 1) = '-' AND
			length(replace(id, '-', '')) = 32 AND
			id NOT GLOB '*[^0-9a-f-]*'
		),
		source_extractor_id TEXT NOT NULL,
		source_version INTEGER NOT NULL CHECK (source_version > 0),
		host TEXT NOT NULL CHECK (
			typeof(host) = 'text' AND length(host) BETWEEN 1 AND 253 AND
			host = lower(host) AND host = trim(host)
		),
		schema_json TEXT NOT NULL CHECK (
			json_valid(schema_json) AND json_type(schema_json) = 'object' AND
			length(schema_json) <= 524288
		),
		schema_hash TEXT NOT NULL CHECK (
			length(schema_hash) = 64 AND schema_hash NOT GLOB '*[^0-9a-f]*'
		),
		content_profile TEXT NOT NULL CHECK (content_profile = 'extract-default-v1'),
		target_template_cluster_id TEXT NOT NULL CHECK (
			length(target_template_cluster_id) = 64 AND
			target_template_cluster_id NOT GLOB '*[^0-9a-f]*'
		),
		target_cluster_simhash BLOB NOT NULL CHECK (
			typeof(target_cluster_simhash) = 'blob' AND
			length(target_cluster_simhash) = 8 AND
			target_cluster_simhash <> zeroblob(8)
		),
		catalog_revision TEXT NOT NULL CHECK (
			length(catalog_revision) = 64 AND
			catalog_revision NOT GLOB '*[^0-9a-f]*'
		),
		candidate_ir TEXT NOT NULL CHECK (
			json_valid(candidate_ir) AND json_type(candidate_ir) = 'object' AND
			length(candidate_ir) <= 524288
		),
		candidate_ir_hash TEXT NOT NULL CHECK (
			length(candidate_ir_hash) = 64 AND
			candidate_ir_hash NOT GLOB '*[^0-9a-f]*'
		),
		candidate_ir_format_version INTEGER NOT NULL CHECK (
			candidate_ir_format_version > 0
		),
		validation_report TEXT NOT NULL CHECK (
			json_valid(validation_report) AND
			json_type(validation_report) = 'object' AND
			length(validation_report) <= 524288 AND
			json_type(validation_report, '$.can_enable') = 'true' AND
			json_extract(validation_report, '$.can_enable') IS 1
		),
		validation REAL NOT NULL CHECK (validation >= 0.9 AND validation <= 1.0),
		samples_json TEXT NOT NULL CHECK (
			json_valid(samples_json) AND json_type(samples_json) = 'array' AND
			json_array_length(samples_json) BETWEEN 3 AND 20 AND
			length(samples_json) <= 16384
		),
		state TEXT NOT NULL DEFAULT 'pending' CHECK (
			state IN ('pending', 'replaying', 'promoted', 'degraded', 'failed')
		),
		terminal_reason TEXT NOT NULL DEFAULT '' CHECK (length(terminal_reason) <= 256),
		lease_id TEXT CHECK (
			lease_id IS NULL OR (
				length(lease_id) = 36 AND
				substr(lease_id, 9, 1) = '-' AND substr(lease_id, 14, 1) = '-' AND
				substr(lease_id, 19, 1) = '-' AND substr(lease_id, 24, 1) = '-' AND
				length(replace(lease_id, '-', '')) = 32 AND
				lease_id NOT GLOB '*[^0-9a-f-]*'
			)
		),
		lease_until TEXT CHECK (
			lease_until IS NULL OR (
				typeof(lease_until) = 'text' AND length(lease_until) = 30 AND
				substr(lease_until, 5, 1) = '-' AND substr(lease_until, 8, 1) = '-' AND
				substr(lease_until, 11, 1) = 'T' AND substr(lease_until, 14, 1) = ':' AND
				substr(lease_until, 17, 1) = ':' AND substr(lease_until, 20, 1) = '.' AND
				substr(lease_until, 30, 1) = 'Z' AND julianday(lease_until) IS NOT NULL
			)
		),
		replay_total INTEGER NOT NULL DEFAULT 0 CHECK (replay_total >= 0),
		replay_matched INTEGER NOT NULL DEFAULT 0 CHECK (
			replay_matched >= 0 AND replay_matched <= replay_total
		),
		replay_ratio REAL CHECK (
			replay_ratio IS NULL OR (replay_ratio >= 0.0 AND replay_ratio <= 1.0)
		),
		promoted_extractor_id TEXT,
		created_at TEXT NOT NULL CHECK (
			typeof(created_at) = 'text' AND length(created_at) = 30 AND
			substr(created_at, 5, 1) = '-' AND substr(created_at, 8, 1) = '-' AND
			substr(created_at, 11, 1) = 'T' AND substr(created_at, 14, 1) = ':' AND
			substr(created_at, 17, 1) = ':' AND substr(created_at, 20, 1) = '.' AND
			substr(created_at, 30, 1) = 'Z' AND julianday(created_at) IS NOT NULL
		),
		updated_at TEXT NOT NULL CHECK (
			typeof(updated_at) = 'text' AND length(updated_at) = 30 AND
			substr(updated_at, 5, 1) = '-' AND substr(updated_at, 8, 1) = '-' AND
			substr(updated_at, 11, 1) = 'T' AND substr(updated_at, 14, 1) = ':' AND
			substr(updated_at, 17, 1) = ':' AND substr(updated_at, 20, 1) = '.' AND
			substr(updated_at, 30, 1) = 'Z' AND julianday(updated_at) IS NOT NULL
		),
		completed_at TEXT CHECK (
			completed_at IS NULL OR (
				typeof(completed_at) = 'text' AND length(completed_at) = 30 AND
				substr(completed_at, 5, 1) = '-' AND substr(completed_at, 8, 1) = '-' AND
				substr(completed_at, 11, 1) = 'T' AND substr(completed_at, 14, 1) = ':' AND
				substr(completed_at, 17, 1) = ':' AND substr(completed_at, 20, 1) = '.' AND
				substr(completed_at, 30, 1) = 'Z' AND julianday(completed_at) IS NOT NULL
			)
		),
		CHECK (updated_at >= created_at),
		CHECK (completed_at IS NULL OR completed_at >= updated_at),
		CHECK (lease_until IS NULL OR lease_until > updated_at),
		CHECK (
			(replay_total = 0 AND replay_matched = 0 AND replay_ratio IS NULL) OR
			(replay_total > 0 AND replay_ratio IS NOT NULL AND
				abs(replay_ratio - CAST(replay_matched AS REAL) / replay_total) <= 0.000000000001)
		),
		CHECK (
			(state = 'pending' AND terminal_reason = '' AND lease_id IS NULL AND
				lease_until IS NULL AND replay_total = 0 AND replay_matched = 0 AND
				replay_ratio IS NULL AND promoted_extractor_id IS NULL AND completed_at IS NULL) OR
			(state = 'replaying' AND terminal_reason = '' AND lease_id IS NOT NULL AND
				lease_until IS NOT NULL AND promoted_extractor_id IS NULL AND completed_at IS NULL) OR
			(state = 'promoted' AND terminal_reason = 'replay_passed' AND
				lease_id IS NULL AND lease_until IS NULL AND replay_total > 0 AND
				replay_ratio >= 0.9 AND promoted_extractor_id IS NOT NULL AND completed_at IS NOT NULL) OR
			(state IN ('degraded', 'failed') AND terminal_reason <> '' AND
				lease_id IS NULL AND lease_until IS NULL AND promoted_extractor_id IS NULL AND
				completed_at IS NOT NULL)
		),
		FOREIGN KEY (source_extractor_id) REFERENCES extractors(id) ON DELETE RESTRICT,
		FOREIGN KEY (promoted_extractor_id) REFERENCES extractors(id) ON DELETE RESTRICT
	) STRICT;

	CREATE UNIQUE INDEX idx_extractor_heal_runs_pending_target
		ON extractor_heal_runs(host, schema_hash, target_template_cluster_id)
		WHERE state IN ('pending', 'replaying');
	CREATE INDEX idx_extractor_heal_runs_source
		ON extractor_heal_runs(source_extractor_id, created_at DESC, id);
	CREATE INDEX idx_extractor_heal_runs_lease
		ON extractor_heal_runs(lease_until, updated_at, id)
		WHERE state = 'replaying';
	CREATE INDEX idx_extractor_heal_runs_terminal
		ON extractor_heal_runs(state, completed_at DESC, id)
		WHERE state IN ('promoted', 'degraded', 'failed');

	CREATE INDEX idx_verifications_exact_heal_replay
		ON verifications(
			extractor_id, schema_hash, template_cluster_id, verified_at DESC, id
		)
		WHERE outcome = 'confirmed' AND extractor_id IS NOT NULL AND
			schema_hash IS NOT NULL AND template_cluster_id IS NOT NULL;

	CREATE TRIGGER trg_extractor_heal_runs_identity_immutable
	BEFORE UPDATE ON extractor_heal_runs
	WHEN
		OLD.id IS NOT NEW.id OR
		OLD.source_extractor_id IS NOT NEW.source_extractor_id OR
		OLD.source_version IS NOT NEW.source_version OR
		OLD.host IS NOT NEW.host OR
		OLD.schema_json IS NOT NEW.schema_json OR
		OLD.schema_hash IS NOT NEW.schema_hash OR
		OLD.content_profile IS NOT NEW.content_profile OR
		OLD.target_template_cluster_id IS NOT NEW.target_template_cluster_id OR
		OLD.target_cluster_simhash IS NOT NEW.target_cluster_simhash OR
		OLD.catalog_revision IS NOT NEW.catalog_revision OR
		OLD.candidate_ir IS NOT NEW.candidate_ir OR
		OLD.candidate_ir_hash IS NOT NEW.candidate_ir_hash OR
		OLD.candidate_ir_format_version IS NOT NEW.candidate_ir_format_version OR
		OLD.validation_report IS NOT NEW.validation_report OR
		OLD.validation IS NOT NEW.validation OR
		OLD.samples_json IS NOT NEW.samples_json OR
		OLD.created_at IS NOT NEW.created_at
	BEGIN
		SELECT RAISE(ABORT, 'extractor heal candidate is immutable');
	END;

	CREATE TRIGGER trg_extractor_heal_runs_source_exact
	BEFORE INSERT ON extractor_heal_runs
	WHEN NOT EXISTS (
		SELECT 1 FROM extractors source
		WHERE source.id = NEW.source_extractor_id AND
			source.version = NEW.source_version AND source.host = NEW.host AND
			source.schema_hash = NEW.schema_hash AND source.state = 'stale'
	)
	BEGIN
		SELECT RAISE(ABORT, 'extractor heal source is not the exact stale revision');
	END;

	CREATE TRIGGER trg_extractor_heal_runs_samples_insert
	BEFORE INSERT ON extractor_heal_runs
	WHEN EXISTS (
		SELECT 1 FROM json_each(NEW.samples_json) sample
		WHERE sample.type <> 'object' OR
			json_type(sample.value, '$.page_hash') IS NOT 'text' OR
			length(json_extract(sample.value, '$.page_hash')) <> 64 OR
			json_extract(sample.value, '$.page_hash') GLOB '*[^0-9a-f]*' OR
			json_type(sample.value, '$.snapshot_id') IS NOT 'text' OR
			length(json_extract(sample.value, '$.snapshot_id')) <> 71 OR
			substr(json_extract(sample.value, '$.snapshot_id'), 1, 7) <> 'sha256:' OR
			substr(json_extract(sample.value, '$.snapshot_id'), 8) GLOB '*[^0-9a-f]*' OR
			json_type(sample.value, '$.sample_simhash') IS NOT 'text' OR
			length(json_extract(sample.value, '$.sample_simhash')) <> 16 OR
			json_extract(sample.value, '$.sample_simhash') GLOB '*[^0-9a-f]*' OR
			json_extract(sample.value, '$.sample_simhash') = '0000000000000000' OR
			json_type(sample.value, '$.fetched_at') IS NOT 'text' OR
			length(json_extract(sample.value, '$.fetched_at')) <> 30 OR
			substr(json_extract(sample.value, '$.fetched_at'), 5, 1) <> '-' OR
			substr(json_extract(sample.value, '$.fetched_at'), 8, 1) <> '-' OR
			substr(json_extract(sample.value, '$.fetched_at'), 11, 1) <> 'T' OR
			substr(json_extract(sample.value, '$.fetched_at'), 14, 1) <> ':' OR
			substr(json_extract(sample.value, '$.fetched_at'), 17, 1) <> ':' OR
			substr(json_extract(sample.value, '$.fetched_at'), 20, 1) <> '.' OR
			substr(json_extract(sample.value, '$.fetched_at'), 30, 1) <> 'Z' OR
			julianday(json_extract(sample.value, '$.fetched_at')) IS NULL OR
			(SELECT COUNT(*) FROM json_each(sample.value)) <> 4
	)
	BEGIN
		SELECT RAISE(ABORT, 'invalid extractor heal sample');
	END;

	CREATE TRIGGER trg_extractor_heal_runs_state_transition
	BEFORE UPDATE OF state ON extractor_heal_runs
	WHEN NOT (
		(OLD.state = NEW.state) OR
		(OLD.state = 'pending' AND NEW.state = 'replaying') OR
		(OLD.state = 'replaying' AND NEW.state IN ('pending', 'promoted', 'degraded', 'failed'))
	)
	BEGIN
		SELECT RAISE(ABORT, 'invalid extractor heal state transition');
	END;

	CREATE TRIGGER trg_extractor_heal_runs_terminal_immutable
	BEFORE UPDATE ON extractor_heal_runs
	WHEN OLD.state IN ('promoted', 'degraded', 'failed')
	BEGIN
		SELECT RAISE(ABORT, 'terminal extractor heal run is immutable');
	END;

	CREATE TRIGGER trg_extractor_heal_runs_updated_monotonic
	BEFORE UPDATE OF updated_at ON extractor_heal_runs
	WHEN NEW.updated_at < OLD.updated_at
	BEGIN
		SELECT RAISE(ABORT, 'extractor heal updated_at cannot move backward');
	END;

	CREATE TRIGGER trg_extractor_heal_runs_delete_guard
	BEFORE DELETE ON extractor_heal_runs
	BEGIN
		SELECT RAISE(ABORT, 'extractor heal audit cannot be deleted');
	END;

	CREATE TRIGGER trg_extractors_delete_audit_guard
	BEFORE DELETE ON extractors
	WHEN
		EXISTS (SELECT 1 FROM verifications WHERE extractor_id = OLD.id) OR
		EXISTS (SELECT 1 FROM extractor_heal_runs
			WHERE source_extractor_id = OLD.id OR promoted_extractor_id = OLD.id)
	BEGIN
		SELECT RAISE(ABORT, 'extractor is referenced by durable audit history');
	END;`,
	`ALTER TABLE outbox_events RENAME TO outbox_events_verification_v2;

	CREATE TABLE outbox_events (
		id TEXT NOT NULL PRIMARY KEY CHECK (
			typeof(id) = 'text' AND
			length(CAST(id AS BLOB)) BETWEEN 1 AND 512 AND
			id = trim(id)
		),
		verification_id TEXT CHECK (
			verification_id IS NULL OR (
				typeof(verification_id) = 'text' AND
				length(CAST(verification_id AS BLOB)) BETWEEN 1 AND 512 AND
				verification_id = trim(verification_id)
			)
		),
		subject_type TEXT NOT NULL CHECK (
			typeof(subject_type) = 'text' AND
			subject_type IN ('verification', 'extractor_heal')
		),
		subject_id TEXT NOT NULL CHECK (
			typeof(subject_id) = 'text' AND
			length(CAST(subject_id AS BLOB)) BETWEEN 1 AND 512 AND
			subject_id = trim(subject_id)
		),
		event_type TEXT NOT NULL CHECK (
			typeof(event_type) = 'text' AND
			length(CAST(event_type AS BLOB)) BETWEEN 1 AND 128 AND
			event_type = trim(event_type)
		),
		destination_url TEXT NOT NULL CHECK (
			typeof(destination_url) = 'text' AND
			length(CAST(destination_url AS BLOB)) BETWEEN 1 AND 16384 AND
			destination_url = trim(destination_url) AND
			(substr(destination_url, 1, 7) = 'http://' OR
				substr(destination_url, 1, 8) = 'https://')
		),
		secret TEXT NOT NULL DEFAULT '' CHECK (
			typeof(secret) = 'text' AND
			length(CAST(secret AS BLOB)) <= 16384
		),
		payload TEXT NOT NULL CHECK (
			typeof(payload) = 'text' AND
			length(CAST(payload AS BLOB)) BETWEEN 1 AND 33554432 AND
			json_valid(payload)
		),
		created_at TEXT NOT NULL CHECK (
			typeof(created_at) = 'text' AND length(created_at) = 30 AND
			substr(created_at, 5, 1) = '-' AND substr(created_at, 8, 1) = '-' AND
			substr(created_at, 11, 1) = 'T' AND substr(created_at, 14, 1) = ':' AND
			substr(created_at, 17, 1) = ':' AND substr(created_at, 20, 1) = '.' AND
			substr(created_at, 30, 1) = 'Z' AND julianday(created_at) IS NOT NULL
		),
		attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (
			typeof(attempt_count) = 'integer' AND attempt_count >= 0
		),
		last_attempt_at TEXT CHECK (
			last_attempt_at IS NULL OR (
				typeof(last_attempt_at) = 'text' AND length(last_attempt_at) = 30 AND
				substr(last_attempt_at, 5, 1) = '-' AND substr(last_attempt_at, 8, 1) = '-' AND
				substr(last_attempt_at, 11, 1) = 'T' AND substr(last_attempt_at, 14, 1) = ':' AND
				substr(last_attempt_at, 17, 1) = ':' AND substr(last_attempt_at, 20, 1) = '.' AND
				substr(last_attempt_at, 30, 1) = 'Z' AND julianday(last_attempt_at) IS NOT NULL
			)
		),
		last_error TEXT NOT NULL DEFAULT '' CHECK (
			typeof(last_error) = 'text' AND
			length(CAST(last_error AS BLOB)) <= 65536
		),
		next_attempt_at TEXT NOT NULL CHECK (
			typeof(next_attempt_at) = 'text' AND length(next_attempt_at) = 30 AND
			substr(next_attempt_at, 5, 1) = '-' AND substr(next_attempt_at, 8, 1) = '-' AND
			substr(next_attempt_at, 11, 1) = 'T' AND substr(next_attempt_at, 14, 1) = ':' AND
			substr(next_attempt_at, 17, 1) = ':' AND substr(next_attempt_at, 20, 1) = '.' AND
			substr(next_attempt_at, 30, 1) = 'Z' AND julianday(next_attempt_at) IS NOT NULL
		),
		delivered_at TEXT CHECK (
			delivered_at IS NULL OR (
				typeof(delivered_at) = 'text' AND length(delivered_at) = 30 AND
				substr(delivered_at, 5, 1) = '-' AND substr(delivered_at, 8, 1) = '-' AND
				substr(delivered_at, 11, 1) = 'T' AND substr(delivered_at, 14, 1) = ':' AND
				substr(delivered_at, 17, 1) = ':' AND substr(delivered_at, 20, 1) = '.' AND
				substr(delivered_at, 30, 1) = 'Z' AND julianday(delivered_at) IS NOT NULL
			)
		),
		failed_at TEXT CHECK (
			failed_at IS NULL OR (
				typeof(failed_at) = 'text' AND length(failed_at) = 30 AND
				substr(failed_at, 5, 1) = '-' AND substr(failed_at, 8, 1) = '-' AND
				substr(failed_at, 11, 1) = 'T' AND substr(failed_at, 14, 1) = ':' AND
				substr(failed_at, 17, 1) = ':' AND substr(failed_at, 20, 1) = '.' AND
				substr(failed_at, 30, 1) = 'Z' AND julianday(failed_at) IS NOT NULL
			)
		),
		UNIQUE (event_type, subject_type, subject_id),
		CHECK (
			(subject_type = 'verification' AND verification_id IS NOT NULL AND
				verification_id = subject_id) OR
			(subject_type = 'extractor_heal' AND verification_id IS NULL)
		),
		CHECK (
			subject_type <> 'extractor_heal' OR (
				length(subject_id) = 36 AND
				substr(subject_id, 9, 1) = '-' AND
				substr(subject_id, 14, 1) = '-' AND
				substr(subject_id, 19, 1) = '-' AND
				substr(subject_id, 24, 1) = '-' AND
				length(replace(subject_id, '-', '')) = 32 AND
				subject_id NOT GLOB '*[^0-9a-f-]*'
			)
		),
		CHECK (
			subject_type <> 'extractor_heal' OR
			event_type IN ('extractor.promoted', 'extractor.degraded')
		),
		CHECK (next_attempt_at >= created_at),
		CHECK (last_attempt_at IS NULL OR last_attempt_at >= created_at),
		CHECK (delivered_at IS NULL OR (
			delivered_at >= created_at AND
			(last_attempt_at IS NULL OR delivered_at >= last_attempt_at)
		)),
		CHECK (failed_at IS NULL OR (
			failed_at >= created_at AND
			(last_attempt_at IS NULL OR failed_at >= last_attempt_at)
		)),
		CHECK (delivered_at IS NULL OR failed_at IS NULL),
		CHECK (failed_at IS NULL OR last_error <> ''),
		CHECK (
			(attempt_count = 0 AND last_attempt_at IS NULL AND last_error = '') OR
			(attempt_count > 0 AND last_attempt_at IS NOT NULL AND last_error <> '')
		)
	) STRICT;

	CREATE TRIGGER trg_outbox_events_verification_subject
	BEFORE INSERT ON outbox_events
	WHEN NEW.subject_type = 'verification' AND NOT EXISTS (
		SELECT 1 FROM verifications verification
		WHERE verification.verification_id = NEW.subject_id
	)
	BEGIN
		SELECT RAISE(ABORT, 'verification outbox subject does not exist');
	END;

	CREATE TRIGGER trg_outbox_events_heal_terminal_subject
	BEFORE INSERT ON outbox_events
	WHEN NEW.subject_type = 'extractor_heal' AND NOT EXISTS (
		SELECT 1 FROM extractor_heal_runs run
		WHERE run.id = NEW.subject_id AND (
			(NEW.event_type = 'extractor.promoted' AND run.state = 'promoted') OR
			(NEW.event_type = 'extractor.degraded' AND
				run.state IN ('degraded', 'failed'))
		)
	)
	BEGIN
		SELECT RAISE(ABORT, 'extractor heal outbox subject is not terminal');
	END;

	INSERT INTO outbox_events (
		id, verification_id, subject_type, subject_id, event_type,
		destination_url, secret, payload, created_at, attempt_count,
		last_attempt_at, last_error, next_attempt_at, delivered_at, failed_at
	)
	SELECT
		id, verification_id, 'verification', verification_id, event_type,
		destination_url, secret, payload, created_at, attempt_count,
		last_attempt_at, last_error, next_attempt_at, delivered_at, failed_at
	FROM outbox_events_verification_v2;

	DROP TABLE outbox_events_verification_v2;

	CREATE INDEX idx_outbox_events_pending
		ON outbox_events(next_attempt_at, created_at, id)
		WHERE delivered_at IS NULL AND failed_at IS NULL;
	CREATE INDEX idx_outbox_events_verification
		ON outbox_events(verification_id)
		WHERE verification_id IS NOT NULL;
	CREATE INDEX idx_outbox_events_subject
		ON outbox_events(subject_type, subject_id);

	CREATE TRIGGER trg_outbox_events_immutable_identity
	BEFORE UPDATE ON outbox_events
	WHEN
		OLD.id IS NOT NEW.id OR
		OLD.verification_id IS NOT NEW.verification_id OR
		OLD.subject_type IS NOT NEW.subject_type OR
		OLD.subject_id IS NOT NEW.subject_id OR
		OLD.event_type IS NOT NEW.event_type OR
		OLD.destination_url IS NOT NEW.destination_url OR
		OLD.secret IS NOT NEW.secret OR
		OLD.payload IS NOT NEW.payload OR
		OLD.created_at IS NOT NEW.created_at
	BEGIN
		SELECT RAISE(ABORT, 'outbox event identity is immutable');
	END;`,
	`CREATE INDEX idx_extractor_heal_runs_actionable
		ON extractor_heal_runs(created_at, id, state, lease_until)
		WHERE state IN ('pending', 'replaying');`,
}
