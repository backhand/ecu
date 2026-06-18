package controlplane

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/backhand/ecu/internal/agent"
	"github.com/backhand/ecu/internal/provider"
	"github.com/backhand/ecu/internal/provider/fake"
	"github.com/backhand/ecu/internal/store"
)

// --- shared C8 test helpers --------------------------------------------------

// postSessions issues POST /sessions through the in-process Handler with the
// given JSON body and auth header, returning the recorder. (doRequest sends no
// body; the persistence paths need {persistent:true}/{restore:...} bodies.)
func postSessions(t *testing.T, srv *Server, body, auth string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// decodeCreate decodes a createSessionResponse from a recorder body.
func decodeCreate(t *testing.T, rec *httptest.ResponseRecorder) createSessionResponse {
	t.Helper()
	var cr createSessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &cr); err != nil {
		t.Fatalf("decode create response: %v (%s)", err, rec.Body.String())
	}
	return cr
}

// seedOpsKey inserts an active key for account "ops" with the given persistent
// capability, used to test authorization (the harness's k_active is admin +
// persistent-allowed).
func seedOpsKey(t *testing.T, st *store.Store, key string, persistentAllowed bool) {
	t.Helper()
	if err := st.InsertKey(store.APIKey{
		Key: key, Account: "ops", Status: "active",
		PersistentAllowed: persistentAllowed, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed ops key %q: %v", key, err)
	}
}

// seedPersistentSession inserts a persistent session row directly with the given
// status, instance id, and snapshot ref (any of which may be empty), so DELETE /
// restore / reaper behavior can be isolated without driving a full provision.
func seedPersistentSession(t *testing.T, st *store.Store, id, account, status, instanceID, snapshot string) {
	t.Helper()
	if err := st.CreateSession(&store.Session{
		ID: id, Account: account, Status: statusProvisioning,
		Persistent: true, Width: defaultWidth, Height: defaultHeight, InstanceID: instanceID,
	}); err != nil {
		t.Fatalf("CreateSession(%s): %v", id, err)
	}
	if snapshot != "" {
		if err := st.UpdateSessionSnapshotImage(id, snapshot); err != nil {
			t.Fatalf("UpdateSessionSnapshotImage(%s): %v", id, snapshot)
		}
	}
	if status == statusStopped {
		// Use MarkSessionStopped so stopped_at is stamped like the real path.
		if err := st.MarkSessionStopped(id, snapshot); err != nil {
			t.Fatalf("MarkSessionStopped(%s): %v", id, err)
		}
	} else if status != statusProvisioning {
		if err := st.UpdateSessionStatus(id, status); err != nil {
			t.Fatalf("UpdateSessionStatus(%s,%s): %v", id, status, err)
		}
	}
}

// --- Authorization -----------------------------------------------------------

// TestPersistentUnauthorizedRejected: a persistent request from an active key
// WITHOUT the persistent capability is REJECTED with 403 and the exact detail —
// never silently downgraded to ephemeral.
func TestPersistentUnauthorizedRejected(t *testing.T) {
	st := newTestStore(t)
	seedOpsKey(t, st, "k_ops", false) // active, NOT persistent-allowed
	srv := NewServer(st, "http://127.0.0.1:8000")

	rec := postSessions(t, srv, `{"persistent":true}`, "Bearer k_ops")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for unauthorized persistent request", rec.Code)
	}
	var m map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("body not JSON: %v (%s)", err, rec.Body.String())
	}
	if m["detail"] != "persistence not authorized for this API key" {
		t.Fatalf("detail = %q, want the exact persistence-not-authorized message", m["detail"])
	}
	// And nothing was created (no downgrade to an ephemeral session).
	if n, _ := st.CountActiveSessions(); n != 0 {
		t.Fatalf("a rejected persistent request created %d session(s); must create none", n)
	}
}

// TestPersistentAuthorizedProvisioned: an authorized key gets a persistent
// session, persistent=true is persisted, and it counts toward the persistent cap.
func TestPersistentAuthorizedProvisioned(t *testing.T) {
	st := newTestStore(t)
	srv := NewServer(st, "http://127.0.0.1:8000", WithMaxPersistentSessions(3))

	rec := postSessions(t, srv, `{"persistent":true}`, "Bearer k_active")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	cr := decodeCreate(t, rec)
	if !cr.Persistent {
		t.Fatalf("response persistent = false, want true")
	}
	sess, found, _ := st.GetSession(cr.SessionID)
	if !found || !sess.Persistent {
		t.Fatalf("persisted session persistent flag not set: found=%v sess=%+v", found, sess)
	}
	if n, _ := st.CountNonTerminatedPersistentSessions(); n != 1 {
		t.Fatalf("persistent count = %d, want 1", n)
	}
}

// --- Cap ----------------------------------------------------------------------

// TestPersistentCapEnforced exercises the ECU_MAX_PERSISTENT_SESSIONS cap at
// exactly 3, that a STOPPED session STILL counts (so the 4th is rejected even
// after one is ended), and that culling a stopped one frees a slot. Dev mode so
// sessions go ready immediately and no provider is needed; we drive the stop +
// cull via the store directly (DELETE in dev has no instance to snapshot).
func TestPersistentCapEnforced(t *testing.T) {
	st := newTestStore(t)
	srv := NewServer(st, "http://127.0.0.1:8000", WithMaxPersistentSessions(3))

	// Create 3 persistent sessions (cap = 3).
	var ids []string
	for i := 0; i < 3; i++ {
		rec := postSessions(t, srv, `{"persistent":true}`, "Bearer k_active")
		if rec.Code != http.StatusOK {
			t.Fatalf("persistent create %d status = %d, want 200; body=%s", i, rec.Code, rec.Body.String())
		}
		ids = append(ids, decodeCreate(t, rec).SessionID)
	}

	// 4th persistent create: over the cap -> 429.
	rec := postSessions(t, srv, `{"persistent":true}`, "Bearer k_active")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("4th persistent create status = %d, want 429; body=%s", rec.Code, rec.Body.String())
	}
	assertJSONDetail(t, rec.Body.Bytes())

	// End one (mark it stopped directly — in dev there is no instance). A STOPPED
	// session STILL counts toward the persistent cap.
	if err := st.MarkSessionStopped(ids[0], "fake-image-ecu-persist-"+ids[0]); err != nil {
		t.Fatalf("MarkSessionStopped: %v", err)
	}
	if n, _ := st.CountNonTerminatedPersistentSessions(); n != 3 {
		t.Fatalf("after stopping one, persistent count = %d, want 3 (stopped still counts)", n)
	}
	rec = postSessions(t, srv, `{"persistent":true}`, "Bearer k_active")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("persistent create after a STOP status = %d, want 429 (stopped still counts toward cap)", rec.Code)
	}

	// Cull the stopped one (terminate it): now a slot frees up.
	if err := st.UpdateSessionStatus(ids[0], statusTerminated); err != nil {
		t.Fatalf("terminate stopped session: %v", err)
	}
	if n, _ := st.CountNonTerminatedPersistentSessions(); n != 2 {
		t.Fatalf("after culling one, persistent count = %d, want 2", n)
	}
	rec = postSessions(t, srv, `{"persistent":true}`, "Bearer k_active")
	if rec.Code != http.StatusOK {
		t.Fatalf("persistent create after a CULL status = %d, want 200 (slot freed); body=%s", rec.Code, rec.Body.String())
	}
}

// --- Persistent DELETE (snapshot-and-stop) -----------------------------------

// TestPersistentDeleteSnapshotsAndStops: DELETE of a persistent session with a
// live instance snapshots it (named ecu-persist-<id>) THEN destroys the instance
// and marks the session 'stopped' carrying the snapshot ref. The response is
// {"status":"stopped"}, NOT terminated.
func TestPersistentDeleteSnapshotsAndStops(t *testing.T) {
	st := newTestStore(t)
	prov := fake.New()

	// Seed a ready persistent session with a live instance.
	inst, _ := prov.CreateInstance(context.Background(), provider.InstanceSpec{})
	id, _ := store.NewSessionID()
	seedPersistentSession(t, st, id, "admin", statusReady, inst.ID, "")

	ts, _ := startCPWithProvider(t, st, prov, ProvisionConfig{ProvisionTimeout: time.Second})

	body, code := deleteSessionBody(t, ts, id)
	if code != http.StatusOK {
		t.Fatalf("persistent DELETE status = %d, want 200; body=%s", code, body)
	}
	if !strings.Contains(body, statusStopped) || strings.Contains(body, statusTerminated) {
		t.Fatalf("persistent DELETE body = %s, want status stopped (not terminated)", body)
	}

	// Snapshot was created with the per-session name, then the instance destroyed.
	imgs := prov.Images()
	wantName := "ecu-persist-" + sanitizeName(id)
	if len(imgs) != 1 || imgs[0].FromInstance != inst.ID || imgs[0].Name != wantName {
		t.Fatalf("CreateImage calls = %+v, want one {%s, %s}", imgs, inst.ID, wantName)
	}
	if !prov.Deleted(inst.ID) {
		t.Fatalf("instance %s was NOT destroyed after snapshot", inst.ID)
	}

	// Session is stopped, carrying the new snapshot ref + a stopped_at stamp.
	sess, _, _ := st.GetSession(id)
	if sess.Status != statusStopped {
		t.Fatalf("status = %q, want stopped", sess.Status)
	}
	wantRef := "fake-image-" + wantName
	if sess.SnapshotImage != wantRef {
		t.Fatalf("snapshot ref = %q, want %q", sess.SnapshotImage, wantRef)
	}
	if sess.StoppedAt.IsZero() {
		t.Fatalf("stopped_at not stamped on stop")
	}

	// A SECOND DELETE on the now-stopped session must NOT re-snapshot; it just
	// reports stopped again (idempotent).
	body2, code2 := deleteSessionBody(t, ts, id)
	if code2 != http.StatusOK || !strings.Contains(body2, statusStopped) {
		t.Fatalf("second persistent DELETE = (%d, %s), want 200 stopped", code2, body2)
	}
	if prov.ImageCount() != 1 {
		t.Fatalf("second DELETE re-snapshotted: ImageCount = %d, want 1", prov.ImageCount())
	}
}

// TestPersistentDeleteReplacesPriorSnapshot: ending a persistent session that
// already has a PRIOR snapshot deletes the prior one after the new snapshot is
// taken (each session keeps at most one).
func TestPersistentDeleteReplacesPriorSnapshot(t *testing.T) {
	st := newTestStore(t)
	prov := fake.New()

	inst, _ := prov.CreateInstance(context.Background(), provider.InstanceSpec{})
	id, _ := store.NewSessionID()
	const priorSnapshot = "fake-image-prior-snapshot"
	seedPersistentSession(t, st, id, "admin", statusReady, inst.ID, priorSnapshot)

	ts, _ := startCPWithProvider(t, st, prov, ProvisionConfig{ProvisionTimeout: time.Second})

	body, code := deleteSessionBody(t, ts, id)
	if code != http.StatusOK || !strings.Contains(body, statusStopped) {
		t.Fatalf("persistent DELETE = (%d, %s), want 200 stopped", code, body)
	}

	// The PRIOR snapshot must have been deleted; the NEW one must NOT (it is the
	// saved state).
	if !prov.DeletedImage(priorSnapshot) {
		t.Fatalf("prior snapshot %q was NOT deleted on re-snapshot", priorSnapshot)
	}
	sess, _, _ := st.GetSession(id)
	if prov.DeletedImage(sess.SnapshotImage) {
		t.Fatalf("the NEW snapshot %q was deleted; it must be kept", sess.SnapshotImage)
	}
}

// TestPersistentDeleteSnapshotFailurePreservesState: if CreateImage fails during
// a persistent DELETE, the response is 500 and the instance is NOT destroyed and
// the session is NOT marked stopped — state is preserved.
func TestPersistentDeleteSnapshotFailurePreservesState(t *testing.T) {
	st := newTestStore(t)
	prov := fake.New()
	prov.CreateImageErr = io.ErrUnexpectedEOF // force the snapshot to fail

	inst, _ := prov.CreateInstance(context.Background(), provider.InstanceSpec{})
	id, _ := store.NewSessionID()
	seedPersistentSession(t, st, id, "admin", statusReady, inst.ID, "")

	ts, _ := startCPWithProvider(t, st, prov, ProvisionConfig{ProvisionTimeout: time.Second})

	body, code := deleteSessionBody(t, ts, id)
	if code != http.StatusInternalServerError {
		t.Fatalf("persistent DELETE with failing snapshot status = %d, want 500; body=%s", code, body)
	}
	// State preserved: instance still live, session still ready (NOT stopped).
	if prov.Deleted(inst.ID) {
		t.Fatalf("instance %s was destroyed despite snapshot failure (state lost!)", inst.ID)
	}
	if len(prov.Instances()) != 1 {
		t.Fatalf("live instances = %d, want 1 (instance must survive a snapshot failure)", len(prov.Instances()))
	}
	sess, _, _ := st.GetSession(id)
	if sess.Status != statusReady {
		t.Fatalf("status = %q, want ready (must NOT be stopped when snapshot failed)", sess.Status)
	}
}

// TestEphemeralDeleteStillDestroys: an ephemeral DELETE still terminates the
// session and destroys the instance (no snapshot), unchanged by C8.
func TestEphemeralDeleteStillDestroys(t *testing.T) {
	st := newTestStore(t)
	prov := fake.New()

	inst, _ := prov.CreateInstance(context.Background(), provider.InstanceSpec{})
	id, _ := store.NewSessionID()
	if err := st.CreateSession(&store.Session{
		ID: id, Account: "admin", Status: statusReady,
		Width: defaultWidth, Height: defaultHeight, InstanceID: inst.ID, // Persistent:false
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	ts, _ := startCPWithProvider(t, st, prov, ProvisionConfig{ProvisionTimeout: time.Second})

	body, code := deleteSessionBody(t, ts, id)
	if code != http.StatusOK || !strings.Contains(body, statusTerminated) {
		t.Fatalf("ephemeral DELETE = (%d, %s), want 200 terminated", code, body)
	}
	if prov.ImageCount() != 0 {
		t.Fatalf("ephemeral DELETE took a snapshot: ImageCount = %d, want 0", prov.ImageCount())
	}
	if !prov.Deleted(inst.ID) {
		t.Fatalf("ephemeral DELETE did not destroy instance %s", inst.ID)
	}
	sess, _, _ := st.GetSession(id)
	if sess.Status != statusTerminated {
		t.Fatalf("status = %q, want terminated", sess.Status)
	}
}

// deleteSessionBody issues DELETE /sessions/{id} and returns the body + status.
func deleteSessionBody(t *testing.T, ts *httptest.Server, id string) (string, int) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/sessions/"+id, nil)
	req.Header.Set("Authorization", "Bearer k_active")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("DELETE /sessions/%s: %v", id, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// --- Restore ------------------------------------------------------------------

// postSessionsTS issues POST /sessions against an httptest.Server with the given
// body + auth, returning the decoded response and status.
func postSessionsTS(t *testing.T, ts *httptest.Server, body, auth string) (createSessionResponse, int) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/sessions", strings.NewReader(body))
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /sessions: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var cr createSessionResponse
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(b, &cr); err != nil {
			t.Fatalf("decode create response: %v (%s)", err, b)
		}
	}
	return cr, resp.StatusCode
}

// TestRestoreBootsFromSnapshot is the headline restore test: a prior STOPPED
// persistent session is restored, booting a NEW instance whose BaseImage is the
// session's saved snapshot ref, reusing the SAME session id, and reaching ready
// via the normal tunnel flow. The snapshot is KEPT (not deleted) on restore.
func TestRestoreBootsFromSnapshot(t *testing.T) {
	st := newTestStore(t)
	prov := fake.New()

	// A prior stopped persistent session owned by admin (k_active) with a saved
	// snapshot ref. No live instance (it was stopped).
	priorID, _ := store.NewSessionID()
	const snapshotRef = "fake-image-ecu-persist-prior"
	seedPersistentSession(t, st, priorID, "admin", statusStopped, "", snapshotRef)

	// A reachable tool server for the restored session's agent.
	toolSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer toolSrv.Close()

	ts, addr := startCPWithProvider(t, st, prov, ProvisionConfig{ProvisionTimeout: 5 * time.Second})
	wsURL := "ws://" + addr + agentConnectPath

	// On create, drive a real agent to ready using the token from the rendered
	// cloud-init (same mechanism as TestProvisionHappyPath).
	agentCtx, cancelAgent := context.WithCancel(context.Background())
	agentDone := make(chan struct{})
	prov.OnCreate = func(spec provider.InstanceSpec) {
		token := tokenFromUserData(t, spec.UserData)
		go func() {
			defer close(agentDone)
			_ = agent.Run(agentCtx, agent.Config{ControlPlaneURL: wsURL, Token: token, ToolServer: toolSrv.URL})
		}()
	}
	t.Cleanup(func() { cancelAgent(); <-agentDone })

	cr, code := postSessionsTS(t, ts, `{"restore":"`+priorID+`"}`, "Bearer k_active")
	if code != http.StatusOK {
		t.Fatalf("restore status = %d, want 200", code)
	}
	// SAME id reused, persistent:true in the response, provisioning immediately.
	if cr.SessionID != priorID {
		t.Fatalf("restore reused id %q, want the SAME prior id %q", cr.SessionID, priorID)
	}
	if !cr.Persistent {
		t.Fatalf("restore response persistent = false, want true")
	}
	if cr.Status != statusProvisioning {
		t.Fatalf("restore status = %q, want provisioning (immediate)", cr.Status)
	}

	// Reaches ready via the tunnel.
	waitForStatus(t, st, priorID, statusReady, 5*time.Second)

	// The NEW instance booted from the snapshot ref (NOT ActiveBootImage).
	creates := prov.Creates()
	if len(creates) != 1 {
		t.Fatalf("CreateInstance calls = %d, want 1", len(creates))
	}
	if creates[0].BaseImage != snapshotRef {
		t.Fatalf("restored instance BaseImage = %q, want the saved snapshot ref %q", creates[0].BaseImage, snapshotRef)
	}

	// The snapshot is KEPT on restore (it's the saved state; the next end replaces
	// it): it must NOT have been deleted.
	if prov.DeletedImage(snapshotRef) {
		t.Fatalf("snapshot %q was deleted on restore; it must be kept", snapshotRef)
	}
	// And the row still carries the snapshot + is persistent + has a fresh instance.
	sess, _, _ := st.GetSession(priorID)
	if sess.SnapshotImage != snapshotRef || !sess.Persistent {
		t.Fatalf("restored session lost snapshot/persistent: %+v", sess)
	}
	if sess.InstanceID == "" {
		t.Fatalf("restored session has no instance id after provisioning")
	}
}

// TestRestoreOwnershipEnforced: an account cannot restore ANOTHER account's
// stopped session — it is reported as 404 (unknown session), not revealed.
func TestRestoreOwnershipEnforced(t *testing.T) {
	st := newTestStore(t)
	seedOpsKey(t, st, "k_ops", true) // ops is active + persistent-allowed
	srv := NewServer(st, "http://127.0.0.1:8000")

	// A stopped persistent session owned by ADMIN.
	priorID, _ := store.NewSessionID()
	seedPersistentSession(t, st, priorID, "admin", statusStopped, "", "fake-image-x")

	// ops (a DIFFERENT, persistent-allowed account) tries to restore it -> 404.
	rec := postSessions(t, srv, `{"restore":"`+priorID+`"}`, "Bearer k_ops")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-account restore status = %d, want 404 (no ownership leak); body=%s", rec.Code, rec.Body.String())
	}
	assertJSONDetail(t, rec.Body.Bytes())
	// The session must be untouched (still stopped, owned by admin).
	sess, _, _ := st.GetSession(priorID)
	if sess.Status != statusStopped || sess.Account != "admin" {
		t.Fatalf("victim session was mutated by a cross-account restore: %+v", sess)
	}
}

// TestRestoreRequiresPersistentCapability: a key WITHOUT the persistent
// capability cannot restore even its own (it could not have created one), and is
// rejected with 403 and the exact detail — the capability is checked first.
func TestRestoreRequiresPersistentCapability(t *testing.T) {
	st := newTestStore(t)
	seedOpsKey(t, st, "k_ops", false) // active, NOT persistent-allowed
	srv := NewServer(st, "http://127.0.0.1:8000")

	// A stopped persistent session owned by ops (seeded directly; ops couldn't
	// actually have created it, but this proves the 403 fires BEFORE the lookup).
	priorID, _ := store.NewSessionID()
	seedPersistentSession(t, st, priorID, "ops", statusStopped, "", "fake-image-x")

	rec := postSessions(t, srv, `{"restore":"`+priorID+`"}`, "Bearer k_ops")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("restore without persistent capability status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	if m["detail"] != "persistence not authorized for this API key" {
		t.Fatalf("detail = %q, want the persistence-not-authorized message", m["detail"])
	}
}

// TestRestoreNonStoppedRejected: restoring a session that exists and is owned but
// is NOT a stopped persistent session (e.g. ready, or ephemeral, or terminated)
// is rejected with 409 (it is not restorable), and an unknown id is 404.
func TestRestoreNonStoppedRejected(t *testing.T) {
	st := newTestStore(t)
	srv := NewServer(st, "http://127.0.0.1:8000")

	// A READY persistent session (owned by admin) is not restorable.
	readyID, _ := store.NewSessionID()
	seedPersistentSession(t, st, readyID, "admin", statusReady, "fake-9", "")
	if rec := postSessions(t, srv, `{"restore":"`+readyID+`"}`, "Bearer k_active"); rec.Code != http.StatusConflict {
		t.Fatalf("restore of a READY persistent session status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}

	// An EPHEMERAL session (not persistent) is not restorable -> 409.
	ephID, _ := store.NewSessionID()
	if err := st.CreateSession(&store.Session{
		ID: ephID, Account: "admin", Status: statusReady, Width: 1, Height: 1,
	}); err != nil {
		t.Fatalf("CreateSession ephemeral: %v", err)
	}
	if rec := postSessions(t, srv, `{"restore":"`+ephID+`"}`, "Bearer k_active"); rec.Code != http.StatusConflict {
		t.Fatalf("restore of an EPHEMERAL session status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}

	// A TERMINATED persistent session is not restorable -> 409.
	termID, _ := store.NewSessionID()
	seedPersistentSession(t, st, termID, "admin", statusTerminated, "", "")
	if rec := postSessions(t, srv, `{"restore":"`+termID+`"}`, "Bearer k_active"); rec.Code != http.StatusConflict {
		t.Fatalf("restore of a TERMINATED persistent session status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}

	// Unknown id -> 404.
	if rec := postSessions(t, srv, `{"restore":"s_does_not_exist"}`, "Bearer k_active"); rec.Code != http.StatusNotFound {
		t.Fatalf("restore of an unknown id status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// --- Cost-aware reaping -------------------------------------------------------

// seedPersistentReap inserts a PERSISTENT session (status set after create) with
// an instance id, returning its real CreatedAt — the clock anchor. Mirrors
// seedSession but sets Persistent so the reaper takes its persistent branch.
func seedPersistentReap(t *testing.T, st *store.Store, id, status, instanceID string) time.Time {
	t.Helper()
	if err := st.CreateSession(&store.Session{
		ID: id, Account: "admin", Status: statusProvisioning,
		Persistent: true, Width: defaultWidth, Height: defaultHeight, InstanceID: instanceID,
	}); err != nil {
		t.Fatalf("CreateSession(%s): %v", id, err)
	}
	if status != statusProvisioning {
		if err := st.UpdateSessionStatus(id, status); err != nil {
			t.Fatalf("UpdateSessionStatus(%s,%s): %v", id, status, err)
		}
	}
	sess, found, err := st.GetSession(id)
	if err != nil || !found {
		t.Fatalf("GetSession(%s): found=%v err=%v", id, found, err)
	}
	return sess.CreatedAt
}

// TestReapActivePersistentIdleSnapshotsAndStops: an ACTIVE persistent session
// reaped for IDLE is snapshot-and-stopped (instance destroyed, snapshot created,
// status 'stopped'), NOT terminated/destroyed-without-snapshot.
func TestReapActivePersistentIdleSnapshotsAndStops(t *testing.T) {
	st := newTestStore(t)
	prov := fake.New()
	clk := &testClock{}
	// Persistent lifetime distinct from (longer than) the elapsed time so idle is
	// what fires, not lifetime.
	s := NewServer(st, "",
		WithProvider(prov),
		WithClock(clk.now),
		WithProvisionConfig(ProvisionConfig{ProvisionTimeout: time.Minute}),
		WithReaperConfig(ReaperConfig{
			IdleTimeout: 30 * time.Minute, MaxLifetime: 0, OrphanGrace: time.Minute,
			PersistentMaxLifetime: 48 * time.Hour, PersistentMaxAge: 7 * 24 * time.Hour,
		}),
	)

	// Pre-create the instance so its id is live, then seed a ready persistent
	// session on it.
	inst, _ := prov.CreateInstance(context.Background(), provider.InstanceSpec{})
	created := seedPersistentReap(t, st, "p_idle", statusReady, inst.ID)
	registerLiveTunnel(t, s, "p_idle") // live tunnel: idle still fires

	clk.set(created.Add(30*time.Minute + time.Second)) // just past idle
	s.reapOnce(context.Background())

	sess, _, _ := st.GetSession("p_idle")
	if sess.Status != statusStopped {
		t.Fatalf("status = %q, want stopped (persistent idle reap = snapshot-and-stop, NOT terminate)", sess.Status)
	}
	if !prov.Deleted(inst.ID) {
		t.Fatalf("instance %s was NOT destroyed by snapshot-and-stop", inst.ID)
	}
	if prov.ImageCount() != 1 {
		t.Fatalf("ImageCount = %d, want 1 (a snapshot must be created before stop)", prov.ImageCount())
	}
	wantRef := "fake-image-ecu-persist-" + sanitizeName("p_idle")
	if sess.SnapshotImage != wantRef {
		t.Fatalf("snapshot ref = %q, want %q", sess.SnapshotImage, wantRef)
	}
}

// TestReapActivePersistentUsesPersistentLifetime proves the lifetime threshold
// is per-kind: with a SHORT ephemeral MaxLifetime and a LONGER
// PersistentMaxLifetime, a persistent session is NOT reaped before its longer
// lifetime, and IS snapshot-and-stopped once past it.
func TestReapActivePersistentUsesPersistentLifetime(t *testing.T) {
	st := newTestStore(t)
	prov := fake.New()
	clk := &testClock{}
	const ephemeralLife = time.Hour
	const persistentLife = 24 * time.Hour
	s := NewServer(st, "",
		WithProvider(prov),
		WithClock(clk.now),
		WithProvisionConfig(ProvisionConfig{ProvisionTimeout: time.Minute}),
		WithReaperConfig(ReaperConfig{
			IdleTimeout: 0, MaxLifetime: ephemeralLife, OrphanGrace: time.Minute,
			PersistentMaxLifetime: persistentLife, PersistentMaxAge: 7 * 24 * time.Hour,
		}),
	)

	inst, _ := prov.CreateInstance(context.Background(), provider.InstanceSpec{})
	created := seedPersistentReap(t, st, "p_life", statusReady, inst.ID)
	registerLiveTunnel(t, s, "p_life")

	// Past the SHORT ephemeral lifetime but within the persistent one: must NOT be
	// reaped (proves the ephemeral MaxLifetime is not applied to a persistent
	// session).
	clk.set(created.Add(ephemeralLife + time.Minute))
	s.reapOnce(context.Background())
	if sess, _, _ := st.GetSession("p_life"); sess.Status != statusReady {
		t.Fatalf("persistent session reaped at ephemeral lifetime: status = %q, want ready", sess.Status)
	}
	if prov.DeleteCount() != 0 || prov.ImageCount() != 0 {
		t.Fatalf("persistent session acted on before its lifetime: deletes=%d images=%d", prov.DeleteCount(), prov.ImageCount())
	}

	// Past the persistent lifetime: now snapshot-and-stop.
	clk.set(created.Add(persistentLife + time.Second))
	s.reapOnce(context.Background())
	sess, _, _ := st.GetSession("p_life")
	if sess.Status != statusStopped {
		t.Fatalf("status = %q, want stopped after persistent lifetime", sess.Status)
	}
	if !prov.Deleted(inst.ID) || prov.ImageCount() != 1 {
		t.Fatalf("persistent lifetime reap did not snapshot-and-stop: deleted=%v images=%d", prov.Deleted(inst.ID), prov.ImageCount())
	}
}

// TestReapActivePersistentSnapshotFailurePreservesState: if the snapshot fails
// during a reaper snapshot-and-stop, the instance is left running and the
// session left active (preserve state, retry next sweep).
func TestReapActivePersistentSnapshotFailurePreservesState(t *testing.T) {
	st := newTestStore(t)
	prov := fake.New()
	prov.CreateImageErr = io.ErrUnexpectedEOF
	clk := &testClock{}
	s := NewServer(st, "",
		WithProvider(prov),
		WithClock(clk.now),
		WithProvisionConfig(ProvisionConfig{ProvisionTimeout: time.Minute}),
		WithReaperConfig(ReaperConfig{
			IdleTimeout: 30 * time.Minute, OrphanGrace: time.Minute,
			PersistentMaxLifetime: 48 * time.Hour, PersistentMaxAge: 7 * 24 * time.Hour,
		}),
	)

	inst, _ := prov.CreateInstance(context.Background(), provider.InstanceSpec{})
	created := seedPersistentReap(t, st, "p_failsnap", statusReady, inst.ID)
	registerLiveTunnel(t, s, "p_failsnap")

	clk.set(created.Add(30*time.Minute + time.Second)) // past idle
	s.reapOnce(context.Background())

	// State preserved: instance NOT destroyed, session still ready.
	if prov.Deleted(inst.ID) {
		t.Fatalf("instance %s destroyed despite snapshot failure during reap (state lost!)", inst.ID)
	}
	if sess, _, _ := st.GetSession("p_failsnap"); sess.Status != statusReady {
		t.Fatalf("status = %q, want ready (preserve state on reaper snapshot failure)", sess.Status)
	}
}

// TestReapStoppedCulledPastMaxAge: a STOPPED persistent session whose StoppedAt
// is older than PersistentMaxAge is culled — its snapshot is DeleteImage'd and
// it is marked terminated (freeing a cap slot). MarkSessionStopped stamps
// stopped_at at real wall-clock now, so we anchor the injected reaper clock to
// that stamped instant and advance it past max-age.
func TestReapStoppedCulledPastMaxAge(t *testing.T) {
	st := newTestStore(t)
	prov := fake.New()
	clk := &testClock{}
	const maxAge = 7 * 24 * time.Hour
	s := NewServer(st, "",
		WithProvider(prov),
		WithClock(clk.now),
		WithProvisionConfig(ProvisionConfig{ProvisionTimeout: time.Minute}),
		WithReaperConfig(ReaperConfig{
			IdleTimeout: 30 * time.Minute, OrphanGrace: time.Minute,
			PersistentMaxLifetime: 48 * time.Hour, PersistentMaxAge: maxAge,
		}),
	)

	const snap = "fake-image-old-snap"
	seedPersistentSession(t, st, "p_old", "admin", statusStopped, "", snap)
	sess, _, _ := st.GetSession("p_old")
	stoppedAt := sess.StoppedAt // stamped ~real now by MarkSessionStopped

	// Just past max-age from the stop instant: cull.
	clk.set(stoppedAt.Add(maxAge + time.Minute))
	s.reapOnce(context.Background())

	if !prov.DeletedImage(snap) {
		t.Fatalf("stopped session's snapshot %q was NOT deleted on cull", snap)
	}
	if sess, _, _ := st.GetSession("p_old"); sess.Status != statusTerminated {
		t.Fatalf("stopped session status = %q, want terminated after cull", sess.Status)
	}
	// Culling freed a persistent-cap slot.
	if n, _ := st.CountNonTerminatedPersistentSessions(); n != 0 {
		t.Fatalf("persistent count after cull = %d, want 0 (cull frees a slot)", n)
	}
}

// TestReapStoppedNotCulledWithinMaxAge: a STOPPED persistent session whose
// StoppedAt is YOUNGER than PersistentMaxAge is NOT culled — snapshot kept,
// status stays stopped.
func TestReapStoppedNotCulledWithinMaxAge(t *testing.T) {
	st := newTestStore(t)
	prov := fake.New()
	clk := &testClock{}
	const maxAge = 7 * 24 * time.Hour
	s := NewServer(st, "",
		WithProvider(prov),
		WithClock(clk.now),
		WithProvisionConfig(ProvisionConfig{ProvisionTimeout: time.Minute}),
		WithReaperConfig(ReaperConfig{
			IdleTimeout: 30 * time.Minute, OrphanGrace: time.Minute,
			PersistentMaxLifetime: 48 * time.Hour, PersistentMaxAge: maxAge,
		}),
	)

	const snap = "fake-image-young-snap"
	seedPersistentSession(t, st, "p_young", "admin", statusStopped, "", snap)
	sess, _, _ := st.GetSession("p_young")
	stoppedAt := sess.StoppedAt

	// Well WITHIN max-age from the stop instant: do not cull.
	clk.set(stoppedAt.Add(maxAge / 2))
	s.reapOnce(context.Background())

	if prov.DeletedImage(snap) {
		t.Fatalf("young stopped session's snapshot %q was deleted; it is within max-age", snap)
	}
	if sess, _, _ := st.GetSession("p_young"); sess.Status != statusStopped {
		t.Fatalf("young stopped session status = %q, want still stopped", sess.Status)
	}
}

// TestReapStoppedNotIdleOrOrphanReaped: a STOPPED persistent session is excluded
// from the idle/orphan sweep entirely (it has no instance). Even with the clock
// advanced far past idle and PersistentMaxAge disabled (no cull), it stays
// stopped and nothing is destroyed.
func TestReapStoppedNotIdleOrOrphanReaped(t *testing.T) {
	st := newTestStore(t)
	prov := fake.New()
	clk := &testClock{}
	s := NewServer(st, "",
		WithProvider(prov),
		WithClock(clk.now),
		WithProvisionConfig(ProvisionConfig{ProvisionTimeout: time.Minute}),
		// Aggressive idle + orphan, but culling DISABLED (PersistentMaxAge=0) so the
		// only thing that could touch it would be the active sweep — which must not.
		WithReaperConfig(ReaperConfig{
			IdleTimeout: time.Second, OrphanGrace: time.Second,
			PersistentMaxLifetime: time.Second, PersistentMaxAge: 0,
		}),
	)

	seedPersistentSession(t, st, "p_stopped", "admin", statusStopped, "", "fake-image-keep")
	// No tunnel registered (a stopped session has none); advance far past idle.
	clk.set(time.Now().Add(time.Hour))
	s.reapOnce(context.Background())

	if sess, _, _ := st.GetSession("p_stopped"); sess.Status != statusStopped {
		t.Fatalf("stopped session status = %q, want still stopped (not idle/orphan-reaped)", sess.Status)
	}
	if prov.DeleteCount() != 0 || prov.DeletedImageCount() != 0 {
		t.Fatalf("a stopped session was acted on: instanceDeletes=%d imageDeletes=%d", prov.DeleteCount(), prov.DeletedImageCount())
	}
}

// TestReapEphemeralStillDestroys: an ephemeral idle reap is unchanged by C8 — it
// terminates and destroys (no snapshot), even with persistent reaper knobs set.
func TestReapEphemeralStillDestroys(t *testing.T) {
	st := newTestStore(t)
	prov := fake.New()
	clk := &testClock{}
	s := NewServer(st, "",
		WithProvider(prov),
		WithClock(clk.now),
		WithProvisionConfig(ProvisionConfig{ProvisionTimeout: time.Minute}),
		WithReaperConfig(ReaperConfig{
			IdleTimeout: 30 * time.Minute, OrphanGrace: time.Minute,
			PersistentMaxLifetime: 48 * time.Hour, PersistentMaxAge: 7 * 24 * time.Hour,
		}),
	)

	created := seedSession(t, st, "e_idle", statusReady, "fake-1") // ephemeral
	registerLiveTunnel(t, s, "e_idle")

	clk.set(created.Add(30*time.Minute + time.Second))
	s.reapOnce(context.Background())

	assertReaped(t, st, prov, "e_idle", "fake-1") // terminated + destroyed
	if prov.ImageCount() != 0 {
		t.Fatalf("ephemeral reap took a snapshot: ImageCount = %d, want 0", prov.ImageCount())
	}
}
