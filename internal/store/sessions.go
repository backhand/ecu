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

	// TunnelLostAt is the instant the session's LIVE reverse tunnel was last
	// lost (the broker stamps it when an established tunnel closes; it is
	// cleared when an agent (re)connects). The zero value means "never lost /
	// currently connected" and is stored as the empty string. The C5 reaper
	// uses max(CreatedAt, TunnelLostAt) as the start of the orphan/reconnect
	// window so a session that briefly loses its tunnel gets a fresh grace
	// period rather than being measured from its original creation.
	TunnelLostAt time.Time

	// SnapshotImage is the provider snapshot ref (image id) holding a persistent
	// session's saved desktop state while it is 'stopped' (C8). It is set when a
	// persistent session is snapshotted+stopped (by DELETE or the reaper) and is
	// the boot image a restore of this session uses. Empty means none (an active
	// or ephemeral session, or a stopped one whose snapshot was culled). Each
	// persistent session keeps at most one snapshot: taking a new one deletes the
	// prior ref.
	SnapshotImage string

	// StoppedAt is the instant a persistent session was snapshotted+stopped
	// (C8). The zero value (stored as the empty string) means "not stopped". The
	// reaper measures the cull age (ECU_PERSISTENT_MAX_AGE) from THIS, not from
	// CreatedAt: a long-lived persistent session that was only just stopped must
	// not be culled as though it were old.
	StoppedAt time.Time
}

// CreateSession inserts a new session row. created_at and last_activity_at are
// set to now (UTC) and stored as RFC3339Nano. The caller supplies the id
// (typically from NewSessionID), account, status, tool endpoint, persistence
// flag, and dimensions.
func (s *Store) CreateSession(sess *Session) error {
	now := time.Now().UTC()
	sess.CreatedAt = now
	sess.LastActivityAt = now
	// A freshly-created session has a live (or imminent) tunnel, so tunnel_lost_at
	// starts empty (never lost). It is stamped only when an established tunnel
	// later closes.
	// snapshot_image / stopped_at start empty: a freshly-created session is never
	// 'stopped' (those are written only when a persistent session is later
	// snapshotted+stopped).
	const q = `
INSERT INTO sessions
    (id, account, status, tool_endpoint, persistent, width, height, created_at, last_activity_at, tunnel_token, instance_id, tunnel_lost_at, snapshot_image, stopped_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);`
	_, err := s.db.Exec(q,
		sess.ID, sess.Account, sess.Status, sess.ToolEndpoint, boolToInt(sess.Persistent),
		sess.Width, sess.Height,
		now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), sess.TunnelToken, sess.InstanceID, "", "", "",
	)
	if err != nil {
		return fmt.Errorf("store: creating session: %w", err)
	}
	return nil
}

// sessionColumns is the canonical column list (and order) shared by every
// session SELECT so the scan targets in scanSession stay in lockstep with the
// queries. Keep this in sync with the CreateSession INSERT column list.
const sessionColumns = `id, account, status, tool_endpoint, persistent, width, height, created_at, last_activity_at, tunnel_token, instance_id, tunnel_lost_at, snapshot_image, stopped_at`

// scanSession scans one sessions row (selected via sessionColumns, in that
// order) into a Session, decoding the 0/1 persistent flag and parsing the
// timestamp columns. created_at/last_activity_at are always present
// (NOT NULL, set on insert); tunnel_lost_at is optional and decodes the empty
// string to the zero time. The src is a *sql.Row or *sql.Rows (anything with
// Scan); callers handle ErrNoRows / iteration themselves.
func scanSession(src interface{ Scan(...any) error }) (Session, error) {
	var (
		out                                             Session
		persistentInt                                   int
		createdAtStr, lastActStr, lostAtStr, stoppedStr string
	)
	if err := src.Scan(
		&out.ID, &out.Account, &out.Status, &out.ToolEndpoint, &persistentInt,
		&out.Width, &out.Height, &createdAtStr, &lastActStr, &out.TunnelToken, &out.InstanceID, &lostAtStr,
		&out.SnapshotImage, &stoppedStr,
	); err != nil {
		return Session{}, err
	}
	out.Persistent = persistentInt != 0
	var err error
	if out.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAtStr); err != nil {
		return Session{}, fmt.Errorf("store: parsing created_at: %w", err)
	}
	if out.LastActivityAt, err = time.Parse(time.RFC3339Nano, lastActStr); err != nil {
		return Session{}, fmt.Errorf("store: parsing last_activity_at: %w", err)
	}
	if out.TunnelLostAt, err = parseTimestamp(lostAtStr); err != nil {
		return Session{}, fmt.Errorf("store: parsing tunnel_lost_at: %w", err)
	}
	if out.StoppedAt, err = parseTimestamp(stoppedStr); err != nil {
		return Session{}, fmt.Errorf("store: parsing stopped_at: %w", err)
	}
	return out, nil
}

// parseTimestamp parses an RFC3339Nano timestamp column, mapping the empty
// string to the zero time (no error). It is used for OPTIONAL timestamp columns
// like tunnel_lost_at whose empty default means "unset", as distinct from the
// always-present created_at/last_activity_at columns.
func parseTimestamp(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, s)
}

// GetSession loads a session by id. found is false when no such row exists;
// that is not an error (found=false, err=nil), matching the convention used by
// LookupKey.
func (s *Store) GetSession(id string) (sess *Session, found bool, err error) {
	q := `SELECT ` + sessionColumns + ` FROM sessions WHERE id = ?;`
	out, err := scanSession(s.db.QueryRow(q, id))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("store: getting session: %w", err)
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

// UpdateSessionToolEndpoint sets a session's tool_endpoint (the URL the proxy
// forwards to). It is used by the C8 dev-mode restore path to re-point a
// reactivated session back at the dev tool server. No-op for unknown ids.
func (s *Store) UpdateSessionToolEndpoint(id, endpoint string) error {
	if _, err := s.db.Exec(`UPDATE sessions SET tool_endpoint = ? WHERE id = ?;`, endpoint, id); err != nil {
		return fmt.Errorf("store: updating session tool endpoint: %w", err)
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
	q := `SELECT ` + sessionColumns + ` FROM sessions WHERE tunnel_token = ? LIMIT 1;`
	out, err := scanSession(s.db.QueryRow(q, token))
	switch {
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
	return &out, true, nil
}

// SetSessionTunnelLost records (or clears) the instant a session's live tunnel
// was lost. A non-zero lostAt is stored as RFC3339Nano; a ZERO lostAt
// (lostAt.IsZero()) clears the column back to the empty string, which the
// reaper reads as "currently connected / never lost". It is a no-op (no error)
// for unknown ids — the UPDATE simply matches no row. The broker stamps loss on
// tunnel close and clears it on (re)connect; the reaper's boot reconcile stamps
// it for sessions that have no tunnel at startup.
func (s *Store) SetSessionTunnelLost(id string, lostAt time.Time) error {
	var val string
	if !lostAt.IsZero() {
		val = lostAt.UTC().Format(time.RFC3339Nano)
	}
	if _, err := s.db.Exec(`UPDATE sessions SET tunnel_lost_at = ? WHERE id = ?;`, val, id); err != nil {
		return fmt.Errorf("store: setting session tunnel_lost_at: %w", err)
	}
	return nil
}

// ListNonTerminalSessions returns every session in a non-terminal status
// (provisioning or ready), fully parsed and ordered by creation time. The
// terminal statuses (error, terminated) are excluded: they back nothing that
// could leak (provisioning tore down failed instances; terminated already
// destroyed theirs), so the reaper need never look at them. This is the
// reaper's per-sweep input and the basis for the active-session cap.
func (s *Store) ListNonTerminalSessions() ([]Session, error) {
	q := `SELECT ` + sessionColumns + `
FROM sessions WHERE status IN ('provisioning', 'ready') ORDER BY created_at;`
	rows, err := s.db.Query(q)
	if err != nil {
		return nil, fmt.Errorf("store: listing non-terminal sessions: %w", err)
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scanning non-terminal session: %w", err)
		}
		out = append(out, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterating non-terminal sessions: %w", err)
	}
	return out, nil
}

// CountActiveSessions returns the number of sessions in an ACTIVE
// (provisioning or ready) status. This is the quantity the global session cap
// (ECU_MAX_SESSIONS) is enforced against: an active session may be holding —
// or about to hold — a paid instance, whereas error/terminated sessions hold
// nothing.
func (s *Store) CountActiveSessions() (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM sessions WHERE status IN ('provisioning', 'ready');`,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: counting active sessions: %w", err)
	}
	return n, nil
}

// CountActiveSessionsForAccount returns the number of ACTIVE (provisioning or
// ready) sessions owned by account. It backs a per-account cap; the global cap
// uses CountActiveSessions.
func (s *Store) CountActiveSessionsForAccount(account string) (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM sessions WHERE status IN ('provisioning', 'ready') AND account = ?;`,
		account,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: counting active sessions for account: %w", err)
	}
	return n, nil
}

// MarkSessionStopped transitions a persistent session to the restorable
// 'stopped' state in a single UPDATE: it sets status='stopped', records the
// snapshot ref holding its saved state, and stamps stopped_at=now (UTC,
// RFC3339Nano) as the basis for the reaper's cull age. It is a no-op (no error)
// for unknown ids. The status literal is duplicated here (the store is
// otherwise status-agnostic) to keep this an atomic write of the three coupled
// fields; the canonical constant lives in controlplane.
func (s *Store) MarkSessionStopped(id, snapshotRef string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.db.Exec(
		`UPDATE sessions SET status = 'stopped', snapshot_image = ?, stopped_at = ? WHERE id = ?;`,
		snapshotRef, now, id,
	); err != nil {
		return fmt.Errorf("store: marking session stopped: %w", err)
	}
	return nil
}

// UpdateSessionSnapshotImage sets a session's snapshot_image ref. It is used
// when the snapshot ref must be written independently of the stop transition
// (MarkSessionStopped already writes it in the common case). No-op for unknown
// ids.
func (s *Store) UpdateSessionSnapshotImage(id, ref string) error {
	if _, err := s.db.Exec(`UPDATE sessions SET snapshot_image = ? WHERE id = ?;`, ref, id); err != nil {
		return fmt.Errorf("store: updating session snapshot image: %w", err)
	}
	return nil
}

// ReactivateSessionForRestore re-activates a previously 'stopped' persistent
// session for a restore (C8): it sets status='provisioning', installs a fresh
// tunnel token, and clears the instance_id / tunnel_lost_at / stopped_at fields
// so the row looks like a brand-new provisioning session bound to the same id
// and account. snapshot_image is INTENTIONALLY preserved: it is the saved state
// the restore boots from, and is only replaced when the session is next ended.
// No-op for unknown ids. The status literal is duplicated here for an atomic
// reset; the canonical constant lives in controlplane.
func (s *Store) ReactivateSessionForRestore(id, newTunnelToken string) error {
	if _, err := s.db.Exec(
		`UPDATE sessions SET status = 'provisioning', tunnel_token = ?, instance_id = '', tunnel_lost_at = '', stopped_at = '' WHERE id = ?;`,
		newTunnelToken, id,
	); err != nil {
		return fmt.Errorf("store: reactivating session for restore: %w", err)
	}
	return nil
}

// CountNonTerminatedPersistentSessions returns the number of persistent
// sessions NOT in the terminal 'terminated' status. This is the basis for the
// ECU_MAX_PERSISTENT_SESSIONS cap: a persistent session occupies a slot while
// provisioning, ready, OR stopped (a stopped session still holds saved state
// and a snapshot, so it counts until it is culled to 'terminated'). Only
// 'terminated' frees a slot.
func (s *Store) CountNonTerminatedPersistentSessions() (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM sessions WHERE persistent = 1 AND status != 'terminated';`,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: counting non-terminated persistent sessions: %w", err)
	}
	return n, nil
}

// ListStoppedPersistentSessions returns every persistent session in the
// 'stopped' state, fully parsed and ordered by when it was stopped. This is the
// reaper's cull-candidate input: a stopped session holds only a stored snapshot
// (no instance), and is culled (snapshot deleted, marked terminated) once its
// StoppedAt is older than ECU_PERSISTENT_MAX_AGE.
func (s *Store) ListStoppedPersistentSessions() ([]Session, error) {
	q := `SELECT ` + sessionColumns + `
FROM sessions WHERE persistent = 1 AND status = 'stopped' ORDER BY stopped_at;`
	rows, err := s.db.Query(q)
	if err != nil {
		return nil, fmt.Errorf("store: listing stopped persistent sessions: %w", err)
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scanning stopped persistent session: %w", err)
		}
		out = append(out, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterating stopped persistent sessions: %w", err)
	}
	return out, nil
}

// boolToInt maps a Go bool to the 0/1 INTEGER convention used in the schema.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
