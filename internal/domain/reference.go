package domain

const (
	InvalidReference          = "INVALID_REFERENCE"
	ReferenceMismatch         = "REFERENCE_MISMATCH"
	InvalidReferenceKind      = "INVALID_REFERENCE_KIND"
	ReferenceAmountMismatch   = "REFERENCE_AMOUNT_MISMATCH"
	ReferenceNotProcessed     = "REFERENCE_NOT_PROCESSED"
	ReferenceNotFound         = "REFERENCE_NOT_FOUND"
	AlreadyReversed           = "ALREADY_REVERSED"
	InsufficientFunds         = "INSUFFICIENT_FUNDS"
	ReversalInsufficientFunds = "REVERSAL_INSUFFICIENT_FUNDS"
	BalanceOverflow           = "BALANCE_OVERFLOW"
)

// Reference is a value snapshot; resolving it cannot mutate either operation.
type Reference struct {
	TransactionData
	Status    Status
	Direction Direction
}
type ReferenceResolution struct {
	Direction   Direction
	FailureCode string
	Waiting     bool
}

func ResolveReference(current TransactionData, ref Reference) ReferenceResolution {
	reject := func(code string) ReferenceResolution { return ReferenceResolution{FailureCode: code} }
	if !current.Money.Valid() || !ref.Money.Valid() || current.TransactionID == "" || ref.TransactionID == "" || current.TransactionID == ref.TransactionID {
		return reject(InvalidReference)
	}
	if current.ProviderID != ref.ProviderID || current.PlayerID != ref.PlayerID || current.WalletID != ref.WalletID || current.RoundID != ref.RoundID || current.GameID != ref.GameID || current.Money.Currency() != ref.Money.Currency() {
		return reject(ReferenceMismatch)
	}
	switch current.Kind {
	case Refund, Win:
		if ref.Kind != Bet {
			return reject(InvalidReferenceKind)
		}
	case Rollback:
		if ref.Kind != Bet && ref.Kind != Win && ref.Kind != Refund {
			return reject(InvalidReferenceKind)
		}
	default:
		return reject(InvalidReferenceKind)
	}
	if current.Kind != Win && current.Money.Minor() != ref.Money.Minor() {
		return reject(ReferenceAmountMismatch)
	}
	if ref.Status == Pending || ref.Status == PendingReference {
		return ReferenceResolution{Waiting: true}
	}
	if ref.Status != Processed {
		return reject(ReferenceNotProcessed)
	}
	direction := Credit
	if current.Kind == Rollback {
		switch ref.Direction {
		case Credit:
			direction = Debit
		case Debit:
			direction = Credit
		default:
			return reject(InvalidReference)
		}
	}
	return ReferenceResolution{Direction: direction}
}
