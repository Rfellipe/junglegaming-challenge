package domain

import "time"

type Wallet struct {
	id, playerID         string
	balance              Money
	version              int64
	createdAt, updatedAt time.Time
}

func NewWallet(id, player string, balance Money, now time.Time) (*Wallet, error) {
	return RestoreWallet(id, player, balance, 1, now, now)
}
func RestoreWallet(id, player string, balance Money, version int64, created, updated time.Time) (*Wallet, error) {
	if id == "" || player == "" || !balance.Valid() || balance.Minor() < 0 || version < 1 || created.IsZero() || updated.Before(created) {
		return nil, ErrInvalid
	}
	return &Wallet{id, player, balance, version, created.UTC(), updated.UTC()}, nil
}
func (w *Wallet) ID() string           { return w.id }
func (w *Wallet) PlayerID() string     { return w.playerID }
func (w *Wallet) Balance() Money       { return w.balance }
func (w *Wallet) Version() int64       { return w.version }
func (w *Wallet) CreatedAt() time.Time { return w.createdAt }
func (w *Wallet) UpdatedAt() time.Time { return w.updatedAt }
func (w *Wallet) Move(direction Direction, amount Money, now time.Time) error {
	if w == nil || !amount.Valid() || amount.Minor() <= 0 || now.Before(w.updatedAt) {
		return ErrInvalid
	}
	var next Money
	var err error
	switch direction {
	case Credit:
		next, err = w.balance.Add(amount)
	case Debit:
		next, err = w.balance.Sub(amount)
	default:
		return ErrInvalid
	}
	if err != nil {
		return err
	}
	if next.Minor() < 0 {
		return ErrInsufficientFunds
	}
	if w.version == int64(1<<63-1) {
		return ErrOverflow
	}
	w.balance, w.version, w.updatedAt = next, w.version+1, now.UTC()
	return nil
}

type Direction string

const (
	Debit  Direction = "DEBIT"
	Credit Direction = "CREDIT"
)

type LedgerEntry struct {
	id, walletID, transactionID string
	direction                   Direction
	money, before, after        Money
	created                     time.Time
}

func NewLedgerEntry(id, wallet, transaction string, direction Direction, money, before, after Money, now time.Time) (LedgerEntry, error) {
	if id == "" || wallet == "" || transaction == "" || !money.Valid() || money.Minor() <= 0 || before.Minor() < 0 || after.Minor() < 0 || now.IsZero() {
		return LedgerEntry{}, ErrInvalid
	}
	var expected Money
	var err error
	switch direction {
	case Credit:
		expected, err = before.Add(money)
	case Debit:
		expected, err = before.Sub(money)
	default:
		return LedgerEntry{}, ErrInvalid
	}
	if err != nil {
		return LedgerEntry{}, err
	}
	cmp, err := expected.Compare(after)
	if err != nil || cmp != 0 {
		return LedgerEntry{}, ErrInvalid
	}
	return LedgerEntry{id, wallet, transaction, direction, money, before, after, now.UTC()}, nil
}
