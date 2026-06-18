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
    last_activity_at TEXT NOT NULL,             -- RFC3339Nano
    tunnel_token     TEXT NOT NULL DEFAULT ''   -- opaque per-session token an agent presents at /agent/connect (C3)
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
	// Idempotent migration for the C3 tunnel_token column. CREATE TABLE IF NOT
	// EXISTS never alters an existing table, so a sessions table created by an
	// earlier build lacks tunnel_token. This is pre-release, so rather than a
	// full migration framework we detect the missing column via PRAGMA
	// table_info and ADD it once. The ALTER is a no-op skip when the column is
	// already present (fresh DBs from the schema above).
	if err := ensureSessionTunnelToken(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// ensureSessionTunnelToken adds the sessions.tunnel_token column if a pre-C3
// database is missing it. It is safe to run on every Open: it queries
// PRAGMA table_info(sessions) and only issues the ALTER when the column is
// absent. modernc.org/sqlite supports both PRAGMA table_info and ALTER TABLE
// ADD COLUMN.
func ensureSessionTunnelToken(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(sessions);`)
	if err != nil {
		return fmt.Errorf("store: inspecting sessions columns: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		// PRAGMA table_info columns: cid, name, type, notnull, dflt_value, pk.
		var (
			cid         int
			name, ctype string
			notnull, pk int
			dfltValue   sql.NullString
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk); err != nil {
			return fmt.Errorf("store: scanning sessions column info: %w", err)
		}
		if name == "tunnel_token" {
			return rows.Err() // already present, nothing to migrate
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: iterating sessions columns: %w", err)
	}
	if _, err := db.Exec(`ALTER TABLE sessions ADD COLUMN tunnel_token TEXT NOT NULL DEFAULT '';`); err != nil {
		return fmt.Errorf("store: adding tunnel_token column: %w", err)
	}
	return nil
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

// NewTunnelToken returns an opaque per-session tunnel token: "t_" + 64 hex
// characters (32 random bytes from crypto/rand). The agent presents this at
// /agent/connect to bind its outbound tunnel to a specific session; it is
// compared with crypto/subtle.ConstantTimeCompare on the server side. The wider
// 32-byte body (vs the 16-byte session id) reflects its role as an
// authentication secret rather than a mere identifier.
func NewTunnelToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("store: generating tunnel token: %w", err)
	}
	return "t_" + hex.EncodeToString(b), nil
}
