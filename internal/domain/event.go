package domain

import "time"

type Event[T any] struct {
	EventID       string    `json:"eventId"`
	EventType     string    `json:"eventType"`
	AggregateID   string    `json:"aggregateId"`
	CorrelationID string    `json:"correlationId"`
	CausationID   string    `json:"causationId,omitempty"`
	OccurredAt    time.Time `json:"occurredAt"`
	Version       int       `json:"version"`
	Data          T         `json:"data"`
}
type TransactionData struct {
	TransactionID string `json:"transactionId"`
	WalletID      string `json:"walletId"`
	PlayerID      string `json:"playerId"`
	ProviderID    string `json:"providerId,omitempty"`
	ExternalID    string `json:"externalTransactionId,omitempty"`
	RoundID       string `json:"roundId,omitempty"`
	GameID        string `json:"gameId,omitempty"`
	Kind          Kind   `json:"kind"`
	Money         Money  `json:"money"`
}
type ProcessedData struct {
	TransactionData
	Balance Money `json:"balance"`
}
type RejectedData struct {
	TransactionData
	FailureCode string `json:"failureCode"`
}
type PendingReferenceData struct {
	TransactionData
	ReferenceExternalID string `json:"referenceExternalTransactionId"`
}
type BalanceChangedData struct {
	WalletID      string    `json:"walletId"`
	TransactionID string    `json:"transactionId"`
	Direction     Direction `json:"direction"`
	Money         Money     `json:"money"`
	BalanceBefore Money     `json:"balanceBefore"`
	BalanceAfter  Money     `json:"balanceAfter"`
	WalletVersion int64     `json:"walletVersion"`
}

func envelope[T any](id, kind, aggregate, correlation string, now time.Time, data T) Event[T] {
	return Event[T]{EventID: id, EventType: kind, AggregateID: aggregate, CorrelationID: correlation, OccurredAt: now.UTC(), Version: 1, Data: data}
}
func NewProcessedEvent(id, correlation string, now time.Time, data ProcessedData) Event[ProcessedData] {
	return envelope(id, "WagerTransactionProcessed", data.TransactionID, correlation, now, data)
}
func NewRejectedEvent(id, correlation string, now time.Time, data RejectedData) Event[RejectedData] {
	return envelope(id, "WagerTransactionRejected", data.TransactionID, correlation, now, data)
}
func NewPendingReferenceEvent(id, correlation string, now time.Time, data PendingReferenceData) Event[PendingReferenceData] {
	return envelope(id, "WagerTransactionPendingReference", data.TransactionID, correlation, now, data)
}
func NewBalanceChangedEvent(id, correlation string, now time.Time, data BalanceChangedData) Event[BalanceChangedData] {
	return envelope(id, "WalletBalanceChanged", data.WalletID, correlation, now, data)
}
