package wagering

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"junglegaming/challenge/internal/domain"
)

type TransactionView struct {
	Request
	Result
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Provider is always part of the predicate, including internal-ID lookups.
func (s *Service) Transaction(ctx context.Context, provider, identity string, external bool) (TransactionView, error) {
	if !validText(provider) || (!external && !ValidID(identity)) || (external && !validText(identity)) {
		return TransactionView{}, domain.ErrInvalid
	}
	predicate := "id=$2::uuid"
	if external {
		predicate = "external_id=$2"
	}
	o, err := scanOperation(s.pool().QueryRow(ctx, `SELECT `+operationColumns+` FROM wagering.transactions WHERE provider_id=$1 AND `+predicate, provider, identity))
	if errors.Is(err, pgx.ErrNoRows) {
		return TransactionView{}, ErrNotFound
	}
	if err != nil {
		return TransactionView{}, err
	}
	zero, _ := domain.Zero(o.request.Money.Currency())
	return TransactionView{o.request, o.result(false, zero), o.created, o.updated}, nil
}

type LedgerEntry struct {
	ID            string           `json:"id"`
	TransactionID string           `json:"transactionId"`
	Direction     domain.Direction `json:"direction"`
	Money         domain.Money     `json:"money"`
	BalanceBefore domain.Money     `json:"balanceBefore"`
	BalanceAfter  domain.Money     `json:"balanceAfter"`
	WalletVersion int64            `json:"walletVersion"`
	CreatedAt     time.Time        `json:"createdAt"`
}
type LedgerPage struct {
	Entries    []LedgerEntry `json:"entries"`
	NextCursor string        `json:"nextCursor,omitempty"`
}
type ledgerCursor struct {
	Wallet  string
	Version int64
}

func (s *Service) Ledger(ctx context.Context, wallet, cursor string, limit int) (LedgerPage, error) {
	page := LedgerPage{Entries: []LedgerEntry{}}
	if !ValidID(wallet) || limit < 1 || limit > 200 {
		return page, domain.ErrInvalid
	}
	var c ledgerCursor
	if cursor != "" {
		b, e := base64.RawURLEncoding.DecodeString(cursor)
		if e != nil || json.Unmarshal(b, &c) != nil || c.Wallet != wallet || c.Version < 1 {
			return page, domain.ErrInvalid
		}
	}
	if _, err := s.Wallet(ctx, wallet); err != nil {
		return page, err
	}
	rows, err := s.pool().Query(ctx, `SELECT id::text,transaction_id::text,direction,amount,currency,balance_before,balance_after,wallet_version,created_at FROM wagering.ledger WHERE wallet_id=$1 AND wallet_version>$2 ORDER BY wallet_version LIMIT $3`, wallet, c.Version, limit+1)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		var e LedgerEntry
		var amount, before, after int64
		var currency string
		if err = rows.Scan(&e.ID, &e.TransactionID, &e.Direction, &amount, &currency, &before, &after, &e.WalletVersion, &e.CreatedAt); err != nil {
			return page, err
		}
		e.Money, _ = domain.FromMinor(amount, currency)
		e.BalanceBefore, _ = domain.FromMinor(before, currency)
		e.BalanceAfter, _ = domain.FromMinor(after, currency)
		if len(page.Entries) == limit {
			b, _ := json.Marshal(ledgerCursor{wallet, page.Entries[limit-1].WalletVersion})
			page.NextCursor = base64.RawURLEncoding.EncodeToString(b)
			break
		}
		page.Entries = append(page.Entries, e)
	}
	return page, rows.Err()
}

type Reconciliation struct {
	WalletID          string       `json:"walletId"`
	StoredBalance     domain.Money `json:"storedBalance"`
	CalculatedBalance domain.Money `json:"calculatedBalance"`
	Difference        domain.Money `json:"difference"`
	Consistent        bool         `json:"consistent"`
	CheckedEntries    int64        `json:"checkedEntries"`
}

func (s *Service) Reconcile(ctx context.Context, id string) (Reconciliation, error) {
	var out Reconciliation
	if !ValidID(id) {
		return out, domain.ErrInvalid
	}
	// A single SQL statement uses one MVCC snapshot for wallet and ledger.
	var stored, calculated, difference int64
	var currency string
	err := s.pool().QueryRow(ctx, `SELECT w.currency,w.balance,l.total::bigint,(w.balance::numeric-l.total)::bigint,l.entries FROM wagering.wallets w CROSS JOIN LATERAL (SELECT COALESCE(sum(CASE direction WHEN 'CREDIT' THEN amount::numeric ELSE -amount::numeric END),0) total,count(*) entries FROM wagering.ledger WHERE wallet_id=w.id) l WHERE w.id=$1`, id).Scan(&currency, &stored, &calculated, &difference, &out.CheckedEntries)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	out.WalletID = id
	out.StoredBalance, _ = domain.FromMinor(stored, currency)
	out.CalculatedBalance, _ = domain.FromMinor(calculated, currency)
	out.Difference, _ = domain.FromMinor(difference, currency)
	out.Consistent = difference == 0
	s.Metrics.Add("reconciliations_total", 1)
	if difference != 0 {
		s.Metrics.Add("reconciliation_mismatches_total", 1)
	}
	s.Logger.Info("wallet reconciliation", "walletId", id, "consistent", out.Consistent, "differenceMinor", difference)

	return out, nil
}
