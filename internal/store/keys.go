package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// APIKey is the stored representation of an API key row.
type APIKey struct {
	Key               string
	Account           string
	Status            string // "active" | "disabled"
	PersistentAllowed bool
	CreatedAt         time.Time
}

// KeyLookup is the minimal result of validating a key for the auth middleware.
type KeyLookup struct {
	Account string
	Active  bool
	Found   bool
}

// SeedBootstrapKey inserts the bootstrap admin key from ECU_API_KEY as an
// active key for account "admin" with the persistent capability, if and only if
// no row with that key already exists.
//
// It uses INSERT ... ON CONFLICT(key) DO NOTHING on purpose: if an operator has
// already created the key and later disabled it, re-running the control plane
// must NOT silently re-activate it. Seeding is therefore strictly
// create-if-absent and is safe to call on every startup (idempotent).
func (s *Store) SeedBootstrapKey(key string) error {
	if key == "" {
		return fmt.Errorf("store: cannot seed empty bootstrap key")
	}
	const q = `
INSERT INTO api_keys (key, account, status, persistent_allowed, created_at)
VALUES (?, 'admin', 'active', 1, ?)
ON CONFLICT(key) DO NOTHING;`
	if _, err := s.db.Exec(q, key, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("store: seeding bootstrap key: %w", err)
	}
	return nil
}

// LookupKey resolves a presented API key to its account and active state.
// found is false when the key does not exist; active reflects status=='active'.
// A non-existent key is not an error (found=false, err=nil).
func (s *Store) LookupKey(key string) (account string, active bool, found bool, err error) {
	const q = `SELECT account, status FROM api_keys WHERE key = ?;`
	var status string
	row := s.db.QueryRow(q, key)
	switch err := row.Scan(&account, &status); {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, false, nil
	case err != nil:
		return "", false, false, fmt.Errorf("store: looking up key: %w", err)
	}
	return account, status == "active", true, nil
}

// CountKeys returns the total number of api_keys rows. Primarily for tests and
// diagnostics (e.g. verifying seed idempotency).
func (s *Store) CountKeys() (int, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM api_keys;`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: counting keys: %w", err)
	}
	return n, nil
}

// InsertKey inserts an arbitrary api_keys row. Component 2 has no operator-key
// management API (that arrives later); this is the building block used to seed
// keys with a specific status — notably disabled keys — from other packages'
// tests and from any future admin path. It errors on a duplicate key.
func (s *Store) InsertKey(k APIKey) error {
	const q = `
INSERT INTO api_keys (key, account, status, persistent_allowed, created_at)
VALUES (?, ?, ?, ?, ?);`
	if _, err := s.db.Exec(q, k.Key, k.Account, k.Status, boolToInt(k.PersistentAllowed),
		k.CreatedAt.UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("store: inserting key: %w", err)
	}
	return nil
}
