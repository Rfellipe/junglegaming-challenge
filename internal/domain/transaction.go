package domain

import "time"

type Kind string

const (
	Opening  Kind = "OPENING"
	Bet      Kind = "BET"
	Win      Kind = "WIN"
	Loss     Kind = "LOSS"
	Refund   Kind = "REFUND"
	Rollback Kind = "ROLLBACK"
)

type Status string

const (
	Pending          Status = "PENDING"
	PendingReference Status = "PENDING_REFERENCE"
	Processed        Status = "PROCESSED"
	Rejected         Status = "REJECTED"
	Failed           Status = "FAILED"
)

func ValidateExternal(kind Kind, money Money, reference string) error {
	if !money.Valid() || money.Minor() < 0 {
		return ErrInvalid
	}
	switch kind {
	case Bet:
		if money.Minor() == 0 || reference != "" {
			return ErrInvalid
		}
	case Win:
		if money.Minor() == 0 {
			return ErrInvalid
		}
	case Loss:
		if money.Minor() != 0 || reference != "" {
			return ErrInvalid
		}
	case Refund, Rollback:
		if money.Minor() == 0 || reference == "" {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

// Transaction owns its state machine. Adapters persist metadata separately.
type Transaction struct {
	id        string
	kind      Kind
	status    Status
	money     Money
	failure   string
	reference string
	updated   time.Time
}

func NewTransaction(id string, kind Kind, money Money, reference string, now time.Time) (*Transaction, error) {
	if err := ValidateExternal(kind, money, reference); err != nil {
		return nil, err
	}
	return RestoreTransaction(id, kind, Pending, money, "", reference, now)
}
func NewOpeningTransaction(id string, money Money, now time.Time) (*Transaction, error) {
	return RestoreTransaction(id, Opening, Processed, money, "", "", now)
}
func RestoreTransaction(id string, kind Kind, status Status, money Money, failure, reference string, now time.Time) (*Transaction, error) {
	if id == "" || !money.Valid() || money.Minor() < 0 || now.IsZero() {
		return nil, ErrInvalid
	}
	switch kind {
	case Opening, Bet, Win, Loss, Refund, Rollback:
	default:
		return nil, ErrInvalid
	}
	if (kind == Loss && money.Minor() != 0) || (kind != Loss && money.Minor() == 0) {
		return nil, ErrInvalid
	}
	switch status {
	case Pending, PendingReference, Processed:
		if failure != "" {
			return nil, ErrInvalid
		}
	case Rejected, Failed:
		if failure == "" {
			return nil, ErrInvalid
		}
	default:
		return nil, ErrInvalid
	}
	if kind == Opening && status != Processed {
		return nil, ErrInvalid
	}
	if kind == Opening {
		if reference != "" {
			return nil, ErrInvalid
		}
	} else if err := ValidateExternal(kind, money, reference); err != nil {
		return nil, err
	}
	if status == PendingReference && reference == "" {
		return nil, ErrInvalid
	}
	return &Transaction{id: id, kind: kind, status: status, money: money, failure: failure, reference: reference, updated: now.UTC()}, nil
}
func (t *Transaction) Status() Status { return t.status }
func (t *Transaction) Transition(next Status, failure string, now time.Time) error {
	if t == nil || (t.status != Pending && t.status != PendingReference) || now.Before(t.updated) {
		return ErrTransition
	}
	switch next {
	case PendingReference, Processed:
		if next == PendingReference && t.reference == "" {
			return ErrTransition
		}
		if failure != "" {
			return ErrTransition
		}
	case Rejected, Failed:
		if failure == "" {
			return ErrTransition
		}
	default:
		return ErrTransition
	}
	t.status, t.failure, t.updated = next, failure, now.UTC()
	return nil
}
