//go:build integration

package wagering

import (
	"context"
	"errors"
	"strings"
	"testing"

	"junglegaming/challenge/internal/domain"
)

func TestQueriesInboxAndLeases(t *testing.T) {
	s, pool, _ := testService(t)
	ctx := context.Background()
	w := wallet(t, s, "100.00")
	r := request(t, w, domain.Bet, "25.00")
	result, err := s.ProcessMessage(ctx, r, r.ExternalID, "message", strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.ProcessMessage(ctx, r, r.ExternalID, "message", strings.Repeat("a", 64))
	if err != nil || !replay.IdempotentReplay || replay.TransactionID != result.TransactionID {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	if _, err = s.ProcessMessage(ctx, r, r.ExternalID, "message", strings.Repeat("b", 64)); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if scalar(t, pool, `SELECT count(*) FROM wagering.inbox WHERE completed_at IS NOT NULL`) != 1 {
		t.Fatal("inbox not completed")
	}
	if _, err = s.Transaction(ctx, "provider-b", result.TransactionID, false); !errors.Is(err, ErrNotFound) {
		t.Fatal("provider leak", err)
	}
	if got, err := s.Transaction(ctx, r.ProviderID, r.ExternalID, true); err != nil || got.Balance.Minor() != 7500 {
		t.Fatal(got, err)
	}
	first, err := s.Ledger(ctx, w.ID, "", 1)
	if err != nil || len(first.Entries) != 1 || first.NextCursor == "" {
		t.Fatal(first, err)
	}
	second, err := s.Ledger(ctx, w.ID, first.NextCursor, 1)
	if err != nil || len(second.Entries) != 1 || second.NextCursor != "" || first.Entries[0].ID == second.Entries[0].ID {
		t.Fatal(second, err)
	}
	if _, err = s.Ledger(ctx, ID(), first.NextCursor, 1); !errors.Is(err, domain.ErrInvalid) {
		t.Fatal("cursor wallet binding", err)
	}
	rec, err := s.Reconcile(ctx, w.ID)
	if err != nil || !rec.Consistent || rec.CheckedEntries != 2 || rec.CalculatedBalance.Minor() != 7500 {
		t.Fatal(rec, err)
	}
	a, err := s.ClaimEvent(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.ClaimEvent(ctx)
	if err != nil || a.EventID == b.EventID {
		t.Fatal("claim overlap", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE wagering.outbox SET lease_until=now()-interval '1 second' WHERE event_id=$1`, a.EventID); err != nil {
		t.Fatal(err)
	}
	// Make this the only eligible row, then claim with a new owner.
	if _, err = pool.Exec(ctx, `UPDATE wagering.outbox SET next_attempt_at=now()+interval '1 hour' WHERE event_id<>$1`, a.EventID); err != nil {
		t.Fatal(err)
	}
	recovered, err := s.ClaimEvent(ctx)
	if err != nil || recovered.EventID != a.EventID || recovered.Token == a.Token {
		t.Fatal(recovered, err)
	}
	if err = s.CompleteEvent(ctx, a, true); err != nil {
		t.Fatal(err)
	}
	if scalar(t, pool, `SELECT count(*) FROM wagering.outbox WHERE event_id=$1 AND published_at IS NOT NULL`, a.EventID) != 0 {
		t.Fatal("stale owner completed event")
	}
	if err = s.CompleteEvent(ctx, recovered, true); err != nil {
		t.Fatal(err)
	}
}

// Deliberate corruption bypasses triggers only in this disposable test database.
func TestReconciliationReportsDifferenceWithoutRepair(t *testing.T) {
	s, pool, _ := testService(t)
	ctx := context.Background()
	w := wallet(t, s, "100.00")
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SET LOCAL session_replication_role=replica`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE wagering.wallets SET balance=10001 WHERE id=$1`, w.ID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	rec, err := s.Reconcile(ctx, w.ID)
	if err != nil || rec.Consistent || rec.Difference.Minor() != 1 || rec.CalculatedBalance.Minor() != 10000 {
		t.Fatal(rec, err)
	}
	got, err := s.Wallet(ctx, w.ID)
	if err != nil || got.Balance.Minor() != 10001 {
		t.Fatal("reconciliation mutated wallet", got, err)
	}
}

func TestInboxAtomicFailureAndReferenceWait(t *testing.T) {
	s, pool, _ := testService(t)
	ctx := context.Background()
	w := wallet(t, s, "100.00")
	r := request(t, w, domain.Bet, "1.00")
	_, err := pool.Exec(ctx, `CREATE FUNCTION wagering.fail_event() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected'; END $$; CREATE TRIGGER fail_event BEFORE INSERT ON wagering.outbox FOR EACH ROW EXECUTE FUNCTION wagering.fail_event()`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ProcessMessage(ctx, r, r.ExternalID, "atomic-failure", strings.Repeat("a", 64)); err == nil {
		t.Fatal("expected injected error")
	}
	if scalar(t, pool, `SELECT count(*) FROM wagering.inbox`) != 0 || scalar(t, pool, `SELECT count(*) FROM wagering.transactions WHERE external_id=$1`, r.ExternalID) != 0 {
		t.Fatal("partial commit")
	}
	got, err := s.Wallet(ctx, w.ID)
	if err != nil || got.Balance.Minor() != 10000 {
		t.Fatal(got, err)
	}
	if _, err = pool.Exec(ctx, `DROP TRIGGER fail_event ON wagering.outbox; DROP FUNCTION wagering.fail_event()`); err != nil {
		t.Fatal(err)
	}
	r.Kind = domain.Refund
	r.ReferenceExternalID = ID()
	result, err := s.ProcessMessage(ctx, r, r.ExternalID, "reference-wait", strings.Repeat("b", 64))
	if err != nil || result.Status != domain.PendingReference {
		t.Fatal(result, err)
	}
	if scalar(t, pool, `SELECT count(*) FROM wagering.inbox WHERE message_id='reference-wait' AND completed_at IS NOT NULL`) != 1 {
		t.Fatal("pending reference not durable")
	}
}
