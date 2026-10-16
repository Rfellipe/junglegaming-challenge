//go:build integration

package wagering

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"junglegaming/challenge/internal/domain"
	"junglegaming/challenge/internal/migration"
)

func testService(t *testing.T) (*Service, *pgxpool.Pool, string) {
	t.Helper()
	ctx := context.Background()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://wagering:wagering-local@localhost:5432/wagering?sslmode=disable"
	}
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("finance_test_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		admin.Close(ctx)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, err := admin.Exec(context.Background(), "DROP DATABASE "+quoted+" WITH (FORCE)")
		if err != nil {
			t.Error(err)
		}
		admin.Close(context.Background())
	})
	u, _ := url.Parse(dsn)
	u.Path = "/" + name
	dir, err := filepath.Abs("../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	if err = migration.Run(u.String(), dir, "up"); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return New(func() *pgxpool.Pool { return pool }), pool, u.String()
}
func cash(t *testing.T, amount string) domain.Money {
	t.Helper()
	m, err := domain.Parse(amount, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return m
}
func wallet(t *testing.T, s *Service, amount string) WalletView {
	t.Helper()
	w, err := s.CreateWallet(context.Background(), ID(), cash(t, amount))
	if err != nil {
		t.Fatal(err)
	}
	return w
}
func request(t *testing.T, w WalletView, kind domain.Kind, amount string) Request {
	t.Helper()
	return Request{ProviderID: "provider-a", ExternalID: ID(), PlayerID: w.PlayerID, WalletID: w.ID, RoundID: "round", GameID: "game", Kind: kind, Money: cash(t, amount)}
}
func process(t *testing.T, s *Service, r Request) Result {
	t.Helper()
	result, err := s.Process(context.Background(), r, r.ExternalID)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func scalar(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
func due(t *testing.T, pool *pgxpool.Pool, id string, attempts int) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE wagering.transactions SET next_attempt_at=now(),attempts=$2 WHERE id=$1`, id, attempts); err != nil {
		t.Fatal(err)
	}
}
func TestFinancialOperations(t *testing.T) {
	s, pool, dsn := testService(t)
	ctx := context.Background()
	t.Run("opening zero and uniqueness", func(t *testing.T) {
		w := wallet(t, s, "100.00")
		if w.Version != 1 || scalar(t, pool, `SELECT count(*) FROM wagering.ledger WHERE wallet_id=$1`, w.ID) != 1 || scalar(t, pool, `SELECT count(*) FROM wagering.outbox WHERE payload->'data'->>'walletId'=$1`, w.ID) != 2 {
			t.Fatal("opening records")
		}
		if _, err := s.CreateWallet(ctx, w.PlayerID, cash(t, "100.00")); !errors.Is(err, ErrConflict) {
			t.Fatal("duplicate wallet", err)
		}
		zero := wallet(t, s, "0.00")
		if scalar(t, pool, `SELECT count(*) FROM wagering.transactions WHERE wallet_id=$1`, zero.ID) != 0 || scalar(t, pool, `SELECT count(*) FROM wagering.outbox WHERE payload->'data'->>'walletId'=$1`, zero.ID) != 0 {
			t.Fatal("zero opening has financial records")
		}
		if result := process(t, s, request(t, zero, domain.Win, "5.00")); result.Status != domain.Processed || result.Balance.Amount() != "5.00" {
			t.Fatal(result)
		}
	})
	t.Run("original replay balance conflicts and restart", func(t *testing.T) {
		w := wallet(t, s, "100.00")
		r := request(t, w, domain.Bet, "25.00")
		first := process(t, s, r)
		if first.Status != domain.Processed || first.Balance.Amount() != "75.00" {
			t.Fatal(first)
		}
		process(t, s, request(t, w, domain.Win, "10.00"))
		otherPool, err := pgxpool.New(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer otherPool.Close()
		restarted := New(func() *pgxpool.Pool { return otherPool })
		replay := process(t, restarted, r)
		if !replay.IdempotentReplay || replay.TransactionID != first.TransactionID || replay.Balance.Amount() != "75.00" {
			t.Fatal(replay)
		}
		changed := r
		changed.Money = cash(t, "26.00")
		if _, err := s.Process(ctx, changed, r.ExternalID); !errors.Is(err, ErrConflict) {
			t.Fatal("payload conflict", err)
		}
		changed = r
		changed.WalletID = ID()
		if _, err := s.Process(ctx, changed, r.ExternalID); !errors.Is(err, ErrConflict) {
			t.Fatal("changed missing wallet should conflict", err)
		}
		changed = r
		changed.PlayerID = ID()
		if _, err := s.Process(ctx, changed, r.ExternalID); !errors.Is(err, ErrConflict) {
			t.Fatal("changed player should conflict", err)
		}
		if _, err := s.Process(ctx, r, "another-key"); !errors.Is(err, ErrConflict) {
			t.Fatal("external identity conflict", err)
		}
		changed = r
		changed.ExternalID = ID()
		if _, err := s.Process(ctx, changed, r.ExternalID); !errors.Is(err, ErrConflict) {
			t.Fatal("key conflict", err)
		}
		before, _ := s.Wallet(ctx, w.ID)
		loss := process(t, s, request(t, w, domain.Loss, "0.00"))
		after, _ := s.Wallet(ctx, w.ID)
		if loss.Status != domain.Processed || before != after || scalar(t, pool, `SELECT count(*) FROM wagering.ledger WHERE transaction_id=$1`, loss.TransactionID) != 0 || scalar(t, pool, `SELECT count(*) FROM wagering.outbox WHERE payload->'data'->>'transactionId'=$1`, loss.TransactionID) != 1 {
			t.Fatal("LOSS mutated balance/version or wrong events")
		}
	})
	t.Run("50 concurrent duplicates and competing writers", func(t *testing.T) {
		w := wallet(t, s, "100.00")
		r := request(t, w, domain.Bet, "25.00")
		var wg sync.WaitGroup
		results := make(chan Result, 50)
		failures := make(chan error, 50)
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				result, err := s.Process(ctx, r, r.ExternalID)
				if err != nil {
					failures <- err
				} else {
					results <- result
				}
			}()
		}
		wg.Wait()
		close(failures)
		close(results)
		for err := range failures {
			t.Error(err)
		}
		fresh := 0
		for result := range results {
			if !result.IdempotentReplay {
				fresh++
			}
			if result.Balance.Amount() != "75.00" {
				t.Error(result)
			}
		}
		if fresh != 1 || scalar(t, pool, `SELECT count(*) FROM wagering.ledger WHERE wallet_id=$1 AND direction='DEBIT'`, w.ID) != 1 {
			t.Fatal("duplicate debit", fresh)
		}
		w = wallet(t, s, "100.00")
		pair := []Request{request(t, w, domain.Bet, "80.00"), request(t, w, domain.Bet, "80.00")}
		out := make(chan Result, 2)
		errs := make(chan error, 2)
		for _, r := range pair {
			wg.Add(1)
			go func(r Request) {
				defer wg.Done()
				result, err := s.Process(ctx, r, r.ExternalID)
				errs <- err
				out <- result
			}(r)
		}
		wg.Wait()
		close(out)
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		success, rejected := 0, 0
		for result := range out {
			if result.Status == domain.Processed {
				success++
			}
			if result.FailureCode == "INSUFFICIENT_FUNDS" {
				rejected++
			}
		}
		current, err := s.Wallet(ctx, w.ID)
		if err != nil || current.Balance.Amount() != "20.00" || success != 1 || rejected != 1 || scalar(t, pool, `SELECT count(*) FROM wagering.ledger WHERE wallet_id=$1 AND direction='DEBIT'`, w.ID) != 1 {
			t.Fatal(current, success, rejected, err)
		}
		for _, r := range pair {
			if !process(t, s, r).IdempotentReplay {
				t.Fatal("not replayed")
			}
		}
	})
	t.Run("independent wallet advances while one is locked", func(t *testing.T) {
		blocked := wallet(t, s, "100.00")
		free := wallet(t, s, "100.00")
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err = tx.Exec(ctx, `SELECT id FROM wagering.wallets WHERE id=$1 FOR UPDATE`, blocked.ID); err != nil {
			t.Fatal(err)
		}
		short, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		r := request(t, free, domain.Bet, "1.00")
		if _, err = s.Process(short, r, r.ExternalID); err != nil {
			t.Fatal("independent wallet blocked", err)
		}
	})
	t.Run("rejected credit overflow is audited", func(t *testing.T) {
		w := wallet(t, s, "92233720368547758.07")
		result := process(t, s, request(t, w, domain.Win, "0.01"))
		if result.Status != domain.Rejected || result.FailureCode != "BALANCE_OVERFLOW" {
			t.Fatal(result)
		}
	})
}

func TestReversalsAndReferenceRecovery(t *testing.T) {
	s, pool, dsn := testService(t)
	ctx := context.Background()
	t.Run("refund rollback and duplicate return protection", func(t *testing.T) {
		w := wallet(t, s, "100.00")
		bet := request(t, w, domain.Bet, "25.00")
		process(t, s, bet)
		refund := request(t, w, domain.Refund, "25.00")
		refund.ReferenceExternalID = bet.ExternalID
		if got := process(t, s, refund); got.Balance.Amount() != "100.00" || got.Status != domain.Processed {
			t.Fatal(got)
		}
		rollback := request(t, w, domain.Rollback, "25.00")
		rollback.ReferenceExternalID = bet.ExternalID
		if got := process(t, s, rollback); got.FailureCode != "ALREADY_REVERSED" {
			t.Fatal(got)
		}
		rollback.ExternalID = ID()
		rollback.ReferenceExternalID = refund.ExternalID
		if got := process(t, s, rollback); got.Status != domain.Processed || got.Balance.Amount() != "75.00" {
			t.Fatal(got)
		}
		refund.ExternalID = ID()
		if got := process(t, s, refund); got.FailureCode != "ALREADY_REVERSED" {
			t.Fatal(got)
		}
	})
	t.Run("rollback credit requires sufficient funds", func(t *testing.T) {
		w := wallet(t, s, "0.00")
		win := request(t, w, domain.Win, "50.00")
		process(t, s, win)
		process(t, s, request(t, w, domain.Bet, "50.00"))
		r := request(t, w, domain.Rollback, "50.00")
		r.ReferenceExternalID = win.ExternalID
		if got := process(t, s, r); got.FailureCode != "REVERSAL_INSUFFICIENT_FUNDS" || got.Balance.Amount() != "0.00" {
			t.Fatal(got)
		}
	})
	t.Run("missing reference resolves after service restart", func(t *testing.T) {
		w := wallet(t, s, "100.00")
		bet := request(t, w, domain.Bet, "25.00")
		refund := request(t, w, domain.Refund, "25.00")
		refund.ReferenceExternalID = bet.ExternalID
		pending := process(t, s, refund)
		if pending.Status != domain.PendingReference {
			t.Fatal(pending)
		}
		if !process(t, s, refund).IdempotentReplay {
			t.Fatal("pending replay")
		}
		process(t, s, bet)
		due(t, pool, pending.TransactionID, 0)
		otherPool, err := pgxpool.New(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer otherPool.Close()
		restarted := New(func() *pgxpool.Pool { return otherPool })
		if worked, err := restarted.RetryReference(ctx); err != nil || !worked {
			t.Fatal(worked, err)
		}
		result := process(t, restarted, refund)
		if result.Status != domain.Processed || result.Balance.Amount() != "100.00" {
			t.Fatal(result)
		}
		if scalar(t, pool, `SELECT count(*) FROM wagering.outbox WHERE event_type='WagerTransactionPendingReference' AND aggregate_id=$1`, pending.TransactionID) != 1 {
			t.Fatal("pending event count")
		}
	})
	t.Run("retry backoff exhaustion and rejected reference", func(t *testing.T) {
		w := wallet(t, s, "100.00")
		refund := request(t, w, domain.Refund, "25.00")
		refund.ReferenceExternalID = ID()
		pending := process(t, s, refund)
		due(t, pool, pending.TransactionID, 0)
		if worked, err := s.RetryReference(ctx); err != nil || !worked {
			t.Fatal(worked, err)
		}
		if scalar(t, pool, `SELECT count(*) FROM wagering.transactions WHERE id=$1 AND attempts=1 AND next_attempt_at>updated_at AND status='PENDING_REFERENCE'`, pending.TransactionID) != 1 {
			t.Fatal("backoff missing")
		}
		due(t, pool, pending.TransactionID, 11)
		if worked, err := s.RetryReference(ctx); err != nil || !worked {
			t.Fatal(worked, err)
		}
		if got := process(t, s, refund); got.FailureCode != "REFERENCE_NOT_FOUND" || got.Status != domain.Rejected {
			t.Fatal(got)
		}
		bet := request(t, w, domain.Bet, "200.00")
		process(t, s, bet)
		refund.ExternalID = ID()
		refund.ReferenceExternalID = bet.ExternalID
		refund.Money = cash(t, "200.00")
		if got := process(t, s, refund); got.FailureCode != "REFERENCE_NOT_PROCESSED" {
			t.Fatal(got)
		}
	})
	t.Run("reference validation and optional WIN reference", func(t *testing.T) {
		w := wallet(t, s, "100.00")
		bet := request(t, w, domain.Bet, "25.00")
		process(t, s, bet)
		for _, field := range []string{"amount", "round", "game", "player", "wallet"} {
			r := request(t, w, domain.Refund, "25.00")
			r.ReferenceExternalID = bet.ExternalID
			if field == "amount" {
				r.Money = cash(t, "24.00")
			}
			if field == "round" {
				r.RoundID = "other"
			}
			if field == "game" {
				r.GameID = "other"
			}
			if field == "player" || field == "wallet" {
				other := wallet(t, s, "0.00")
				r.PlayerID, r.WalletID = other.PlayerID, other.ID
			}
			if got := process(t, s, r); got.Status != domain.Rejected {
				t.Fatal(field, got)
			}
		}
		win := request(t, w, domain.Win, "10.00")
		win.ReferenceExternalID = bet.ExternalID
		if got := process(t, s, win); got.Status != domain.Processed || got.Balance.Amount() != "85.00" {
			t.Fatal(got)
		}
		self := request(t, w, domain.Rollback, "1.00")
		self.ReferenceExternalID = self.ExternalID
		if got := process(t, s, self); got.FailureCode != "INVALID_REFERENCE" {
			t.Fatal(got)
		}
	})
	t.Run("concurrent reversal exclusivity", func(t *testing.T) {
		w := wallet(t, s, "100.00")
		bet := request(t, w, domain.Bet, "25.00")
		process(t, s, bet)
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		results := make(chan Result, 2)
		for _, kind := range []domain.Kind{domain.Refund, domain.Rollback} {
			r := request(t, w, kind, "25.00")
			r.ReferenceExternalID = bet.ExternalID
			wg.Add(1)
			go func() {
				defer wg.Done()
				result, err := s.Process(ctx, r, r.ExternalID)
				errs <- err
				results <- result
			}()
		}
		wg.Wait()
		close(errs)
		close(results)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		success := 0
		for result := range results {
			if result.Status == domain.Processed {
				success++
			}
		}
		if success != 1 {
			t.Fatal("duplicate reversal", success)
		}
	})
}

func TestFinancialDatabaseGuardsAndAtomicity(t *testing.T) {
	s, pool, _ := testService(t)
	ctx := context.Background()
	w := wallet(t, s, "100.00")
	r := request(t, w, domain.Bet, "25.00")
	result := process(t, s, r)
	for _, sql := range []string{
		`UPDATE wagering.ledger SET amount=amount WHERE wallet_id=$1`,
		`DELETE FROM wagering.ledger WHERE wallet_id=$1`,
		`UPDATE wagering.wallets SET balance=-1 WHERE id=$1`,
		`UPDATE wagering.wallets SET balance=balance+1 WHERE id=$1`,
		`UPDATE wagering.wallets SET version=version+1 WHERE id=$1`,
		`UPDATE wagering.wallets SET balance=balance+1,version=version+1 WHERE id=$1`,
		`UPDATE wagering.transactions SET status='PENDING' WHERE wallet_id=$1`,
		`UPDATE wagering.outbox SET payload='{}' WHERE payload->'data'->>'walletId'=$1::text`,
	} {
		if _, err := pool.Exec(ctx, sql, w.ID); err == nil {
			t.Errorf("database accepted: %s", sql)
		}
	}
	if _, err := pool.Exec(ctx, `TRUNCATE wagering.ledger`); err == nil {
		t.Fatal("ledger truncate allowed")
	}
	// Inject an event write failure to verify the entire financial transaction rolls back.
	if _, err := pool.Exec(ctx, `CREATE FUNCTION wagering.fail_event() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected event failure'; END $$; CREATE TRIGGER fail_event BEFORE INSERT ON wagering.outbox FOR EACH ROW EXECUTE FUNCTION wagering.fail_event()`); err != nil {
		t.Fatal(err)
	}
	failed := request(t, w, domain.Bet, "10.00")
	if _, err := s.Process(ctx, failed, failed.ExternalID); err == nil {
		t.Fatal("expected injected error")
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER fail_event ON wagering.outbox; DROP FUNCTION wagering.fail_event()`); err != nil {
		t.Fatal(err)
	}
	current, err := s.Wallet(ctx, w.ID)
	if err != nil || current.Balance.Amount() != "75.00" || scalar(t, pool, `SELECT count(*) FROM wagering.transactions WHERE external_id=$1`, failed.ExternalID) != 0 || scalar(t, pool, `SELECT count(*) FROM wagering.ledger WHERE wallet_id=$1`, w.ID) != 2 {
		t.Fatal("partial financial commit", current, err)
	}
	if scalar(t, pool, `SELECT count(*) FROM wagering.transactions WHERE id=$1 AND result_balance=7500`, result.TransactionID) != 1 {
		t.Fatal("original result changed")
	}
}

func TestReferenceExpiryAndConcurrentWorkers(t *testing.T) {
	s, pool, _ := testService(t)
	ctx := context.Background()
	w := wallet(t, s, "100.00")
	bet := request(t, w, domain.Bet, "25.00")
	refund := request(t, w, domain.Refund, "25.00")
	refund.ReferenceExternalID = bet.ExternalID
	pending := process(t, s, refund)
	process(t, s, bet)
	// Advance only the use case's explicit time argument. No sleeps or changes to
	// immutable expiry metadata are necessary to exercise the TTL boundary.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	locked, err := scanWallet(tx.QueryRow(ctx, `SELECT `+walletColumns+` FROM wagering.wallets WHERE id=$1 FOR UPDATE`, w.ID))
	if err != nil {
		t.Fatal(err)
	}
	o, err := scanOperation(tx.QueryRow(ctx, `SELECT `+operationColumns+` FROM wagering.transactions WHERE id=$1 FOR UPDATE`, pending.TransactionID))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.apply(ctx, tx, &o, locked, o.expires.Add(time.Second), true); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if got := process(t, s, refund); got.FailureCode != domain.ReferenceNotFound || got.Status != domain.Rejected {
		t.Fatal(got)
	}

	w = wallet(t, s, "100.00")
	bet = request(t, w, domain.Bet, "25.00")
	refund = request(t, w, domain.Refund, "25.00")
	refund.ReferenceExternalID = bet.ExternalID
	pending = process(t, s, refund)
	process(t, s, bet)
	due(t, pool, pending.TransactionID, 0)
	var wg sync.WaitGroup
	errors := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := s.RetryReference(ctx); errors <- err }()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if scalar(t, pool, `SELECT count(*) FROM wagering.ledger WHERE transaction_id=$1`, pending.TransactionID) != 1 || process(t, s, refund).Status != domain.Processed {
		t.Fatal("workers duplicated or lost operation")
	}
}
