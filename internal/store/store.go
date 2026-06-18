// Package store is the control plane's embedded persistence layer. It wraps an
// SQLite database accessed through the pure-Go driver modernc.org/sqlite (no
// cgo), which keeps the control plane a single statically-linkable binary that
// cross-compiles cleanly.
//
// The store owns two tables — api_keys and sessions — created idempotently on
// Open, plus typed CRUD for each. All timestamps are stored as RFC3339Nano
// strings for a single consistent representation across the schema. Identifiers
// and tokens are generated with crypto/rand.
package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	// Blank import registers the pure-Go "sqlite" driver with database/sql.
	_ "modernc.org/sqlite"
)

// Store wraps the SQLite *sql.DB and exposes typed operations. It is safe for
// concurrent use: *sql.DB manages its own connection pool, and the schema sets
// a busy timeout plus WAL mode so concurrent readers/writers don't trip over
// each other.
type Store struct {
	db *sql.DB
}

// schema creates both tables idempotently. Columns documented inline; statuses
// and capability flags follow the API contract. INTEGER 0/1 is used for
// booleans (SQLite has no native bool).
const schema = `
CREATE TABLE IF NOT EXISTS api_keys (
    key                TEXT PRIMARY KEY,
    account            TEXT NOT NULL,
    status             TEXT NOT NULL,          -- 'active' | 'disabled'
    persistent_allowed INTEGER NOT NULL,        -- 0/1 capability (used in C8)
    created_at         TEXT NOT NULL            -- RFC3339Nano
);

CREATE TABLE IF NOT EXISTS sessions (
    id               TEXT PRIMARY KEY,          -- 's_' + crypto/rand hex
    account          TEXT NOT NULL,
    status           TEXT NOT NULL,             -- 'provisioning' | 'ready' | 'error' | 'terminated'
    tool_endpoint    TEXT NOT NULL,             -- URL the proxy forwards to (may be empty until ready)
    persistent       INTEGER NOT NULL,           -- 0/1
    width            INTEGER NOT NULL,
    height           INTEGER NOT NULL,
    created_at       TEXT NOT NULL,             -- RFC3339Nano
    last_activity_at TEXT NOT NULL              -- RFC3339Nano
);
`

// Open opens (creating if necessary) the SQLite database at dbPath, ensures the
// parent directory exists, applies the schema, and returns a ready Store. The
// DSN enables a 5s busy timeout and WAL journaling so the single-file database
// tolerates concurrent access from the HTTP handlers.
func Open(dbPath string) (*Store, error) {
	if dbPath == "" {
		return nil, fmt.Errorf("store: empty database path")
	}
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		return nil, fmt.Errorf("store: resolving db path %q: %w", dbPath, err)
	}
	if dir := filepath.Dir(abs); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("store: creating db dir %s: %w", dir, err)
		}
	}

	// file: DSN with pragmas understood by modernc.org/sqlite.
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", abs)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: opening sqlite %s: %w", abs, err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: pinging sqlite %s: %w", abs, err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: applying schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the underlying database.
func (s *Store) Close() error {
	return s.db.Close()
}

// NewSessionID returns a session identifier in the canonical "s_" + 32 hex
// characters (16 random bytes) form, sourced from crypto/rand.
func NewSessionID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("store: generating session id: %w", err)
	}
	return "s_" + hex.EncodeToString(b), nil
}
