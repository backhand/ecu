package controlplane

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/backhand/ecu/internal/store"
)

// persistSnapshotName is the per-session snapshot NAME a persistent session is
// saved under. It is stable and derived from the session id (sanitizeName
// mirrors instanceName's RFC-1123 sanitization) so the snapshot is
// unambiguously tied to its session. The reference actually used to BOOT a
// restore is the snapshot's id (bootRefForImage / hcloud's numeric-id path),
// not this name; the name is the human-facing description/label.
func persistSnapshotName(sessionID string) string {
	return "ecu-persist-" + sanitizeName(sessionID)
}

// snapshotAndStop is the shared "preserve a persistent session's state and stop
// it" operation behind BOTH the persistent DELETE and the cost-aware reaper's
// active-persistent branch. It snapshots the session's live instance, then (only
// on snapshot success) destroys the instance, marks the session 'stopped'
// carrying the NEW snapshot ref, and best-effort deletes any PRIOR snapshot so a
// persistent session keeps at most one.
//
// Ordering is state-preserving and concurrency-safe (mirrors the C5 reaper
// invariants):
//
//  1. CreateImage FIRST. If it FAILS, return the error and DO NOTHING else — the
//     instance is left running and the session is left as-is, so no saved work
//     is lost. The caller maps this to 500 (DELETE) or a logged retry-next-sweep
//     (reaper).
//  2. On snapshot success, DeleteInstance (idempotent — a concurrent DELETE /
//     reap that also destroyed it is harmless).
//  3. MarkSessionStopped(newRef): a single UPDATE flipping status='stopped',
//     snapshot_image=newRef, stopped_at=now. This is the state transition; a
//     racing actor that re-reads the now-stopped session short-circuits.
//  4. Delete the PRIOR snapshot ref (if any, and different from the new one),
//     best-effort: DeleteImage is idempotent, so a failure or a double call just
//     leaves an orphan image to retry, never loses the live state.
//
// A duplicate snapshot from a concurrent DELETE+reap creates a second image; the
// prior-snapshot cleanup on the next end reclaims it, so it is at worst a
// transient extra image, never a lost state or a leaked instance.
//
// It deliberately derives its OWN bounded context (not a caller's) for the
// commit-critical CreateImage/DeleteImage so a cancelled request context (DELETE
// returns) or a tripped reaper-sweep cannot abort the snapshot we are about to
// rely on — mirroring teardownInstance and the C7 baker.
func (s *Server) snapshotAndStop(sess *store.Session) error {
	priorSnapshot := sess.SnapshotImage

	// 1. Snapshot FIRST. Use a fresh bounded context (not the request context,
	// which may be cancelled the instant DELETE returns) so the snapshot we are
	// about to rely on cannot be aborted mid-flight.
	snapCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	img, err := s.provider.CreateImage(snapCtx, sess.InstanceID, persistSnapshotName(sess.ID))
	if err != nil {
		// PRESERVE STATE: do not destroy the instance, do not mark stopped.
		return fmt.Errorf("snapshot persistent session %s instance %s: %w", sess.ID, sess.InstanceID, err)
	}
	newRef := bootRefForImage(img)

	// 2. Snapshot succeeded — now destroy the instance (idempotent).
	s.teardownInstance(sess.ID, sess.InstanceID)

	// 3. Transition to 'stopped' carrying the new snapshot ref + stopped_at=now.
	if err := s.store.MarkSessionStopped(sess.ID, newRef); err != nil {
		// The instance is already gone and the snapshot exists; a failed status
		// write is logged but not fatal — the next sweep / a retry reconciles. We
		// still attempt the prior-snapshot cleanup below.
		log.Printf("ecu persist: session %s: mark stopped (snapshot=%s): %v", sess.ID, newRef, err)
	}

	// 4. Best-effort delete of the PRIOR snapshot (each session keeps one). Skip
	// when there was none or it is somehow the same ref as the new one.
	if priorSnapshot != "" && priorSnapshot != newRef {
		if err := s.provider.DeleteImage(snapCtx, priorSnapshot); err != nil {
			log.Printf("ecu persist: session %s: FAILED to delete prior snapshot %s (orphan image, will retry next end): %v", sess.ID, priorSnapshot, err)
		} else {
			log.Printf("ecu persist: session %s: deleted prior snapshot %s (replaced by %s)", sess.ID, priorSnapshot, newRef)
		}
	}
	log.Printf("ecu persist: session %s snapshotted to %s and stopped (instance %s destroyed)", sess.ID, newRef, sess.InstanceID)
	return nil
}

// cullStoppedSession ages out a STOPPED persistent session: it deletes the saved
// snapshot and marks the session terminated (freeing a persistent-cap slot). It
// is the cost-aware reaper's stopped-session branch.
//
// DeleteImage is idempotent and the snapshot is deleted BEFORE the terminate, so
// culling is retry-safe: if the terminate write is lost the next sweep re-deletes
// the (already-gone) image and retries the terminate. A terminated session no
// longer appears in ListStoppedPersistentSessions, so it is culled at most once.
// Like snapshotAndStop it derives its own bounded context for the delete.
func (s *Server) cullStoppedSession(sess store.Session) {
	log.Printf("ecu reaper: culling stopped persistent session %s (snapshot=%s, stopped_at=%s)", sess.ID, sess.SnapshotImage, sess.StoppedAt.Format(time.RFC3339))
	if sess.SnapshotImage != "" && s.provider != nil {
		delCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		if err := s.provider.DeleteImage(delCtx, sess.SnapshotImage); err != nil {
			// Leave the session stopped so the next sweep retries the delete; do not
			// terminate while the snapshot may still exist (avoid a leaked image we
			// can no longer find via the session row).
			log.Printf("ecu reaper: session %s: FAILED to delete snapshot %s during cull (will retry): %v", sess.ID, sess.SnapshotImage, err)
			cancel()
			return
		}
		cancel()
	}
	if err := s.store.UpdateSessionStatus(sess.ID, statusTerminated); err != nil {
		log.Printf("ecu reaper: session %s: mark terminated during cull: %v", sess.ID, err)
	}
}
