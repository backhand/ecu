package store

import (
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Session is the stored representation of a sessions row.
type Session struct {
	ID             string
	Account        string
	Status         string // provisioning | ready | error | terminated
	ToolEndpoint   string // URL the proxy forwards to (never exposed to clients)
	Persistent     bool
	Width          int
	Height         int
	CreatedAt      time.Time
	LastActivityAt time.Time
	// TunnelToken is the opaque secret an agent presents at /agent/connect to
	// bind its reverse tunnel to this session (C3). Empty for dev-mode sessions
	// (which reach the tool server directly) and for sessions created before
	// the tunnel_token migration. Never exposed to clients except via the
	// dev-only ECU_DEV_EXPOSE_TUNNEL_TOKEN seam.
	TunnelToken string

	// InstanceID is the provider instance id backing this session, set once the
	// control plane provisions an instance (C4). Empty for dev-mode sessions
	// (no real instance) and for sessions created before the instance_id
	// migration. DELETE uses it to destroy the instance via the Provider.
	InstanceID string
}

// CreateSession inserts a new session row. created_at and last_activity_at are
// set to now (UTC) and stored as RFC3339Nano. The caller supplies the id
// (typically from NewSessionID), account, status, tool endpoint, persistence
// flag, and dimensions.
func (s *Store) CreateSession(sess *Session) error {
	now := time.Now().UTC()
	sess.CreatedAt = now
	sess.LastActivityAt = now
	const q = `
INSERT INTO sessions
    (id, account, status, tool_endpoint, persistent, width, height, created_at, last_activity_at, tunnel_token, instance_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);`
	_, err := s.db.Exec(q,
		sess.ID, sess.Account, sess.Status, sess.ToolEndpoint, boolToInt(sess.Persistent),
		sess.Width, sess.Height,
		now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), sess.TunnelToken, sess.InstanceID,
	)
	if err != nil {
		return fmt.Errorf("store: creating session: %w", err)
	}
	return nil
}

// GetSession loads a session by id. found is false when no such row exists;
// that is not an error (found=false, err=nil), matching the convention used by
// LookupKey.
func (s *Store) GetSession(id string) (sess *Session, found bool, err error) {
	const q = `
SELECT id, account, status, tool_endpoint, persistent, width, height, created_at, last_activity_at, tunnel_token, instance_id
FROM sessions WHERE id = ?;`
	var (
		out                      Session
		persistentInt            int
		createdAtStr, lastActStr string
	)
	row := s.db.QueryRow(q, id)
	switch err := row.Scan(
		&out.ID, &out.Account, &out.Status, &out.ToolEndpoint, &persistentInt,
		&out.Width, &out.Height, &createdAtStr, &lastActStr, &out.TunnelToken, &out.InstanceID,
	); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("store: getting session: %w", err)
	}
	out.Persistent = persistentInt != 0
	if out.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAtStr); err != nil {
		return nil, false, fmt.Errorf("store: parsing created_at: %w", err)
	}
	if out.LastActivityAt, err = time.Parse(time.RFC3339Nano, lastActStr); err != nil {
		return nil, false, fmt.Errorf("store: parsing last_activity_at: %w", err)
	}
	return &out, true, nil
}

// UpdateSessionStatus sets the status of a session. It is a no-op (no error) if
// the id does not exist.
func (s *Store) UpdateSessionStatus(id, status string) error {
	if _, err := s.db.Exec(`UPDATE sessions SET status = ? WHERE id = ?;`, status, id); err != nil {
		return fmt.Errorf("store: updating session status: %w", err)
	}
	return nil
}

// UpdateSessionInstanceID records the provider instance id backing a session
// (set by the C4 provisioning flow once the instance is created). It is a
// no-op (no error) if the id does not exist.
func (s *Store) UpdateSessionInstanceID(id, instanceID string) error {
	if _, err := s.db.Exec(`UPDATE sessions SET instance_id = ? WHERE id = ?;`, instanceID, id); err != nil {
		return fmt.Errorf("store: updating session instance id: %w", err)
	}
	return nil
}

// TouchSession sets last_activity_at to now (UTC). Called on every proxied tool
// call so the idle reaper (C5) can later measure inactivity. No-op for unknown
// ids.
func (s *Store) TouchSession(id string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.db.Exec(`UPDATE sessions SET last_activity_at = ? WHERE id = ?;`, now, id); err != nil {
		return fmt.Errorf("store: touching session: %w", err)
	}
	return nil
}

// SessionByTunnelToken resolves a presented tunnel token to its session. It is
// the authentication primitive behind /agent/connect.
//
// Security properties:
//   - An empty presented token NEVER authenticates (rejected before any query),
//     so the tunnel_token=” default on dev-mode / pre-migration rows can never
//     be matched by an empty or absent Authorization header.
//   - The final equality decision uses crypto/subtle.ConstantTimeCompare on the
//     stored vs presented bytes, even though the lookup is by token, as
//     defense-in-depth against timing oracles. A length/!=1 result yields
//     found=false.
//
// found is false (with err=nil) when no row matches; that is not an error,
// matching the LookupKey convention.
func (s *Store) SessionByTunnelToken(token string) (sess *Session, found bool, err error) {
	if token == "" {
		return nil, false, nil
	}
	const q = `
SELECT id, account, status, tool_endpoint, persistent, width, height, created_at, last_activity_at, tunnel_token, instance_id
FROM sessions WHERE tunnel_token = ? LIMIT 1;`
	var (
		out                      Session
		persistentInt            int
		createdAtStr, lastActStr string
	)
	row := s.db.QueryRow(q, token)
	switch err := row.Scan(
		&out.ID, &out.Account, &out.Status, &out.ToolEndpoint, &persistentInt,
		&out.Width, &out.Height, &createdAtStr, &lastActStr, &out.TunnelToken, &out.InstanceID,
	); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("store: getting session by tunnel token: %w", err)
	}
	// Defense-in-depth constant-time compare; also guards the (impossible here)
	// case where the stored token is empty.
	if out.TunnelToken == "" ||
		subtle.ConstantTimeCompare([]byte(out.TunnelToken), []byte(token)) != 1 {
		return nil, false, nil
	}
	out.Persistent = persistentInt != 0
	if out.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAtStr); err != nil {
		return nil, false, fmt.Errorf("store: parsing created_at: %w", err)
	}
	if out.LastActivityAt, err = time.Parse(time.RFC3339Nano, lastActStr); err != nil {
		return nil, false, fmt.Errorf("store: parsing last_activity_at: %w", err)
	}
	return &out, true, nil
}

// boolToInt maps a Go bool to the 0/1 INTEGER convention used in the schema.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
