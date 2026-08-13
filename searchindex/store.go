package searchindex

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const (
	busyTimeoutMilliseconds = 5000

	LangEnglish = "en"
	LangChinese = "zh"
)

var (
	ErrClosed        = errors.New("searchindex: store is closed")
	ErrInvalidConfig = errors.New("searchindex: invalid configuration")
	ErrInvalidPage   = errors.New("searchindex: invalid page")
)

// Page is one indexed document. Body is cleaned text only.
type Page struct {
	URL         string
	Root        string
	Title       string
	Body        string
	Lang        string
	FetchedAt   time.Time
	ContentHash string
	ETag        string
	LastMod     string
	NeedsRender bool
}

// Store owns the index SQLite file. Writes are serialized.
type Store struct {
	db      *sql.DB
	path    string
	gate    sync.RWMutex
	writeMu sync.Mutex
	closed  bool
}

// Open creates or opens the index database at path, applies migrations, and
// sets the same WAL / busy-timeout / exclusive-write posture as ledger.
func Open(path string) (*Store, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("%w: database path is required", ErrInvalidConfig)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("searchindex: resolve path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return nil, fmt.Errorf("searchindex: create directory: %w", err)
	}
	if err := os.Chmod(filepath.Dir(abs), 0o700); err != nil && !errors.Is(err, os.ErrPermission) {
		return nil, fmt.Errorf("searchindex: secure directory: %w", err)
	}

	dsn := (&url.URL{
		Scheme: "file",
		Path:   abs,
		RawQuery: url.Values{
			"_busy_timeout": []string{fmt.Sprint(busyTimeoutMilliseconds)},
			"_foreign_keys": []string{"on"},
			"_journal_mode": []string{"wal"},
			"_synchronous":  []string{"normal"},
			"_txlock":       []string{"immediate"},
		}.Encode(),
	}).String()

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("searchindex: open database: %w", err)
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	db.SetConnMaxLifetime(0)

	store := &Store{db: db, path: abs}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("searchindex: connect database: %w", err)
	}
	if err := store.applyMigrations(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := os.Chmod(abs, 0o600); err != nil && !errors.Is(err, os.ErrPermission) {
		_ = db.Close()
		return nil, fmt.Errorf("searchindex: secure database: %w", err)
	}
	return store, nil
}

// Path returns the absolute database filename.
func (s *Store) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// Close waits for in-flight work and closes the database.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.gate.Lock()
	defer s.gate.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.db.Close()
}

// HashContent returns the stable content hash used for page dedup.
func HashContent(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// Upsert writes one page and keeps the matching FTS table in sync. A repeat
// URL with the same content hash is a no-op.
func (s *Store) Upsert(ctx context.Context, page Page) error {
	_, err := s.UpsertMany(ctx, []Page{page})
	return err
}

// CountPages returns the number of stored pages.
func (s *Store) CountPages(ctx context.Context) (int, error) {
	if err := s.guard(ctx); err != nil {
		return 0, err
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pages`).Scan(&count); err != nil {
		return 0, fmt.Errorf("searchindex: count pages: %w", err)
	}
	return count, nil
}

func (s *Store) guard(ctx context.Context) error {
	if s == nil {
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.gate.RLock()
	defer s.gate.RUnlock()
	if s.closed {
		return ErrClosed
	}
	return nil
}

func (s *Store) applyMigrations(ctx context.Context) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("searchindex: begin migrations: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("searchindex: create migration table: %w", err)
	}
	for index, statement := range migrations {
		version := index + 1
		var exists int
		err := tx.QueryRowContext(ctx, "SELECT 1 FROM schema_migrations WHERE version = ?", version).Scan(&exists)
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("searchindex: read migration %d: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("searchindex: apply migration %d: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)",
			version, time.Now().UTC().Format(time.RFC3339Nano),
		); err != nil {
			return fmt.Errorf("searchindex: record migration %d: %w", version, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("searchindex: commit migrations: %w", err)
	}
	return nil
}

func normalizePage(page Page) (Page, error) {
	page.URL = strings.TrimSpace(page.URL)
	page.Root = strings.TrimSpace(page.Root)
	page.Title = strings.TrimSpace(page.Title)
	page.Lang = strings.TrimSpace(page.Lang)
	if page.URL == "" || page.Root == "" {
		return Page{}, fmt.Errorf("%w: url and root are required", ErrInvalidPage)
	}
	if page.Lang != LangEnglish && page.Lang != LangChinese {
		return Page{}, fmt.Errorf("%w: lang must be en or zh", ErrInvalidPage)
	}
	if page.FetchedAt.IsZero() {
		page.FetchedAt = time.Now().UTC()
	}
	if page.ContentHash == "" {
		page.ContentHash = HashContent(page.Body)
	}
	return page, nil
}

// insertFTS writes the rewritten token stream. The table is contentless, so
// this stream is free to differ from the text kept in pages.
func insertFTS(ctx context.Context, tx *sql.Tx, id int64, page Page) error {
	title, err := indexText(page.Lang, page.Title)
	if err != nil {
		return err
	}
	body, err := indexText(page.Lang, page.Body)
	if err != nil {
		return err
	}
	table := ftsTable(page.Lang)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO `+table+`(rowid, title, body) VALUES(?,?,?)`,
		id, title, body,
	); err != nil {
		return fmt.Errorf("searchindex: insert fts: %w", err)
	}
	return nil
}

// deleteFTS relies on contentless_delete, so it no longer has to replay the
// exact indexed tokens to retract a row.
func deleteFTS(ctx context.Context, tx *sql.Tx, id int64, lang string) error {
	table := ftsTable(lang)
	if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE rowid = ?`, id); err != nil {
		return fmt.Errorf("searchindex: delete fts: %w", err)
	}
	return nil
}

func ftsTable(lang string) string {
	if lang == LangChinese {
		return "pages_fts_zh"
	}
	return "pages_fts_en"
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
